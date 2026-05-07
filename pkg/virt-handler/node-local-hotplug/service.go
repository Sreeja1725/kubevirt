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
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	k8sv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"

	apiv1 "kubevirt.io/kubevirt/pkg/virt-handler/node-local-hotplug/v1"
)

// Event reasons emitted on the VMI for AttachDevice / DetachDevice
// outcomes. Match the existing virt-handler convention of CamelCase
// reason strings (see HotplugFailed / NicHotplug in vm.go).
const (
	eventReasonAttached      = "NodeLocalHotplugAttached"
	eventReasonAttachFailed  = "NodeLocalHotplugAttachFailed"
	eventReasonDetached      = "NodeLocalHotplugDetached"
	eventReasonDetachFailed  = "NodeLocalHotplugDetachFailed"
)

// Service implements the gRPC NodeLocalHotplug API.
//
// The service is a pure spec-mutator + node-local mount helper. It does
// NOT call SyncVirtualMachine on virt-launcher: virt-handler's existing
// VirtualMachineController owns that. Patching the VMI here triggers a
// watch event which drives the standard reconcile (which in turn calls
// SyncVirtualMachine, runs the converter, and lets libvirt's
// AttachDeviceFlags / DetachDeviceFlags happen). This avoids racing the
// controller for ownership of the launcher's sync.
type Service struct {
	vmiStore    cache.Store
	domainStore cache.Store
	virtCli     kubecli.KubevirtClient
	mounter     Mounter
	// recorder, when non-nil, is used to emit Normal/Warning Events on
	// the VMI for AttachDevice / DetachDevice outcomes. Lets operators
	// see the result via `kubectl describe vmi` without needing to
	// scrape the gRPC caller's logs.
	recorder record.EventRecorder
	// host is the local node name used to scope Recover() to VMIs
	// running on this virt-handler instance.
	host string

	// per-VMI-UID mutex map so attach/detach on the same VMI serialise.
	// Entries are reference-counted: when the last waiter drops the
	// lock, the entry is removed so the map cannot grow unbounded as
	// VMIs come and go.
	mu      sync.Mutex
	vmiLock map[types.UID]*vmiLockEntry
}

type vmiLockEntry struct {
	mu   sync.Mutex
	refs int
}

// NewService builds a Service ready to be registered with a gRPC server.
// host must be the same node name virt-handler uses elsewhere (see
// app.HostOverride); it scopes Recover() to local VMIs. domainStore is
// the local Domain informer's cache; DetachDevice uses it to wait for
// the launcher's libvirt domain to drop the disk before unmounting the
// host-side staging. recorder may be nil (in which case Events are
// silently dropped) for tests and CLI use; production virt-handler
// always passes the shared recorder so failures appear on the VMI.
func NewService(
	vmiStore cache.Store,
	domainStore cache.Store,
	virtCli kubecli.KubevirtClient,
	mounter Mounter,
	recorder record.EventRecorder,
	host string,
) *Service {
	return &Service{
		vmiStore:    vmiStore,
		domainStore: domainStore,
		virtCli:     virtCli,
		mounter:     mounter,
		recorder:    recorder,
		host:        host,
		vmiLock:     make(map[types.UID]*vmiLockEntry),
	}
}

// emit records an Event against vmi if a recorder is configured. Safe
// to call with a nil vmi (best-effort — used by failure paths where
// lookupVMI may have failed before we could resolve the object).
func (s *Service) emit(vmi *v1.VirtualMachineInstance, eventType, reason, msg string) {
	if s.recorder == nil || vmi == nil {
		return
	}
	s.recorder.Event(vmi, eventType, reason, msg)
}

// lockVMI returns a release func that unlocks AND drops the VMI's lock
// entry if no other goroutine is waiting on it. Callers MUST defer the
// returned func.
func (s *Service) lockVMI(uid types.UID) func() {
	s.mu.Lock()
	e, ok := s.vmiLock[uid]
	if !ok {
		e = &vmiLockEntry{}
		s.vmiLock[uid] = e
	}
	e.refs++
	s.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		s.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(s.vmiLock, uid)
		}
		s.mu.Unlock()
	}
}

// Recover re-asserts the host-side staging for every NodeLocalDevice
// volume in vmi.Spec.Volumes for VMIs running on this node. It is
// safe to call any time the informer cache is populated, but the
// natural time is once after virt-handler startup, before the gRPC
// server begins serving.
//
// Recover only handles the "spec says volume is here, host stage is
// missing" half of the recovery problem: the converter would otherwise
// fail to build a libvirt disk source for a volume whose mknod /
// bind-mount was wiped by a node reboot or virt-handler crash. The
// reverse case (host stage exists, spec entry never landed) is left
// alone: a subsequent AttachDevice on the same volume_name is
// idempotent and re-asserts; the next Detach removes any leftover.
//
// Errors per VMI/volume are logged and skipped; Recover never returns
// an error so it can't gate startup. The per-VMI lock is acquired so
// recovery can't race with a concurrent Attach/Detach.
func (s *Service) Recover(ctx context.Context) {
	if s.host == "" {
		log.Log.Warning("[node-local-hotplug] Recover skipped: empty host")
		return
	}
	for _, obj := range s.vmiStore.List() {
		if err := ctx.Err(); err != nil {
			log.Log.Reason(err).Warning("[node-local-hotplug] Recover cancelled")
			return
		}
		vmi, ok := obj.(*v1.VirtualMachineInstance)
		if !ok {
			continue
		}
		if vmi.Status.NodeName != s.host {
			continue
		}
		// Only Running VMIs have a launcher pod able to host a
		// hotplug-disks dir. Pre-Running phases haven't created the
		// staging dir yet; post-Running phases (Succeeded/Failed)
		// don't need it.
		if vmi.Status.Phase != v1.Running {
			continue
		}
		if !hasNodeLocalDeviceVolume(vmi) {
			continue
		}
		s.recoverVMI(vmi)
	}
}

// recoverVMI re-asserts mounts for the given VMI under its lock and,
// for any volumeStatus entry stuck at phase=Bound (indicating a
// previous AttachDevice crashed between the intent patch and the
// mounted patch), advances it to MountedToPod after the mount has
// been re-asserted.
func (s *Service) recoverVMI(vmi *v1.VirtualMachineInstance) {
	unlock := s.lockVMI(vmi.UID)
	defer unlock()

	diskRO := readonlyByDiskName(vmi)
	statusPhase := volumeStatusPhaseByName(vmi)
	// Best-effort: cap the time we spend per VMI so a stuck patch
	// retry can't wedge startup.
	ctx, cancel := context.WithTimeout(context.Background(), recoverPerVMITimeout)
	defer cancel()
	for i := range vmi.Spec.Volumes {
		vol := &vmi.Spec.Volumes[i]
		nl := vol.VolumeSource.NodeLocalDevice
		if nl == nil {
			continue
		}
		mounted := false
		switch nl.Format {
		case v1.NodeLocalDeviceFormatBlock:
			if err := s.mounter.MountBlock(vmi, vol.Name, nl.Path); err != nil {
				log.Log.Object(vmi).Warningf("[node-local-hotplug] Recover MountBlock %s (%s): %v", vol.Name, nl.Path, err)
				continue
			}
			mounted = true
			log.Log.Object(vmi).V(2).Infof("[node-local-hotplug] Recover re-asserted block mount %s", vol.Name)
		case v1.NodeLocalDeviceFormatFile:
			if err := s.mounter.MountFile(vmi, vol.Name, nl.Path, diskRO[vol.Name]); err != nil {
				log.Log.Object(vmi).Warningf("[node-local-hotplug] Recover MountFile %s (%s): %v", vol.Name, nl.Path, err)
				continue
			}
			mounted = true
			log.Log.Object(vmi).V(2).Infof("[node-local-hotplug] Recover re-asserted file mount %s", vol.Name)
		default:
			log.Log.Object(vmi).Warningf("[node-local-hotplug] Recover unknown NodeLocalDevice format %q for %s", nl.Format, vol.Name)
		}
		if !mounted {
			continue
		}
		// Advance Bound -> MountedToPod for entries that the previous
		// process didn't get a chance to. applyAttachMountedPatch is
		// idempotent: it returns success without patching when the
		// entry is already at MountedToPod or Ready.
		if statusPhase[vol.Name] == v1.VolumeBound {
			if err := s.applyAttachMountedPatch(ctx, vmi, vol.Name); err != nil {
				log.Log.Object(vmi).Warningf("[node-local-hotplug] Recover advance %s Bound->MountedToPod: %v", vol.Name, err)
				continue
			}
			log.Log.Object(vmi).Infof("[node-local-hotplug] Recover advanced %s Bound->MountedToPod after re-mount", vol.Name)
		}
	}
}

// hasNodeLocalDeviceVolume reports whether vmi has at least one
// NodeLocalDevice volume; used to short-circuit Recover.
func hasNodeLocalDeviceVolume(vmi *v1.VirtualMachineInstance) bool {
	for _, v := range vmi.Spec.Volumes {
		if v.VolumeSource.NodeLocalDevice != nil {
			return true
		}
	}
	return false
}

// readonlyByDiskName indexes the readonly flag from
// Spec.Domain.Devices.Disks by disk name, so Recover can re-apply
// the original bind-mount mode for file-backed NodeLocalDevice volumes.
func readonlyByDiskName(vmi *v1.VirtualMachineInstance) map[string]bool {
	out := make(map[string]bool, len(vmi.Spec.Domain.Devices.Disks))
	for _, d := range vmi.Spec.Domain.Devices.Disks {
		if d.DiskDevice.Disk != nil {
			out[d.Name] = d.DiskDevice.Disk.ReadOnly
		}
	}
	return out
}

// volumeStatusPhaseByName indexes vmi.Status.VolumeStatus by name so
// Recover can detect entries stuck at phase=Bound (the previous
// process landed the intent patch but never landed the mounted
// patch).
func volumeStatusPhaseByName(vmi *v1.VirtualMachineInstance) map[string]v1.VolumePhase {
	out := make(map[string]v1.VolumePhase, len(vmi.Status.VolumeStatus))
	for i := range vmi.Status.VolumeStatus {
		out[vmi.Status.VolumeStatus[i].Name] = vmi.Status.VolumeStatus[i].Phase
	}
	return out
}

// recoverPerVMITimeout caps the wall-clock spent recovering a single
// VMI so a stuck PATCH retry can't wedge virt-handler startup. The
// natural retry budget is retry.DefaultRetry (~5 attempts, ~10s
// total); we add headroom for two slow apiserver round-trips.
const recoverPerVMITimeout = 30 * time.Second

// AttachDevice implements the gRPC AttachDevice RPC.
//
// On success the host device is mounted into the launcher pod and the
// VMI's spec/status carries a NodeLocalDevice volume + matching disk
// entry. The actual libvirt AttachDeviceFlags is performed
// asynchronously by virt-handler's normal VMI reconcile (triggered by
// our patches). The caller can poll vmi.Status.VolumeStatus[<volume>]
// to wait for VolumeReady if synchronous-attach semantics are needed.
//
// AttachDevice runs a three-step state machine, observable via
// VolumeStatus[<volume>].phase:
//
//	 1. Bound          (intent patch)  - validation passed, host path
//	                                     is known good, bind-mount /
//	                                     mknod into launcher pending.
//	 2. MountedToPod   (mounted patch) - host-side staging done; the
//	                                     launcher can read the disk.
//	 3. Ready          (downstream)    - libvirt has live-attached the
//	                                     disk; set by virt-handler's
//	                                     normal reconcile when the
//	                                     domain disk alias appears.
//
// Spec-first ordering: spec.volumes / spec.domain.devices.disks are
// added in step 1, BEFORE the mount, so spec is the source of truth.
// If we crash between step 1 and step 3, Recover() iterates spec and
// re-asserts the mount + advances stuck-at-Bound entries to
// MountedToPod - we never get an "orphan host mount with no spec
// entry" recovery problem.
//
// AttachDevice is idempotent: if the named volume is already attached
// to this VMI with the same path/format, the RPC re-asserts the
// mount and re-asserts MountedToPod (both are no-ops if already at
// the desired state) and returns Success.
func (s *Service) AttachDevice(ctx context.Context, req *apiv1.AttachDeviceRequest) (*apiv1.AttachDeviceResponse, error) {
	vmi, err := s.lookupVMI(req.GetNamespace(), req.GetName())
	if err != nil {
		// vmi may be nil here; emit best-effort and return.
		return s.failAttach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VMI_NOT_FOUND, err), nil
	}
	unlock := s.lockVMI(vmi.UID)
	defer unlock()

	if err := validateAttach(req, vmi); err != nil {
		return s.failAttach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, err), nil
	}

	class, err := classifyAttachAgainstExisting(req, vmi)
	if err != nil {
		return s.failAttach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VOLUME_CONFLICT, err), nil
	}

	dev := req.GetDevice()
	log.Log.Object(vmi).V(1).Infof("[node-local-hotplug] AttachDevice volume=%s path=%s format=%v class=%v", req.VolumeName, dev.GetDevicePath(), dev.GetFormat(), class)

	// Step 1: intent patch (only on a fresh attach). On the
	// idempotent path the volume + disk + status entry are already
	// there - skip directly to the mount + MountedToPod re-assertion.
	//
	// We carry the patched VMI forward (rather than reusing the
	// pre-patch informer copy) so steps 2 and 3 see our just-added
	// /spec/volumes and /status/volumeStatus entries. Without this,
	// applyAttachMountedPatch would findVolumeStatus on stale data
	// and surface "volumeStatus not found" even though the entry is
	// live on the apiserver.
	current := vmi
	if class == attachFresh {
		patched, err := s.applyAttachIntentPatch(ctx, vmi, req)
		if err != nil {
			return s.failAttach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_PATCH_FAILED, fmt.Errorf("intent patch VMI: %w", err)), nil
		}
		current = patched
		log.Log.Object(vmi).Infof("[node-local-hotplug] volume %s staged on VMI %s/%s (phase=Bound); mounting...", req.VolumeName, vmi.Namespace, vmi.Name)
	}

	// Step 2: host-side mount.
	if _, err := s.mountForAttach(current, req); err != nil {
		// If we landed the intent patch in this same call, roll it
		// back so the next retry sees a clean slate. On the
		// idempotent path we don't roll back - the existing entries
		// pre-dated us.
		if class == attachFresh {
			s.rollbackAttachIntent(ctx, current, &apiv1.DetachDeviceRequest{
				Namespace:  req.Namespace,
				Name:       req.Name,
				VmiUid:     req.VmiUid,
				VolumeName: req.VolumeName,
			})
		}
		return s.failAttach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_MOUNT_FAILED, fmt.Errorf("mount: %w", err)), nil
	}

	// Step 3: mounted patch (Bound -> MountedToPod). Idempotent: a
	// no-op when the entry is already at MountedToPod or Ready.
	if err := s.applyAttachMountedPatch(ctx, current, req.VolumeName); err != nil {
		// Mount + spec entries are in place; the next AttachDevice
		// retry or the next Recover() pass will advance the phase.
		// Surface the error so the caller knows step 3 didn't land
		// (their poll loop on VolumeStatus[<vol>].phase would still
		// see Bound, which is inconsistent with returning Success).
		return s.failAttach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_PATCH_FAILED, fmt.Errorf("mounted patch VMI: %w", err)), nil
	}

	if class == attachIdempotent {
		log.Log.Object(vmi).Infof("[node-local-hotplug] volume %s already attached to VMI %s/%s; mount and MountedToPod re-asserted", req.VolumeName, vmi.Namespace, vmi.Name)
		// Idempotent path: don't spam an Event on every retry; one
		// Normal Event was already emitted on the first successful
		// attach.
		return &apiv1.AttachDeviceResponse{
			Success: true,
			Message: "already attached",
			Code:    apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_OK,
		}, nil
	}

	log.Log.Object(vmi).Infof("[node-local-hotplug] volume %s mounted on VMI %s/%s (phase=MountedToPod); libvirt attach driven by reconcile", req.VolumeName, vmi.Namespace, vmi.Name)
	s.emit(vmi, k8sv1.EventTypeNormal, eventReasonAttached,
		fmt.Sprintf("volume %q staged for attach (path=%s, format=%v); libvirt attach is asynchronous, watch VolumeStatus[%s] for VolumeReady", req.VolumeName, dev.GetDevicePath(), dev.GetFormat(), req.VolumeName))
	return &apiv1.AttachDeviceResponse{
		Success: true,
		Code:    apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_OK,
	}, nil
}

// DetachDevice implements the gRPC DetachDevice RPC.
//
// The spec entry is removed first; virt-handler's reconcile then drives
// the libvirt detach. Once the volume is observably gone from
// vmi.Status.VolumeStatus (the signal that the live domain has actually
// released the disk) we unmount the host bind-mount / mknod entry from
// the launcher pod.
//
// DetachDevice is idempotent: if the named volume is not present on
// the VMI it returns Success after a best-effort cleanup of any host-
// side leftover. If the libvirt-side detach does not complete within
// detachWaitTimeout we return a hard failure rather than yanking the
// host device from under a still-attached domain; the caller can
// safely retry.
func (s *Service) DetachDevice(ctx context.Context, req *apiv1.DetachDeviceRequest) (*apiv1.DetachDeviceResponse, error) {
	vmi, err := s.lookupVMI(req.GetNamespace(), req.GetName())
	if err != nil {
		return s.failDetach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_VMI_NOT_FOUND, err), nil
	}
	unlock := s.lockVMI(vmi.UID)
	defer unlock()

	if err := validateDetach(req, vmi); err != nil {
		return s.failDetach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_INVALID_REQUEST, err), nil
	}

	vol := getVolume(vmi, req.VolumeName)
	if vol == nil {
		// Idempotent no-op. Best-effort cleanup of any orphaned host
		// mount left behind by a previous crashed Detach; format is
		// unknown so try both. Don't emit an Event - the caller is
		// retrying after success.
		s.bestEffortCleanupBothFormats(vmi, req.VolumeName)
		log.Log.Object(vmi).Infof("[node-local-hotplug] DetachDevice volume %s already absent on VMI %s/%s; no-op", req.VolumeName, vmi.Namespace, vmi.Name)
		return &apiv1.DetachDeviceResponse{
			Success: true,
			Message: "already detached",
			Code:    apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_OK,
		}, nil
	}
	format := vol.VolumeSource.NodeLocalDevice.Format

	if _, err := s.applyDetachPatch(ctx, vmi, req); err != nil {
		return s.failDetach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_PATCH_FAILED, fmt.Errorf("patch VMI: %w", err)), nil
	}

	// Wait until the local libvirt domain no longer carries the disk.
	// The domain informer is updated by the launcher's metadata channel
	// after AttachDeviceFlags / DetachDeviceFlags return, so the disk
	// disappearing from the cached domain spec is our authoritative
	// signal that libvirt has released the device. If we never see
	// that, surface a timeout to the caller instead of unmounting
	// under a still-attached domain.
	if err := s.waitDomainDiskAbsent(ctx, vmi.Namespace, vmi.Name, req.VolumeName, detachWaitTimeout); err != nil {
		return s.failDetach(vmi, req, apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_LIBVIRT_TIMEOUT, fmt.Errorf("libvirt detach did not complete in %s: %w", detachWaitTimeout, err)), nil
	}
	if err := s.unmountForDetach(vmi, req.VolumeName, format); err != nil {
		log.Log.Object(vmi).Warningf("[node-local-hotplug] post-detach unmount failed for volume %s: %v", req.VolumeName, err)
	}

	log.Log.Object(vmi).Infof("[node-local-hotplug] volume %s detached from VMI %s/%s", req.VolumeName, vmi.Namespace, vmi.Name)
	s.emit(vmi, k8sv1.EventTypeNormal, eventReasonDetached,
		fmt.Sprintf("volume %q detached", req.VolumeName))
	return &apiv1.DetachDeviceResponse{
		Success: true,
		Code:    apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_OK,
	}, nil
}

// bestEffortCleanupBothFormats is invoked on the idempotent DetachDevice
// path to remove any host-side leftover when the spec already shows the
// volume is gone. We don't know whether the previous attach was block
// or file, so we attempt both; both unmount paths are themselves
// idempotent.
func (s *Service) bestEffortCleanupBothFormats(vmi *v1.VirtualMachineInstance, volumeName string) {
	if err := s.mounter.UnmountBlock(vmi, volumeName); err != nil {
		log.Log.Object(vmi).V(2).Infof("[node-local-hotplug] best-effort UnmountBlock %s: %v", volumeName, err)
	}
	if err := s.mounter.UnmountFile(vmi, volumeName); err != nil {
		log.Log.Object(vmi).V(2).Infof("[node-local-hotplug] best-effort UnmountFile %s: %v", volumeName, err)
	}
}

func (s *Service) lookupVMI(namespace, name string) (*v1.VirtualMachineInstance, error) {
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("namespace and name are required")
	}
	obj, exists, err := s.vmiStore.GetByKey(namespace + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("informer lookup: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("VMI %s/%s not found on this node", namespace, name)
	}
	vmi, ok := obj.(*v1.VirtualMachineInstance)
	if !ok {
		return nil, fmt.Errorf("informer returned unexpected type %T for %s/%s", obj, namespace, name)
	}
	return vmi.DeepCopy(), nil
}

func (s *Service) mountForAttach(vmi *v1.VirtualMachineInstance, req *apiv1.AttachDeviceRequest) (undo func(), err error) {
	dev := req.GetDevice()
	switch dev.GetFormat() {
	case apiv1.DeviceFormat_DEVICE_FORMAT_BLOCK:
		if err := s.mounter.MountBlock(vmi, req.VolumeName, dev.GetDevicePath()); err != nil {
			return func() {}, err
		}
		return func() {
			if err := s.mounter.UnmountBlock(vmi, req.VolumeName); err != nil {
				log.Log.Object(vmi).Warningf("[node-local-hotplug] rollback unmount block %s: %v", req.VolumeName, err)
			}
		}, nil
	case apiv1.DeviceFormat_DEVICE_FORMAT_FILE:
		if err := s.mounter.MountFile(vmi, req.VolumeName, dev.GetDevicePath(), dev.GetReadonly()); err != nil {
			return func() {}, err
		}
		return func() {
			if err := s.mounter.UnmountFile(vmi, req.VolumeName); err != nil {
				log.Log.Object(vmi).Warningf("[node-local-hotplug] rollback unmount file %s: %v", req.VolumeName, err)
			}
		}, nil
	default:
		return func() {}, fmt.Errorf("unsupported device.format %v", dev.GetFormat())
	}
}

func (s *Service) unmountForDetach(vmi *v1.VirtualMachineInstance, volumeName string, format v1.NodeLocalDeviceFormat) error {
	if format == v1.NodeLocalDeviceFormatFile {
		return s.mounter.UnmountFile(vmi, volumeName)
	}
	return s.mounter.UnmountBlock(vmi, volumeName)
}

// errVolumeStatusStale is the sentinel used by patch-helpers when the
// in-memory VMI we built the patch from is missing a volumeStatus
// entry that the apiserver definitely has (because we just added it
// in the previous patch). The classifier below treats it as
// retriable so retry.OnError loops with a freshly-GET'd VMI.
var errVolumeStatusStale = fmt.Errorf("volumeStatus stale on cached VMI")

// isPatchRetriable returns true when the error from a JSONPatch apply
// should be retried with a fresh read of the VMI: a resourceVersion
// Conflict (409), an Invalid (422 — what the apiserver returns when a
// JSONPatch "test" op fails because the field changed underneath us),
// or our own errVolumeStatusStale (the cached VMI we built the patch
// from is missing an entry we just added; the next iteration with a
// fresh GET will see it). All three resolve by re-reading and
// re-building the patch.
func isPatchRetriable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsInvalid(err) ||
		errors.Is(err, errVolumeStatusStale)
}

// applyAttachIntentPatch sends the FIRST of two attach patches: it
// adds the volume + disk to spec and seeds an initial volumeStatus
// entry at phase=Bound. It carries JSONPatch "test" ops on the three
// slices so concurrent writes to other VMI fields do NOT cause us to
// conflict. On a true conflict (Conflict / Invalid) we re-GET the
// VMI, re-validate, re-classify, and retry. If the re-classification
// shows the volume already landed (idempotent), we return the fresh
// VMI without further patching.
//
// We deliberately use Patch (not Update) because VMI is a hot object
// whose status is continuously rewritten by virt-handler reconcile
// and virt-controller. An Update-based approach would conflict on
// every concurrent status write; the JSONPatch + targeted "test"
// design narrows the conflict surface to the slices we actually
// touch.
//
// Why we can write /status/volumeStatus through the main resource
// patch: VMI's CRD does NOT enable the "/status" subresource (see
// NewVirtualMachineInstanceCrd). If that ever changes, this code
// must split the spec ops and the status ops across two requests.
func (s *Service) applyAttachIntentPatch(ctx context.Context, vmi *v1.VirtualMachineInstance, req *apiv1.AttachDeviceRequest) (*v1.VirtualMachineInstance, error) {
	current := vmi
	var patched *v1.VirtualMachineInstance
	err := retry.OnError(retry.DefaultRetry, isPatchRetriable, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, err := buildAttachIntentPatch(current, req)
		if err != nil {
			return err
		}
		patched, err = s.virtCli.VirtualMachineInstance(current.Namespace).Patch(ctx, current.Name, types.JSONPatchType, payload, metav1.PatchOptions{})
		if err != nil && isPatchRetriable(err) {
			fresh, getErr := s.virtCli.VirtualMachineInstance(current.Namespace).Get(ctx, current.Name, metav1.GetOptions{})
			if getErr != nil {
				return err
			}
			if vErr := validateAttach(req, fresh); vErr != nil {
				return vErr
			}
			// Re-classify on the fresh VMI; if the volume showed up
			// in the meantime with a different shape, surface that.
			if class, cErr := classifyAttachAgainstExisting(req, fresh); cErr != nil {
				return cErr
			} else if class == attachIdempotent {
				patched = fresh
				return nil
			}
			current = fresh
		}
		return err
	})
	return patched, err
}

// applyAttachMountedPatch sends the SECOND of two attach patches: it
// transitions the existing volumeStatus entry from phase=Bound to
// phase=MountedToPod, signalling that the host-side bind-mount /
// mknod has succeeded and the launcher pod can now consume the
// device. It is idempotent: if the entry is already at
// MountedToPod or VolumeReady (e.g. a concurrent reconcile observed
// the live disk first), we return success without patching.
//
// On Conflict / Invalid (someone else mutated the entry between our
// GET and our PATCH) we re-GET, re-evaluate, and retry.
func (s *Service) applyAttachMountedPatch(ctx context.Context, vmi *v1.VirtualMachineInstance, volumeName string) error {
	current := vmi
	return retry.OnError(retry.DefaultRetry, isPatchRetriable, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, vs, ferr := findVolumeStatus(current, volumeName)
		if ferr != nil {
			// Two reasons we land here:
			//   1. The cached VMI we were handed is older than the
			//      intent patch we just sent (caller passed the
			//      pre-patch informer copy). A fresh GET will see
			//      the entry.
			//   2. Some other controller wiped the entry between
			//      the intent patch landing and now. A fresh GET
			//      will confirm and the next iteration's
			//      findVolumeStatus will fail again, ultimately
			//      surfacing PATCH_FAILED to the caller. Recover()
			//      will sweep the orphaned host-stage on next
			//      virt-handler restart.
			fresh, getErr := s.virtCli.VirtualMachineInstance(current.Namespace).Get(ctx, current.Name, metav1.GetOptions{})
			if getErr != nil {
				return ferr
			}
			current = fresh
			// Wrap the not-found error so isPatchRetriable matches
			// it (errors.Is) and retry.OnError reruns this closure
			// with the freshly-GET'd VMI bound to current.
			return fmt.Errorf("%w: %v", errVolumeStatusStale, ferr)
		}
		// Already at-or-past MountedToPod: nothing to do.
		if vs.Phase == v1.HotplugVolumeMounted || vs.Phase == v1.VolumeReady {
			return nil
		}
		payload, err := buildAttachMountedPatch(current, volumeName)
		if err != nil {
			return err
		}
		_, err = s.virtCli.VirtualMachineInstance(current.Namespace).Patch(ctx, current.Name, types.JSONPatchType, payload, metav1.PatchOptions{})
		if err != nil && isPatchRetriable(err) {
			fresh, getErr := s.virtCli.VirtualMachineInstance(current.Namespace).Get(ctx, current.Name, metav1.GetOptions{})
			if getErr != nil {
				return err
			}
			current = fresh
		}
		return err
	})
}

// applyDetachPatch sends a JSONPatch for the desired detach state with
// the same retry-on-Conflict / retry-on-Invalid semantics as
// applyAttachIntentPatch. Detach stays single-phase: a Detaching
// observability window between two patches would be near-zero
// because the wait-for-libvirt + unmount happen AFTER spec removal,
// not between patches.
func (s *Service) applyDetachPatch(ctx context.Context, vmi *v1.VirtualMachineInstance, req *apiv1.DetachDeviceRequest) (*v1.VirtualMachineInstance, error) {
	current := vmi
	var patched *v1.VirtualMachineInstance
	err := retry.OnError(retry.DefaultRetry, isPatchRetriable, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// If the volume disappeared since we last observed it (e.g. a
		// concurrent detach landed first), this is an idempotent
		// success; nothing to patch.
		if !hasVolume(current, req.VolumeName) {
			patched = current
			return nil
		}
		payload, err := buildDetachPatch(current, req.VolumeName)
		if err != nil {
			return err
		}
		patched, err = s.virtCli.VirtualMachineInstance(current.Namespace).Patch(ctx, current.Name, types.JSONPatchType, payload, metav1.PatchOptions{})
		if err != nil && isPatchRetriable(err) {
			fresh, getErr := s.virtCli.VirtualMachineInstance(current.Namespace).Get(ctx, current.Name, metav1.GetOptions{})
			if getErr != nil {
				return err
			}
			// Re-validate: between our last observation and now the
			// volume might have morphed into a non-NodeLocalDevice
			// source, become the boot disk, or had the VMI start
			// migrating. validateDetach is silent on absence (the
			// idempotent no-op is handled by the hasVolume check
			// above on the next iteration).
			if vErr := validateDetach(req, fresh); vErr != nil {
				return vErr
			}
			current = fresh
		}
		return err
	})
	return patched, err
}

// rollbackAttachIntent is a best-effort cleanup invoked when the
// host-side mount fails AFTER the intent patch (Bound) has already
// landed. It applies the same payload as a regular Detach so the
// caller can safely retry without leaving the VMI in a half-staged
// state. Errors are logged at warning level only - a stuck
// volume / disk / Bound entry is recoverable: the next
// AttachDevice retry will re-classify, re-mount, and finish; if
// the caller never retries, Recover() will mount + advance to
// MountedToPod.
func (s *Service) rollbackAttachIntent(ctx context.Context, vmi *v1.VirtualMachineInstance, req *apiv1.DetachDeviceRequest) {
	if _, err := s.applyDetachPatch(ctx, vmi, req); err != nil {
		log.Log.Object(vmi).Warningf("[node-local-hotplug] rollback of attach intent for volume %s failed: %v", req.VolumeName, err)
	}
}

// detachWaitTimeout bounds how long DetachDevice will wait for the
// launcher's libvirt domain to drop the disk before we proceed with
// the host-side unmount anyway (we surface a timeout to the caller
// rather than yanking a still-in-use device).
const detachWaitTimeout = 30 * time.Second

// detachPollInterval is the interval at which DetachDevice polls the
// domain informer cache to see whether the disk has been released.
const detachPollInterval = 250 * time.Millisecond

// waitDomainDiskAbsent blocks until the named volume is no longer
// present in the local libvirt domain's disk list, which is the signal
// that the standard reconcile has driven SyncVMI on the launcher and
// libvirt's DetachDeviceFlags has returned (the launcher pushes its
// updated DomainSpec via the metadata socket; the notify-server feeds
// the domain informer this Service reads from).
//
// Polling vmi.Spec/Status would be unsound: Spec was just rewritten by
// our own patch, and Status entries for NodeLocalDevice volumes are
// removed by virt-controller when the spec entry vanishes - neither
// reflects what libvirt has actually done. Returns nil when the disk
// is gone (or the domain itself is gone), the context error if
// cancelled, or a timeout error after the bound elapses.
func (s *Service) waitDomainDiskAbsent(ctx context.Context, namespace, name, volumeName string, timeout time.Duration) error {
	if s.domainStore == nil {
		return fmt.Errorf("domain informer not configured")
	}
	deadline := time.Now().Add(timeout)
	key := namespace + "/" + name
	for {
		if !s.domainHasDisk(key, volumeName) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for libvirt domain %s/%s to release disk %s", timeout, namespace, name, volumeName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(detachPollInterval):
		}
	}
}

// domainHasDisk reports whether the local libvirt domain at key still
// lists a disk whose alias matches volumeName. A missing or
// soft-deleted (DeletionTimestamp set) domain is treated as
// disk-absent: there is no longer anything libvirt can be holding on
// to. Cache-lookup errors are treated as disk-still-present so the
// poll loop retries rather than declaring detach complete prematurely.
func (s *Service) domainHasDisk(key, volumeName string) bool {
	obj, exists, err := s.domainStore.GetByKey(key)
	if err != nil || !exists {
		return false
	}
	domain, ok := obj.(*api.Domain)
	if !ok {
		return false
	}
	if domain.ObjectMeta.DeletionTimestamp != nil {
		return false
	}
	for i := range domain.Spec.Devices.Disks {
		alias := domain.Spec.Devices.Disks[i].Alias
		if alias != nil && alias.GetName() == volumeName {
			return true
		}
	}
	return false
}

// failAttach builds an AttachDeviceResponse for a failure path. It:
//   - extracts a precise NodeLocalHotplugErrorCode from err if it was
//     produced by validator.go (or any other call site that wrapped
//     with codedError); fallback is used otherwise.
//   - emits a Warning Event on the VMI so operators see the failure
//     via `kubectl describe vmi` without needing to scrape the gRPC
//     caller's logs. vmi may be nil (e.g. lookup failed before we had
//     an object) - emit() handles that.
//   - logs at warning level with full context.
func (s *Service) failAttach(vmi *v1.VirtualMachineInstance, req *apiv1.AttachDeviceRequest, fallback apiv1.NodeLocalHotplugErrorCode, err error) *apiv1.AttachDeviceResponse {
	code := codeOf(err, fallback)
	msg := err.Error()
	if vmi != nil {
		log.Log.Object(vmi).Warningf("[node-local-hotplug] AttachDevice volume=%s failed (code=%s): %v", req.GetVolumeName(), code, err)
	} else {
		log.Log.Warningf("[node-local-hotplug] AttachDevice ns=%s name=%s volume=%s failed (code=%s): %v", req.GetNamespace(), req.GetName(), req.GetVolumeName(), code, err)
	}
	s.emit(vmi, k8sv1.EventTypeWarning, eventReasonAttachFailed,
		fmt.Sprintf("volume %q attach failed: %s (code=%s)", req.GetVolumeName(), msg, code))
	return &apiv1.AttachDeviceResponse{
		Success: false,
		Message: msg,
		Code:    code,
	}
}

// failDetach is the DetachDevice mirror of failAttach.
func (s *Service) failDetach(vmi *v1.VirtualMachineInstance, req *apiv1.DetachDeviceRequest, fallback apiv1.NodeLocalHotplugErrorCode, err error) *apiv1.DetachDeviceResponse {
	code := codeOf(err, fallback)
	msg := err.Error()
	if vmi != nil {
		log.Log.Object(vmi).Warningf("[node-local-hotplug] DetachDevice volume=%s failed (code=%s): %v", req.GetVolumeName(), code, err)
	} else {
		log.Log.Warningf("[node-local-hotplug] DetachDevice ns=%s name=%s volume=%s failed (code=%s): %v", req.GetNamespace(), req.GetName(), req.GetVolumeName(), code, err)
	}
	s.emit(vmi, k8sv1.EventTypeWarning, eventReasonDetachFailed,
		fmt.Sprintf("volume %q detach failed: %s (code=%s)", req.GetVolumeName(), msg, code))
	return &apiv1.DetachDeviceResponse{
		Success: false,
		Message: msg,
		Code:    code,
	}
}
