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
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/tests/framework/kubevirt"
	"kubevirt.io/kubevirt/tests/libvmifact"
	"kubevirt.io/kubevirt/tests/testsuite"
)

const (
	vmBatchStartupLimit = 5 * time.Minute
	defaultNodeCount    = 100
	defaultVMCount      = 1000
)

var (
	nodeCount = getNodeCount()
	vmCount   = getVMCount()
)

var _ = Describe("Control Plane Performance Density Testing using kwok", Label("KWOK", "sig-performance", "Serial"), Serial, Ordered, func() {
	var (
		kubevirtClient kubecli.KubevirtClient
		k8sClient      *kubernetes.Clientset
		startTime      time.Time
	)

	artifactsDir, _ := os.LookupEnv("ARTIFACTS")

	BeforeEach(func() {
		skipIfNoKWOKPerformanceTests()
		startTime = time.Now()
	})

	Describe("kwok density tests", func() {
		Context(fmt.Sprintf("create a batch of %d fake Nodes", nodeCount), func() {
			It("should successfully create fake nodes", func() {
				kubevirtClient = kubevirt.Client()

				config, err := kubecli.GetKubevirtClientConfig()
				if err != nil {
					Expect(err).NotTo(HaveOccurred())
				}

				k8sClient, err = kubernetes.NewForConfig(config)
				if err != nil {
					Expect(err).NotTo(HaveOccurred())
				}

				By("create fake Nodes")
				createFakeNodesWithKwok(k8sClient, nodeCount)

				By("Get the list of nodes")
				_, err = k8sClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					Expect(err).NotTo(HaveOccurred())
				}

				//wait for 5 mins to bring the metrics to steady state
				time.Sleep(5 * time.Minute)
			})
		})

		Context(fmt.Sprintf("create a batch of %d fake VMIs", vmCount), func() {
			It("should sucessfully create all fake VMIs", func() {
				By("Creating a batch of fake VMIs")
				createFakeVMIBatchWithKWOK(kubevirtClient, vmCount)

				By("Waiting for a batch of VMIs")
				waitRunningVMI(kubevirtClient, vmCount, vmBatchStartupLimit)

				By("Deleting fake VMIs")
				deleteAndVerifyFakeVMIBatch(kubevirtClient, vmCount, vmBatchStartupLimit)

				By("Collecting metrics")
				collectMetrics(startTime, filepath.Join(artifactsDir, "VMI-kwok-perf-audit-results.json"))

				//wait for 5 mins to bring the metrics to steady state
				time.Sleep(5 * time.Minute)
			})
		})

		Context(fmt.Sprintf("create a batch of %d fake VMs", vmCount), func() {
			It("should sucessfully create all fake VMs", func() {
				By("Creating a batch of VMs")
				createFakeBatchRunningVMWithKwok(kubevirtClient, vmCount)

				By("Waiting for a batch of VMs")
				waitRunningVMI(kubevirtClient, vmCount, vmBatchStartupLimit)

				By("Deleting fake VMs")
				deleteAndVerifyFakeVMBatch(kubevirtClient, vmCount, vmBatchStartupLimit)

				By("Collecting metrics")
				collectMetrics(startTime, filepath.Join(artifactsDir, "VM-kwok-perf-audit-results.json"))
			})
		})

		Context("Deleting fake nodes", func() {
			It("Successfully delete fake nodes", func() {
				for i := 1; i <= nodeCount; i++ {
					nodeName := fmt.Sprintf("kwok-node-%d", i)
					err := k8sClient.CoreV1().Nodes().Delete(context.TODO(), nodeName, metav1.DeleteOptions{})
					if err != nil {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			})
		})

	})
})

func createFakeNodesWithKwok(k8sClient *kubernetes.Clientset, count int) {
	for i := 1; i <= count; i++ {
		nodeName := fmt.Sprintf("kwok-node-%d", i)
		node := createFakeNode(k8sClient, nodeName)
		_, err := k8sClient.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
		if err != nil {
			log.Fatalf("Failed to create node %s: %v", nodeName, err)
		}
	}
}

func createFakeNode(k8sClient *kubernetes.Clientset, nodeName string) *k8sv1.Node {
	node := &k8sv1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				"beta.kubernetes.io/arch":       "amd64",
				"beta.kubernetes.io/os":         "linux",
				"kubernetes.io/arch":            "amd64",
				"kubernetes.io/hostname":        nodeName,
				"kubernetes.io/os":              "linux",
				"kubernetes.io/role":            "agent",
				"node-role.kubernetes.io/agent": "",
				"kubevirt.io/schedulable":       "true",
				"type":                          "kwok",
			},
			Annotations: map[string]string{
				"node.alpha.kubernetes.io/ttl": "0",
				"kwok.x-k8s.io/node":           "fake",
			},
		},
		Spec: k8sv1.NodeSpec{
			Taints: []k8sv1.Taint{
				{
					Key:    "kwok.x-k8s.io/node",
					Value:  "fake",
					Effect: "NoSchedule",
				},
				{
					Key:    "CriticalAddonsOnly",
					Effect: k8sv1.TaintEffectNoSchedule,
				},
			},
		},

		Status: k8sv1.NodeStatus{
			Allocatable: k8sv1.ResourceList{
				k8sv1.ResourceCPU:               resource.MustParse("32"),
				k8sv1.ResourceMemory:            resource.MustParse("256Gi"),
				k8sv1.ResourceEphemeralStorage:  resource.MustParse("100Gi"),
				k8sv1.ResourcePods:              resource.MustParse("110"),
				"devices.kubevirt.io/kvm":       resource.MustParse("1k"),
				"devices.kubevirt.io/tun":       resource.MustParse("1k"),
				"devices.kubevirt.io/vhost-net": resource.MustParse("1k"),
			},
			Capacity: k8sv1.ResourceList{
				k8sv1.ResourceCPU:               resource.MustParse("32"),
				k8sv1.ResourceMemory:            resource.MustParse("256Gi"),
				k8sv1.ResourceEphemeralStorage:  resource.MustParse("100Gi"),
				k8sv1.ResourcePods:              resource.MustParse("110"),
				"devices.kubevirt.io/kvm":       resource.MustParse("1k"),
				"devices.kubevirt.io/tun":       resource.MustParse("1k"),
				"devices.kubevirt.io/vhost-net": resource.MustParse("1k"),
			},
		},
	}

	return node
}

func createFakeVMIBatchWithKWOK(kubevirtClient kubecli.KubevirtClient, vmCount int) {
	for i := 1; i <= vmCount; i++ {
		vmi := createFakeVMISpecWithResources()

		_, err := kubevirtClient.VirtualMachineInstance(testsuite.NamespaceTestDefault).Create(context.Background(), vmi, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		time.Sleep(100 * time.Millisecond)
	}
}

func deleteAndVerifyFakeVMIBatch(kubevirtClient kubecli.KubevirtClient, vmCount int, timeout time.Duration) {
	err := kubevirtClient.VirtualMachineInstance(testsuite.NamespaceTestDefault).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
	if err != nil {
		log.Fatal("Failed to delete VMIs")
	}

	Eventually(func() int {
		vmis, err := kubevirtClient.VirtualMachineInstance(testsuite.NamespaceTestDefault).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())

		return len(vmis.Items)
	}, timeout, 10*time.Second).Should(Equal(0))
}

func deleteAndVerifyFakeVMBatch(kubevirtClient kubecli.KubevirtClient, vmCount int, timeout time.Duration) {
	err := kubevirtClient.VirtualMachine(testsuite.NamespaceTestDefault).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
	if err != nil {
		log.Fatal("Failed to delete VMs")
	}

	Eventually(func() int {
		vmis, err := kubevirtClient.VirtualMachine(testsuite.NamespaceTestDefault).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())

		return len(vmis.Items)
	}, timeout, 10*time.Second).Should(Equal(0))
}

func createFakeBatchRunningVMWithKwok(virtClient kubecli.KubevirtClient, vmCount int) {
	for i := 1; i <= vmCount; i++ {
		vmi := createFakeVMISpecWithResources()
		vm := libvmi.NewVirtualMachine(vmi, libvmi.WithRunning())

		_, err := virtClient.VirtualMachine(testsuite.NamespaceTestDefault).Create(context.Background(), vm, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		// interval for throughput control
		time.Sleep(100 * time.Millisecond)
	}
}

func createFakeVMISpecWithResources() *v1.VirtualMachineInstance {
	cpuLimit := "100m"
	memLimit := "90Mi"
	vmi := libvmifact.NewCirros(
		libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
		libvmi.WithNetwork(v1.DefaultPodNetwork()),
		libvmi.WithResourceMemory(memLimit),
		libvmi.WithLimitMemory(memLimit),
		libvmi.WithResourceCPU(cpuLimit),
		libvmi.WithLimitCPU(cpuLimit),
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
	return vmi
}

func getNodeCount() int {
	nodeCountString := os.Getenv("NODE_COUNT")
	if nodeCountString == "" {
		return defaultNodeCount
	}

	nodeCount, err := strconv.Atoi(nodeCountString)
	if err != nil {
		return defaultNodeCount
	}

	return nodeCount
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
