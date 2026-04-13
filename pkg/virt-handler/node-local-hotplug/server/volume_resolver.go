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
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubevirt.io/kubevirt/pkg/util"
	virt_chroot "kubevirt.io/kubevirt/pkg/virt-handler/virt-chroot"
)

// managedMountBase is where virt-handler mounts network volumes (NFS, etc.)
// that need to be made available for hotplug. Path is relative to the host root.
const managedMountBase = "/var/run/kubevirt/node-local-hotplug/mounts"

// resolvedVolume describes how a PVC maps to a host resource.
type resolvedVolume struct {
	// HostPath is set for non-CSI volumes (Local, HostPath, NFS, iSCSI, FC, RBD).
	// The path is already verified to exist on the host.
	HostPath string

	// PV is set for all PVC-backed volumes.
	PV *k8sv1.PersistentVolume

	// CSI is true when the PV requires CSI RPCs (ControllerPublish + NodeStage + NodePublish)
	// instead of a direct host path.
	CSI bool
}

// resolveVolume looks up the PVC/DataVolume referenced by opts and determines
// how the volume should be published.
//
// For non-CSI PVs (Local, HostPath, NFS, iSCSI, FC, RBD) it resolves the
// on-node host path. For CSI PVs it returns the PV metadata so the caller
// can perform CSI RPCs. NGN volumes are flagged so the caller can skip them.
func resolveVolume(ctx context.Context, virtCli kubecli.KubevirtClient, kubeletPodsDir, namespace string, opts *v1.AddVolumeOptions) (*resolvedVolume, error) {
	if opts == nil || opts.VolumeSource == nil {
		return nil, fmt.Errorf("volume source is required")
	}

	claimName := pvcNameFromOptions(opts)
	if claimName == "" {
		return nil, fmt.Errorf("no PVC or DataVolume reference in volume source")
	}

	pvc, err := virtCli.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get PVC %s/%s: %w", namespace, claimName, err)
	}
	if pvc.Status.Phase != k8sv1.ClaimBound {
		return nil, fmt.Errorf("PVC %s/%s is not bound (phase: %s)", namespace, claimName, pvc.Status.Phase)
	}
	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return nil, fmt.Errorf("PVC %s/%s has no bound PV", namespace, claimName)
	}

	pv, err := virtCli.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get PV %s: %w", pvName, err)
	}

	if pv != nil && pv.Spec.CSI != nil {
		log.DefaultLogger().V(3).Infof("PVC %s/%s -> PV %s is CSI-backed (driver=%s); will use CSI RPCs", namespace, claimName, pvName, pv.Spec.CSI.Driver)
		return &resolvedVolume{PV: pv, CSI: true}, nil
	}

	hostPath, err := hostPathFromPV(pv, kubeletPodsDir)
	if err != nil {
		return nil, fmt.Errorf("resolve host path for PV %s: %w", pvName, err)
	}

	if _, err := os.Stat(hostPath); err != nil {
		return nil, fmt.Errorf("resolved path %q does not exist on this node: %w", hostPath, err)
	}

	log.DefaultLogger().V(3).Infof("Resolved PVC %s/%s -> PV %s -> host path %s", namespace, claimName, pvName, hostPath)
	return &resolvedVolume{HostPath: hostPath, PV: pv}, nil
}

// lookupPVForVolume resolves a KubeVirt Volume's PVC/DataVolume to its bound PV.
// Returns nil if the volume has no PVC reference or the PV cannot be found.
func lookupPVForVolume(ctx context.Context, virtCli kubecli.KubevirtClient, namespace string, vol *v1.Volume) *k8sv1.PersistentVolume {
	claimName := pvcClaimNameFromVolume(vol)
	if claimName == "" {
		return nil
	}
	pvc, err := virtCli.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil || pvc.Spec.VolumeName == "" {
		return nil
	}
	pv, err := virtCli.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return pv
}

// cleanupManagedMount unmounts any managed mount (NFS, etc.) that was created
// during resolution for a PVC-backed volume. It is safe to call even when no
// managed mount exists.
func cleanupManagedMount(ctx context.Context, virtCli kubecli.KubevirtClient, namespace string, vol *v1.Volume) error {
	claimName := pvcClaimNameFromVolume(vol)
	if claimName == "" {
		return nil
	}

	pvc, err := virtCli.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		log.DefaultLogger().V(3).Infof("skip managed mount cleanup for %s/%s: PVC lookup failed: %v", namespace, claimName, err)
		return nil
	}
	if pvc.Spec.VolumeName == "" {
		return nil
	}

	pv, err := virtCli.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		log.DefaultLogger().V(3).Infof("skip managed mount cleanup for PV %s: %v", pvc.Spec.VolumeName, err)
		return nil
	}

	return unmountManagedIfNeeded(pv)
}

func pvcNameFromOptions(opts *v1.AddVolumeOptions) string {
	if opts.VolumeSource.PersistentVolumeClaim != nil {
		return opts.VolumeSource.PersistentVolumeClaim.ClaimName
	}
	if opts.VolumeSource.DataVolume != nil {
		return opts.VolumeSource.DataVolume.Name
	}
	return ""
}

func pvcClaimNameFromVolume(vol *v1.Volume) string {
	if vol == nil {
		return ""
	}
	if vol.PersistentVolumeClaim != nil {
		return vol.PersistentVolumeClaim.ClaimName
	}
	if vol.DataVolume != nil {
		return vol.DataVolume.Name
	}
	return ""
}

// hostPathFromPV determines the on-node host path for a non-CSI PV.
// CSI PVs are handled separately via CSI RPCs and should not reach this function.
func hostPathFromPV(pv *k8sv1.PersistentVolume, kubeletPodsDir string) (string, error) {
	switch {
	case pv.Spec.Local != nil:
		return localPVPath(pv)
	case pv.Spec.HostPath != nil:
		return hostPathPVPath(pv)
	case pv.Spec.NFS != nil:
		return nfsPVPath(pv)
	case pv.Spec.ISCSI != nil:
		return iscsiPVDevicePath(pv)
	case pv.Spec.FC != nil:
		return fcPVDevicePath(pv)
	case pv.Spec.RBD != nil:
		return rbdPVDevicePath(pv)
	default:
		return "", fmt.Errorf(
			"unsupported non-CSI PV type for %s; supported: Local, HostPath, NFS, iSCSI, FC, RBD", pv.Name)
	}
}

func localPVPath(pv *k8sv1.PersistentVolume) (string, error) {
	p := strings.TrimSpace(pv.Spec.Local.Path)
	if p == "" {
		return "", fmt.Errorf("local PV %s has empty path", pv.Name)
	}
	return filepath.Join(util.HostRootMount, p), nil
}

func hostPathPVPath(pv *k8sv1.PersistentVolume) (string, error) {
	p := strings.TrimSpace(pv.Spec.HostPath.Path)
	if p == "" {
		return "", fmt.Errorf("HostPath PV %s has empty path", pv.Name)
	}
	return filepath.Join(util.HostRootMount, p), nil
}

// nfsPVPath mounts the NFS share to a managed directory on the host and returns
// the host-visible mount path. The mount is idempotent.
func nfsPVPath(pv *k8sv1.PersistentVolume) (string, error) {
	nfs := pv.Spec.NFS
	if nfs.Server == "" || nfs.Path == "" {
		return "", fmt.Errorf("NFS PV %s has empty server or path", pv.Name)
	}

	hostMountPoint := filepath.Join(managedMountBase, "nfs", pv.Name)
	procMountPoint := filepath.Join(util.HostRootMount, hostMountPoint)

	if isMountedOnHost(hostMountPoint) {
		return procMountPoint, nil
	}

	if err := os.MkdirAll(procMountPoint, 0750); err != nil {
		return "", fmt.Errorf("create NFS mount point %s: %w", procMountPoint, err)
	}

	source := nfs.Server + ":" + nfs.Path
	args := []string{"-t", "nfs"}
	if nfs.ReadOnly {
		args = append(args, "-o", "ro")
	}
	args = append(args, source, hostMountPoint)

	if err := mountInHostNS(args...); err != nil {
		return "", fmt.Errorf("mount NFS %s on %s: %w", source, hostMountPoint, err)
	}

	log.DefaultLogger().V(3).Infof("Mounted NFS %s at %s", source, hostMountPoint)
	return procMountPoint, nil
}

// iscsiPVDevicePath derives the expected block device path for an iSCSI PV.
// The device must already be attached (iSCSI session established by kubelet or admin).
// Convention: /dev/disk/by-path/ip-<portal>-iscsi-<iqn>-lun-<lun>
func iscsiPVDevicePath(pv *k8sv1.PersistentVolume) (string, error) {
	iscsi := pv.Spec.ISCSI
	if iscsi.TargetPortal == "" || iscsi.IQN == "" {
		return "", fmt.Errorf("iSCSI PV %s has empty TargetPortal or IQN", pv.Name)
	}
	portal := iscsi.TargetPortal
	if idx := strings.LastIndex(portal, ":"); idx > 0 {
		portal = portal[:idx]
	}
	devPath := fmt.Sprintf("/dev/disk/by-path/ip-%s-iscsi-%s-lun-%d", portal, iscsi.IQN, iscsi.Lun)
	resolved, err := resolveDevicePath(devPath)
	if err != nil {
		return "", fmt.Errorf("iSCSI PV %s: device not found at %s (is the iSCSI session active on this node?): %w", pv.Name, devPath, err)
	}
	return resolved, nil
}

// fcPVDevicePath derives the expected block device path for a Fibre Channel PV.
// Convention:
//
//	WWIDs:      /dev/disk/by-id/scsi-<wwid>
//	WWNs+Lun:   /dev/disk/by-path/fc-0x<wwn>-lun-<lun>
func fcPVDevicePath(pv *k8sv1.PersistentVolume) (string, error) {
	fc := pv.Spec.FC
	if fc == nil {
		return "", fmt.Errorf("FC PV %s has nil FC spec", pv.Name)
	}

	if len(fc.WWIDs) > 0 {
		devPath := "/dev/disk/by-id/scsi-" + fc.WWIDs[0]
		resolved, err := resolveDevicePath(devPath)
		if err != nil {
			return "", fmt.Errorf("FC PV %s: device not found at %s: %w", pv.Name, devPath, err)
		}
		return resolved, nil
	}

	if len(fc.TargetWWNs) > 0 && fc.Lun != nil {
		wwn := strings.ToLower(fc.TargetWWNs[0])
		if !strings.HasPrefix(wwn, "0x") {
			wwn = "0x" + wwn
		}
		devPath := fmt.Sprintf("/dev/disk/by-path/fc-%s-lun-%d", wwn, *fc.Lun)
		resolved, err := resolveDevicePath(devPath)
		if err != nil {
			return "", fmt.Errorf("FC PV %s: device not found at %s: %w", pv.Name, devPath, err)
		}
		return resolved, nil
	}

	return "", fmt.Errorf("FC PV %s has no WWIDs or TargetWWNs+Lun", pv.Name)
}

// rbdPVDevicePath derives the expected block device path for a Ceph RBD PV.
// Convention: /dev/rbd/<pool>/<image>
func rbdPVDevicePath(pv *k8sv1.PersistentVolume) (string, error) {
	rbd := pv.Spec.RBD
	if rbd.RBDImage == "" {
		return "", fmt.Errorf("RBD PV %s has empty RBDImage", pv.Name)
	}
	pool := rbd.RBDPool
	if pool == "" {
		pool = "rbd"
	}
	devPath := filepath.Join("/dev/rbd", pool, rbd.RBDImage)
	resolved, err := resolveDevicePath(devPath)
	if err != nil {
		return "", fmt.Errorf("RBD PV %s: device not found at %s (is the RBD image mapped on this node?): %w", pv.Name, devPath, err)
	}
	return resolved, nil
}

// unmountManagedIfNeeded unmounts a managed NFS mount if one exists for this PV.
func unmountManagedIfNeeded(pv *k8sv1.PersistentVolume) error {
	var hostMountPoint string
	switch {
	case pv.Spec.NFS != nil:
		hostMountPoint = filepath.Join(managedMountBase, "nfs", pv.Name)
	default:
		return nil
	}

	if !isMountedOnHost(hostMountPoint) {
		return nil
	}

	if err := unmountInHostNS(hostMountPoint); err != nil {
		return fmt.Errorf("unmount managed mount %s: %w", hostMountPoint, err)
	}

	procMountPoint := filepath.Join(util.HostRootMount, hostMountPoint)
	os.Remove(procMountPoint)

	log.DefaultLogger().V(3).Infof("Unmounted managed mount at %s", hostMountPoint)
	return nil
}

func kubeletRootFromPodsDir(kubeletPodsDir string) string {
	return filepath.Dir(strings.TrimSuffix(filepath.Clean(kubeletPodsDir), "/"))
}

// resolveDevicePath takes a /dev/disk/by-* path (which is often a symlink),
// prepends the host root mount, follows symlinks, and returns the
// host-root-prefixed real device path.
func resolveDevicePath(devPath string) (string, error) {
	hostDev := filepath.Join(util.HostRootMount, devPath)

	target, err := os.Readlink(hostDev)
	if err != nil {
		if _, serr := os.Stat(hostDev); serr != nil {
			return "", fmt.Errorf("device %s not found: %w", hostDev, serr)
		}
		return hostDev, nil
	}

	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(devPath), target)
	}
	target = filepath.Clean(target)
	return filepath.Join(util.HostRootMount, target), nil
}

// isMountedOnHost checks the host's /proc/1/mountinfo to see if hostPath is
// an active mount point. hostPath must be an absolute path relative to the
// host root (not prefixed with /proc/1/root).
func isMountedOnHost(hostPath string) bool {
	f, err := os.Open("/proc/1/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()

	clean := filepath.Clean(hostPath)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 5 && filepath.Clean(fields[4]) == clean {
			return true
		}
	}
	return false
}

// mountInHostNS runs "mount <args...>" inside the host's mount namespace using
// the virt-chroot binary.
func mountInHostNS(args ...string) error {
	chrootArgs := []string{"--mount", virt_chroot.GetChrootMountNamespace(), "mount"}
	chrootArgs = append(chrootArgs, args...)
	cmd := exec.Command(virt_chroot.GetChrootBinaryPath(), chrootArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// unmountInHostNS runs "umount <path>" inside the host's mount namespace.
func unmountInHostNS(hostPath string) error {
	cmd := virt_chroot.UnsafeUmountChroot(hostPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
