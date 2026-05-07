/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package nodelocalhotplug

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/devices"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/types"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	diskutils "kubevirt.io/kubevirt/pkg/ephemeral-disk-utils"
	hotplugdisk "kubevirt.io/kubevirt/pkg/hotplug-disk"
	"kubevirt.io/kubevirt/pkg/safepath"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/virt-handler/cgroup"
	"kubevirt.io/kubevirt/pkg/virt-handler/selinux"
	virtchroot "kubevirt.io/kubevirt/pkg/virt-handler/virt-chroot"
)

// Mounter exposes a host-local block device or regular file to a running
// virt-launcher pod by placing it under the launcher pod's hotplug-disks
// emptyDir at the path the libvirt converter expects.
//
// The interface is small on purpose: it does just enough to make the
// node-local hotplug RPC work and stays out of the existing
// pkg/virt-handler/hotplug-disk pipeline (which is driven by VMI spec
// reconciliation and assumes attachment pods).
//
//go:generate mockgen -source $GOFILE -package=$GOPACKAGE -destination=mock_$GOFILE
type Mounter interface {
	// MountBlock makes hostDevicePath visible inside the launcher pod as a
	// block device file at <launcher hotplug-disks>/<volumeName> and adds
	// a cgroup allow-rule for that (major,minor) so the launcher's QEMU
	// can open it. The cgroup rule is always "rwm"; read-only enforcement
	// happens via libvirt's <readonly/> on the disk XML.
	MountBlock(vmi *v1.VirtualMachineInstance, volumeName, hostDevicePath string) error
	// MountFile bind-mounts hostFilePath at
	// <launcher hotplug-disks>/<volumeName>.img and sets qemu ownership.
	// When readonly is true the bind mount is established with the "ro"
	// option as defense-in-depth on top of libvirt's <readonly/>.
	MountFile(vmi *v1.VirtualMachineInstance, volumeName, hostFilePath string, readonly bool) error
	// UnmountBlock unmounts the block device entry for volumeName and
	// removes the cgroup allow-rule.
	UnmountBlock(vmi *v1.VirtualMachineInstance, volumeName string) error
	// UnmountFile unmounts the file-backed disk for volumeName.
	UnmountFile(vmi *v1.VirtualMachineInstance, volumeName string) error
}

// CgroupManagerProvider builds a cgroup.Manager for vmi on the local
// node. It is injected into the mounter so this package does not have
// to depend on virtconfig.ClusterConfig or hypervisor-detection logic;
// the caller (cmd/virt-handler) already has both and passes a closure
// that reads cluster-config flags (e.g. AllowEmulation) at call time.
//
// The closure MUST pass a non-empty hypervisor device name to
// cgroup.NewManagerFromVM. Passing "" causes cgroup/util.go to build a
// path of "/dev/", which safepath rejects.
type CgroupManagerProvider func(vmi *v1.VirtualMachineInstance) (cgroup.Manager, error)

// NewMounter builds the default Mounter for a virt-handler instance.
// kubeletPodsDir is the host's /var/lib/kubelet/pods (or the configured
// equivalent), host is the node name, and cgroupProvider builds a
// cgroup manager for the launcher pod when MountBlock/UnmountBlock
// need to add or remove an allow-rule for the staged device.
func NewMounter(kubeletPodsDir, host string, cgroupProvider CgroupManagerProvider) Mounter {
	return &mounter{
		hotplugDiskManager: hotplugdisk.NewHotplugDiskManager(kubeletPodsDir),
		ownershipManager:   diskutils.DefaultOwnershipManager,
		host:               host,
		cgroupFor:          cgroupProvider,
	}
}

type mounter struct {
	hotplugDiskManager hotplugdisk.HotplugDiskManagerInterface
	ownershipManager   diskutils.OwnershipManagerInterface
	host               string
	cgroupFor          CgroupManagerProvider
}

// indirections kept as package vars so they can be swapped from tests.
var (
	mounterMknod = func(basePath *safepath.Path, deviceName string, dev uint64, mode os.FileMode) error {
		return safepath.MknodAtNoFollow(basePath, deviceName, mode|syscall.S_IFBLK, dev)
	}
	mounterMount = func(source, target *safepath.Path, readonly bool) ([]byte, error) {
		return virtchroot.MountChroot(source, target, readonly).CombinedOutput()
	}
	mounterUnmount = func(target *safepath.Path) ([]byte, error) {
		return virtchroot.UmountChroot(target).CombinedOutput()
	}
	mounterRelabel = func(continueOnError bool, target *safepath.Path) error {
		return selinux.RelabelFilesUnprivileged(continueOnError, target)
	}
)

// findVirtlauncherUID returns the launcher pod UID for a VMI on the
// local node, or "" if no unambiguous local launcher can be found.
//
// During live migration vmi.Status.ActivePods can contain both the
// source and target launcher pod UIDs. We avoid the failure mode in
// pkg/virt-handler/hotplug-disk/mount.go (which silently considered
// pods on remote nodes if their hotplug dir happened to exist locally)
// by filtering on ActivePods[uid] == m.host first: a pod scheduled on
// some other node must never be picked as the local target, even if
// the kubelet pods directory layout briefly contains a leftover
// directory matching its UID.
//
// If more than one local launcher pod has a usable hotplug-disks dir
// (e.g. migrate-to-self mid-cutover, or a leftover from a crashed
// previous launcher) we return "" rather than pick arbitrarily; this
// matches the existing kubevirt behaviour for hotplug operations and
// surfaces as an explicit "no virt-launcher pod found" error to the
// gRPC caller, who can retry once the migration state has settled.
func (m *mounter) findVirtlauncherUID(vmi *v1.VirtualMachineInstance) types.UID {
	var found types.UID
	cnt := 0
	for podUID, podHost := range vmi.Status.ActivePods {
		if podHost != m.host {
			continue
		}
		if _, err := m.hotplugDiskManager.GetHotplugTargetPodPathOnHost(podUID); err != nil {
			continue
		}
		found = podUID
		cnt++
	}
	if cnt == 1 {
		return found
	}
	return ""
}

// hostBlockMajorMinor stats the host block device path and returns its
// major/minor packed into a uint64 plus its mode. The stat is performed
// through util.HostRootMount because virt-handler's container has its
// own /dev (kept empty by the kubelet); the path supplied by the caller
// is an absolute path on the node, not in the container.
func hostBlockMajorMinor(hostPath string) (uint64, os.FileMode, error) {
	info, err := os.Stat(filepath.Join(util.HostRootMount, hostPath))
	if err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", hostPath, err)
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return 0, 0, fmt.Errorf("%s is not a block device", hostPath)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("unexpected stat type for %s", hostPath)
	}
	return st.Rdev, info.Mode(), nil
}

func (m *mounter) MountBlock(vmi *v1.VirtualMachineInstance, volumeName, hostDevicePath string) error {
	podUID := m.findVirtlauncherUID(vmi)
	if podUID == "" {
		return fmt.Errorf("no virt-launcher pod found for VMI %s/%s on this node", vmi.Namespace, vmi.Name)
	}

	dev, mode, err := hostBlockMajorMinor(hostDevicePath)
	if err != nil {
		return err
	}

	targetDir, err := m.hotplugDiskManager.GetHotplugTargetPodPathOnHost(podUID)
	if err != nil {
		return fmt.Errorf("resolve hotplug target dir: %w", err)
	}

	if _, err := safepath.JoinNoFollow(targetDir, volumeName); errors.Is(err, os.ErrNotExist) {
		if err := mounterMknod(targetDir, volumeName, dev, mode); err != nil && !os.IsExist(err) {
			return fmt.Errorf("mknod %s in launcher pod: %w", volumeName, err)
		}
	} else if err != nil {
		return err
	}

	devicePath, err := safepath.JoinNoFollow(targetDir, volumeName)
	if err != nil {
		return err
	}

	cgMgr, err := m.cgroupFor(vmi)
	if err != nil {
		return fmt.Errorf("cgroup manager: %w", err)
	}
	if err := setBlockCgroup(cgMgr, dev, true); err != nil {
		return err
	}

	if err := m.ownershipManager.SetFileOwnership(devicePath); err != nil {
		return fmt.Errorf("set ownership on %s: %w", devicePath, err)
	}

	log.Log.Object(vmi).Infof("[node-local-hotplug] block device %s exposed as volume %s for VMI %s/%s", hostDevicePath, volumeName, vmi.Namespace, vmi.Name)
	return nil
}

func (m *mounter) MountFile(vmi *v1.VirtualMachineInstance, volumeName, hostFilePath string, readonly bool) error {
	podUID := m.findVirtlauncherUID(vmi)
	if podUID == "" {
		return fmt.Errorf("no virt-launcher pod found for VMI %s/%s on this node", vmi.Namespace, vmi.Name)
	}

	info, err := os.Stat(filepath.Join(util.HostRootMount, hostFilePath))
	if err != nil {
		return fmt.Errorf("stat %s: %w", hostFilePath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", hostFilePath)
	}

	target, err := m.hotplugDiskManager.GetFileSystemDiskTargetPathFromHostView(podUID, volumeName, true)
	if err != nil {
		return fmt.Errorf("resolve hotplug file target: %w", err)
	}

	// The bind-mount source must resolve to the host file even though we
	// are running inside virt-handler's mount namespace; virt-chroot
	// strips the /proc/1/root prefix when handing the path to the
	// nsenter'd mount(8). See pkg/virt-handler/virt-chroot/virt-chroot.go.
	source, err := safepath.JoinAndResolveWithRelativeRoot(util.HostRootMount, hostFilePath)
	if err != nil {
		return fmt.Errorf("resolve host file path: %w", err)
	}

	if out, err := mounterMount(source, target, readonly); err != nil {
		return fmt.Errorf("bind-mount %s -> %s: %v: %w", hostFilePath, target, string(out), err)
	}

	// Bind mounts share the source inode, so the file inside the
	// launcher pod inherits whatever SELinux label the host file
	// already had. The launcher process runs with a per-VMI MCS
	// pair (e.g. s0:c86,c796) and cannot read a file labelled with
	// any other MCS categories, so the open(O_RDWR) that
	// virt-launcher's checkIfDiskReadyToUse performs would fail
	// silently and the disk would never be plugged into libvirt.
	//
	// Relabel to system_u:object_r:container_file_t:s0 (no MCS
	// categories). s0 with no categories is dominated by every
	// per-pod MCS, so any launcher can read it. This matches what
	// KubeVirt does for unprivileged container files elsewhere
	// (see selinux.RelabelFilesUnprivileged) and is a no-op when
	// SELinux is in permissive mode or disabled.
	//
	// continueOnError=true: if SELinux is permissive the relabel
	// can fail in obscure ways but the bind mount still works;
	// don't fail the attach in that case.
	if err := mounterRelabel(true, target); err != nil {
		log.Log.Object(vmi).Warningf("[node-local-hotplug] selinux relabel of %s failed (continuing): %v", target, err)
	}

	if err := m.ownershipManager.SetFileOwnership(target); err != nil {
		return fmt.Errorf("set ownership on %s: %w", target, err)
	}

	log.Log.Object(vmi).Infof("[node-local-hotplug] file %s exposed as volume %s (ro=%v) for VMI %s/%s", hostFilePath, volumeName, readonly, vmi.Namespace, vmi.Name)
	return nil
}

func (m *mounter) UnmountBlock(vmi *v1.VirtualMachineInstance, volumeName string) error {
	podUID := m.findVirtlauncherUID(vmi)
	if podUID == "" {
		return nil // nothing to clean up on this node
	}
	targetDir, err := m.hotplugDiskManager.GetHotplugTargetPodPathOnHost(podUID)
	if err != nil {
		return err
	}
	devicePath, err := safepath.JoinNoFollow(targetDir, volumeName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	dev, _, err := blockMajorMinorAtPath(devicePath)
	if err != nil {
		return err
	}

	cgMgr, cgErr := m.cgroupFor(vmi)
	if cgErr == nil {
		if err := setBlockCgroup(cgMgr, dev, false); err != nil {
			log.Log.Object(vmi).Warningf("[node-local-hotplug] cgroup deny for %s failed: %v", volumeName, err)
		}
	} else {
		log.Log.Object(vmi).Warningf("[node-local-hotplug] cgroup manager unavailable during unmount: %v", cgErr)
	}

	if err := safepath.UnlinkAtNoFollow(devicePath); err != nil && errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove block device entry %s: %w", devicePath, err)
	}
	return nil
}

func (m *mounter) UnmountFile(vmi *v1.VirtualMachineInstance, volumeName string) error {
	podUID := m.findVirtlauncherUID(vmi)
	if podUID == "" {
		return nil
	}
	target, err := m.hotplugDiskManager.GetFileSystemDiskTargetPathFromHostView(podUID, volumeName, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if out, err := mounterUnmount(target); err != nil {
		// ignore "not mounted" - the file may already be detached
		log.Log.Object(vmi).V(2).Infof("[node-local-hotplug] umount %s: %v: %v", target, string(out), err)
	}
	if err := safepath.UnlinkAtNoFollow(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", target, err)
	}
	return nil
}

func blockMajorMinorAtPath(path *safepath.Path) (uint64, os.FileMode, error) {
	info, err := safepath.StatAtNoFollow(path)
	if err != nil {
		return 0, 0, err
	}
	if info.Mode()&os.ModeDevice == 0 {
		return 0, 0, fmt.Errorf("%v is not a block device", path)
	}
	st := info.Sys().(*syscall.Stat_t)
	return st.Rdev, info.Mode(), nil
}

func setBlockCgroup(cgMgr cgroup.Manager, dev uint64, allow bool) error {
	if cgMgr == nil {
		return fmt.Errorf("cgroup manager is nil")
	}
	rule := &devices.Rule{
		Type:        devices.BlockDevice,
		Major:       int64(unix.Major(dev)),
		Minor:       int64(unix.Minor(dev)),
		Permissions: "rwm",
		Allow:       allow,
	}
	if err := cgMgr.Set(&configs.Resources{Devices: []*devices.Rule{rule}}); err != nil {
		return fmt.Errorf("apply cgroup rule (allow=%v): %w", allow, err)
	}
	return nil
}
