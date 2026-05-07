/*
This file is part of the KubeVirt project

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Copyright The KubeVirt Authors.
*/

// nlh-cli is a small developer tool for talking to virt-handler's
// NodeLocalHotplug gRPC service over its unix domain socket. It is
// intended for manual testing of the attach/detach flow on a live
// cluster while the feature is being brought up; it is NOT a
// supported user-facing tool.
//
// Build a static linux/amd64 binary:
//
//	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
//	    go build -o nlh-cli ./cmd/nlh-cli
//
// Then either:
//   - copy it into the virt-handler pod and exec it there, or
//   - run it from any pod that hostPath-mounts
//     /var/lib/kubelet/plugins/node-local-hotplug.
//
// Examples (run inside the virt-handler pod):
//
//	nlh-cli attach \
//	    -namespace default -name nlh-vmi -volume scratch \
//	    -path /dev/loop7 -format block -bus virtio
//
//	nlh-cli detach \
//	    -namespace default -name nlh-vmi -volume scratch
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	apiv1 "kubevirt.io/kubevirt/pkg/virt-handler/node-local-hotplug/v1"
)

const (
	defaultSocket  = "/var/lib/kubelet/plugins/node-local-hotplug/grpc.sock"
	defaultTimeout = 60 * time.Second
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "attach":
		os.Exit(runAttach(os.Args[2:]))
	case "detach":
		os.Exit(runDetach(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `nlh-cli: manual-test client for virt-handler's NodeLocalHotplug gRPC API.

Usage:
  nlh-cli attach -namespace <ns> -name <vmi> -volume <name> -path <host-path> [flags]
  nlh-cli detach -namespace <ns> -name <vmi> -volume <name> [flags]

Run "nlh-cli attach -h" or "nlh-cli detach -h" for the full flag list.
`)
}

func runAttach(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	var (
		socket    = fs.String("socket", defaultSocket, "Path to virt-handler's NodeLocalHotplug UDS.")
		timeout   = fs.Duration("timeout", defaultTimeout, "Overall RPC deadline.")
		namespace = fs.String("namespace", "", "VMI namespace. (required)")
		name      = fs.String("name", "", "VMI name. (required)")
		vmiUID    = fs.String("vmi-uid", "", "Optional: VMI UID fence.")
		volume    = fs.String("volume", "", "Logical volume name to expose inside the VMI. (required)")
		path      = fs.String("path", "", "Absolute host path to the device or file. (required)")
		format    = fs.String("format", "block", "Format of -path: block | file.")
		bus       = fs.String("bus", "virtio", "Libvirt target bus: virtio | scsi.")
		serial    = fs.String("serial", "", "Optional libvirt disk serial.")
		readonly  = fs.Bool("readonly", false, "Attach as read-only.")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := requireFlags(map[string]string{
		"-namespace": *namespace,
		"-name":      *name,
		"-volume":    *volume,
		"-path":      *path,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fs.Usage()
		return 2
	}

	devFmt, err := parseFormat(*format)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	devBus, err := parseBus(*bus)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client, conn, err := dial(ctx, *socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", *socket, err)
		return 1
	}
	defer conn.Close()

	resp, err := client.AttachDevice(ctx, &apiv1.AttachDeviceRequest{
		Namespace:  *namespace,
		Name:       *name,
		VmiUid:     *vmiUID,
		VolumeName: *volume,
		Device: &apiv1.Device{
			DevicePath: *path,
			Format:     devFmt,
			TargetBus:  devBus,
			Serial:     *serial,
			Readonly:   *readonly,
		},
	})
	return reportResult("AttachDevice", resp.GetSuccess(), resp.GetMessage(), err)
}

func runDetach(args []string) int {
	fs := flag.NewFlagSet("detach", flag.ExitOnError)
	var (
		socket    = fs.String("socket", defaultSocket, "Path to virt-handler's NodeLocalHotplug UDS.")
		timeout   = fs.Duration("timeout", defaultTimeout, "Overall RPC deadline.")
		namespace = fs.String("namespace", "", "VMI namespace. (required)")
		name      = fs.String("name", "", "VMI name. (required)")
		vmiUID    = fs.String("vmi-uid", "", "Optional: VMI UID fence.")
		volume    = fs.String("volume", "", "Logical volume name to detach. (required)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := requireFlags(map[string]string{
		"-namespace": *namespace,
		"-name":      *name,
		"-volume":    *volume,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fs.Usage()
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client, conn, err := dial(ctx, *socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", *socket, err)
		return 1
	}
	defer conn.Close()

	resp, err := client.DetachDevice(ctx, &apiv1.DetachDeviceRequest{
		Namespace:  *namespace,
		Name:       *name,
		VmiUid:     *vmiUID,
		VolumeName: *volume,
	})
	return reportResult("DetachDevice", resp.GetSuccess(), resp.GetMessage(), err)
}

func dial(ctx context.Context, socket string) (apiv1.NodeLocalHotplugClient, *grpc.ClientConn, error) {
	conn, err := grpc.DialContext(ctx, "unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(c context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(c, "unix", strings.TrimPrefix(addr, "unix://"))
		}),
	)
	if err != nil {
		return nil, nil, err
	}
	return apiv1.NewNodeLocalHotplugClient(conn), conn, nil
}

func reportResult(rpc string, success bool, message string, err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s RPC error: %v\n", rpc, err)
		return 1
	}
	fmt.Printf("%s success=%v message=%q\n", rpc, success, message)
	if !success {
		return 1
	}
	return 0
}

func requireFlags(req map[string]string) error {
	var missing []string
	for name, val := range req {
		if val == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required flag(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func parseFormat(s string) (apiv1.DeviceFormat, error) {
	switch strings.ToLower(s) {
	case "block":
		return apiv1.DeviceFormat_DEVICE_FORMAT_BLOCK, nil
	case "file":
		return apiv1.DeviceFormat_DEVICE_FORMAT_FILE, nil
	default:
		return 0, fmt.Errorf("unknown -format %q (want block|file)", s)
	}
}

func parseBus(s string) (apiv1.TargetBus, error) {
	switch strings.ToLower(s) {
	case "", "virtio":
		return apiv1.TargetBus_TARGET_BUS_VIRTIO, nil
	case "scsi":
		return apiv1.TargetBus_TARGET_BUS_SCSI, nil
	default:
		return 0, fmt.Errorf("unknown -bus %q (want virtio|scsi)", s)
	}
}
