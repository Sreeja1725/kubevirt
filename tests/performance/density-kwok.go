package performance

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kvv1 "kubevirt.io/api/core/v1"
	v1 "kubevirt.io/api/core/v1"
	instancetypeapi "kubevirt.io/api/instancetype"
	instancetypev1beta1 "kubevirt.io/api/instancetype/v1beta1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/tests/framework/kubevirt"
	"kubevirt.io/kubevirt/tests/util"
)

var _ = SIGDescribe("[KWOK]Control Plane Performance Density Testing using kwok", func() {
	var (
		kubevirtClient kubecli.KubevirtClient
		k8sClient      *kubernetes.Clientset
		//startTime      time.Time
	)

	//artifactsDir, _ := os.LookupEnv("ARTIFACTS")

	BeforeEach(func() {
		skipIfNoKWOKPerfTests()
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
		createFakeNodesWithKwok(k8sClient, 1)

		By("Get the list of nodes")
		_, err = k8sClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Fatalf("Failed to list nodes: %v", err)
		}

		//startTime = time.Now()
	})

	Describe("kwok density tests", func() {
		vmCount := 1
		vmBatchStartupLimit := 5 * time.Minute

		Context(fmt.Sprintf("[small] create a batch of %d fake VMIs", vmCount), func() {
			It("should sucessfully create all fake VMIS", func() {
				By("Creating a batch of fake VMIs")
				createFakeVMIBatchWithKWOK(kubevirtClient, vmCount)

				By("Waiting a batch of VMIs")
				waitRunningVMI(kubevirtClient, vmCount+1, vmBatchStartupLimit)
				//collectMetrics(startTime, filepath.Join(artifactsDir, "VMI-kwok-perf-audit-results.json"))
			})
		})

		Context(fmt.Sprintf("[small] create a batch of %d running VMs using a single instancetype and preference", vmCount), func() {
			It("should sucessfully create all VMS with instancetype and preference", func() {
				By("Creating an instancetype and preference for the test")
				instancetype := createFakeInstancetype(kubevirtClient)
				preference := createPreference(kubevirtClient)

				By("Creating a batch of VMs")
				createFakeBatchRunningVMWithInstancetypeWithRateControl(kubevirtClient, vmCount, instancetype.Name, preference.Name)

				By("Waiting a batch of VMs")
				waitRunningVMI(kubevirtClient, vmCount, vmBatchStartupLimit)
				//collectMetrics(startTime, filepath.Join(artifactsDir, "VM-instance-type-preference-perf-audit-results.json"))
			})
		})
	})

	AfterEach(func() {
		for i := 0; i < 1; i++ {
			nodeName := fmt.Sprintf("kwok-node-%d", i)
			By(fmt.Sprintf("Creating VMI %s", nodeName))
			err := k8sClient.CoreV1().Nodes().Delete(context.TODO(), nodeName, metav1.DeleteOptions{})
			if err != nil {
				log.Fatalf("Failed to create node %s: %v", nodeName, err)
			}
			fmt.Printf("Node %s created successfully\n", nodeName)
		}
	})
})

func createFakeBatchRunningVMWithInstancetypeWithRateControl(virtClient kubecli.KubevirtClient, vmCount int, instancetypeName, preferenceName string) {
	vm := &kvv1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kvv1.GroupVersion.String(),
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmi.Name,
			Namespace: vmi.Namespace,
		},
		Spec: v1.VirtualMachineSpec{
			Running: pointer.P(false),
			Template: &v1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: vmi.ObjectMeta.Annotations,
					Labels:      vmi.ObjectMeta.Labels,
				},
				Spec: v1.VirtualMachineInstanceSpec{
					NodeSelector: map[string]string{},
					Tolerations:  []corev1.Toleration{},
				},
			},
		},
	}
	createBatchRunningVMWithRateControl(virtClient, vmCount, func() *kvv1.VirtualMachine {
		vm := libvmi.NewVirtualMachine(vm, libvmi.WithRunning())

		vm.Spec.Template.Spec.NodeSelector = map[string]string{
			"type": "kwok",
		}

		vm.Spec.Template.Spec.Tolerations = append(vm.Spec.Template.Spec.Tolerations, corev1.Toleration{
			Key:      "kwok.x-k8s.io/node",
			Effect:   corev1.TaintEffectNoSchedule,
			Operator: corev1.TolerationOpExists})

		vm.Spec.Template.Spec.Domain.Resources = kvv1.ResourceRequirements{}
		vm.Spec.Instancetype = &kvv1.InstancetypeMatcher{
			Name: instancetypeName,
			Kind: instancetypeapi.SingularResourceName,
		}
		vm.Spec.Preference = &kvv1.PreferenceMatcher{
			Name: preferenceName,
			Kind: instancetypeapi.SingularPreferenceResourceName,
		}
		return vm
	})
}

func createFakeNodesWithKwok(k8sClient *kubernetes.Clientset, count int) {
	for i := 0; i < count; i++ {
		nodeName := fmt.Sprintf("kwok-node-%d", i)
		node := createFakeNode(k8sClient, nodeName)
		By(fmt.Sprintf("Creating VMI %s", nodeName))
		_, err := k8sClient.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
		if err != nil {
			log.Fatalf("Failed to create node %s: %v", nodeName, err)
		}
		fmt.Printf("Node %s created successfully\n", nodeName)
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
			},
		},

		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("32"),
				corev1.ResourceMemory:           resource.MustParse("256Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
				corev1.ResourcePods:             resource.MustParse("110"),
				"devices.kubevirt.io/kvm":       resource.MustParse("0"),
				"devices.kubevirt.io/tun":       resource.MustParse("1k"),
				"devices.kubevirt.io/vhost-net": resource.MustParse("1k"),
			},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("32"),
				corev1.ResourceMemory:           resource.MustParse("256Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
				corev1.ResourcePods:             resource.MustParse("110"),
				"devices.kubevirt.io/kvm":       resource.MustParse("0"),
				"devices.kubevirt.io/tun":       resource.MustParse("1k"),
				"devices.kubevirt.io/vhost-net": resource.MustParse("1k"),
			},
		},
	}

	return node
}

func createFakeInstancetype(virtClient kubecli.KubevirtClient) *instancetypev1beta1.VirtualMachineInstancetype {
	instancetype := &instancetypev1beta1.VirtualMachineInstancetype{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "instancetype",
		},
		Spec: instancetypev1beta1.VirtualMachineInstancetypeSpec{
			// FIXME - We don't have a way of expressing resources via instancetypes yet, replace this when we do.
			CPU: instancetypev1beta1.CPUInstancetype{
				Guest: 1,
			},
			Memory: instancetypev1beta1.MemoryInstancetype{
				Guest: resource.MustParse("90Mi"),
			},
			NodeSelector: map[string]string{
				"type": "kwok",
			},
		},
	}
	instancetype, err := virtClient.VirtualMachineInstancetype(util.NamespaceTestDefault).Create(context.Background(), instancetype, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
	return instancetype
}

func createFakeVMIBatchWithKWOK(kubevirtClient kubecli.KubevirtClient, vmCount int) {
	for i := 1; i <= vmCount; i++ {
		vmName := fmt.Sprintf("test-kwok-vmi-%d", i)
		vmi := createFakeVMI(kubevirtClient, vmName)

		By(fmt.Sprintf("Creating VMI %s", vmi.ObjectMeta.Name))
		_, err := kubevirtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(context.Background(), vmi, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		time.Sleep(100 * time.Millisecond)
	}
}

func createFakeVMI(kubevirtClient kubecli.KubevirtClient, vmName string) *kubevirtv1.VirtualMachineInstance {
	guestMemory := resource.MustParse("50M")
	vmi := &kubevirtv1.VirtualMachineInstance{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kubevirt.io/v1",
			Kind:       "VirtualMachineInstance",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmName,
			Namespace: util.NamespaceTestDefault,
		},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Domain: kubevirtv1.DomainSpec{
				Memory: &kubevirtv1.Memory{
					Guest: &guestMemory,
				},
				Devices: kubevirtv1.Devices{
					Disks: []kubevirtv1.Disk{
						{
							Name: "containerdisk",
							DiskDevice: kubevirtv1.DiskDevice{
								Disk: &kubevirtv1.DiskTarget{
									Bus: "virtio",
								},
							},
						},
					},
				},
			},
			Volumes: []kubevirtv1.Volume{
				{
					Name: "containerdisk",
					VolumeSource: kubevirtv1.VolumeSource{
						ContainerDisk: &kubevirtv1.ContainerDiskSource{
							Image: "quay.io/kubevirt/cirros-container-disk-demo",
						},
					},
				},
			},
			NodeSelector: map[string]string{
				"type": "kwok",
			},
			Tolerations: []corev1.Toleration{
				{
					Key:      "kwok.x-k8s.io/node",
					Effect:   corev1.TaintEffectNoSchedule,
					Operator: corev1.TolerationOpExists,
				},
			},
		},
	}
	return vmi
}
