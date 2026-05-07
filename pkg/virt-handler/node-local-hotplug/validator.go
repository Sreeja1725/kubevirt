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
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/util"
	apiv1 "kubevirt.io/kubevirt/pkg/virt-handler/node-local-hotplug/v1"
)

// hostStat resolves an admin-supplied absolute host path under the node
// root before stat'ing it. virt-handler runs with HostPID=true (see
// pkg/virt-operator/.../daemonsets.go), so /proc/1/root is the host's
// root filesystem. Without this redirection a request for e.g.
// /dev/loop100 would be looked up against the container's own /dev
// (which is empty) and the validator would refuse legitimate host
// devices. Kept as a var so tests can swap in an in-memory fake.
var hostStat = func(path string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(util.HostRootMount, path))
}

// targetBusToLibvirt translates the proto TargetBus enum into the
// libvirt <target bus='...'/> attribute string. UNSPECIFIED defaults to
// "virtio".
//
// Only the buses kubevirt's own hotplug admitter accepts (virtio, scsi)
// are translated; SATA is intentionally rejected here because
// pkg/storage/admitters.ValidateHotplugDiskConfiguration would block
// the resulting VMI patch with "requires bus to be 'scsi' or 'virtio'".
// Returns an empty string when the enum value is outside the supported
// set; callers MUST treat that as a validation error rather than
// passing it through to libvirt.
func targetBusToLibvirt(b apiv1.TargetBus) string {
	switch b {
	case apiv1.TargetBus_TARGET_BUS_UNSPECIFIED, apiv1.TargetBus_TARGET_BUS_VIRTIO:
		return "virtio"
	case apiv1.TargetBus_TARGET_BUS_SCSI:
		return "scsi"
	default:
		return ""
	}
}

// validateAttach checks that an AttachDevice request is safe to act on
// against the current state of vmi. It does NOT check whether the
// volume already exists on the VMI; callers must use
// classifyAttachAgainstExisting for that so they can distinguish an
// idempotent re-attach from a real conflict.
//
// Errors returned here are typed via codedError so the service layer
// can emit a precise NodeLocalHotplugErrorCode without re-classifying.
func validateAttach(req *apiv1.AttachDeviceRequest, vmi *v1.VirtualMachineInstance) error {
	if req == nil {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "nil request")
	}
	if req.Namespace == "" || req.Name == "" {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "namespace and name are required")
	}
	if err := validateVMI(req.VmiUid, vmi); err != nil {
		return err
	}
	if err := validateVolumeName(req.VolumeName); err != nil {
		return err
	}

	dev := req.GetDevice()
	if dev == nil {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "device is required")
	}
	if dev.DevicePath == "" {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "device.device_path is required")
	}
	if !filepath.IsAbs(dev.DevicePath) {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "device.device_path must be absolute, got %q", dev.DevicePath)
	}

	switch dev.Format {
	case apiv1.DeviceFormat_DEVICE_FORMAT_BLOCK:
		if err := requireBlockDevice(dev.DevicePath); err != nil {
			return err
		}
	case apiv1.DeviceFormat_DEVICE_FORMAT_FILE:
		if err := requireRegularFile(dev.DevicePath); err != nil {
			return err
		}
	case apiv1.DeviceFormat_DEVICE_FORMAT_UNSPECIFIED:
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "device.format is required")
	default:
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "unsupported device.format %v", dev.Format)
	}

	if targetBusToLibvirt(dev.TargetBus) == "" {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "unsupported device.target_bus %v for hotplug (allowed: VIRTIO, SCSI)", dev.TargetBus)
	}

	return nil
}

// attachClassification describes how an AttachDevice request relates to
// the current VMI spec.
type attachClassification int

const (
	// attachFresh: nothing for this volume_name exists yet; proceed with
	// a normal attach.
	attachFresh attachClassification = iota
	// attachIdempotent: an identical NodeLocalDevice volume entry already
	// exists; the RPC should be a successful no-op (best-effort re-mount
	// for crash safety).
	attachIdempotent
	// attachConflict: a different volume / disk with the same name
	// already exists; the RPC must fail.
	attachConflict
)

// classifyAttachAgainstExisting decides whether this AttachDevice
// request is fresh, an idempotent retry, or a conflict against the
// current VMI state. err is non-nil only for the conflict case.
func classifyAttachAgainstExisting(req *apiv1.AttachDeviceRequest, vmi *v1.VirtualMachineInstance) (attachClassification, error) {
	existing := getVolume(vmi, req.VolumeName)
	if existing == nil {
		// No spec volume. If a leftover disk or volumeStatus entry
		// exists (e.g. a crashed previous attempt) treat it as a
		// conflict so we don't end up with split state.
		if hasDisk(vmi, req.VolumeName) {
			return attachConflict, codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VOLUME_CONFLICT, "disk %q already exists on VMI %s/%s without a matching volume", req.VolumeName, vmi.Namespace, vmi.Name)
		}
		if hasVolumeStatus(vmi, req.VolumeName) {
			return attachConflict, codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VOLUME_CONFLICT, "volume status %q already present on VMI %s/%s without a matching volume", req.VolumeName, vmi.Namespace, vmi.Name)
		}
		// Refuse to attach the same host device under two different
		// volume names on the same VMI: that would install a second
		// mknod / bind-mount / cgroup allow rule pointing at the same
		// underlying resource and let the guest see the device as
		// two competing libvirt disks.
		dev := req.GetDevice()
		wantFormat := nodeLocalFormatFromProto(dev.GetFormat())
		if other := findVolumeByNodeLocalPath(vmi, wantFormat, dev.GetDevicePath()); other != nil {
			return attachConflict, codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VOLUME_CONFLICT, "device path %q is already attached to VMI %s/%s as volume %q; refusing to attach the same host device twice",
				dev.GetDevicePath(), vmi.Namespace, vmi.Name, other.Name)
		}
		return attachFresh, nil
	}

	src := existing.VolumeSource.NodeLocalDevice
	if src == nil {
		return attachConflict, codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VOLUME_CONFLICT, "volume %q already exists on VMI %s/%s as a non-node-local source; refusing to overwrite", req.VolumeName, vmi.Namespace, vmi.Name)
	}

	dev := req.GetDevice()
	wantFormat := nodeLocalFormatFromProto(dev.GetFormat())
	if src.Format != wantFormat || src.Path != dev.GetDevicePath() {
		return attachConflict, codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VOLUME_CONFLICT, "volume %q already attached to %s/%s with format=%s path=%s; cannot re-attach with format=%s path=%s",
			req.VolumeName, vmi.Namespace, vmi.Name,
			src.Format, src.Path, wantFormat, dev.GetDevicePath())
	}
	return attachIdempotent, nil
}

// findVolumeByNodeLocalPath returns the first NodeLocalDevice volume on
// vmi whose (format, path) matches the request, or nil if none. Only
// volumes that already have a NodeLocalDevice source are considered;
// non-node-local volumes that happen to share a path (e.g. a HostDisk)
// are out of scope here and handled by the standard kubevirt admitters.
func findVolumeByNodeLocalPath(vmi *v1.VirtualMachineInstance, format v1.NodeLocalDeviceFormat, path string) *v1.Volume {
	for i := range vmi.Spec.Volumes {
		vol := &vmi.Spec.Volumes[i]
		src := vol.VolumeSource.NodeLocalDevice
		if src == nil {
			continue
		}
		if src.Format == format && src.Path == path {
			return vol
		}
	}
	return nil
}

// nodeLocalFormatFromProto translates the proto DeviceFormat enum into
// the kubevirt API string. Kept in validator.go (not patcher.go) so
// classifyAttachAgainstExisting doesn't pull in a patcher-package import.
func nodeLocalFormatFromProto(f apiv1.DeviceFormat) v1.NodeLocalDeviceFormat {
	if f == apiv1.DeviceFormat_DEVICE_FORMAT_FILE {
		return v1.NodeLocalDeviceFormatFile
	}
	return v1.NodeLocalDeviceFormatBlock
}

// validateDetach checks that a DetachDevice request is well-formed and
// safe to act on. Absence of the volume is NOT treated as an error
// here: per the API contract, detaching a missing volume is an
// idempotent no-op. The service layer detects that case before patching.
func validateDetach(req *apiv1.DetachDeviceRequest, vmi *v1.VirtualMachineInstance) error {
	if req == nil {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "nil request")
	}
	if req.Namespace == "" || req.Name == "" {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "namespace and name are required")
	}
	if err := validateVMI(req.VmiUid, vmi); err != nil {
		return err
	}
	if err := validateVolumeName(req.VolumeName); err != nil {
		return err
	}
	if vol := getVolume(vmi, req.VolumeName); vol != nil {
		if vol.VolumeSource.NodeLocalDevice == nil {
			return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VOLUME_CONFLICT, "volume %q is not a node-local-hotplug volume; refusing to detach", req.VolumeName)
		}
		if isBootDisk(vmi, req.VolumeName) {
			return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "volume %q is the boot disk and cannot be detached", req.VolumeName)
		}
	}
	return nil
}

func validateVMI(reqUID string, vmi *v1.VirtualMachineInstance) error {
	if vmi == nil {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VMI_NOT_FOUND, "VMI not found on this node")
	}
	if reqUID != "" && string(vmi.UID) != reqUID {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VMI_UID_MISMATCH, "vmi_uid mismatch: request %q vs current %q", reqUID, string(vmi.UID))
	}
	if vmi.DeletionTimestamp != nil {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VMI_NOT_RUNNING, "VMI %s/%s is being deleted", vmi.Namespace, vmi.Name)
	}
	if vmi.Status.Phase != v1.Running {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VMI_NOT_RUNNING, "VMI %s/%s is not Running (phase=%s)", vmi.Namespace, vmi.Name, vmi.Status.Phase)
	}
	if vmi.Status.MigrationState != nil && !vmi.Status.MigrationState.Completed && !vmi.Status.MigrationState.Failed {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VMI_NOT_RUNNING, "VMI %s/%s is currently migrating; hotplug not allowed", vmi.Namespace, vmi.Name)
	}
	return nil
}

func validateVolumeName(name string) error {
	if name == "" {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "volume_name is required")
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, "volume_name %q is not a valid DNS-1123 label: %s", name, strings.Join(errs, "; "))
	}
	return nil
}

func requireBlockDevice(path string) error {
	info, err := hostStat(path)
	if err != nil {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_HOST_PATH_INVALID, "stat %q: %v", path, err)
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_HOST_PATH_INVALID, "%q is not a block device", path)
	}
	return nil
}

func requireRegularFile(path string) error {
	info, err := hostStat(path)
	if err != nil {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_HOST_PATH_INVALID, "stat %q: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return codedErrorf(apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_HOST_PATH_INVALID, "%q is not a regular file", path)
	}
	return nil
}

func hasVolume(vmi *v1.VirtualMachineInstance, name string) bool {
	return getVolume(vmi, name) != nil
}

func getVolume(vmi *v1.VirtualMachineInstance, name string) *v1.Volume {
	for i := range vmi.Spec.Volumes {
		if vmi.Spec.Volumes[i].Name == name {
			return &vmi.Spec.Volumes[i]
		}
	}
	return nil
}

func hasDisk(vmi *v1.VirtualMachineInstance, name string) bool {
	for _, d := range vmi.Spec.Domain.Devices.Disks {
		if d.Name == name {
			return true
		}
	}
	return false
}

func hasVolumeStatus(vmi *v1.VirtualMachineInstance, name string) bool {
	for _, vs := range vmi.Status.VolumeStatus {
		if vs.Name == name {
			return true
		}
	}
	return false
}

func isBootDisk(vmi *v1.VirtualMachineInstance, name string) bool {
	for _, d := range vmi.Spec.Domain.Devices.Disks {
		if d.Name == name && d.BootOrder != nil {
			return true
		}
	}
	return false
}
