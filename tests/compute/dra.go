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

package compute

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/libvmi"

	"kubevirt.io/kubevirt/tests/decorators"
	"kubevirt.io/kubevirt/tests/framework/kubevirt"
	"kubevirt.io/kubevirt/tests/framework/matcher"
	"kubevirt.io/kubevirt/tests/libnet"
	"kubevirt.io/kubevirt/tests/libvmifact"
	"kubevirt.io/kubevirt/tests/testsuite"
)

const (
	draVfioClaimTemplateName          = "vfio-gpu-claim-template"
	draVfioRequestName                = "vfio-gpu"
	draVfioResourceClaimName          = "vfio-gpu-claim"
	draVfioResourceClaimGenerateName  = "dra-vfio-claim-"
	draVfioHostDeviceName             = "gpu0"
	draVfioDeviceClass                = "vfio-gpu.example.com"
	draVfioPCIBusID                   = "faca:00:05.0"
	draVfioFakeVendorID               = "e1a5"
	draVfioFakePCISelector            = `device.attributes["resource.kubernetes.io"].pcieRoot == "pcifaca:00"`
	draVfioUnmatchablePCIBusID        = "cccc:cc:cc.c"
	draVfioInvalidCELExpression       = `device.attributes["resource.kubernetes.io"].pciBusID.string == "faca:00:00.0"`
	draVfioVMICount                   = 4
	draVfioMultiDeviceRequestCount    = 3
	draVfioMatchAttributeRequestCount = 2
	draVfioVendorIDMatchAttribute     = "vfio-gpu.example.com/vendorID"
	draVfioPCIBusIDMatchAttribute     = "resource.kubernetes.io/pciBusID"
	draVfioMemoryRequest              = "32Mi"
	draVfioMultiDeviceMemoryRequest   = "128Mi"
	libvmiRandNamePrefix              = "testvmi-"
	libvmiRandNameRandomLen           = 5
	timeout                           = 2 * time.Minute
	pollingInterval                   = 5 * time.Second
)

var _ = Describe("[sig-compute]DRA", Serial, decorators.SigCompute, decorators.DRAGPU, func() {
	var namespace string

	BeforeEach(func() {
		namespace = testsuite.GetTestNamespace(nil)
	})

	Context("create four VMIs backed by the ResourceClaimTemplate", func() {
		It("should create four VMIs backed by the ResourceClaimTemplate", func() {
			By("creating the shared ResourceClaimTemplate")
			rct := vfioGPUResourceClaimTemplate(namespace)
			_, err := kubevirt.Client().ResourceV1().ResourceClaimTemplates(namespace).Create(
				context.Background(), rct, metav1.CreateOptions{},
			)
			Expect(err).ToNot(HaveOccurred())

			By("creating four VMIs backed by the ResourceClaimTemplate")
			vmiNames := make([]string, 0, draVfioVMICount)
			for range draVfioVMICount {
				createdVMI := createVFIOGPUVMI(namespace,
					WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
					WithVfioGPUHostDevice(),
				)
				vmiNames = append(vmiNames, createdVMI.Name)
			}

			By("Waiting for all VMIs to reach Running")
			waitForVMIsToBeRunning(namespace, draVfioVMICount)

			By("Fetch all the ResourceClaims and check that they are all bound")
			expectBoundResourceClaimsForVMIs(namespace, draVfioVMICount, vmiNames...)
		})
	})

	Context("create four VMIs and pre-created ResourceClaims individually", func() {
		It("creating four VMIs and pre-created ResourceClaims individually", func() {
			vmiNames := make([]string, 0, draVfioVMICount)
			for range draVfioVMICount {
				createdClaim := createVFIOGPUResourceClaim(namespace)
				createdVMI := createVFIOGPUVMI(namespace,
					WithVfioGPUResourceClaim(createdClaim.Name),
					WithVfioGPUHostDevice(WithVfioHostDeviceClaimRequest(createdClaim.Name, draVfioRequestName)),
				)
				vmiNames = append(vmiNames, createdVMI.Name)
			}

			By("Waiting for all VMIs to reach Running")
			waitForVMIsToBeRunning(namespace, draVfioVMICount)

			By("Fetch all the ResourceClaims and check that they are all bound")
			expectBoundResourceClaimsForVMIs(namespace, draVfioVMICount, vmiNames...)
		})
	})

	Context("create a VMI with a strict CEL based ResourceClaimTemplate", func() {
		It("should allocate a vfio-gpu device matching the fake vendor ID", func() {
			By("creating the ResourceClaimTemplate with a strict CEL based selector")
			resourceClaimTemplate := vfioGPUResourceClaimTemplate(namespace,
				WithVfioGPUSelectors(fmt.Sprintf(`device.attributes["vfio-gpu.example.com"].vendorID == %q`, draVfioFakeVendorID)),
			)
			_, err := kubevirt.Client().ResourceV1().ResourceClaimTemplates(namespace).Create(context.Background(), resourceClaimTemplate, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("creating the VMI")
			createdVMI := createVFIOGPUVMI(namespace,
				WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
				WithVfioGPUHostDevice(),
			)

			By("Waiting for the VMI to reach Running")
			waitForVMIsToBeRunning(namespace, 1)

			By("Fetch the ResourceClaim and check that it is bound")
			expectBoundResourceClaimsForVMIs(namespace, 1, createdVMI.Name)
		})

		It("should allocate a vfio-gpu device matching the CEL selector", func() {
			By("creating the ResourceClaimTemplate with a CEL based selector")
			celExpression := fmt.Sprintf(
				`device.attributes["resource.kubernetes.io"].pciBusID == %q`,
				draVfioPCIBusID,
			)
			resourceClaimTemplate := vfioGPUResourceClaimTemplate(namespace,
				WithVfioGPUSelectors(celExpression),
			)
			_, err := kubevirt.Client().ResourceV1().ResourceClaimTemplates(namespace).Create(context.Background(), resourceClaimTemplate, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("creating the VMI")
			createdVMI := createVFIOGPUVMI(namespace,
				WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
				WithVfioGPUHostDevice(),
			)

			By("Waiting for the VMI to reach Running")
			waitForVMIsToBeRunning(namespace, 1)

			By("Fetch the ResourceClaim and check that it is bound")
			expectBoundResourceClaimsForVMIs(namespace, 1, createdVMI.Name)
		})
	})

	Context("create a VMI with a matchattribute based ResourceClaimTemplate", func() {
		It("should allocate vfio-gpu devices sharing vendorID via matchAttribute", func() {
			By("creating the ResourceClaimTemplate with matchAttribute on vendorID across two requests")
			resourceClaimTemplate := vfioGPUResourceClaimTemplate(namespace,
				WithVfioGPUMultipleRequests(draVfioMatchAttributeRequestCount),
				WithVfioGPURequestMatchAttribute(
					draVfioVendorIDMatchAttribute,
					vfioGPUIndexedRequestName(0),
					vfioGPUIndexedRequestName(1),
				),
			)
			_, err := kubevirt.Client().ResourceV1().ResourceClaimTemplates(namespace).Create(context.Background(), resourceClaimTemplate, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("creating the VMI with two host devices backed by the matched requests")
			createdVMI := createVFIOGPUVMI(namespace,
				WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
				WithVfioGPUMultipleHostDevices(draVfioMatchAttributeRequestCount),
			)

			By("Waiting for the VMI to reach Running")
			waitForVMIsToBeRunning(namespace, 1)

			By("Fetch the ResourceClaim and check that two devices were allocated")
			expectBoundResourceClaimsForVMIs(namespace, 1, createdVMI.Name)
			expectAllocatedDeviceCountForVMI(namespace, createdVMI.Name, draVfioMatchAttributeRequestCount)
		})
	})

	Context("create a VMI with multiple device requests in one ResourceClaim", func() {
		It("should allocate three vfio-gpu devices via separate requests and reach Running", func() {
			By("creating the ResourceClaimTemplate with multiple device requests")
			resourceClaimTemplate := vfioGPUResourceClaimTemplate(namespace,
				WithVfioGPUMultipleRequests(draVfioMultiDeviceRequestCount),
			)
			_, err := kubevirt.Client().ResourceV1().ResourceClaimTemplates(namespace).Create(context.Background(), resourceClaimTemplate, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("creating the VMI with multiple device requests")
			createdVMI := createVFIOGPUVMI(namespace,
				WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
				WithVfioGPUMultipleHostDevices(draVfioMultiDeviceRequestCount),
				withVfioGPUMultiDeviceMemory(),
			)

			By("Waiting for the VMI to reach Running")
			waitForVMIsToBeRunning(namespace, 1)

			By("Fetch the ResourceClaim and check that three devices were allocated")
			expectBoundResourceClaimsForVMIs(namespace, 1, createdVMI.Name)
			expectAllocatedDeviceCountForVMI(namespace, createdVMI.Name, draVfioMultiDeviceRequestCount)
		})
	})
})

var _ = Describe("[sig-compute]DRA failing scenarios", Serial, decorators.SigCompute, func() {
	var namespace string

	BeforeEach(func() {
		namespace = testsuite.GetTestNamespace(nil)
	})

	Context("failing scenarios", func() {
		It("should reject a VMI when the host device references an unknown resourceClaim", func() {
			vmi := vfioGPUVMI(
				WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
				WithVfioGPUHostDevice(WithVfioHostDeviceClaimRequest("unknown-claim", draVfioRequestName)),
			)

			_, err := kubevirt.Client().VirtualMachineInstance(namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("resourceClaims must specify all claims"))
		})

		It("should fail when a ResourceClaimTemplate requests more than one device per request", func() {
			By("creating the ResourceClaimTemplate")
			resourceClaimTemplate := vfioGPUResourceClaimTemplate(namespace, WithVfioGPUDeviceCount(3))
			_, err := kubevirt.Client().ResourceV1().ResourceClaimTemplates(namespace).Create(context.Background(), resourceClaimTemplate, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("creating the VMI")
			vmi := vfioGPUVMI(
				WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
				WithVfioGPUHostDevice(),
			)
			vmi, err = kubevirt.Client().VirtualMachineInstance(namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			time.Sleep(20 * time.Second)

			By("waiting for the VMI to be scheduled and not running")
			Consistently(matcher.ThisVMI(vmi), 30*time.Second, pollingInterval).Should(matcher.BeInPhase(v1.Scheduled))
		})

		It("should remain unschedulable when the CEL selector uses invalid attribute access", func() {
			By("creating the ResourceClaimTemplate")
			resourceClaimTemplate := vfioGPUResourceClaimTemplate(namespace,
				WithVfioGPUSelectors(draVfioInvalidCELExpression),
			)
			_, err := kubevirt.Client().ResourceV1().ResourceClaimTemplates(namespace).Create(context.Background(), resourceClaimTemplate, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("creating the VMI")
			vmi := vfioGPUVMI(
				WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
				WithVfioGPUHostDevice(),
			)
			vmi, err = kubevirt.Client().VirtualMachineInstance(namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("waiting for the VMI to be unschedulable")
			time.Sleep(10 * time.Second)
			Consistently(matcher.ThisVMI(vmi), 30*time.Second, pollingInterval).Should(matcher.BeInPhase(v1.Scheduling))
			verifyVMIPodUnschedulable(namespace, vmi.Name, ContainSubstring("no such key: string"))
		})

		It("should remain unschedulable when the CEL selector matches no device", func() {
			By("creating the ResourceClaimTemplate")
			celExpression := fmt.Sprintf(
				`device.attributes["resource.kubernetes.io"].pciBusID == %q`,
				draVfioUnmatchablePCIBusID,
			)
			resourceClaimTemplate := vfioGPUResourceClaimTemplate(namespace,
				WithVfioGPUSelectors(celExpression),
			)
			_, err := kubevirt.Client().ResourceV1().ResourceClaimTemplates(namespace).Create(context.Background(), resourceClaimTemplate, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("creating the VMI")
			vmi := vfioGPUVMI(
				WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
				WithVfioGPUHostDevice(),
			)
			vmi, err = kubevirt.Client().VirtualMachineInstance(namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			time.Sleep(10 * time.Second)

			By("waiting for the VMI to be unschedulable")
			Consistently(matcher.ThisVMI(vmi), 30*time.Second, pollingInterval).Should(matcher.BeInPhase(v1.Scheduling))
			verifyVMIPodUnschedulable(namespace, vmi.Name, ContainSubstring("cannot allocate all claims"))
		})
	})

	Context("create a VMI with a matchattribute based ResourceClaimTemplate", func() {
		It("should fail when the matchattribute has different values", func() {
			By("creating the ResourceClaimTemplate with matchAttribute on pciBusID across two requests")
			resourceClaimTemplate := vfioGPUResourceClaimTemplate(namespace,
				WithVfioGPUMultipleRequests(draVfioMatchAttributeRequestCount),
				WithVfioGPURequestMatchAttribute(
					draVfioPCIBusIDMatchAttribute,
					vfioGPUIndexedRequestName(0),
					vfioGPUIndexedRequestName(1),
				),
			)
			_, err := kubevirt.Client().ResourceV1().ResourceClaimTemplates(namespace).Create(context.Background(), resourceClaimTemplate, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("creating the VMI with two host devices backed by the matched requests")
			vmi := vfioGPUVMI(
				WithVfioGPUResourceClaimTemplate(draVfioClaimTemplateName),
				WithVfioGPUMultipleHostDevices(draVfioMatchAttributeRequestCount),
			)
			vmi, err = kubevirt.Client().VirtualMachineInstance(namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			time.Sleep(10 * time.Second)

			By("VMI should remain in Scheduling phase as the matchattribute has different values")
			Consistently(matcher.ThisVMI(vmi), 30*time.Second, pollingInterval).Should(matcher.BeInPhase(v1.Scheduling))
			verifyVMIPodUnschedulable(namespace, vmi.Name, ContainSubstring("cannot allocate all claims"))
		})
	})
})

type vfioGPUResourceClaimTemplateOption func(*resourcev1.ResourceClaimTemplate)

func vfioGPUResourceClaimTemplate(namespace string, opts ...vfioGPUResourceClaimTemplateOption) *resourcev1.ResourceClaimTemplate {
	rct := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      draVfioClaimTemplateName,
			Namespace: namespace,
		},
		Spec: resourcev1.ResourceClaimTemplateSpec{
			Spec: resourcev1.ResourceClaimSpec{
				Devices: resourcev1.DeviceClaim{
					Requests: []resourcev1.DeviceRequest{vfioGPUDeviceRequest(draVfioRequestName)},
				},
			},
		},
	}
	for _, opt := range opts {
		opt(rct)
	}
	return rct
}

func WithVfioGPUSelectors(expression string) vfioGPUResourceClaimTemplateOption {
	return func(rct *resourcev1.ResourceClaimTemplate) {
		rct.Spec.Spec.Devices.Requests[0].Exactly.Selectors = append(
			rct.Spec.Spec.Devices.Requests[0].Exactly.Selectors,
			resourcev1.DeviceSelector{
				CEL: &resourcev1.CELDeviceSelector{Expression: expression},
			},
		)
	}
}

func vfioGPUDeviceRequest(name string) resourcev1.DeviceRequest {
	return vfioGPUDeviceRequestWithSelector(name, draVfioFakePCISelector)
}

func vfioGPUDeviceRequestWithSelector(name, celExpression string) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: draVfioDeviceClass,
			Selectors: []resourcev1.DeviceSelector{{
				CEL: &resourcev1.CELDeviceSelector{Expression: celExpression},
			}},
		},
	}
}

func vfioGPUIndexedPCIBusID(index int) string {
	return fmt.Sprintf("faca:00:%02d.0", index)
}

func vfioGPUPCIBusIDSelector(pciBusID string) string {
	return fmt.Sprintf(`device.attributes["resource.kubernetes.io"].pciBusID == %q`, pciBusID)
}

func WithVfioGPURequestMatchAttribute(attribute string, requestNames ...string) vfioGPUResourceClaimTemplateOption {
	return func(rct *resourcev1.ResourceClaimTemplate) {
		fqName := resourcev1.FullyQualifiedName(attribute)
		constraint := resourcev1.DeviceConstraint{
			MatchAttribute: &fqName,
		}
		if len(requestNames) > 0 {
			constraint.Requests = requestNames
		}
		rct.Spec.Spec.Devices.Constraints = append(
			rct.Spec.Spec.Devices.Constraints,
			constraint,
		)
	}
}

func WithVfioGPUDeviceCount(count int64) vfioGPUResourceClaimTemplateOption {
	return func(rct *resourcev1.ResourceClaimTemplate) {
		rct.Spec.Spec.Devices.Requests[0].Exactly.Count = count
	}
}

func WithVfioGPUMultipleRequests(count int) vfioGPUResourceClaimTemplateOption {
	return func(rct *resourcev1.ResourceClaimTemplate) {
		requests := make([]resourcev1.DeviceRequest, count)
		for i := range count {
			requests[i] = vfioGPUDeviceRequestWithSelector(
				vfioGPUIndexedRequestName(i),
				vfioGPUPCIBusIDSelector(vfioGPUIndexedPCIBusID(i)),
			)
		}
		rct.Spec.Spec.Devices.Requests = requests
	}
}

func vfioGPUIndexedRequestName(index int) string {
	return fmt.Sprintf("%s-%d", draVfioRequestName, index)
}

func vfioGPUIndexedHostDeviceName(index int) string {
	return fmt.Sprintf("gpu%d", index)
}

type vfioGPUVMIOption func(*v1.VirtualMachineInstance)

func vfioGPUVMI(opts ...vfioGPUVMIOption) *v1.VirtualMachineInstance {
	vmi := libvmifact.NewAlpineWithTestTooling(
		libnet.WithMasqueradeNetworking(),
		libvmi.WithAutoattachGraphicsDevice(false),
		libvmi.WithMemoryRequest(draVfioMemoryRequest),
		libvmi.WithTerminationGracePeriod(30),
	)
	for _, opt := range opts {
		opt(vmi)
	}
	return vmi
}

func createVFIOGPUVMI(namespace string, opts ...vfioGPUVMIOption) *v1.VirtualMachineInstance {
	vmi := vfioGPUVMI(opts...)
	createdVMI, err := kubevirt.Client().VirtualMachineInstance(namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
	return createdVMI
}

func WithVfioGPUResourceClaimTemplate(templateName string) vfioGPUVMIOption {
	return func(vmi *v1.VirtualMachineInstance) {
		libvmi.WithResourceClaim(v1.VirtualMachineInstanceResourceClaim{
			Name:                      draVfioResourceClaimName,
			ResourceClaimTemplateName: &templateName,
		})(vmi)
	}
}

func WithVfioGPUResourceClaim(claimName string) vfioGPUVMIOption {
	return func(vmi *v1.VirtualMachineInstance) {
		libvmi.WithResourceClaim(v1.VirtualMachineInstanceResourceClaim{
			Name:              claimName,
			ResourceClaimName: &claimName,
		})(vmi)
	}
}

func WithVfioGPUHostDevice(opts ...vfioGPUHostDeviceOption) vfioGPUVMIOption {
	return func(vmi *v1.VirtualMachineInstance) {
		libvmi.WithHostDevice(vfioGPUHostDevice(opts...))(vmi)
	}
}

func WithVfioGPUMultipleHostDevices(count int) vfioGPUVMIOption {
	return func(vmi *v1.VirtualMachineInstance) {
		for i := range count {
			libvmi.WithHostDevice(vfioGPUHostDevice(
				WithVfioHostDeviceName(vfioGPUIndexedHostDeviceName(i)),
				WithVfioHostDeviceClaimRequest(draVfioResourceClaimName, vfioGPUIndexedRequestName(i)),
			))(vmi)
		}
	}
}

type vfioGPUHostDeviceOption func(*v1.HostDevice)

func vfioGPUHostDevice(opts ...vfioGPUHostDeviceOption) v1.HostDevice {
	hostDevice := v1.HostDevice{
		Name: draVfioHostDeviceName,
		ClaimRequest: &v1.ClaimRequest{
			ClaimName:   draVfioResourceClaimName,
			RequestName: draVfioRequestName,
		},
	}
	for _, opt := range opts {
		opt(&hostDevice)
	}
	return hostDevice
}

func WithVfioHostDeviceName(name string) vfioGPUHostDeviceOption {
	return func(hostDevice *v1.HostDevice) {
		hostDevice.Name = name
	}
}

func WithVfioHostDeviceClaimRequest(claimName, requestName string) vfioGPUHostDeviceOption {
	return func(hostDevice *v1.HostDevice) {
		hostDevice.ClaimRequest = &v1.ClaimRequest{
			ClaimName:   claimName,
			RequestName: requestName,
		}
	}
}

func verifyVMIPodUnschedulable(namespace, vmiName string, messageMatcher OmegaMatcher) {
	vmi, err := kubevirt.Client().VirtualMachineInstance(namespace).Get(context.Background(), vmiName, metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())
	cond := controller.NewVirtualMachineInstanceConditionManager().GetCondition(
		vmi, v1.VirtualMachineInstanceConditionType(k8sv1.PodScheduled),
	)
	Expect(cond).NotTo(BeNil())
	Expect(cond.Status).To(Equal(k8sv1.ConditionFalse))
	Expect(cond.Reason).To(SatisfyAny(
		Equal(k8sv1.PodReasonUnschedulable),
		Equal(k8sv1.PodReasonSchedulerError),
	))
	Expect(cond.Message).To(messageMatcher)
}

func withVfioGPUMultiDeviceMemory() vfioGPUVMIOption {
	return func(vmi *v1.VirtualMachineInstance) {
		libvmi.WithMemoryRequest(draVfioMultiDeviceMemoryRequest)(vmi)
	}
}

func waitForVMIsToBeRunning(namespace string, vmiCount int) {
	Eventually(func() int {
		vmiList, err := kubevirt.Client().VirtualMachineInstance(namespace).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())
		running := 0
		for _, vmi := range vmiList.Items {
			if vmi.Status.Phase == v1.Running {
				running++
			}
		}
		return running
	}, timeout, pollingInterval).Should(BeNumerically(">=", vmiCount))
}

// virtLauncherPodNamePrefixForVMI returns the pod name prefix used to match ResourceClaim
// ReservedFor entries. libvmi randName() appends a long run of "x" chars to reach the DNS
// label max length, but virt-launcher pod names are also capped at 63 chars, so the "x"
// padding is truncated from the pod name. Matching only the unique testvmi-{random}- prefix
// avoids that mismatch.
func virtLauncherPodNamePrefixForVMI(vmiName string) string {
	if strings.HasPrefix(vmiName, libvmiRandNamePrefix) &&
		len(vmiName) >= len(libvmiRandNamePrefix)+libvmiRandNameRandomLen+1 {
		uniqueName := vmiName[:len(libvmiRandNamePrefix)+libvmiRandNameRandomLen+1]
		return fmt.Sprintf("virt-launcher-%s", uniqueName)
	}
	return fmt.Sprintf("virt-launcher-%s-", vmiName)
}

func resourceClaimBelongsToVMI(claim resourcev1.ResourceClaim, vmiName string) bool {
	podPrefix := virtLauncherPodNamePrefixForVMI(vmiName)
	for _, reservedFor := range claim.Status.ReservedFor {
		if reservedFor.Resource == "pods" && strings.HasPrefix(reservedFor.Name, podPrefix) {
			return true
		}
	}
	return false
}

func listResourceClaimsForVMIs(namespace string, vmiNames ...string) ([]resourcev1.ResourceClaim, error) {
	resourceClaimList, err := kubevirt.Client().ResourceV1().ResourceClaims(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var claims []resourcev1.ResourceClaim
	for _, resourceClaim := range resourceClaimList.Items {
		for _, vmiName := range vmiNames {
			if resourceClaimBelongsToVMI(resourceClaim, vmiName) {
				claims = append(claims, resourceClaim)
				break
			}
		}
	}
	return claims, nil
}

func expectAllocatedDeviceCountForVMI(namespace, vmiName string, expectedDeviceCount int) {
	claims, err := listResourceClaimsForVMIs(namespace, vmiName)
	Expect(err).ToNot(HaveOccurred())
	Expect(claims).To(HaveLen(1))
	Expect(claims[0].Status.Allocation).NotTo(BeNil())
	Expect(claims[0].Status.Allocation.Devices.Results).To(HaveLen(expectedDeviceCount))
	Expect(claims[0].Status.ReservedFor).NotTo(BeEmpty())
	Expect(resourceClaimBelongsToVMI(claims[0], vmiName)).To(BeTrue())
}

func expectBoundResourceClaimsForVMIs(namespace string, expectedCount int, vmiNames ...string) {
	Eventually(func(g Gomega) {
		claims, err := listResourceClaimsForVMIs(namespace, vmiNames...)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(claims).To(HaveLen(expectedCount))
		for _, resourceClaim := range claims {
			var matchedVMIName string
			for _, vmiName := range vmiNames {
				if resourceClaimBelongsToVMI(resourceClaim, vmiName) {
					matchedVMIName = vmiName
					break
				}
			}
			g.Expect(matchedVMIName).NotTo(BeEmpty(), "claim %s should belong to one of the VMIs", resourceClaim.Name)

			g.Expect(resourceClaim.Status).NotTo(BeNil())
			g.Expect(resourceClaim.Status.Allocation).NotTo(BeNil())
			g.Expect(resourceClaim.Status.ReservedFor).NotTo(BeEmpty())
			g.Expect(resourceClaim.Status.ReservedFor[0].Resource).To(Equal("pods"))
			g.Expect(resourceClaim.Status.ReservedFor[0].Name).To(HavePrefix(virtLauncherPodNamePrefixForVMI(matchedVMIName)))
		}
	}, timeout, pollingInterval).Should(Succeed())
}

func createVFIOGPUResourceClaim(namespace string) *resourcev1.ResourceClaim {
	resourceClaim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: draVfioResourceClaimGenerateName,
			Namespace:    namespace,
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{vfioGPUDeviceRequest(draVfioRequestName)},
			},
		},
	}
	createdClaim, err := kubevirt.Client().ResourceV1().ResourceClaims(namespace).Create(context.Background(), resourceClaim, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
	return createdClaim
}
