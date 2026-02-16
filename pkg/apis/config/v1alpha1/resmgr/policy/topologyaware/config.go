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
	"errors"
	"fmt"
	"strings"

	policy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/cpuallocator"
	v1 "k8s.io/api/core/v1"
)

type (
	Constraints      = policy.Constraints
	Domain           = policy.Domain
	Amount           = policy.Amount
	AmountKind       = policy.AmountKind
	CPUTopologyLevel = policy.CPUTopologyLevel
	SchedulingClass  = policy.SchedulingClass
)

const (
	CPU                 = policy.CPU
	Memory              = policy.Memory
	AmountAbsent        = policy.AmountAbsent
	AmountQuantity      = policy.AmountQuantity
	AmountCPUSet        = policy.AmountCPUSet
	AmountExcludeCPUSet = policy.AmountExcludeCPUSet

	CPUTopologyLevelUndefined = policy.CPUTopologyLevelUndefined
	CPUTopologyLevelSystem    = policy.CPUTopologyLevelSystem
	CPUTopologyLevelPackage   = policy.CPUTopologyLevelPackage
	CPUTopologyLevelDie       = policy.CPUTopologyLevelDie
	CPUTopologyLevelNuma      = policy.CPUTopologyLevelNuma
	CPUTopologyLevelL2Cache   = policy.CPUTopologyLevelL2Cache
	CPUTopologyLevelCore      = policy.CPUTopologyLevelCore
	CPUTopologyLevelThread    = policy.CPUTopologyLevelThread

	SchedulingPolicyUndefined  = policy.SchedulingPolicyUndefined
	SchedulingPolicyNone       = policy.SchedulingPolicyNone
	SchedulingPolicyOther      = policy.SchedulingPolicyOther
	SchedulingPolicyFifo       = policy.SchedulingPolicyFifo
	SchedulingPolicyRr         = policy.SchedulingPolicyRr
	SchedulingPolicyBatch      = policy.SchedulingPolicyBatch
	SchedulingPolicyIdle       = policy.SchedulingPolicyIdle
	SchedulingPolicyDeadline   = policy.SchedulingPolicyDeadline
	SchedulingFlagResetOnFork  = policy.SchedulingFlagResetOnFork
	SchedulingFlagReclaimable  = policy.SchedulingFlagReclaimable
	SchedulingFlagDlOverrun    = policy.SchedulingFlagDlOverrun
	SchedulingFlagKeepPolicy   = policy.SchedulingFlagKeepPolicy
	SchedulingFlagKeepParams   = policy.SchedulingFlagKeepParams
	SchedulingFlagUtilClampMin = policy.SchedulingFlagUtilClampMin
	SchedulingFlagUtilClampMax = policy.SchedulingFlagUtilClampMax
)

var (
	CPUTopologyLevelCount = policy.CPUTopologyLevelCount
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

// +kubebuilder:object:generate=true
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
	PreferIsolated *bool `json:"preferIsolatedCPUs,omitempty"`
	// PreferShared controls whether exclusive CPU allocation is considered for
	// all eligible containers. If set to trues, exclusive CPU allocation is only
	// considered for eligible containers which are explicitly annotated to opt
	// out from shared allocation.
	// +optional
	PreferShared *bool `json:"preferSharedCPUs,omitempty"`
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
	// UnlimitedBurstable defines the preferred topology level for containers
	// with unlimited burstability.
	// +kubebuilder:validation:Enum=system;package;die;numa
	// +kubebuilder:default=package
	// +kubebuilder:validation:Format:string
	UnlimitedBurstable CPUTopologyLevel `json:"unlimitedBurstable,omitempty"`
	// SchedulingClasses define known scheduling classes. Each class is a
	// combination of Linux scheduling policy and I/O priority parameters.
	// Containers with exclusive CPU allocation can be annotated to a class.
	// +optional
	SchedulingClasses []*SchedulingClass `json:"schedulingClasses,omitempty"`
	// NamespaceSchedulingClasses assign default scheduling classes to namespaces.
	// If a namespace has an assigned class, containers in that namespace inherit
	// it unless they are annotated otherwise. Any default namespace scheduling
	// class takes precedence over any default Pod QoS scheduling class.
	// +optional
	NamespaceSchedulingClasses map[string]string `json:"namespaceSchedulingClasses,omitempty"`
	// PodQoSSchedulingClasses assign default scheduling classes to Pod QoS
	// classes. If a QoS class has an assigned scheduling class, containers
	// in that QoS class inherit it unless they are annotated otherwise.
	// +optional
	PodQoSSchedulingClasses map[string]string `json:"podQoSSchedulingClasses,omitempty"`
}

var (
	validQoSClasses = map[string]bool{
		strings.ToLower(string(v1.PodQOSBestEffort)): true,
		strings.ToLower(string(v1.PodQOSBurstable)):  true,
		strings.ToLower(string(v1.PodQOSGuaranteed)): true,
	}
)

// Validate the configuration.
func (c *Config) Validate() error {
	var errs []error

	if len(c.PodQoSSchedulingClasses) > 0 {
		canonical := map[string]string{}
		for qos, scheduling := range c.PodQoSSchedulingClasses {
			lower := strings.ToLower(qos)
			valid := validQoSClasses[lower]
			class := c.GetSchedulingClass(scheduling)
			if !valid {
				errs = append(errs,
					fmt.Errorf("invalid QoS class %q (with scheduling %q)", qos, scheduling))
			}
			if class == nil {
				errs = append(errs, fmt.Errorf("unknown scheduling class %q", scheduling))
			}
			if valid && class != nil {
				canonical[lower] = scheduling
			}
		}
		if len(errs) == 0 {
			c.PodQoSSchedulingClasses = canonical
		}
	}

	for ns, scheduling := range c.NamespaceSchedulingClasses {
		if c.GetSchedulingClass(scheduling) == nil {
			errs = append(errs,
				fmt.Errorf("unknown scheduling class %q for namespace %q", scheduling, ns))
		}
	}

	return errors.Join(errs...)
}

// GetSchedulingClass returns the named class or nil if it is not defined.
func (c *Config) GetSchedulingClass(name string) *SchedulingClass {
	for _, sc := range c.SchedulingClasses {
		if sc.Name == name {
			return sc
		}
	}
	return nil
}

// GetNamespaceSchedulingClass returns the scheduling class for the given namespace.
func (c *Config) GetNamespaceSchedulingClass(ns string) (*SchedulingClass, error) {
	if name, ok := c.NamespaceSchedulingClasses[ns]; ok {
		sc := c.GetSchedulingClass(name)
		if sc == nil {
			return nil, fmt.Errorf("unknown scheduling class %q for namespace %q", name, ns)
		}
		return sc, nil
	}

	return nil, nil
}

// GetPodQoSSchedulingClass returns the scheduling class for the given Pod QoS class.
func (c *Config) GetPodQoSSchedulingClass(qos v1.PodQOSClass) (*SchedulingClass, error) {
	if name, ok := c.PodQoSSchedulingClasses[strings.ToLower(string(qos))]; ok {
		sc := c.GetSchedulingClass(name)
		if sc == nil {
			return nil, fmt.Errorf("unknown scheduling class %q for QoS class %v", name, qos)
		}
		return sc, nil
	}

	return nil, nil
}

// GetDefaultSchedulingClass returns the scheduling class inherited from
// a namespace or a Pod QoS class.
func (c *Config) GetDefaultSchedulingClass(ns string, qos v1.PodQOSClass) (*SchedulingClass, error) {
	sc, err := c.GetNamespaceSchedulingClass(ns)
	if sc != nil || err != nil {
		return sc, err
	}

	return c.GetPodQoSSchedulingClass(qos)
}
