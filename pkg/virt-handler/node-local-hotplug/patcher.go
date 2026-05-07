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
	"fmt"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/apimachinery/patch"
	apiv1 "kubevirt.io/kubevirt/pkg/virt-handler/node-local-hotplug/v1"
)

// buildAttachVolume / buildAttachDisk produce the desired Volume + Disk
// for a fresh AttachDevice request.
func buildAttachVolume(req *apiv1.AttachDeviceRequest) v1.Volume {
	dev := req.GetDevice()
	return v1.Volume{
		Name: req.VolumeName,
		VolumeSource: v1.VolumeSource{
			NodeLocalDevice: &v1.NodeLocalDeviceSource{
				Format: deviceFormatToAPI(dev.GetFormat()),
				Path:   dev.GetDevicePath(),
			},
		},
	}
}

func buildAttachDisk(req *apiv1.AttachDeviceRequest) v1.Disk {
	dev := req.GetDevice()
	return v1.Disk{
		Name: req.VolumeName,
		DiskDevice: v1.DiskDevice{
			Disk: &v1.DiskTarget{
				Bus:      v1.DiskBus(targetBusToLibvirt(dev.GetTargetBus())),
				ReadOnly: dev.GetReadonly(),
			},
		},
		Serial: dev.GetSerial(),
	}
}

// Reasons / messages on the volumeStatus entry. Stable enough to log
// and to query with kubectl, but not a contract for programmatic
// callers.
const (
	reasonNodeLocalHotplugBound   = "VolumeBound"
	reasonNodeLocalHotplugMounted = "MountedToPod"

	messageNodeLocalHotplugBound   = "host path validated; bind-mount into virt-launcher pending"
	messageNodeLocalHotplugMounted = "device exposed under launcher hotplug-disks dir"
)

func buildAttachVolumeStatus(req *apiv1.AttachDeviceRequest) v1.VolumeStatus {
	return v1.VolumeStatus{
		Name:          req.VolumeName,
		Phase:         v1.VolumeBound,
		Reason:        reasonNodeLocalHotplugBound,
		Message:       messageNodeLocalHotplugBound,
		HotplugVolume: &v1.HotplugVolumeStatus{},
	}
}

// buildAttachIntentPatch is the FIRST of two patches in an attach
// flow. It adds the volume + disk to spec and seeds an initial
// volumeStatus entry at phase=Bound, telling observers the host path
// has been validated and the launcher-side mount is in flight.
//
// JSONPatch "test" ops on each of the three slices keep the conflict
// surface narrow: an unrelated concurrent write to other VMI fields
// (status.conditions, status.interfaces, ...) does NOT cause our patch
// to fail. The apiserver returns 422 Invalid when a "test" op misses,
// which the caller treats as retriable.
//
// Why a single payload covers both spec and status: VMI's CRD does
// NOT enable the "/status" subresource (see
// NewVirtualMachineInstanceCrd). Without a status subresource the
// apiserver does not split spec and status, so a JSONPatch sent to
// the main resource may write either or both.
func buildAttachIntentPatch(vmi *v1.VirtualMachineInstance, req *apiv1.AttachDeviceRequest) ([]byte, error) {
	vol := buildAttachVolume(req)
	disk := buildAttachDisk(req)
	status := buildAttachVolumeStatus(req)

	ps := patch.New()

	if len(vmi.Spec.Volumes) == 0 {
		ps.AddOption(patch.WithAdd("/spec/volumes", []v1.Volume{vol}))
	} else {
		ps.AddOption(
			patch.WithTest("/spec/volumes", vmi.Spec.Volumes),
			patch.WithAdd("/spec/volumes/-", vol),
		)
	}

	if len(vmi.Spec.Domain.Devices.Disks) == 0 {
		ps.AddOption(patch.WithAdd("/spec/domain/devices/disks", []v1.Disk{disk}))
	} else {
		ps.AddOption(
			patch.WithTest("/spec/domain/devices/disks", vmi.Spec.Domain.Devices.Disks),
			patch.WithAdd("/spec/domain/devices/disks/-", disk),
		)
	}

	if len(vmi.Status.VolumeStatus) == 0 {
		ps.AddOption(patch.WithAdd("/status/volumeStatus", []v1.VolumeStatus{status}))
	} else {
		ps.AddOption(
			patch.WithTest("/status/volumeStatus", vmi.Status.VolumeStatus),
			patch.WithAdd("/status/volumeStatus/-", status),
		)
	}

	payload, err := ps.GeneratePayload()
	if err != nil {
		return nil, fmt.Errorf("generate attach intent patch: %w", err)
	}
	return payload, nil
}

// buildAttachMountedPatch is the SECOND of two patches in an attach
// flow. It transitions the volume's existing volumeStatus entry from
// phase=Bound to phase=MountedToPod after the host-side mount has
// succeeded. The "test" op on /status/volumeStatus/<idx> is what
// distinguishes "Bound is still there to be advanced" from "someone
// else changed this entry"; the caller must re-GET and retry on a
// "test" miss.
//
// volumeName MUST identify an entry already present in
// vmi.Status.VolumeStatus (i.e. the intent patch has already landed).
func buildAttachMountedPatch(vmi *v1.VirtualMachineInstance, volumeName string) ([]byte, error) {
	idx, existing, err := findVolumeStatus(vmi, volumeName)
	if err != nil {
		return nil, err
	}

	transitioned := *existing.DeepCopy()
	transitioned.Phase = v1.HotplugVolumeMounted
	transitioned.Reason = reasonNodeLocalHotplugMounted
	transitioned.Message = messageNodeLocalHotplugMounted

	ps := patch.New(
		patch.WithTest(volumeStatusPath(idx), *existing),
		patch.WithReplace(volumeStatusPath(idx), transitioned),
	)

	payload, err := ps.GeneratePayload()
	if err != nil {
		return nil, fmt.Errorf("generate attach mounted patch: %w", err)
	}
	return payload, nil
}

// buildDetachPatch removes the named volume / disk / volumeStatus by
// rewriting the three slices to their post-detach value. Detach is
// kept as a single patch (unlike attach) because the wait-for-libvirt
// and host-side unmount happen AFTER spec removal: a "Detaching"
// observability window between two patches would be near-zero. Same
// status-writes-via-main-resource remarks as buildAttachIntentPatch.
func buildDetachPatch(vmi *v1.VirtualMachineInstance, volumeName string) ([]byte, error) {
	newVolumes := filterVolumes(vmi.Spec.Volumes, volumeName)
	newDisks := filterDisks(vmi.Spec.Domain.Devices.Disks, volumeName)
	newStatus := filterVolumeStatus(vmi.Status.VolumeStatus, volumeName)

	ps := patch.New(
		patch.WithTest("/spec/volumes", vmi.Spec.Volumes),
		patch.WithReplace("/spec/volumes", newVolumes),
		patch.WithTest("/spec/domain/devices/disks", vmi.Spec.Domain.Devices.Disks),
		patch.WithReplace("/spec/domain/devices/disks", newDisks),
		patch.WithTest("/status/volumeStatus", vmi.Status.VolumeStatus),
		patch.WithReplace("/status/volumeStatus", newStatus),
	)

	payload, err := ps.GeneratePayload()
	if err != nil {
		return nil, fmt.Errorf("generate detach patch: %w", err)
	}
	return payload, nil
}

// findVolumeStatus locates the volumeStatus entry for volumeName and
// returns its index plus a pointer to the entry itself. err is
// non-nil if no entry exists; callers in the patch path treat that
// as a fatal-but-retriable misalignment between the VMI state we
// observed and the VMI state we are about to patch.
func findVolumeStatus(vmi *v1.VirtualMachineInstance, volumeName string) (int, *v1.VolumeStatus, error) {
	for i := range vmi.Status.VolumeStatus {
		if vmi.Status.VolumeStatus[i].Name == volumeName {
			return i, &vmi.Status.VolumeStatus[i], nil
		}
	}
	return -1, nil, fmt.Errorf("volumeStatus entry %q not found on VMI %s/%s", volumeName, vmi.Namespace, vmi.Name)
}

func volumeStatusPath(idx int) string {
	return fmt.Sprintf("/status/volumeStatus/%d", idx)
}

func filterVolumes(in []v1.Volume, drop string) []v1.Volume {
	out := make([]v1.Volume, 0, len(in))
	for _, v := range in {
		if v.Name != drop {
			out = append(out, v)
		}
	}
	return out
}

func filterDisks(in []v1.Disk, drop string) []v1.Disk {
	out := make([]v1.Disk, 0, len(in))
	for _, d := range in {
		if d.Name != drop {
			out = append(out, d)
		}
	}
	return out
}

func filterVolumeStatus(in []v1.VolumeStatus, drop string) []v1.VolumeStatus {
	out := make([]v1.VolumeStatus, 0, len(in))
	for _, vs := range in {
		if vs.Name != drop {
			out = append(out, vs)
		}
	}
	return out
}

func deviceFormatToAPI(f apiv1.DeviceFormat) v1.NodeLocalDeviceFormat {
	if f == apiv1.DeviceFormat_DEVICE_FORMAT_FILE {
		return v1.NodeLocalDeviceFormatFile
	}
	return v1.NodeLocalDeviceFormatBlock
}
