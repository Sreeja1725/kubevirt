package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "kubevirt.io/kubevirt/pkg/virt-handler/node-local-hotplug/v1"
)

const socketPath = "/var/run/kubevirt/node-local-hotplug.sock"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := pb.NewNodeLocalHotplugClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	switch os.Args[1] {
	case "attach-pvc":
		if len(os.Args) != 6 {
			fmt.Fprintf(os.Stderr, "attach-pvc requires: <namespace> <vmi> <vol-name> <pvc-name>\n")
			os.Exit(1)
		}
		resp, err := client.AttachVolume(ctx, &pb.AttachVolumeRequest{
			Namespace: os.Args[2],
			VmiName:   os.Args[3],
			AttachSpec: &pb.HotplugAttachSpec{
				Name: os.Args[4],
				VolumeSource: &pb.HotplugVolumeSourceSpec{
					PvcClaimName: os.Args[5],
				},
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "AttachVolume RPC error: %v\n", err)
			os.Exit(1)
		}
		printJSON(resp)

	case "attach-ephemeral":
		if len(os.Args) != 6 {
			fmt.Fprintf(os.Stderr, "attach-ephemeral requires: <namespace> <vmi> <vol-name> <size>\n")
			fmt.Fprintf(os.Stderr, "  example: nlh-client attach-ephemeral default test-vmi vol1 1Gi\n")
			os.Exit(1)
		}
		resp, err := client.AttachVolume(ctx, &pb.AttachVolumeRequest{
			Namespace: os.Args[2],
			VmiName:   os.Args[3],
			AttachSpec: &pb.HotplugAttachSpec{
				Name: os.Args[4],
				VolumeSource: &pb.HotplugVolumeSourceSpec{
					CustomVolume: &pb.CustomVolumeSourceSpec{
						EphemeralLocal: &pb.EphemeralCustomSpec{
							Size: os.Args[5],
						},
					},
				},
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "AttachVolume RPC error: %v\n", err)
			os.Exit(1)
		}
		printJSON(resp)

	case "attach-persistent":
		if len(os.Args) < 6 {
			fmt.Fprintf(os.Stderr, "attach-persistent requires: <namespace> <vmi> <vol-name> <handle> [--encrypted]\n")
			os.Exit(1)
		}
		unencrypted := true
		for _, arg := range os.Args[6:] {
			if arg == "--encrypted" {
				unencrypted = false
			}
		}
		resp, err := client.AttachVolume(ctx, &pb.AttachVolumeRequest{
			Namespace: os.Args[2],
			VmiName:   os.Args[3],
			AttachSpec: &pb.HotplugAttachSpec{
				Name: os.Args[4],
				VolumeSource: &pb.HotplugVolumeSourceSpec{
					CustomVolume: &pb.CustomVolumeSourceSpec{
						PersistentRegional: &pb.PersistentRegionalSpec{
							Handle:      os.Args[5],
							Unencrypted: unencrypted,
						},
					},
				},
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "AttachVolume RPC error: %v\n", err)
			os.Exit(1)
		}
		printJSON(resp)

	case "attach-json":
		if len(os.Args) != 5 {
			fmt.Fprintf(os.Stderr, "attach-json requires: <namespace> <vmi> <json-file>\n")
			fmt.Fprintf(os.Stderr, "  The JSON file should contain an AddVolumeOptions payload.\n")
			os.Exit(1)
		}
		data, err := os.ReadFile(os.Args[4])
		if err != nil {
			fmt.Fprintf(os.Stderr, "read json file: %v\n", err)
			os.Exit(1)
		}
		resp, err := client.AttachVolume(ctx, &pb.AttachVolumeRequest{
			Namespace:         os.Args[2],
			VmiName:           os.Args[3],
			AttachOptionsJson: data,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "AttachVolume RPC error: %v\n", err)
			os.Exit(1)
		}
		printJSON(resp)

	case "detach":
		if len(os.Args) != 5 {
			fmt.Fprintf(os.Stderr, "detach requires: <namespace> <vmi> <vol-name>\n")
			os.Exit(1)
		}
		resp, err := client.RemoveVolume(ctx, &pb.RemoveVolumeRequest{
			Namespace:  os.Args[2],
			VmiName:    os.Args[3],
			VolumeName: os.Args[4],
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "RemoveVolume RPC error: %v\n", err)
			os.Exit(1)
		}
		printJSON(resp)

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  nlh-client attach-pvc         <ns> <vmi> <vol-name> <pvc-name>\n")
	fmt.Fprintf(os.Stderr, "  nlh-client attach-ephemeral   <ns> <vmi> <vol-name> <size>\n")
	fmt.Fprintf(os.Stderr, "  nlh-client attach-persistent  <ns> <vmi> <vol-name> <handle> [--encrypted]\n")
	fmt.Fprintf(os.Stderr, "  nlh-client attach-json        <ns> <vmi> <json-file>\n")
	fmt.Fprintf(os.Stderr, "  nlh-client detach             <ns> <vmi> <vol-name>\n")
}

func printJSON(v interface{}) {
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(out))
}

