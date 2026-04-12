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

package v1

import (
	"context"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"

	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"

	pb "kubevirt.io/kubevirt/pkg/virt-handler/node-local-hotplug/v1"
)

const SocketPath = "/var/run/kubevirt/node-local-hotplug.sock"

type Server struct {
	virtCli       kubecli.KubevirtClient
	clusterConfig *virtconfig.ClusterConfig
}

func NewServer(virtCli kubecli.KubevirtClient, clusterConfig *virtconfig.ClusterConfig) *Server {
	return &Server{
		virtCli:       virtCli,
		clusterConfig: clusterConfig,
	}
}

func (s *Server) AttachVolume(ctx context.Context, req *pb.AttachVolumeRequest) (*pb.AttachVolumeResponse, error) {
	ns, vmiName := req.GetNamespace(), req.GetVmiName()
	if ns == "" || vmiName == "" {
		return &pb.AttachVolumeResponse{
			Success: false,
			Message: "namespace and vmi_name are required",
		}, nil
	}

	spec := req.GetAttachSpec()
	if spec == nil {
		return &pb.AttachVolumeResponse{
			Success: false,
			Message: "attach_spec is required",
		}, nil
	}

	if spec.GetName() == "" {
		return &pb.AttachVolumeResponse{
			Success: false,
			Message: "attach_spec.name is required",
		}, nil
	}

	// TODO: resolve host path from PVC→PV, validate it exists on this node,
	// patch VMI spec, and perform the bind-mount into virt-launcher.
	return &pb.AttachVolumeResponse{
		Success: true,
		Message: fmt.Sprintf("attach request accepted for volume %q on %s/%s (not yet implemented)", spec.GetName(), ns, vmiName),
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

	// TODO: unmount the volume from virt-launcher and patch the VMI spec.
	return &pb.RemoveVolumeResponse{
		Success: true,
		Message: fmt.Sprintf("remove request accepted for volume %q on %s/%s (not yet implemented)", volName, ns, vmiName),
	}, nil
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
