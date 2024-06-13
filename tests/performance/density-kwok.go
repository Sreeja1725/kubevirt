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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/tests/framework/kubevirt"
	"kubevirt.io/kubevirt/tests/libvmifact"
	"kubevirt.io/kubevirt/tests/util"
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

var _ = SIGDescribe("[KWOK] Control Plane Performance Density Testing using kwok", func() {
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
					fmt.Fprintf(os.Stderr, "failed to get client config: %v\n", err)
					return
				}

				k8sClient, err = kubernetes.NewForConfig(config)
				if err != nil {
					fmt.Fprintf(os.Stderr, "failed to create k8s client: %v\n", err)
					panic(err)
				}

				By("create fake Nodes")
				createFakeNodesWithKwok(k8sClient, nodeCount)

				By("Get the list of nodes")
				_, err = k8sClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					log.Fatalf("Failed to list nodes: %v", err)
				}
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
						log.Fatalf("Failed to delete node %s: %v", nodeName, err)
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

func createFakeNode(k8sClient *kubernetes.Clientset, nodeName string) *corev1.Node {
	node := &corev1.Node{
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
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{
				{
					Key:    "kwok.x-k8s.io/node",
					Value:  "fake",
					Effect: "NoSchedule",
				},
				{
					Key:    "CriticalAddonsOnly",
					Effect: corev1.TaintEffectNoSchedule,
				},
			},
		},

		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("32"),
				corev1.ResourceMemory:           resource.MustParse("256Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
				corev1.ResourcePods:             resource.MustParse("110"),
				"devices.kubevirt.io/kvm":       resource.MustParse("1k"),
				"devices.kubevirt.io/tun":       resource.MustParse("1k"),
				"devices.kubevirt.io/vhost-net": resource.MustParse("1k"),
			},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("32"),
				corev1.ResourceMemory:           resource.MustParse("256Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
				corev1.ResourcePods:             resource.MustParse("110"),
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

		_, err := kubevirtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(context.Background(), vmi, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		time.Sleep(100 * time.Millisecond)
	}
}

func deleteAndVerifyFakeVMIBatch(kubevirtClient kubecli.KubevirtClient, vmCount int, timeout time.Duration) {
	err := kubevirtClient.VirtualMachineInstance(util.NamespaceTestDefault).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
	if err != nil {
		log.Fatal("Failed to delete VMIs")
	}

	Eventually(func() int {
		vmis, err := kubevirtClient.VirtualMachineInstance(util.NamespaceTestDefault).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())

		return len(vmis.Items)
	}, timeout, 10*time.Second).Should(Equal(0))
}

func deleteAndVerifyFakeVMBatch(kubevirtClient kubecli.KubevirtClient, vmCount int, timeout time.Duration) {
	err := kubevirtClient.VirtualMachine(util.NamespaceTestDefault).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
	if err != nil {
		log.Fatal("Failed to delete VMs")
	}

	Eventually(func() int {
		vmis, err := kubevirtClient.VirtualMachine(util.NamespaceTestDefault).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())

		return len(vmis.Items)
	}, timeout, 10*time.Second).Should(Equal(0))
}

func createFakeBatchRunningVMWithKwok(virtClient kubecli.KubevirtClient, vmCount int) {
	for i := 1; i <= vmCount; i++ {
		vmi := createFakeVMISpecWithResources()
		vm := libvmi.NewVirtualMachine(vmi, libvmi.WithRunning())

		_, err := virtClient.VirtualMachine(util.NamespaceTestDefault).Create(context.Background(), vm, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		// interval for throughput control
		time.Sleep(100 * time.Millisecond)
	}
}

func createFakeVMISpecWithResources() *kubevirtv1.VirtualMachineInstance {
	cpuLimit := "100m"
	memLimit := "90Mi"
	vmi := libvmifact.NewCirros(
		libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
		libvmi.WithNetwork(kubevirtv1.DefaultPodNetwork()),
		libvmi.WithResourceMemory(memLimit),
		libvmi.WithLimitMemory(memLimit),
		libvmi.WithResourceCPU(cpuLimit),
		libvmi.WithLimitCPU(cpuLimit),
		libvmi.WithNodeSelector("type", "kwok"),
		libvmi.WithTolerations([]corev1.Toleration{
			{
				Key:      "CriticalAddonsOnly",
				Operator: corev1.TolerationOpExists,
			},
			{
				Key:      "kwok.x-k8s.io/node",
				Effect:   corev1.TaintEffectNoSchedule,
				Operator: corev1.TolerationOpExists,
			},
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
