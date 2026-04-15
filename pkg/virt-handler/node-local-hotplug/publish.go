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
	"strings"
	"syscall"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/devices"
	"golang.org/x/sys/unix"

	v1 "kubevirt.io/api/core/v1"

	diskutils "kubevirt.io/kubevirt/pkg/ephemeral-disk-utils"
	hotplugdisk "kubevirt.io/kubevirt/pkg/hotplug-disk"
	"kubevirt.io/kubevirt/pkg/safepath"
	"kubevirt.io/kubevirt/pkg/unsafepath"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/virt-handler/cgroup"
	virt_chroot "kubevirt.io/kubevirt/pkg/virt-handler/virt-chroot"

	"k8s.io/apimachinery/pkg/types"
)

func launcherHotplugBase(kubeletPodsDir string, uid types.UID) (*safepath.Path, error) {
	podsBase := filepath.Join(util.HostRootMount, kubeletPodsDir)
	podpath := hotplugdisk.TargetPodBasePath(podsBase, uid)
	return safepath.JoinAndResolveWithRelativeRoot("/", podpath)
}

func validateSourcePath(src string) (clean string, err error) {
	clean = filepath.Clean(strings.TrimSpace(src))
	if clean == "" || clean == "." {
		return "", fmt.Errorf("source path is required")
	}
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("source path must be absolute: %q", clean)
	}
	fi, err := os.Lstat(clean)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("source path must not be a symlink: %q", clean)
	}
	return clean, nil
}

type sourceKind int

const (
	sourceBlock sourceKind = iota
	sourceFile
	sourceDir
)

func classifySource(path string) (sourceKind, os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, nil, err
	}
	if fi.IsDir() {
		return sourceDir, fi, nil
	}
	if fi.Mode()&os.ModeDevice != 0 && fi.Mode()&os.ModeCharDevice == 0 {
		return sourceBlock, fi, nil
	}
	if fi.Mode().IsRegular() {
		return sourceFile, fi, nil
	}
	return 0, nil, fmt.Errorf("%s is not a block device, regular file, or directory", path)
}

func allowBlockDevice(vmi *v1.VirtualMachineInstance, host string, rdev uint64) error {
	return setBlockDeviceCgroup(vmi, host, rdev, true)
}

func denyBlockDevice(vmi *v1.VirtualMachineInstance, host string, rdev uint64) error {
	return setBlockDeviceCgroup(vmi, host, rdev, false)
}

func setBlockDeviceCgroup(vmi *v1.VirtualMachineInstance, host string, rdev uint64, allow bool) error {
	deviceRule := &devices.Rule{
		Type:        devices.BlockDevice,
		Major:       int64(unix.Major(rdev)),
		Minor:       int64(unix.Minor(rdev)),
		Permissions: "rwm",
		Allow:       allow,
	}
	cgroupManager, err := cgroup.NewManagerFromVM(vmi, host, "")
	if err != nil {
		return err
	}
	return cgroupManager.Set(&configs.Resources{Devices: []*devices.Rule{deviceRule}})
}

// publishToLauncher takes a host source path (block device, file, or directory)
// provided by the node-local agent, and makes it available inside the
// virt-launcher's hotplug-disks directory. The agent needs zero elevated
// privileges — all privileged operations (mknod, bind-mount, cgroup, chown)
// are performed by virt-handler.
//
//   - Block device: mknod with same major/minor → cgroup allow → chown
//   - File: bind-mount → chown (target: <vol>.img)
//   - Directory: bind-mount → chown (target: <vol>/)
func publishToLauncher(vmi *v1.VirtualMachineInstance, host, kubeletPodsDir, volumeName, sourcePath string) error {
	clean, err := validateSourcePath(sourcePath)
	if err != nil {
		return err
	}
	kind, fi, err := classifySource(clean)
	if err != nil {
		return err
	}
	uid, err := FindVirtlauncherUID(vmi)
	if err != nil {
		return err
	}
	targetBase, err := launcherHotplugBase(kubeletPodsDir, uid)
	if err != nil {
		return fmt.Errorf("resolve launcher hotplug base: %w", err)
	}

	switch kind {
	case sourceBlock:
		return publishBlock(vmi, host, targetBase, volumeName, clean, fi)
	case sourceFile:
		return publishFile(targetBase, volumeName, clean)
	case sourceDir:
		return publishDir(targetBase, volumeName, clean)
	default:
		return fmt.Errorf("unsupported source type for %q", clean)
	}
}

func publishBlock(vmi *v1.VirtualMachineInstance, host string, targetBase *safepath.Path, volumeName, sourcePath string, fi os.FileInfo) error {
	st := fi.Sys().(*syscall.Stat_t)
	rdev := st.Rdev

	leaf, leafErr := safepath.JoinNoFollow(targetBase, volumeName)
	if leafErr != nil && !errors.Is(leafErr, os.ErrNotExist) {
		return leafErr
	}
	if leafErr == nil {
		existFI, err := safepath.StatAtNoFollow(leaf)
		if err != nil {
			return err
		}
		isBlock := existFI.Mode()&os.ModeDevice != 0 && existFI.Mode()&os.ModeCharDevice == 0
		if isBlock && existFI.Sys().(*syscall.Stat_t).Rdev == rdev {
			// Already the correct device — just re-finalize.
			if err := allowBlockDevice(vmi, host, rdev); err != nil {
				return err
			}
			p, _ := safepath.NewPathNoFollow(filepath.Join(unsafepath.UnsafeAbsolute(targetBase.Raw()), volumeName))
			return diskutils.DefaultOwnershipManager.SetFileOwnership(p)
		}
		if err := safepath.UnlinkAtNoFollow(leaf); err != nil {
			return fmt.Errorf("remove stale hotplug target: %w", err)
		}
	}

	mkMode := fi.Mode() | syscall.S_IFBLK
	if err := safepath.MknodAtNoFollow(targetBase, volumeName, mkMode, rdev); err != nil {
		return fmt.Errorf("mknod: %w", err)
	}

	if err := allowBlockDevice(vmi, host, rdev); err != nil {
		if l, e := safepath.JoinNoFollow(targetBase, volumeName); e == nil {
			_ = safepath.UnlinkAtNoFollow(l)
		}
		return fmt.Errorf("cgroup allow: %w", err)
	}

	devicePath, err := safepath.JoinNoFollow(targetBase, volumeName)
	if err != nil {
		return err
	}
	return diskutils.DefaultOwnershipManager.SetFileOwnership(devicePath)
}

func publishFile(targetBase *safepath.Path, volumeName, sourcePath string) error {
	diskName := fmt.Sprintf("%s.img", volumeName)
	if err := safepath.TouchAtNoFollow(targetBase, diskName, 0666); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create target file: %w", err)
	}
	target, err := safepath.JoinNoFollow(targetBase, diskName)
	if err != nil {
		return err
	}
	src, err := safepath.NewPathNoFollow(sourcePath)
	if err != nil {
		return err
	}
	if out, err := virt_chroot.MountChroot(src, target, false).CombinedOutput(); err != nil {
		return fmt.Errorf("bind-mount file: %s: %w", string(out), err)
	}
	return diskutils.DefaultOwnershipManager.SetFileOwnership(target)
}

func publishDir(targetBase *safepath.Path, volumeName, sourcePath string) error {
	if _, err := safepath.JoinNoFollow(targetBase, volumeName); errors.Is(err, os.ErrNotExist) {
		if err := safepath.MkdirAtNoFollow(targetBase, volumeName, 0750); err != nil {
			return fmt.Errorf("create target dir: %w", err)
		}
	}
	target, err := safepath.JoinNoFollow(targetBase, volumeName)
	if err != nil {
		return err
	}
	src, err := safepath.NewPathNoFollow(sourcePath)
	if err != nil {
		return err
	}
	if out, err := virt_chroot.MountChroot(src, target, false).CombinedOutput(); err != nil {
		return fmt.Errorf("bind-mount dir: %s: %w", string(out), err)
	}
	return diskutils.DefaultOwnershipManager.SetFileOwnership(target)
}

// cleanupFromLauncher revokes cgroup rules (for block devices), unmounts
// bind-mounts (for files/dirs), and removes the target entry. Returns
// (true, nil) if a target was found and cleaned, (false, nil) if nothing
// existed at the expected paths.
func cleanupFromLauncher(vmi *v1.VirtualMachineInstance, host, kubeletPodsDir, volumeName string) (bool, error) {
	uid, err := FindVirtlauncherUID(vmi)
	if err != nil {
		return false, err
	}
	targetBase, err := launcherHotplugBase(kubeletPodsDir, uid)
	if err != nil {
		return false, err
	}

	// Try <vol> (block device or directory) then <vol>.img (file).
	for _, name := range []string{volumeName, fmt.Sprintf("%s.img", volumeName)} {
		leaf, lerr := safepath.JoinNoFollow(targetBase, name)
		if errors.Is(lerr, os.ErrNotExist) {
			continue
		}
		if lerr != nil {
			return false, lerr
		}
		fi, serr := safepath.StatAtNoFollow(leaf)
		if serr != nil {
			return false, serr
		}

		isBlock := fi.Mode()&os.ModeDevice != 0 && fi.Mode()&os.ModeCharDevice == 0
		if isBlock {
			st := fi.Sys().(*syscall.Stat_t)
			if err := denyBlockDevice(vmi, host, st.Rdev); err != nil {
				return false, fmt.Errorf("cgroup deny: %w", err)
			}
			if err := safepath.UnlinkAtNoFollow(leaf); err != nil {
				return false, fmt.Errorf("remove block node: %w", err)
			}
			return true, nil
		}

		// File or directory — unmount first, then remove.
		if out, uerr := virt_chroot.UmountChroot(leaf).CombinedOutput(); uerr != nil {
			return false, fmt.Errorf("unmount %s: %s: %w", unsafepath.UnsafeAbsolute(leaf.Raw()), string(out), uerr)
		}
		if err := safepath.UnlinkAtNoFollow(leaf); err != nil {
			return false, fmt.Errorf("remove %s: %w", unsafepath.UnsafeAbsolute(leaf.Raw()), err)
		}
		return true, nil
	}
	return false, nil
}
