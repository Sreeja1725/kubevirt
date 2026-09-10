package dra

import (
	"context"
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	"kubevirt.io/kubevirt/pkg/pointer"
)

func CPUResourceClaimName(vmiName string) string {
	return fmt.Sprintf("%s-cpu-claim", vmiName)
}

func CPUClaimRef(vmiName string) string {
	return fmt.Sprintf("%s-cpu-claim-ref", vmiName)
}

func EnsureCPUResourceClaimOnVMI(vmi *v1.VirtualMachineInstance) {
	claimName := CPUResourceClaimName(vmi.Name)
	claimRef := CPUClaimRef(vmi.Name)
	for _, claim := range vmi.Spec.ResourceClaims {
		if claim.Name == claimRef {
			return
		}
	}
	vmi.Spec.ResourceClaims = append(vmi.Spec.ResourceClaims, v1.VirtualMachineInstanceResourceClaim{
		Name:              claimRef,
		ResourceClaimName: pointer.P(claimName),
	})
}

func generateCPUResourceClaim(vmi *v1.VirtualMachineInstance) (*resourcev1.ResourceClaim, error) {
	claimName := CPUResourceClaimName(vmi.Name)
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
				Requests:    nil,
				Constraints: nil,
			},
		},
	}
	return claim, nil
}

func CreateCPUResourceClaim(vmi *v1.VirtualMachineInstance, clientset kubecli.KubevirtClient) error {
	logger := log.Log.Object(vmi)

	claim, err := generateCPUResourceClaim(vmi)
	if err != nil {
		return fmt.Errorf("failed to generate CPU ResourceClaim: %v", err)
	}

	_, err = clientset.ResourceV1().ResourceClaims(vmi.Namespace).Get(context.TODO(), claim.Name, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get existing CPU ResourceClaim: %v", err)
		}
		_, err = clientset.ResourceV1().ResourceClaims(vmi.Namespace).Create(context.TODO(), claim, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create CPU ResourceClaim: %v", err)
		}
	} else {
		logger.V(4).Infof("CPU ResourceClaim %s/%s already exists", claim.Namespace, claim.Name)
	}

	return nil
}
