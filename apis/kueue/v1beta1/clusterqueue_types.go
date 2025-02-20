/*
Copyright 2023 The Kubernetes Authors.

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

package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterQueueSpec defines the desired state of ClusterQueue
type ClusterQueueSpec struct {
	// resourceGroups describes groups of resources.
	// Each resource group defines the list of resources and a list of flavors
	// that provide quotas for these resources.
	// Each resource and each flavor can only form part of one resource group.
	// resourceGroups can be up to 16.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	ResourceGroups []ResourceGroup `json:"resourceGroups,omitempty"`

	// cohort that this ClusterQueue belongs to. CQs that belong to the
	// same cohort can borrow unused resources from each other.
	//
	// A CQ can be a member of a single borrowing cohort. A workload submitted
	// to a queue referencing this CQ can borrow quota from any CQ in the cohort.
	// Only quota for the [resource, flavor] pairs listed in the CQ can be
	// borrowed.
	// If empty, this ClusterQueue cannot borrow from any other ClusterQueue and
	// vice versa.
	//
	// A cohort is a name that links CQs together, but it doesn't reference any
	// object.
	//
	// Validation of a cohort name is equivalent to that of object names:
	// subdomain in DNS (RFC 1123).
	Cohort string `json:"cohort,omitempty"`

	// QueueingStrategy indicates the queueing strategy of the workloads
	// across the queues in this ClusterQueue. This field is immutable.
	// Current Supported Strategies:
	//
	// - StrictFIFO: workloads are ordered strictly by creation time.
	// Older workloads that can't be admitted will block admitting newer
	// workloads even if they fit available quota.
	// - BestEffortFIFO: workloads are ordered by creation time,
	// however older workloads that can't be admitted will not block
	// admitting newer workloads that fit existing quota.
	//
	// +kubebuilder:default=BestEffortFIFO
	// +kubebuilder:validation:Enum=StrictFIFO;BestEffortFIFO
	QueueingStrategy QueueingStrategy `json:"queueingStrategy,omitempty"`

	// namespaceSelector defines which namespaces are allowed to submit workloads to
	// this clusterQueue. Beyond this basic support for policy, an policy agent like
	// Gatekeeper should be used to enforce more advanced policies.
	// Defaults to null which is a nothing selector (no namespaces eligible).
	// If set to an empty selector `{}`, then all namespaces are eligible.
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// preemption describes policies to preempt Workloads from this ClusterQueue
	// or the ClusterQueue's cohort.
	//
	// Preemption can happen in two scenarios:
	//
	// - When a Workload fits within the nominal quota of the ClusterQueue, but
	//   the quota is currently borrowed by other ClusterQueues in the cohort.
	//   Preempting Workloads in other ClusterQueues allows this ClusterQueue to
	//   reclaim its nominal quota.
	// - When a Workload doesn't fit within the nominal quota of the ClusterQueue
	//   and there are admitted Workloads in the ClusterQueue with lower priority.
	//
	// The preemption algorithm tries to find a minimal set of Workloads to
	// preempt to accomomdate the pending Workload, preempting Workloads with
	// lower priority first.
	Preemption *ClusterQueuePreemption `json:"preemption,omitempty"`
}

type QueueingStrategy string

const (
	// StrictFIFO means that workloads are ordered strictly by creation time.
	// Older workloads that can't be admitted will block admitting newer
	// workloads even if they fit available quota.
	StrictFIFO QueueingStrategy = "StrictFIFO"

	// BestEffortFIFO means that workloads are ordered by creation time,
	// however older workloads that can't be admitted will not block
	// admitting newer workloads that fit existing quota.
	BestEffortFIFO QueueingStrategy = "BestEffortFIFO"
)

type ResourceGroup struct {
	// coveredResources is the list of resources covered by the flavors in this
	// group.
	// Examples: cpu, memory, vendor.com/gpu.
	// The list cannot be empty and it can contain up to 16 resources.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	CoveredResources []corev1.ResourceName `json:"coveredResources"`

	// flavors is the list of flavors that provide the resources of this group.
	// Typically, different flavors represent different hardware models
	// (e.g., gpu models, cpu architectures) or pricing models (on-demand vs spot
	// cpus).
	// Each flavor MUST list all the resources listed for this group in the same
	// order as the .resources field.
	// The list cannot be empty and it can contain up to 16 flavors.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Flavors []FlavorQuotas `json:"flavors"`
}

type FlavorQuotas struct {
	// name of this flavor. The name should match the .metadata.name of a
	// ResourceFlavor. If a matching ResourceFlavor does not exist, the
	// ClusterQueue will have an Active condition set to False.
	Name ResourceFlavorReference `json:"name"`

	// resources is the list of quotas for this flavor per resource.
	// There could be up to 16 resources.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Resources []ResourceQuota `json:"resources"`
}

type ResourceQuota struct {
	// name of this resource.
	Name corev1.ResourceName `json:"name"`

	// nominalQuota is the quantity of this resource that is available for
	// Workloads admitted by this ClusterQueue at a point in time.
	// The nominalQuota must be non-negative.
	// nominalQuota should represent the resources in the cluster available for
	// running jobs (after discounting resources consumed by system components
	// and pods not managed by kueue). In an autoscaled cluster, nominalQuota
	// should account for resources that can be provided by a component such as
	// Kubernetes cluster-autoscaler.
	//
	// If the ClusterQueue belongs to a cohort, the sum of the quotas for each
	// (flavor, resource) combination defines the maximum quantity that can be
	// allocated by a ClusterQueue in the cohort.
	NominalQuota resource.Quantity `json:"nominalQuota"`

	// borrowingLimit is the maximum amount of quota for the [flavor, resource]
	// combination that this ClusterQueue is allowed to borrow from the unused
	// quota of other ClusterQueues in the same cohort.
	// In total, at a given time, Workloads in a ClusterQueue can consume a
	// quantity of quota equal to nominalQuota+borrowingLimit, assuming the other
	// ClusterQueues in the cohort have enough unused quota.
	// If null, it means that there is no borrowing limit.
	// If not null, it must be non-negative.
	// borrowingLimit must be null if spec.cohort is empty.
	// +optional
	BorrowingLimit *resource.Quantity `json:"borrowingLimit,omitempty"`
}

// ResourceFlavorReference is the name of the ResourceFlavor.
type ResourceFlavorReference string

// ClusterQueueStatus defines the observed state of ClusterQueue
type ClusterQueueStatus struct {
	// flavorsUsage are the used quotas, by flavor, currently in use by the
	// workloads assigned to this ClusterQueue.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	// +optional
	FlavorsUsage []FlavorUsage `json:"flavorsUsage"`

	// pendingWorkloads is the number of workloads currently waiting to be
	// admitted to this clusterQueue.
	// +optional
	PendingWorkloads int32 `json:"pendingWorkloads"`

	// admittedWorkloads is the number of workloads currently admitted to this
	// clusterQueue and haven't finished yet.
	// +optional
	AdmittedWorkloads int32 `json:"admittedWorkloads"`

	// conditions hold the latest available observations of the ClusterQueue
	// current state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type FlavorUsage struct {
	// name of the flavor.
	Name ResourceFlavorReference `json:"name"`

	// resources lists the quota usage for the resources in this flavor.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Resources []ResourceUsage `json:"resources"`
}

type ResourceUsage struct {
	// name of the resource
	Name corev1.ResourceName `json:"name"`

	// total is the total quantity of used quota, including the amount borrowed
	// from the cohort.
	Total resource.Quantity `json:"total,omitempty"`

	// Borrowed is quantity of quota that is borrowed from the cohort. In other
	// words, it's the used quota that is over the nominalQuota.
	Borrowed resource.Quantity `json:"borrowed,omitempty"`
}

const (
	// ClusterQueueActive indicates that the ClusterQueue can admit new workloads and its quota
	// can be borrowed by other ClusterQueues in the same cohort.
	ClusterQueueActive string = "Active"
)

type PreemptionPolicy string

const (
	PreemptionPolicyNever         PreemptionPolicy = "Never"
	PreemptionPolicyAny           PreemptionPolicy = "Any"
	PreemptionPolicyLowerPriority PreemptionPolicy = "LowerPriority"
)

// ClusterQueuePreemption contains policies to preempt Workloads from this
// ClusterQueue or the ClusterQueue's cohort.
type ClusterQueuePreemption struct {
	// reclaimWithinCohort determines whether a pending Workload can preempt
	// Workloads from other ClusterQueues in the cohort that are using more than
	// their nominal quota. The possible values are:
	//
	// - `Never` (default): do not preempt Workloads in the cohort.
	// - `LowerPriority`: if the pending Workload fits within the nominal
	//   quota of its ClusterQueue, only preempt Workloads in the cohort that have
	//   lower priority than the pending Workload.
	// - `Any`: if the pending Workload fits within the nominal quota of its
	//   ClusterQueue, preempt any Workload in the cohort, irrespective of
	//   priority.
	//
	// +kubebuilder:default=Never
	// +kubebuilder:validation:Enum=Never;LowerPriority;Any
	ReclaimWithinCohort PreemptionPolicy `json:"reclaimWithinCohort,omitempty"`

	// withinClusterQueue determines whether a pending Workload that doesn't fit
	// within the nominal quota for its ClusterQueue, can preempt active Workloads in
	// the ClusterQueue. The possible values are:
	//
	// - `Never` (default): do not preempt Workloads in the ClusterQueue.
	// - `LowerPriority`: only preempt Workloads in the ClusterQueue that have
	//   lower priority than the pending Workload.
	//
	// +kubebuilder:default=Never
	// +kubebuilder:validation:Enum=Never;LowerPriority
	WithinClusterQueue PreemptionPolicy `json:"withinClusterQueue,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:storageversion
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Cohort",JSONPath=".spec.cohort",type=string,description="Cohort that this ClusterQueue belongs to"
//+kubebuilder:printcolumn:name="Strategy",JSONPath=".spec.queueingStrategy",type=string,description="The queueing strategy used to prioritize workloads",priority=1
//+kubebuilder:printcolumn:name="Pending Workloads",JSONPath=".status.pendingWorkloads",type=integer,description="Number of pending workloads"
//+kubebuilder:printcolumn:name="Admitted Workloads",JSONPath=".status.admittedWorkloads",type=integer,description="Number of admitted workloads that haven't finished yet",priority=1

// ClusterQueue is the Schema for the clusterQueue API.
type ClusterQueue struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterQueueSpec   `json:"spec,omitempty"`
	Status ClusterQueueStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ClusterQueueList contains a list of ClusterQueue
type ClusterQueueList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterQueue `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterQueue{}, &ClusterQueueList{})
}
