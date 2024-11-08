// Copyright The NRI Plugins Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package topologyaware

import (
	"strings"

	policy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/cpuallocator"
)

type (
	Constraints = policy.Constraints
	Domain      = policy.Domain
	Amount      = policy.Amount
	AmountKind  = policy.AmountKind
)

const (
	CPU            = policy.CPU
	Memory         = policy.Memory
	AmountAbsent   = policy.AmountAbsent
	AmountQuantity = policy.AmountQuantity
	AmountCPUSet   = policy.AmountCPUSet
)

type CPUPriority string

const (
	PriorityHigh   CPUPriority = "high"
	PriorityNormal CPUPriority = "normal"
	PriorityLow    CPUPriority = "low"
	PriorityNone   CPUPriority = "none"
)

func (p CPUPriority) Value() cpuallocator.CPUPriority {
	switch strings.ToLower(string(p)) {
	case string(PriorityHigh):
		return cpuallocator.PriorityHigh
	case string(PriorityNormal):
		return cpuallocator.PriorityNormal
	case string(PriorityLow):
		return cpuallocator.PriorityLow
	}
	return cpuallocator.PriorityNone
}

// +k8s:deepcopy-gen=true
// +optional
type Config struct {
	// PinCPU controls whether the policy pins containers to allocated CPUs.
	// +kubebuilder:default=true
	// +optional
	PinCPU bool `json:"pinCPU,omitempty"`
	// PinMemory controls whether the policy pins containers allocated memory nodes.
	// +kubebuilder:default=true
	// +optional
	PinMemory bool `json:"pinMemory,omitempty"`
	// PreferIsolated controls whether kernel-isolated CPUs are preferred for
	// Guaranteed QoS-class containers that request 1 full CPU.
	// +kubebuilder:default=true
	//+optional
	PreferIsolated bool `json:"preferIsolatedCPUs,omitempty"`
	// PreferShared controls whether exclusive CPU allocation is considered for
	// all eligible containers. If set to trues, exclusive CPU allocation is only
	// considered for eligible containers which are explicitly annotated to opt
	// out from shared allocation.
	// +optional
	PreferShared bool `json:"preferSharedCPUs,omitempty"`
	// ColocatePods controls whether an attempt is made to allocate containers
	// within the same pod close to each other (to the same topology zone).
	// +optional
	ColocatePods bool `json:"colocatePods,omitempty"`
	// ColocateNamespaces controls whether an attempt is made to allocate all
	// containers of pods in a namespace close to each other (to the same topology
	// zone).
	// +optional
	ColocateNamespaces bool `json:"colocateNamespaces,omitempty"`
	// ReservedPoolNamespaces lists extra namespaces which are treated like
	// 'kube-system' (resources allocate from the reserved pool).
	// +optional
	// +listType=set
	ReservedPoolNamespaces []string `json:"reservedPoolNamespaces,omitempty"`
	// AvailableResources defines the bounding set for the policy to allocate
	// resources from.
	// +optional
	AvailableResources Constraints `json:"availableResources,omitempty"`
	// ReservedResources defines the resources reserved namespaces get assigned
	// to. If AvailableResources is defined, ReservedResources must be a subset
	// of it.
	// +kubebuilder:validation:Required
	ReservedResources Constraints `json:"reservedResources"`
	// DefaultCPUPriority (high, normal, low, none) is the preferred CPU
	// priority for allocated CPUs when a container has not been annotated
	// with any other CPU preference.
	// Notes: Currently this option only affects exclusive CPU allocations.
	// +kubebuilder:validation:Enum=high;normal;low;none
	// +kubebuilder:default=none
	// +kubebuilder:validation:Format:string
	DefaultCPUPriority CPUPriority `json:"defaultCPUPriority,omitempty"`
}
