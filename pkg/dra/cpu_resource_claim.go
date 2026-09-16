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

package dra

import (
	"context"
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/util/hardware"
)

const (
	CPUSocketIDAttribute = "dra.cpu/socket-id"
	CPUDeviceClassName   = "dra.cpu"
	CPUCapacityAttribute = resourcev1.QualifiedName("dra.cpu/cpu")
)

func CPUResourceClaimName(vmiName string) string {
	return fmt.Sprintf("%s-cpu-claim", vmiName)
}

func CPUClaimRef(vmiName string) string {
	return fmt.Sprintf("%s-cpu-claim-ref", vmiName)
}

func CPUSocketRequestName(socketIdx uint32) string {
	return fmt.Sprintf("req-cpu-socket-%d", socketIdx)
}

func guestCPUTopology(cpu *v1.CPU) (cores, threads, sockets uint32) {
	cores, threads, sockets = 1, 1, 1
	if cpu == nil {
		return cores, threads, sockets
	}
	if cpu.Cores != 0 {
		cores = cpu.Cores
	}
	if cpu.Threads != 0 {
		threads = cpu.Threads
	}
	if cpu.Sockets != 0 {
		sockets = cpu.Sockets
	}
	return cores, threads, sockets
}

// hostCPUsPerSocketRequest returns dra.cpu/cpu capacity for one guest socket request, including
// supplemental host CPUs (IO threads, emulator) on socket 0 only — aligned with WithCPUPinning.
func hostCPUsPerSocketRequest(vmi *v1.VirtualMachineInstance, socketIdx, cores, threads, sockets uint32) int64 {
	perSocket := int64(cores * threads)
	if socketIdx != 0 {
		return perSocket
	}
	return perSocket + supplementalHostCPUsForClaim(vmi, cores, threads, sockets)
}

func supplementalHostCPUsForClaim(vmi *v1.VirtualMachineInstance, cores, threads, sockets uint32) int64 {
	guestVCPUs := hardware.GetNumberOfVCPUs(&v1.CPU{
		Cores:   cores,
		Threads: threads,
		Sockets: sockets,
	})
	return hardware.SupplementalDedicatedHostCPUs(vmi, guestVCPUs)
}

func generateCPUResourceClaim(vmi *v1.VirtualMachineInstance, claimName string) (*resourcev1.ResourceClaim, error) {
	var requests []resourcev1.DeviceRequest
	var constraints []resourcev1.DeviceConstraint

	cpu := vmi.Spec.Domain.CPU
	if cpu == nil || !cpu.DedicatedCPUPlacement {
		return nil, nil
	}

	cores, threads, sockets := guestCPUTopology(cpu)

	var reqNames []string
	for socketIdx := uint32(0); socketIdx < sockets; socketIdx++ {
		name := CPUSocketRequestName(socketIdx)
		reqNames = append(reqNames, name)
		capacity := hostCPUsPerSocketRequest(vmi, socketIdx, cores, threads, sockets)
		requests = append(requests, resourcev1.DeviceRequest{
			Name: name,
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: CPUDeviceClassName,
				Count:           1,
				AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
				Capacity: &resourcev1.CapacityRequirements{
					Requests: map[resourcev1.QualifiedName]resource.Quantity{
						CPUCapacityAttribute: *resource.NewQuantity(capacity, resource.DecimalSI),
					},
				},
			},
		})
	}

	if len(reqNames) > 1 {
		attrName := resourcev1.FullyQualifiedName(CPUSocketIDAttribute)
		constraints = append(constraints, resourcev1.DeviceConstraint{
			DistinctAttribute: &attrName,
			Requests:          reqNames,
		})
	}

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: vmi.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vmi, v1.VirtualMachineInstanceGroupVersionKind),
			},
			Labels: map[string]string{
				v1.CreatedByLabel:      string(vmi.UID),
				v1.AppLabel:            "virt-launcher",
				"kubevirt.io/resource": "cpu-dra",
			},
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests:    requests,
				Constraints: constraints,
			},
		},
	}
	return claim, nil
}

func CreateCPUResourceClaim(vmi *v1.VirtualMachineInstance, clientset kubecli.KubevirtClient) error {
	logger := log.Log.Object(vmi)
	claimName := CPUResourceClaimName(vmi.Name)

	_, err := clientset.ResourceV1().ResourceClaims(vmi.Namespace).Get(context.TODO(), claimName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get existing CPU ResourceClaim: %v", err)
		}

		claim, err := generateCPUResourceClaim(vmi, claimName)
		if err != nil {
			return fmt.Errorf("failed to generate CPU ResourceClaim: %v", err)
		}
		if claim == nil {
			return nil
		}

		_, err = clientset.ResourceV1().ResourceClaims(vmi.Namespace).Create(context.TODO(), claim, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create CPU ResourceClaim: %v", err)
		}
	} else {
		logger.V(4).Infof("CPU ResourceClaim %s/%s already exists", vmi.Namespace, claimName)
	}

	return nil
}
