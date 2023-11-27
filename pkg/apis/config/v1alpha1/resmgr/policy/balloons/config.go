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

package balloons

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

// +k8s:deepcopy-gen=true
type Config struct {
	// PinCPU controls pinning containers to CPUs.
	// +kubebuilder:default=true
	PinCPU *bool `json:"pinCPU,omitempty"`
	// PinMemory controls pinning containers to memory nodes.
	// +kubebuilder:default=true
	PinMemory *bool `json:"pinMemory,omitempty"`
	// IdleCpuClass controls how unusded CPUs outside any a
	// balloons are (re)configured.
	IdleCpuClass string `json:"idleCPUClass",omitempty"`
	// ReservedPoolNamespaces is a list of namespace globs that
	// will be allocated to reserved CPUs.
	ReservedPoolNamespaces []string `json:"reservedPoolNamespaces,omitempty"`
	// If AllocatorTopologyBalancing is true, balloons are
	// allocated and resized so that all topology elements
	// (packages, dies, numa nodes, cores) have roughly same
	// amount of allocations. The default is false: balloons are
	// packed tightly to optimize power efficiency. The value set
	// here can be overridden with the balloon type specific
	// setting with the same name.
	AllocatorTopologyBalancing bool `json:"allocatorTopologyBalancing,omitempty"`
	// PreferSpreadOnPhysicalCores prefers allocating logical CPUs
	// (possibly hyperthreads) for a balloon from separate physical CPU
	// cores. This prevents workloads in the balloon from interfering with
	// themselves as they do not compete on the resources of the same CPU
	// cores. On the other hand, it allows more interference between
	// workloads in different balloons. The default is false: balloons
	// are packed tightly to a minimum number of physical CPU cores. The
	// value set here is the default for all balloon types, but it can be
	// overridden with the balloon type specific setting with the same
	// name.
	PreferSpreadOnPhysicalCores bool `json:"preferSpreadOnPhysicalCores,omitempty"`
	// BallonDefs contains balloon type definitions.
	BalloonDefs []*BalloonDef `json:"balloonTypes,omitempty"`
	// Available/allowed (CPU) resources to use.
	AvailableResources Constraints `json:"availableResources,omitempty"`
	// Reserved (CPU) resources for kube-system namespace.
	// +kubebuilder:validation:Required
	ReservedResources Constraints `json:"reservedResources"`
}

type CPUTopologyLevel string

const (
	CPUTopologyLevelUndefined = ""
	CPUTopologyLevelSystem    = "system"
	CPUTopologyLevelPackage   = "package"
	CPUTopologyLevelDie       = "die"
	CPUTopologyLevelNuma      = "numa"
	CPUTopologyLevelCore      = "core"
	CPUTopologyLevelThread    = "thread"
)

var (
	cpuTopologyLevelValues = map[CPUTopologyLevel]int{
		CPUTopologyLevelUndefined: 0,
		CPUTopologyLevelSystem:    1,
		CPUTopologyLevelPackage:   2,
		CPUTopologyLevelDie:       3,
		CPUTopologyLevelNuma:      4,
		CPUTopologyLevelCore:      5,
		CPUTopologyLevelThread:    6,
	}

	CPUTopologyLevelCount = len(cpuTopologyLevelValues)
)

func (l CPUTopologyLevel) String() string {
	return string(l)
}

func (l CPUTopologyLevel) Value() int {
	if i, ok := cpuTopologyLevelValues[l]; ok {
		return i
	}
	return cpuTopologyLevelValues[CPUTopologyLevelUndefined]
}

// BalloonDef contains a balloon definition.
// +k8s:deepcopy-gen=true
type BalloonDef struct {
	// Name of the balloon definition.
	Name string `json:"name"`
	// Namespaces control which namespaces are assigned into
	// balloon instances from this definition. This is used by
	// namespace assign methods.
	Namespaces []string `json:"namespaces,omitempty"`
	// MaxCpus specifies the maximum number of CPUs exclusively
	// usable by containers in a balloon. Balloon size will not be
	// inflated larger than MaxCpus.
	MaxCpus int `json:"maxCPUs,omitempty"`
	// MinCpus specifies the minimum number of CPUs exclusively
	// usable by containers in a balloon. When new balloon is created,
	// this will be the number of CPUs reserved for it even if a container
	// would request less.
	MinCpus int `json:"minCPUs,omitempty"`
	// AllocatorPriority (High, Normal, Low, None)
	// This parameter is passed to CPU allocator when creating or
	// resizing a balloon. At init, balloons with highest priority
	// CPUs are allocated first.
	// +kubebuilder:validation:Enum=high;normal;low;none
	// +kubebuilder:default=high
	// +kubebuilder:validation:Format:string
	AllocatorPriority CPUPriority `json:"allocatorPriority,omitempty"`
	// PreferSpreadOnPhysicalCores is the balloon type specific
	// parameter of the policy level parameter with the same name.
	PreferSpreadOnPhysicalCores *bool `json:"preferSpreadOnPhysicalCores,omitempty"`
	// AllocatorTopologyBalancing is the balloon type specific
	// parameter of the policy level parameter with the same name.
	AllocatorTopologyBalancing *bool `json:"allocatorTopologyBalancing,omitempty"`
	// CpuClass controls how CPUs of a balloon are (re)configured
	// whenever a balloon is created, inflated or deflated.
	CpuClass string `json:"cpuClass,omitempty"`
	// MinBalloons is the number of balloon instances that always
	// exist even if they would become empty. At init this number
	// of instances will be created before assigning any
	// containers.
	MinBalloons int `json:"minBalloons,omitempty"`
	// MaxBalloons is the maximum number of balloon instances that
	// is allowed to co-exist. If reached, new balloons cannot be
	// created anymore.
	MaxBalloons int `json:"maxBalloons,omitempty"`
	// PreferSpreadingPods: containers of the same pod may be
	// placed on separate balloons. The default is false: prefer
	// placing containers of a pod to the same balloon(s).
	PreferSpreadingPods bool `json:"preferSpreadingPods,omitempty"`
	// PreferPerNamespaceBalloon: if true, containers in different
	// namespaces are preferrably placed in separate balloons,
	// even if the balloon type is the same for all of them. On
	// the other hand, containers in the same namespace will be
	// placed in the same balloon instances. The default is false:
	// namespaces have no effect on placement.
	PreferPerNamespaceBalloon bool `json:"preferPerNamespaceBalloon,omitempty"`
	// PreferNewBalloons: prefer creating new balloons over adding
	// containers to existing balloons. The default is false:
	// prefer using filling free capacity and possibly inflating
	// existing balloons before creating new ones.
	PreferNewBalloons bool `json:"preferNewBalloons,omitempty"`
	// ShareIdleCpusInSame <topology-level>: if there are idle
	// CPUs, that is CPUs not in any balloon, in the same
	// <topology-level> as any CPU in the balloon, then allow
	// workloads to run on those (shared) CPUs in addition to the
	// (dedicated) CPUs of the balloon.
	// +kubebuilder:validation:Enum="";system;package;die;numa;core;thread
	// +kubebuilder:validation:Format:string
	ShareIdleCpusInSame CPUTopologyLevel `json:"shareIdleCPUsInSame,omitempty"`
	// PreferCloseToDevices: prefer creating new balloons of this
	// type close to listed devices.
	PreferCloseToDevices []string `json:"preferCloseToDevices",omitempty`
	// PreferFarFromDevices: prefer creating new balloons of this
	// type far from listed devices.
	PreferFarFromDevices []string `json:"preferFarFromDevices",omitempty`
}

// String stringifies a BalloonDef
func (bdef BalloonDef) String() string {
	return bdef.Name
}

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
