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
	"encoding/json"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"

	pb "kubevirt.io/kubevirt/pkg/virt-handler/node-local-hotplug/v1"
)

const SocketPath = "/var/run/kubevirt/node-local-hotplug.sock"

type Server struct {
	host           string
	virtCli        kubecli.KubevirtClient
	clusterConfig  *virtconfig.ClusterConfig
	kubeletPodsDir string
}

func NewServer(host string, virtCli kubecli.KubevirtClient, clusterConfig *virtconfig.ClusterConfig) *Server {
	return &Server{
		host:          host,
		virtCli:       virtCli,
		clusterConfig: clusterConfig,
	}
}

// patchVolumePhase patches a single volume's phase, message, and reason on the
// VMI status subresource. It also ensures HotplugVolume.NodeLocal is set to
// true so that virt-controller skips attachment pod creation for this volume.
func (s *Server) patchVolumePhase(ctx context.Context, ns, vmiName, volumeName string, phase v1.VolumePhase, reason, message string) error {
	vmi, err := s.virtCli.VirtualMachineInstance(ns).Get(ctx, vmiName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get VMI %s/%s: %w", ns, vmiName, err)
	}

	found := false
	for i := range vmi.Status.VolumeStatus {
		vs := &vmi.Status.VolumeStatus[i]
		if vs.Name == volumeName {
			if vs.HotplugVolume == nil {
				vs.HotplugVolume = &v1.HotplugVolumeStatus{}
			}
			vs.HotplugVolume.NodeLocal = true
			vs.Phase = phase
			vs.Reason = reason
			vs.Message = message
			found = true
			break
		}
	}
	if !found {
		vmi.Status.VolumeStatus = append(vmi.Status.VolumeStatus, v1.VolumeStatus{
			Name: volumeName,
			HotplugVolume: &v1.HotplugVolumeStatus{
				NodeLocal: true,
			},
			Phase:   phase,
			Reason:  reason,
			Message: message,
		})
	}

	statusJSON, err := json.Marshal(map[string]interface{}{
		"status": map[string]interface{}{
			"volumeStatus": vmi.Status.VolumeStatus,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}

	_, err = s.virtCli.VirtualMachineInstance(ns).Patch(ctx, vmiName, types.MergePatchType, statusJSON, metav1.PatchOptions{}, "status")
	if err != nil {
		return fmt.Errorf("patch VMI status for volume %q phase %s: %w", volumeName, phase, err)
	}

	log.DefaultLogger().V(3).Infof("Patched volume %q phase to %s on VMI %s/%s", volumeName, phase, ns, vmiName)
	return nil
}

func (s *Server) AttachVolume(ctx context.Context, req *pb.AttachVolumeRequest) (*pb.AttachVolumeResponse, error) {
	ns, vmiName := req.GetNamespace(), req.GetVmiName()
	if ns == "" || vmiName == "" {
		return &pb.AttachVolumeResponse{
			Success: false,
			Message: "namespace and vmi_name are required",
		}, nil
	}

	hasJSON := len(req.GetAttachOptionsJson()) > 0
	hasSpec := req.GetAttachSpec() != nil
	if hasJSON && hasSpec {
		return &pb.AttachVolumeResponse{Success: false, Message: "attach_options_json and attach_spec are mutually exclusive"}, nil
	}
	if !hasJSON && !hasSpec {
		return &pb.AttachVolumeResponse{Success: false, Message: "either attach_options_json or attach_spec is required"}, nil
	}

	var opts *v1.AddVolumeOptions
	var err error
	if hasJSON {
		opts = &v1.AddVolumeOptions{}
		if err := json.Unmarshal(req.GetAttachOptionsJson(), opts); err != nil {
			return &pb.AttachVolumeResponse{Success: false, Message: fmt.Sprintf("invalid attach_options_json: %v", err)}, nil
		}
		if opts.Name == "" {
			return &pb.AttachVolumeResponse{Success: false, Message: "addVolumeOptions.name is required"}, nil
		}
		if opts.Disk == nil || opts.VolumeSource == nil {
			return &pb.AttachVolumeResponse{Success: false, Message: "addVolumeOptions.disk and volumeSource are required"}, nil
		}
	} else {
		opts, err = buildAddVolumeOptions(req.GetAttachSpec())
		if err != nil {
			return &pb.AttachVolumeResponse{Success: false, Message: err.Error()}, nil
		}
	}

	err = s.virtCli.VirtualMachineInstance(ns).AddVolume(ctx, vmiName, opts)
	if err != nil {
		return &pb.AttachVolumeResponse{Success: false, Message: fmt.Sprintf("failed to add volume %q on %s/%s: %v", opts.Name, ns, vmiName, err)}, nil
	}

	// Mark the volume as Bound + NodeLocal so virt-controller skips attachment pod creation.
	if patchErr := s.patchVolumePhase(ctx, ns, vmiName, opts.Name,
		v1.VolumeBound, "NodeLocalHotplug", fmt.Sprintf("Volume %s bound on node via node-local hotplug", opts.Name)); patchErr != nil {
		log.DefaultLogger().Reason(patchErr).Errorf("Failed to patch Bound phase for volume %s on %s/%s", opts.Name, ns, vmiName)
	}

	err = s.attachNodeLocalHotplugToVMI(ctx, s.virtCli, ns, vmiName, opts)
	if err != nil {
		if patchErr := s.patchVolumePhase(ctx, ns, vmiName, opts.Name,
			v1.VolumeBound, "AttachFailed", fmt.Sprintf("Failed to attach volume %s: %v", opts.Name, err)); patchErr != nil {
			log.DefaultLogger().Reason(patchErr).Errorf("Failed to patch error phase for volume %s on %s/%s", opts.Name, ns, vmiName)
		}
		return &pb.AttachVolumeResponse{Success: false, Message: fmt.Sprintf("failed to attach volume %q on %s/%s: %v", opts.Name, ns, vmiName, err)}, nil
	}

	// Attach succeeded — mark AttachedToNode. virt-handler's existing
	// updateHotplugVolumeStatus loop will advance to MountedToPod → Ready.
	if patchErr := s.patchVolumePhase(ctx, ns, vmiName, opts.Name,
		v1.HotplugVolumeAttachedToNode, "NodeLocalHotplug", fmt.Sprintf("Volume %s attached to node via node-local hotplug", opts.Name)); patchErr != nil {
		log.DefaultLogger().Reason(patchErr).Errorf("Failed to patch AttachedToNode phase for volume %s on %s/%s", opts.Name, ns, vmiName)
	}

	return &pb.AttachVolumeResponse{
		Success: true,
		Message: fmt.Sprintf("volume %q attached on %s/%s via node-local hotplug", opts.Name, ns, vmiName),
	}, nil
}

func (s *Server) RemoveVolume(ctx context.Context, req *pb.RemoveVolumeRequest) (*pb.RemoveVolumeResponse, error) {
	ns, vmiName := req.GetNamespace(), req.GetVmiName()
	if ns == "" || vmiName == "" {
		return &pb.RemoveVolumeResponse{
			Success: false,
			Message: "namespace and vmi_name are required",
		}, nil
	}

	volName := req.GetVolumeName()
	if volName == "" {
		return &pb.RemoveVolumeResponse{
			Success: false,
			Message: "volume_name is required",
		}, nil
	}

	opts := &v1.RemoveVolumeOptions{
		Name: volName,
	}

	// Fetch VMI and volume spec before removal so we can perform cleanup.
	vmi, err := s.virtCli.VirtualMachineInstance(ns).Get(ctx, vmiName, metav1.GetOptions{})
	if err != nil {
		return &pb.RemoveVolumeResponse{Success: false, Message: fmt.Sprintf("failed to get VMI %s/%s: %v", ns, vmiName, err)}, nil
	}

	preRemoveVol := volumeSpecByName(vmi, volName)

	volReq := &v1.VirtualMachineVolumeRequest{
		RemoveVolumeOptions: &v1.RemoveVolumeOptions{
			Name:   req.GetVolumeName(),
			DryRun: req.GetDryRun(),
		},
	}
	if err := verifyVolumeOption(vmi.Spec.Volumes, volReq); err != nil {
		return &pb.RemoveVolumeResponse{Success: false, Message: err.Error()}, nil
	}

	if err := s.virtCli.VirtualMachineInstance(ns).RemoveVolume(ctx, vmiName, opts); err != nil {
		return &pb.RemoveVolumeResponse{Success: false, Message: fmt.Sprintf("failed to remove volume %q on %s/%s: %v", volName, ns, vmiName, err)}, nil
	}

	// Signal that detach has started.
	if patchErr := s.patchVolumePhase(ctx, ns, vmiName, volName,
		v1.HotplugVolumeDetaching, "NodeLocalHotplug", fmt.Sprintf("Detaching volume %s via node-local hotplug", volName)); patchErr != nil {
		log.DefaultLogger().Reason(patchErr).Errorf("Failed to patch Detaching phase for volume %s on %s/%s", volName, ns, vmiName)
	}

	if err := s.detachNodeLocalHotplugFromVMI(ctx, ns, vmiName, preRemoveVol); err != nil {
		if patchErr := s.patchVolumePhase(ctx, ns, vmiName, volName,
			v1.HotplugVolumeDetaching, "DetachFailed", fmt.Sprintf("Failed to detach volume %s: %v", volName, err)); patchErr != nil {
			log.DefaultLogger().Reason(patchErr).Errorf("Failed to patch error phase for volume %s on %s/%s", volName, ns, vmiName)
		}
		return &pb.RemoveVolumeResponse{Success: false, Message: fmt.Sprintf("failed to detach volume %q on %s/%s: %v", volName, ns, vmiName, err)}, nil
	}

	// Detach succeeded — mark UnMountedFromPod. virt-controller will drop
	// the status entry once the volume is gone from spec.
	if patchErr := s.patchVolumePhase(ctx, ns, vmiName, volName,
		v1.HotplugVolumeUnMounted, "NodeLocalHotplug", fmt.Sprintf("Volume %s unmounted via node-local hotplug", volName)); patchErr != nil {
		log.DefaultLogger().Reason(patchErr).Errorf("Failed to patch UnMountedFromPod phase for volume %s on %s/%s", volName, ns, vmiName)
	}

	return &pb.RemoveVolumeResponse{
		Success: true,
		Message: fmt.Sprintf("volume %q detached from %s/%s via node-local hotplug", volName, ns, vmiName),
	}, nil
}

func volumeSpecByName(vmi *v1.VirtualMachineInstance, name string) *v1.Volume {
	for i := range vmi.Spec.Volumes {
		if vmi.Spec.Volumes[i].Name == name {
			return &vmi.Spec.Volumes[i]
		}
	}
	return nil
}

// StartUnix starts the gRPC server on a Unix domain socket. It removes any
// stale socket file, binds, registers the service, and serves in the
// background. The server shuts down gracefully when ctx is cancelled.
func StartUnix(ctx context.Context, socketPath string, srv *Server) error {
	if socketPath == "" {
		socketPath = SocketPath
	}

	if err := os.RemoveAll(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale node-local hotplug socket: %w", err)
	}

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %q: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = lis.Close()
		return fmt.Errorf("chmod node-local hotplug socket: %w", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterNodeLocalHotplugServer(grpcServer, srv)

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	logger := log.Log.With("component", "virt-handler-nodelocalhotplug")
	logger.Infof("node-local hotplug gRPC listening on unix://%s", socketPath)

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			logger.Reason(err).Error("node-local hotplug gRPC server exited")
		}
	}()

	return nil
}

func buildAddVolumeOptions(spec *pb.HotplugAttachSpec) (*v1.AddVolumeOptions, error) {
	if spec.GetName() == "" {
		return nil, fmt.Errorf("attach_spec.name is required")
	}
	vs := spec.GetVolumeSource()
	if vs == nil {
		return nil, fmt.Errorf("attach_spec.volume_source is required")
	}
	pvc := vs.GetPvcClaimName()
	dv := vs.GetDataVolumeName()
	if (pvc == "" && dv == "") || (pvc != "" && dv != "") {
		return nil, fmt.Errorf("attach_spec.volume_source must set exactly one of pvc_claim_name or data_volume_name")
	}

	opts := &v1.AddVolumeOptions{
		Name:         spec.GetName(),
		VolumeSource: &v1.HotplugVolumeSource{},
	}
	if pvc != "" {
		opts.VolumeSource.PersistentVolumeClaim = &v1.PersistentVolumeClaimVolumeSource{
			PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvc,
			},
			Hotpluggable: true,
		}
	} else {
		opts.VolumeSource.DataVolume = &v1.DataVolumeSource{
			Name:         dv,
			Hotpluggable: true,
		}
	}

	diskPart := spec.GetDisk()
	if diskPart != nil && len(diskPart.GetDiskJson()) > 0 {
		d := &v1.Disk{}
		if err := json.Unmarshal(diskPart.GetDiskJson(), d); err != nil {
			return nil, fmt.Errorf("invalid disk_json: %w", err)
		}
		opts.Disk = d
	} else {
		opts.Disk = &v1.Disk{
			Name:   spec.GetName(),
			Serial: spec.GetName(),
			DiskDevice: v1.DiskDevice{
				Disk: &v1.DiskTarget{
					Bus: v1.DiskBusSCSI,
				},
			},
		}
	}

	if spec.GetBootOrder() != 0 {
		bo := uint(spec.GetBootOrder())
		opts.Disk.BootOrder = &bo
	}

	return opts, nil
}

func verifyVolumeOption(volumes []v1.Volume, volumeRequest *v1.VirtualMachineVolumeRequest) error {
	foundRemoveVol := false
	for _, volume := range volumes {
		if volumeRequest.AddVolumeOptions != nil {
			volSourceName := volumeSourceName(volumeRequest.AddVolumeOptions.VolumeSource)
			if volumeNameExists(volume, volumeRequest.AddVolumeOptions.Name) {
				return fmt.Errorf("Unable to add volume [%s] because volume with that name already exists", volumeRequest.AddVolumeOptions.Name)
			}
			if volumeSourceExists(volume, volSourceName) {
				return fmt.Errorf("Unable to add volume source [%s] because it already exists", volSourceName)
			}
		} else if volumeRequest.RemoveVolumeOptions != nil && volumeExists(volume, volumeRequest.RemoveVolumeOptions.Name) {
			if !volumeHotpluggable(volume) {
				return fmt.Errorf("Unable to remove volume [%s] because it is not hotpluggable", volume.Name)
			}
			foundRemoveVol = true
			break
		}
	}

	if volumeRequest.RemoveVolumeOptions != nil && !foundRemoveVol {
		return fmt.Errorf("Unable to remove volume [%s] because it does not exist", volumeRequest.RemoveVolumeOptions.Name)
	}

	return nil
}

func volumeHotpluggable(volume v1.Volume) bool {
	return (volume.DataVolume != nil && volume.DataVolume.Hotpluggable) ||
		(volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.Hotpluggable)
}

func volumeSourceExists(volume v1.Volume, volumeName string) bool {
	return (volume.DataVolume != nil && volume.DataVolume.Name == volumeName) ||
		(volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == volumeName)
}

func volumeExists(volume v1.Volume, volumeName string) bool {
	return volumeNameExists(volume, volumeName) || volumeSourceExists(volume, volumeName)
}

func volumeNameExists(volume v1.Volume, volumeName string) bool {
	return volume.Name == volumeName
}

func volumeSourceName(volumeSource *v1.HotplugVolumeSource) string {
	if volumeSource == nil {
		return ""
	}
	if volumeSource.DataVolume != nil {
		return volumeSource.DataVolume.Name
	}
	if volumeSource.PersistentVolumeClaim != nil {
		return volumeSource.PersistentVolumeClaim.ClaimName
	}
	return ""
}
