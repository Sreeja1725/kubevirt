/*
Copyright 2024 The KubeVirt Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package performance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/tests/flags"
	"kubevirt.io/kubevirt/tests/framework/kubevirt"
	"kubevirt.io/kubevirt/tests/libvmifact"
	"kubevirt.io/kubevirt/tests/testsuite"
)

const (
	vmBatchStartupLimit = 5 * time.Minute
	defaultVMCount      = 1000
)

var (
	vmCount = getVMCount()
)

var _ = KWOKDescribe("Control Plane Performance Density Testing using kwok", func() {
	artifactsDir, _ := os.LookupEnv("ARTIFACTS")
	var (
		virtClient kubecli.KubevirtClient
		startTime  time.Time
	)

	BeforeEach(func() {
		if !flags.DeployFakeKwokNodesFlag {
			Skip("Skipping test as KWOK flag is not enabled")
		}

		virtClient = kubevirt.Client()
		startTime = time.Now()
	})

	Describe("kwok density tests", func() {
		Context(fmt.Sprintf("create a batch of %d fake VMIs", vmCount), func() {
			It("should sucessfully create all fake VMIs", func() {
				By("Creating a batch of fake VMIs")
				createFakeVMIBatchWithKWOK(virtClient)

				By("Waiting for a batch of VMIs")
				waitRunningVMI(virtClient, vmCount, vmBatchStartupLimit)

				By("Deleting fake VMIs")
				deleteAndVerifyFakeVMIBatch(virtClient, vmBatchStartupLimit)

				By("Collecting metrics")
				collectMetrics(startTime, filepath.Join(artifactsDir, "VMI-kwok-perf-audit-results.json"))

				//wait for 5 mins to bring the metrics to steady state
				time.Sleep(5 * time.Minute)
			})
		})

		Context(fmt.Sprintf("create a batch of %d fake VMs", vmCount), func() {
			It("should sucessfully create all fake VMs", func() {
				By("Creating a batch of VMs")
				createFakeBatchRunningVMWithKwok(virtClient)

				By("Waiting for a batch of VMs")
				waitRunningVMI(virtClient, vmCount, vmBatchStartupLimit)

				By("Deleting fake VMs")
				deleteAndVerifyFakeVMBatch(virtClient, vmBatchStartupLimit)

				By("Collecting metrics")
				collectMetrics(startTime, filepath.Join(artifactsDir, "VM-kwok-perf-audit-results.json"))
			})
		})
	})
})

func createFakeVMIBatchWithKWOK(virtClient kubecli.KubevirtClient) {
	for i := 1; i <= vmCount; i++ {
		vmi := newFakeVMISpecWithResources()

		_, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(nil)).Create(context.Background(), vmi, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
	}
}

func deleteAndVerifyFakeVMIBatch(virtClient kubecli.KubevirtClient, timeout time.Duration) {
	err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(nil)).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() []v1.VirtualMachineInstance {
		vmis, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(nil)).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())

		return vmis.Items
	}, timeout, 10*time.Second).Should(BeEmpty())
}

func deleteAndVerifyFakeVMBatch(virtClient kubecli.KubevirtClient, timeout time.Duration) {
	err := virtClient.VirtualMachine(testsuite.GetTestNamespace(nil)).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() []v1.VirtualMachineInstance {
		vmis, err := virtClient.VirtualMachineInstance(testsuite.GetTestNamespace(nil)).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())

		return vmis.Items
	}, timeout, 10*time.Second).Should(BeEmpty())
}

func createFakeBatchRunningVMWithKwok(virtClient kubecli.KubevirtClient) {
	for i := 1; i <= vmCount; i++ {
		vmi := newFakeVMISpecWithResources()
		vm := libvmi.NewVirtualMachine(vmi, libvmi.WithRunning())

		_, err := virtClient.VirtualMachine(testsuite.GetTestNamespace(nil)).Create(context.Background(), vm, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
	}
}

func newFakeVMISpecWithResources() *v1.VirtualMachineInstance {
	return libvmifact.NewCirros(
		libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
		libvmi.WithNetwork(v1.DefaultPodNetwork()),
		libvmi.WithResourceMemory("90Mi"),
		libvmi.WithLimitMemory("90Mi"),
		libvmi.WithResourceCPU("100m"),
		libvmi.WithLimitCPU("100m"),
		libvmi.WithNodeSelector("type", "kwok"),
		libvmi.WithToleration(k8sv1.Toleration{
			Key:      "CriticalAddonsOnly",
			Operator: k8sv1.TolerationOpExists,
		}),
		libvmi.WithToleration(k8sv1.Toleration{
			Key:      "kwok.x-k8s.io/node",
			Effect:   k8sv1.TaintEffectNoSchedule,
			Operator: k8sv1.TolerationOpExists,
		}),
	)
}

func getVMCount() int {
	vmCountString := os.Getenv("VM_COUNT")
	if vmCountString == "" {
		return defaultVMCount
	}

	vmCount, err := strconv.Atoi(vmCountString)
	if err != nil {
		return defaultVMCount
	}

	return vmCount
}
