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
	"errors"
	"strings"

	policy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	resmgr "github.com/containers/nri-plugins/pkg/apis/resmgr/v1alpha1"
	"github.com/containers/nri-plugins/pkg/cpuallocator"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
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

// +kubebuilder:object:generate=true
type Config struct {
	// PinCPU controls pinning containers to CPUs.
	// +kubebuilder:default=true
	PinCPU *bool `json:"pinCPU,omitempty"`
	// PinMemory controls pinning containers to memory nodes.
	// +kubebuilder:default=true
	PinMemory *bool `json:"pinMemory,omitempty"`
	// IdleCpuClass controls how unusded CPUs outside any a
	// balloons are (re)configured.
	IdleCpuClass string `json:"idleCPUClass,omitempty"`
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
	// Preserve specifies containers whose resource pinning must not be
	// modified by the policy.
	Preserve *ContainerMatchConfig `json:"preserve,omitempty"`
	// ShowContainersInNrt controls whether containers in balloons
	// are exposed as part of NodeResourceTopology. If true,
	// noderesourcetopologies.topology.node.k8s.io custom
	// resources provide visibility to CPU and memory affinity of
	// containers assigned into balloons on any node. The default
	// is false. Use balloon-type option with the same name to
	// override the policy-level default. Exposing affinities of
	// all containers on all nodes may generate a lot of traffic
	// and large CR object updates to Kubernetes API server. This
	// options has no effect unless agent:NodeResourceTopology
	// enables basic topology exposure.
	ShowContainersInNrt *bool `json:"showContainersInNrt,omitempty"`
	// LoadClasses specify available loads in balloon types.
	LoadClasses []LoadClass `json:"loadClasses,omitempty"`
}

type CPUTopologyLevel string

const (
	CPUTopologyLevelUndefined = ""
	CPUTopologyLevelSystem    = "system"
	CPUTopologyLevelPackage   = "package"
	CPUTopologyLevelDie       = "die"
	CPUTopologyLevelNuma      = "numa"
	CPUTopologyLevelL2Cache   = "l2cache"
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
		CPUTopologyLevelL2Cache:   5,
		CPUTopologyLevelCore:      6,
		CPUTopologyLevelThread:    7,
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
// +kubebuilder:object:generate=true
type BalloonDef struct {
	// Name of the balloon definition.
	Name string `json:"name"`
	// Components is a list of component properties. Every
	// component has a balloonType property according to which
	// CPUs are allocated for that component. Specifying the
	// Components list makes this a composite balloon type whose
	// instances uses all CPUs of its component instances, and no
	// other CPUs.
	Components []BalloonDefComponent `json:"components,omitempty"`
	// Namespaces control which namespaces are assigned into
	// balloon instances from this definition. This is used by
	// namespace assign methods.
	Namespaces []string `json:"namespaces,omitempty"`
	// GroupBy groups containers into same balloon instances if
	// their GroupBy expressions evaluate to the same group.
	// Expressions are strings where key references like
	// ${pod/labels/mylabel} will be substituted with
	// corresponding values.
	GroupBy string `json:"groupBy,omitempty"`
	// MatchExpressions specifies one or more expressions which are evaluated
	// to see if a container should be assigned into balloon instances from
	// this definition.
	MatchExpressions []resmgr.Expression `json:"matchExpressions,omitempty"`
	// MaxCpus specifies the maximum number of CPUs exclusively
	// usable by containers in a balloon. Balloon size will not be
	// inflated larger than MaxCpus.
	MaxCpus int `json:"maxCPUs,omitempty"`
	// MinCpus specifies the minimum number of CPUs exclusively
	// usable by containers in a balloon. When new balloon is created,
	// this will be the number of CPUs reserved for it even if a container
	// would request less.
	MinCpus int `json:"minCPUs,omitempty"`
	// MemoryTypes lists memory types allowed to containers in a
	// balloon. Supported types are: DRAM, HBM, PMEM. By default
	// all memory types in the system are allowed.
	// +listType=set
	// +kubebuilder:validation:items:XValidation:rule="self == 'DRAM' || self == 'HBM' || self == 'PMEM'",messageExpression="\"invalid memory type: \" + self + \", expected DRAM, HBM, or PMEM\""
	MemoryTypes []string `json:"memoryTypes,omitempty"`
	// PinMemory controls pinning containers to memory nodes.
	// Overrides the policy level PinMemory setting in this balloon type.
	PinMemory *bool `json:"pinMemory,omitempty"`
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
	// HideHyperthreads allows containers in a balloon use only
	// one hyperthread from each physical CPU core in the
	// balloon. For instance, if a balloon contains 16 logical
	// CPUs from 8 physical cores and this option is true, then
	// containers in the balloon will be allowed to use 8 logical
	// CPUs, one from each physical core. This option is best used
	// with PreferSpreadOnPhysicalCores=false in order to allocate
	// all hyperthreads of each physical core into the same
	// balloon, but allow containers to use only one hyperthread
	// from each core. This will ensure that hidden hyperthreads
	// will remain completely idle as they cannot be allocated to
	// other balloons.
	HideHyperthreads *bool `json:"hideHyperthreads,omitempty"`
	// AllocatorTopologyBalancing is the balloon type specific
	// parameter of the policy level parameter with the same name.
	AllocatorTopologyBalancing *bool `json:"allocatorTopologyBalancing,omitempty"`
	// CpuClass controls how CPUs of a balloon are (re)configured
	// whenever a balloon is created, inflated or deflated.
	CpuClass string `json:"cpuClass,omitempty"`
	// Loads is a list of loadClasses that describe load generated
	// by containers in these balloons. CPUs are selected to
	// balloons with loads so that overloading any part of the
	// system is avoided.
	// +listType=set
	Loads []string `json:"loads,omitempty"`
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
	// namespaces are preferably placed in separate balloons,
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
	// +kubebuilder:validation:Enum="";system;package;die;numa;l2cache;core;thread
	// +kubebuilder:validation:Format:string
	ShareIdleCpusInSame CPUTopologyLevel `json:"shareIdleCPUsInSame,omitempty"`
	// PreferCloseToDevices: prefer creating new balloons of this
	// type close to listed devices.
	PreferCloseToDevices []string `json:"preferCloseToDevices,omitempty"`
	// PreferFarFromDevices: prefer creating new balloons of this
	// type far from listed devices.
	// TODO: PreferFarFromDevices is considered too untested for usage. Hence,
	// for the time being we prevent its usage through CRDs.
	PreferFarFromDevices []string `json:"-"`
	// preferIsolCpus: prefer kernel isolated cpus
	// +kubebuilder:default=false
	PreferIsolCpus bool `json:"preferIsolCpus,omitempty"`
	// preferCoreType: prefer performance or efficient (P/E) CPU cores on
	// hybrid architectures.
	// +optional
	// +kubebuilder:validation:Enum=efficient;performance
	PreferCoreType string `json:"preferCoreType,omitempty"`
	// ShowContainersInNrt controls showing containers and their
	// resource affinities as part of
	// NodeResourceTopology. Overrides the policy level
	// ShowContainersInNrt setting for containers in this balloon
	// type. Requires agent:NodeResourceTopology. Note that this
	// may generate a lot of traffic and large CR object updates
	// to Kubernetes API server.
	ShowContainersInNrt *bool `json:"showContainersInNrt,omitempty"`
}

// BalloonDefComponent contains a balloon component definition.
// +kubebuilder:object:generate=true
type BalloonDefComponent struct {
	// BalloonType is the name of the balloon type of this
	// component. It must match the name of a balloon type
	// defined in the ballonTypes of the policy.
	// +kubebuilder:validation:Required
	DefName string `json:"balloonType"`
}

// LoadClass specifies how a load affects the system and load
// generating containers themselves.
type LoadClass struct {
	// Name of the load class.
	// +kube:validation:Required
	Name string `json:"name"`
	// Level specifies hardware topology level loaded by loads in this class.
	// +kubebuilder:validation:Enum=l2cache;core
	// +kubebuilder:validation:Format:string
	Level CPUTopologyLevel `json:"level"`
	// OverloadsLevelInBalloon controls whether containers in the
	// same balloon should avoid the load generated by themselves.
	// The default is false: CPUs for a balloon that generates
	// this load can be selected as close each other as
	// possible. Set to true, for instance, to select only one
	// hyperthread per core or only one CPU per L2 cache block in
	// to a balloon. Unselected CPUs from these domains can be
	// selected into balloons that do not load the same level.
	OverloadsLevelInBalloon bool `json:"overloadsLevelInBalloon,omitempty"`
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

// ContainerMatchConfig contains container matching configurations.
// +kubebuilder:object:generate=true
type ContainerMatchConfig struct {
	// MatchExpressions specifies one or more expressions.
	MatchExpressions []resmgr.Expression `json:"matchExpressions,omitempty"`
}

func (cmc *ContainerMatchConfig) MatchContainer(c cache.Container) (string, error) {
	for _, expr := range cmc.MatchExpressions {
		if expr.Evaluate(c) {
			return expr.String(), nil
		}
	}
	return "", nil
}

func (c *Config) Validate() error {
	errs := []error{}
	if c.Preserve != nil {
		for _, expr := range c.Preserve.MatchExpressions {
			if err := expr.Validate(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, blnDef := range c.BalloonDefs {
		for _, expr := range blnDef.MatchExpressions {
			if err := expr.Validate(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
