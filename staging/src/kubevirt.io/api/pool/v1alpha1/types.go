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
 * Copyright 2021 Red Hat, Inc.
 *
 */

package v1alpha1

import (
	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstr "k8s.io/apimachinery/pkg/util/intstr"

	virtv1 "kubevirt.io/api/core/v1"
)

const (
	VirtualMachinePoolKind                = "VirtualMachinePool"
	VirtualMachinePoolControllerFinalizer = "pool.kubevirt.io/finalizer"
)

const (
	// Base selection policies
	VirtualMachinePoolBasePolicyRandom          VirtualMachinePoolBasePolicy = "Random"
	VirtualMachinePoolBasePolicyOldest          VirtualMachinePoolBasePolicy = "Oldest"
	VirtualMachinePoolBasePolicyNewest          VirtualMachinePoolBasePolicy = "Newest"
	VirtualMachinePoolBasePolicyDescendingOrder VirtualMachinePoolBasePolicy = "DescendingOrder"
	VirtualMachinePoolBasePolicyAscendingOrder  VirtualMachinePoolBasePolicy = "AscendingOrder"
)

type StatePreservation string

const (
	StatePreservationDisabled StatePreservation = "Disabled"
	StatePreservationOffline  StatePreservation = "Offline"
	StatePreservationOnline   StatePreservation = "Online"
)

type ScaleInStrategyType string

const (
	ScaleInStrategyTypeOpportunistic ScaleInStrategyType = "opportunistic"
	ScaleInStrategyTypeProactive     ScaleInStrategyType = "proactive"
)

// VirtualMachinePool resource contains a VirtualMachine configuration
// that can be used to replicate multiple VirtualMachine resources.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:openapi-gen=true
// +genclient
type VirtualMachinePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualMachinePoolSpec   `json:"spec" valid:"required"`
	Status VirtualMachinePoolStatus `json:"status,omitempty"`
}

// +k8s:openapi-gen=true
type VirtualMachineTemplateSpec struct {
	// +kubebuilder:pruning:PreserveUnknownFields
	// +nullable
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`
	// VirtualMachineSpec contains the VirtualMachine specification.
	Spec virtv1.VirtualMachineSpec `json:"spec,omitempty" valid:"required"`
}

// +k8s:openapi-gen=true
type VirtualMachinePoolConditionType string

const (
	// VirtualMachinePoolReplicaFailure is added in a pool when one of its vms
	// fails to be created.
	VirtualMachinePoolReplicaFailure VirtualMachinePoolConditionType = "ReplicaFailure"

	// VirtualMachinePoolReplicaPaused is added in a pool when the pool got paused by the controller.
	// After this condition was added, it is safe to remove or add vms by hand and adjust the replica count manually
	VirtualMachinePoolReplicaPaused VirtualMachinePoolConditionType = "ReplicaPaused"
)

// +k8s:openapi-gen=true
type VirtualMachinePoolCondition struct {
	Type   VirtualMachinePoolConditionType `json:"type"`
	Status k8sv1.ConditionStatus           `json:"status"`
	// +nullable
	LastProbeTime metav1.Time `json:"lastProbeTime,omitempty"`
	// +nullable
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
}

// +k8s:openapi-gen=true
type VirtualMachinePoolStatus struct {
	// LastDesiredReplicas is the desired number of replicas at the time the last scale-in occurred.
	LastDesiredReplicas int32 `json:"lastDesiredReplicas,omitempty" optional:"true"`

	Replicas int32 `json:"replicas,omitempty" optional:"true"`

	ReadyReplicas int32 `json:"readyReplicas,omitempty" optional:"true"`

	// +listType=atomic
	Conditions []VirtualMachinePoolCondition `json:"conditions,omitempty" optional:"true"`

	// Canonical form of the label selector for HPA which consumes it through the scale subresource.
	LabelSelector string `json:"labelSelector,omitempty"`
}

// +k8s:openapi-gen=true
type VirtualMachinePoolSpec struct {
	// Number of desired pods. This is a pointer to distinguish between explicit
	// zero and not specified. Defaults to 1.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Label selector for pods. Existing Poolss whose pods are
	// selected by this will be the ones affected by this deployment.
	Selector *metav1.LabelSelector `json:"selector" valid:"required"`

	// Template describes the VM that will be created.
	VirtualMachineTemplate *VirtualMachineTemplateSpec `json:"virtualMachineTemplate" valid:"required"`

	// Indicates that the pool is paused.
	// +optional
	Paused bool `json:"paused,omitempty" protobuf:"varint,7,opt,name=paused"`

	// Options for the name generation in a pool.
	// +optional
	NameGeneration *VirtualMachinePoolNameGeneration `json:"nameGeneration,omitempty"`

	// (Defaults to 100%) Integer or string pointer, that when set represents either a percentage or number of VMs in a pool that can be unavailable (ready condition false) at a time during automated update.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty" protobuf:"bytes,3,opt,name=maxUnavailable"`

	// ScaleInStrategy specifies how the VMPool controller manages scaling in VMs within a VMPool
	// +optional
	ScaleInStrategy *VirtualMachinePoolScaleInStrategy `json:"scaleInStrategy,omitempty"`
}

// +k8s:openapi-gen=true
type VirtualMachinePoolNameGeneration struct {
	AppendIndexToConfigMapRefs *bool `json:"appendIndexToConfigMapRefs,omitempty"`
	AppendIndexToSecretRefs    *bool `json:"appendIndexToSecretRefs,omitempty"`
}

// VirtualMachinePoolList is a list of VirtualMachinePool resources.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:openapi-gen=true
type VirtualMachinePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualMachinePool `json:"items"`
}

// VirtualMachinePoolScaleInStrategy specifies how the VMPool controller manages scaling in VMs within a VMPool
// +k8s:openapi-gen=true
type VirtualMachinePoolScaleInStrategy struct {
	// The VM is never touched after creation. Users are responsible for scaling in the VM manually.
	// +optional
	Unmanaged *bool `json:"unmanaged,omitempty"`

	// Opportunistic scale-in of VMs which are in a halted state
	// +optional
	Opportunistic *VirtualMachinePoolOpportunisticScaleInStrategy `json:"opportunistic,omitempty"`

	// Proactive scale-in by forcing VMs to shutdown during scale-in (Default)
	// +optional
	Proactive *VirtualMachinePoolProactiveScaleInStrategy `json:"proactive,omitempty"`
}

// VirtualMachinePoolOpportunisticScaleInStrategy represents opportunistic scale-in strategy
// +k8s:openapi-gen=true
type VirtualMachinePoolOpportunisticScaleInStrategy struct {
	// Enable specifies if the opportunistic scale-in strategy is enabled
	// +optional
	Enable *bool `json:"enable,omitempty"`

	// Specifies if and how to preserve state of VMs selected for scale-in
	// +optional
	// +kubebuilder:validation:Enum=Disabled;Offline;Online
	StatePreservation *StatePreservation `json:"statePreservation,omitempty"`
}

// VirtualMachinePoolProactiveScaleInStrategy represents proactive scale-in strategy
// +k8s:openapi-gen=true
type VirtualMachinePoolProactiveScaleInStrategy struct {
	// SelectionPolicy defines the priority in which VM instances are selected for proactive scale-in
	// Defaults to "Random" base policy when no SelectionPolicy is configured
	// +optional
	SelectionPolicy *VirtualMachinePoolSelectionPolicy `json:"selectionPolicy,omitempty"`

	// Specifies if and how to preserve state of VMs selected for scale-in
	// +optional
	// +kubebuilder:validation:Enum=Disabled;Offline;Online
	StatePreservation *StatePreservation `json:"statePreservation,omitempty"`
}

// VirtualMachinePoolSelectionPolicy defines the priority in which VM instances are selected for scale-in
// +k8s:openapi-gen=true
type VirtualMachinePoolSelectionPolicy struct {
	// BasePolicy is a catch-all policy [Random|Oldest|Newest|DescendingOrder|AscendingOrder]
	// +optional
	// +kubebuilder:validation:Enum=Random;Oldest;Newest;DescendingOrder;AscendingOrder
	BasePolicy *VirtualMachinePoolBasePolicy `json:"basePolicy,omitempty"`

	// OrderedPolicies is a Ordered list of selection policies. Initial policies include [LabelSelector]. Future policies may include a [NodeSelector] or other selection mechanisms.
	// +optional
	OrderedPolicies *VirtualMachinePoolOrderedPolicy `json:"orderedPolicies,omitempty"`
}

// +k8s:openapi-gen=true
type VirtualMachinePoolOrderedPolicy struct {
	// LabelSelector is a list of label selector for VMs.
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`

	// NodeSelectorRequirementMatcher is a list of node selector requirement for VMs.
	// +optional
	NodeSelectorRequirementMatcher *[]k8sv1.NodeSelectorRequirement `json:"nodeSelectorRequirementMatcher,omitempty"`
}

// +k8s:openapi-gen=true
type VirtualMachinePoolBasePolicy string
