package nodelocalhotplug

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

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"

	"kubevirt.io/client-go/log"

	grpcutil "kubevirt.io/kubevirt/pkg/util/net/grpc"
	apiv1 "kubevirt.io/kubevirt/pkg/virt-handler/node-local-hotplug/v1"
)

// DefaultSocketPath is the kubelet plugin-style location used by the
// node-local hotplug API. virt-handler exposes the directory as a
// hostPath volume and creates this socket on startup.
const DefaultSocketPath = "/var/lib/kubelet/plugins/node-local-hotplug/grpc.sock"

// shutdownGraceSeconds is how long RunServer waits for in-flight RPCs
// to finish before forcing the gRPC server down.
const shutdownGraceSeconds = 10

// RunServer starts a UDS-backed gRPC server hosting the NodeLocalHotplug
// service. It blocks until stopChan is closed, then performs a graceful
// shutdown bounded by shutdownGraceSeconds.
//
// The directory containing socketPath must already exist (in the
// virt-handler DaemonSet this is provided as a hostPath volume).
func RunServer(socketPath string, stopChan <-chan struct{}, svc *Service) error {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return fmt.Errorf("create node-local hotplug socket dir: %w", err)
	}

	listener, err := grpcutil.CreateSocket(socketPath)
	if err != nil {
		return fmt.Errorf("create node-local hotplug socket %q: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("chmod node-local hotplug socket: %w", err)
	}

	server := grpc.NewServer()
	apiv1.RegisterNodeLocalHotplugServer(server, svc)

	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Log.Infof("[node-local-hotplug] gRPC listening on unix://%s", socketPath)
		if err := server.Serve(listener); err != nil {
			log.Log.Reason(err).Error("[node-local-hotplug] gRPC server exited")
		}
	}()

	select {
	case <-done:
		log.Log.Info("[node-local-hotplug] gRPC server done")
	case <-stopChan:
		gracefulStop(server)
	}
	return nil
}

func gracefulStop(server *grpc.Server) {
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()

	timer := time.NewTimer(time.Duration(shutdownGraceSeconds) * time.Second)
	defer timer.Stop()

	select {
	case <-timer.C:
		log.Log.Infof("[node-local-hotplug] GracefulStop timed out after %ds, forcing Stop", shutdownGraceSeconds)
		server.Stop()
	case <-stopped:
		log.Log.Infof("[node-local-hotplug] GracefulStop complete")
	}
}
