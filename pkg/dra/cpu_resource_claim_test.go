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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/libvmi"
)

var _ = Describe("CPU ResourceClaim synthesis", func() {
	It("should generate per-socket requests with distinct socket constraint", func() {
		vmi := libvmi.New(
			libvmi.WithName("testvmi"),
			libvmi.WithNamespace("default"),
			libvmi.WithDedicatedCPUPlacement(),
			libvmi.WithCPUCount(2, 2, 2),
		)
		vmi.UID = "abc-123"

		claim, err := generateCPUResourceClaim(vmi, CPUResourceClaimName(vmi.Name))
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Spec.Devices.Requests).To(HaveLen(2))
		Expect(claim.Spec.Devices.Requests[0].Exactly.DeviceClassName).To(Equal(CPUDeviceClassName))
		qty := claim.Spec.Devices.Requests[0].Exactly.Capacity.Requests[CPUCapacityAttribute]
		Expect(qty.Value()).To(Equal(int64(4)))
		qty = claim.Spec.Devices.Requests[1].Exactly.Capacity.Requests[CPUCapacityAttribute]
		Expect(qty.Value()).To(Equal(int64(4)))
		Expect(claim.Spec.Devices.Constraints).To(HaveLen(1))
		Expect(claim.Spec.Devices.Constraints[0].Requests).To(Equal([]string{
			CPUSocketRequestName(0), CPUSocketRequestName(1),
		}))
	})

	It("should add supplemental CPUs to the first socket request only", func() {
		vmi := libvmi.New(
			libvmi.WithDedicatedCPUPlacement(),
			libvmi.WithCPUCount(4, 1, 1),
			libvmi.WithIOThreadsPolicy(v1.IOThreadsPolicySupplementalPool),
			libvmi.WithSupplementalPoolThreadCount(2),
			libvmi.WithIsolateEmulatorThread(),
		)

		claim, err := generateCPUResourceClaim(vmi, "claim")
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Spec.Devices.Requests).To(HaveLen(1))
		qty := claim.Spec.Devices.Requests[0].Exactly.Capacity.Requests[CPUCapacityAttribute]
		Expect(qty.Value()).To(Equal(int64(7)))
	})

	It("should use two emulator thread CPUs with even parity annotation", func() {
		vmi := libvmi.New(
			libvmi.WithDedicatedCPUPlacement(),
			libvmi.WithCPUCount(6, 1, 1),
			libvmi.WithIOThreadsPolicy(v1.IOThreadsPolicySupplementalPool),
			libvmi.WithSupplementalPoolThreadCount(2),
			libvmi.WithIsolateEmulatorThread(),
		)
		vmi.Annotations = map[string]string{v1.EmulatorThreadCompleteToEvenParity: ""}

		claim, err := generateCPUResourceClaim(vmi, "claim")
		Expect(err).NotTo(HaveOccurred())
		qty := claim.Spec.Devices.Requests[0].Exactly.Capacity.Requests[CPUCapacityAttribute]
		Expect(qty.Value()).To(Equal(int64(10)))
	})

	It("should generate one request per guest socket", func() {
		vmi := libvmi.New(
			libvmi.WithDedicatedCPUPlacement(),
			libvmi.WithCPUCount(2, 1, 3),
		)
		claim, err := generateCPUResourceClaim(vmi, "claim")
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Spec.Devices.Requests).To(HaveLen(3))
		for i := range claim.Spec.Devices.Requests {
			request := claim.Spec.Devices.Requests[i]
			Expect(request.Name).To(Equal(CPUSocketRequestName(uint32(i))))
			Expect(request.Exactly.DeviceClassName).To(Equal(CPUDeviceClassName))
			qty := request.Exactly.Capacity.Requests[CPUCapacityAttribute]
			Expect(qty.Value()).To(Equal(int64(2)))
		}
	})

	It("should default to a single request when the topology is unset", func() {
		vmi := libvmi.New(libvmi.WithDedicatedCPUPlacement())
		claim, err := generateCPUResourceClaim(vmi, "claim")
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Spec.Devices.Requests).To(HaveLen(1))
		Expect(claim.Spec.Devices.Requests[0].Exactly.DeviceClassName).To(Equal(CPUDeviceClassName))
		qty := claim.Spec.Devices.Requests[0].Exactly.Capacity.Requests[CPUCapacityAttribute]
		Expect(qty.Value()).To(Equal(int64(1)))
	})

	It("should omit distinct constraint for a single guest socket", func() {
		vmi := libvmi.New(
			libvmi.WithDedicatedCPUPlacement(),
			libvmi.WithCPUCount(8, 1, 1),
		)
		claim, err := generateCPUResourceClaim(vmi, "claim")
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Spec.Devices.Constraints).To(BeEmpty())
	})
})
