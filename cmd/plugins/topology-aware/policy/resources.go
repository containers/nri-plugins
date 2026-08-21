// Copyright 2019 Intel Corporation. All Rights Reserved.
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
	"strconv"
	"time"

	"github.com/containers/nri-plugins/pkg/agent/podresapi"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/topology"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"

	"github.com/containers/nri-plugins/pkg/cpuallocator"
	"github.com/containers/nri-plugins/pkg/kubernetes"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	topoutil "github.com/containers/nri-plugins/pkg/utils/topology"
)

type (
	cpuPrio = cpuallocator.CPUPriority
)

const (
	highPrio   = cpuallocator.PriorityHigh
	normalPrio = cpuallocator.PriorityNormal
	lowPrio    = cpuallocator.PriorityLow
	nonePrio   = cpuallocator.PriorityNone
)

var (
	defaultPrio = nonePrio

	cpuPrioByName = map[string]cpuPrio{
		"high":   highPrio,
		"normal": normalPrio,
		"low":    lowPrio,
		"none":   nonePrio,
	}
)

// Supply represents avaialbe CPU and memory capacity of a node.
type Supply interface {
	// GetNode returns the node supplying this capacity.
	GetNode() Node
	// Clone creates a copy of this supply.
	Clone() Supply
	// IsolatedCPUs returns the isolated cpuset in this supply.
	IsolatedCPUs() cpuset.CPUSet
	// ReservedCPUs returns the reserved cpuset in this supply.
	ReservedCPUs() cpuset.CPUSet
	// SharableCPUs returns the sharable cpuset in this supply.
	SharableCPUs() cpuset.CPUSet
	// GrantedReserved returns the locally granted reserved CPU capacity in this supply.
	GrantedReserved() int
	// GrantedShared returns the locally granted shared CPU capacity in this supply.
	GrantedShared() int
	// Cumulate cumulates the given supply into this one.
	Cumulate(Supply)
	// AccountAllocateCPU accounts for (removes) allocated exclusive capacity from the supply.
	AccountAllocateCPU(Grant)
	// AccountReleaseCPU accounts for (reinserts) released exclusive capacity into the supply.
	AccountReleaseCPU(Grant)
	// GetScore calculates how well this supply fits/fulfills the given request.
	GetScore(Request) Score
	// AllocatableSharedCPU calculates the allocatable amount of shared CPU of this supply.
	AllocatableSharedCPU(...bool) int
	// SliceableCPUs calculates the shared cpuset we can slice exclusive CPUs off of.
	SliceableCPUs() (cpuset.CPUSet, error)
	// Allocate allocates a grant from the supply.
	Allocate(Request, *libmem.Offer) (Grant, map[string]libmem.NodeMask, error)
	// ReleaseCPU releases a previously allocated CPU grant from this supply.
	ReleaseCPU(Grant)

	// GetCPUOffer returns the exclusive CPUs that would be allocated for the given request.
	GetCPUOffer(Request) (cpuset.CPUSet, error)

	// Reserve accounts for CPU grants after reloading cached allocations.
	Reserve(Grant, *libmem.Offer) (map[string]libmem.NodeMask, error)
	// DumpCapacity returns a printable representation of the supply's resource capacity.
	DumpCapacity() string
	// DumpAllocatable returns a printable representation of the supply's alloctable resources.
	DumpAllocatable() string
}

// Request represents CPU and memory resources requested by a container.
type Request interface {
	// GetContainer returns the container requesting CPU capacity.
	GetContainer() cache.Container
	// String returns a printable representation of this request.
	String() string
	// CPUType returns the type of requested CPU.
	CPUType() cpuType
	// CPUPrio returns the preferred priority of requested CPU.
	CPUPrio() cpuPrio
	// SetCPUType sets the type of requested CPU.
	SetCPUType(cpuType cpuType)
	// FullCPUs return the number of full CPUs requested.
	FullCPUs() int
	// CPUFraction returns the amount of fractional milli-CPU requested.
	CPUFraction() int
	// CPULimit returns the amount of fractional CPU limit.
	CPULimit() int
	// Isolate returns whether isolated CPUs are preferred for this request.
	Isolate() bool
	// MemoryType returns the type(s) of requested memory.
	MemoryType() memoryType
	// MemAmountToAllocate retuns how much memory we need to reserve for a request.
	MemAmountToAllocate() int64
	// MemoryLimit returns the memory limit for the request.
	MemoryLimit() int64
	// PickByHints returns if picking resources by hints is preferred for this request.
	PickByHints() bool
	// IrqAffinity returns the IRQ affinity for this request.
	IrqAffinity() *IrqAffinity
	// ColdStart returns the cold start timeout.
	ColdStart() time.Duration
}

// Grant represents CPU and memory capacity allocated to a container from a node.
type Grant interface {
	// SetCPUPortion sets the fraction CPU portion for the grant.
	SetCPUPortion(fraction int)
	// Clone creates a copy of this grant.
	Clone() Grant
	// RefetchNodes updates the stored cpu and memory nodes of this grant by name.
	RefetchNodes() error
	// GetContainer returns the container CPU capacity is granted to.
	GetContainer() cache.Container
	// GetCPUNode returns the node that granted CPU capacity to the container.
	GetCPUNode() Node
	// GetMemorySize returns the amount of memory allocated to this grant.
	GetMemorySize() int64
	// GetMemoryZone returns the memory zone allocated granted to the container.
	GetMemoryZone() libmem.NodeMask
	// CPUType returns the type of granted CPUs
	CPUType() cpuType
	// CPUClass returns the CPU class for this grant.
	CPUClass() string
	// SetCPUClass sets the CPU class for this grant.
	SetCPUClass(string)
	// CPUPortion returns granted milli-CPUs of non-full CPUs of CPUType().
	// CPUPortion() == ReservedPortion() + SharedPortion().
	CPUPortion() int
	// ExclusiveCPUs returns the exclusively granted non-isolated cpuset.
	ExclusiveCPUs() cpuset.CPUSet
	// ReservedCPUs returns the reserved granted cpuset.
	ReservedCPUs() cpuset.CPUSet
	// ReservedPortion() returns the amount of CPUs in milli-CPU granted.
	ReservedPortion() int
	// SharedCPUs returns the shared granted cpuset.
	SharedCPUs() cpuset.CPUSet
	// SharedPortion returns the amount of CPUs in milli-CPU granted.
	SharedPortion() int
	// IsolatedCpus returns the exclusively granted isolated cpuset.
	IsolatedCPUs() cpuset.CPUSet
	// MemoryType returns the type(s) of granted memory.
	MemoryType() memoryType
	// SetMemoryType sets the memory type for this grant.
	SetMemoryType(memoryType)
	// SetMemoryZone sets the memory zone for this grant.
	SetMemoryZone(libmem.NodeMask)
	// SetMemorySize sets the amount of memory to allocate.
	SetMemorySize(int64)
	// IrqAffinity returns the IRQ affinity for this grant.
	IrqAffinity() *IrqAffinity

	// SetColdstart sets coldstart period for the grant.
	SetColdstart(time.Duration)

	// String returns a printable representation of this grant.
	String() string
	// Release releases the grant from all the Supplys it uses.
	Release()
	// Reallocate memory with the given types.
	ReallocMemory(types libmem.TypeMask) error
	// AccountAllocateCPU accounts for (removes) allocated exclusive capacity for this grant.
	AccountAllocateCPU()
	// AccountReleaseCPU accounts for (reinserts) released exclusive capacity for this grant.
	AccountReleaseCPU()
	// ColdStart returns the cold start timeout.
	ColdStart() time.Duration
	// AddTimer adds a cold start timer.
	AddTimer(*time.Timer)
	// StopTimer stops a cold start timer.
	StopTimer()
	// ClearTimer clears the cold start timer pointer.
	ClearTimer()
}

// Score represents how well a supply can satisfy a request.
type Score interface {
	// Calculate the actual score from the collected parameters.
	Eval() float64
	// Supply returns the supply associated with this score.
	Supply() Supply
	// Request returns the request associated with this score.
	Request() Request

	IsolatedCapacity() int
	ReservedCapacity() int
	SharedCapacity() int
	Colocated() int
	HintScores() map[string]float64
	PrioCapacity(cpuPrio) int
	CpuClassHints() *cpuclass.AllocationHints
	CpuClassCpus() cpuset.CPUSet

	MemOffer() *libmem.Offer
	CPUOffer() cpuset.CPUSet

	String() string
}

// supply implements our Supply interface.
type supply struct {
	node            Node          // node supplying CPUs and memory
	isolated        cpuset.CPUSet // isolated CPUs at this node
	reserved        cpuset.CPUSet // reserved CPUs at this node
	sharable        cpuset.CPUSet // sharable CPUs at this node
	grantedReserved int           // amount of reserved CPUs allocated
	grantedShared   int           // amount of shareable CPUs allocated
}

var _ Supply = &supply{}

// request implements our Request interface.
type request struct {
	container   cache.Container // container for this request
	full        int             // number of full CPUs requested
	fraction    int             // amount of fractional CPU requested
	limit       int             // CPU limit, MaxInt for no limit
	isolate     bool            // prefer isolated exclusive CPUs
	cpuType     cpuType         // preferred CPU type (normal, reserved)
	cpuClass    string          // requested or default CPU class
	prio        cpuPrio         // CPU priority preference, ignored for fraction requests
	memReq      int64           // memory request
	memLim      int64           // memory limit
	memType     memoryType      // requested types of memory
	pickByHints bool            // preference to pick resources by hints
	irqs        *IrqAffinity    // IRQ affinity for this request

	// coldStart tells the timeout (in milliseconds) how long to wait until
	// a DRAM memory controller should be added to a container asking for a
	// mixed DRAM/PMEM memory allocation. This allows for a "cold start" where
	// initial memory requests are made to the PMEM memory. A value of 0
	// indicates that cold start is not explicitly requested.
	coldStart time.Duration
}

var _ Request = &request{}

// grant implements our Grant interface.
type grant struct {
	container      cache.Container // container CPU is granted to
	node           Node            // node CPU is supplied from
	exclusive      cpuset.CPUSet   // exclusive CPUs
	cpuType        cpuType         // type of CPUs (normal, reserved, ...)
	cpuPortion     int             // milliCPUs granted from CPUs of cpuType
	memType        memoryType      // requested types of memory
	coldStart      time.Duration   // how long until cold start is done
	coldStartTimer *time.Timer     // timer to trigger cold start timeout
	memSize        int64           // amount of memory to allocate
	memZone        libmem.NodeMask // allocated memory zone
	cpuClass       string          // CPU class to apply to exclusive CPUs
	irqs           *IrqAffinity    // IRQ affinity for this request
}

var _ Grant = &grant{}

// score implements our Score interface.
type score struct {
	supply    Supply                    // CPU supply (node)
	req       Request                   // CPU request (container)
	mem       *libmem.Offer             // possible memory allocation
	cpu       cpuset.CPUSet             // CPUs offered by this supply for the request
	isolated  int                       // remaining isolated CPUs
	reserved  int                       // remaining reserved CPUs
	shared    int                       // remaining shared capacity
	prio      map[cpuPrio]int           // low/normal/high-prio CPU capacity
	colocated int                       // number of colocated containers
	hints     map[string]float64        // hint scores
	ccHints   *cpuclass.AllocationHints // CPU class hints
	ccCpus    cpuset.CPUSet             // CPU class-hinted CPUs for the offered CPUs
}

var _ Score = &score{}

// newSupply creates CPU supply for the given node, cpusets and existing grant.

func newSupply(n Node, isolated, reserved, sharable cpuset.CPUSet, grantedReserved int, grantedShared int) Supply {
	return &supply{
		node:            n,
		isolated:        isolated.Clone(),
		reserved:        reserved.Clone(),
		sharable:        sharable.Clone(),
		grantedReserved: grantedReserved,
		grantedShared:   grantedShared,
	}
}

// GetNode returns the node supplying CPU and memory.
func (cs *supply) GetNode() Node {
	return cs.node
}

// Clone clones the given CPU supply.
func (cs *supply) Clone() Supply {
	return newSupply(cs.node, cs.isolated, cs.reserved, cs.sharable, cs.grantedReserved, cs.grantedShared)
}

// IsolatedCpus returns the isolated CPUSet of this supply.
func (cs *supply) IsolatedCPUs() cpuset.CPUSet {
	return cs.isolated.Clone()
}

// ReservedCpus returns the reserved CPUSet of this supply.
func (cs *supply) ReservedCPUs() cpuset.CPUSet {
	return cs.reserved.Clone()
}

// SharableCpus returns the sharable CPUSet of this supply.
func (cs *supply) SharableCPUs() cpuset.CPUSet {
	return cs.sharable.Clone()
}

// GrantedReserved returns the locally granted reserved CPU capacity.
func (cs *supply) GrantedReserved() int {
	return cs.grantedReserved
}

// GrantedShared returns the locally granted sharable CPU capacity.
func (cs *supply) GrantedShared() int {
	return cs.grantedShared
}

// Cumulate more CPU to supply.
func (cs *supply) Cumulate(more Supply) {
	mcs := more.(*supply)

	cs.isolated = cs.isolated.Union(mcs.isolated)
	cs.reserved = cs.reserved.Union(mcs.reserved)
	cs.sharable = cs.sharable.Union(mcs.sharable)
	cs.grantedReserved += mcs.grantedReserved
	cs.grantedShared += mcs.grantedShared
}

// AccountAllocateCPU accounts for (removes) allocated exclusive capacity from the supply.
func (cs *supply) AccountAllocateCPU(g Grant) {
	if cs.node.IsSameNode(g.GetCPUNode()) {
		return
	}
	exclusive := g.ExclusiveCPUs()
	cs.isolated = cs.isolated.Difference(exclusive)
	cs.sharable = cs.sharable.Difference(exclusive)
}

// AccountReleaseCPU accounts for (reinserts) released exclusive capacity into the supply.
func (cs *supply) AccountReleaseCPU(g Grant) {
	if cs.node.IsSameNode(g.GetCPUNode()) {
		return
	}

	ncs := cs.node.GetSupply()
	nodecpus := ncs.IsolatedCPUs().Union(ncs.SharableCPUs())
	grantcpus := g.ExclusiveCPUs().Intersection(nodecpus)

	isolated := grantcpus.Intersection(ncs.IsolatedCPUs())
	sharable := grantcpus.Intersection(ncs.SharableCPUs())
	cs.isolated = cs.isolated.Union(isolated)
	cs.sharable = cs.sharable.Union(sharable)
}

// Allocate allocates a grant from the supply.
func (cs *supply) Allocate(r Request, o *libmem.Offer) (Grant, map[string]libmem.NodeMask, error) {
	if o == nil {
		return nil, nil, fmt.Errorf("nil libmem offer")
	}

	grant, err := cs.AllocateCPU(r)
	if err != nil {
		return nil, nil, err
	}

	if err := r.(*request).verifyStrictPreferences(grant); err != nil {
		cs.ReleaseCPU(grant)
		return nil, nil, err
	}

	zone, updates, err := o.Commit()
	if err != nil {
		cs.ReleaseCPU(grant)
		return nil, nil, fmt.Errorf("failed to commit memory offer: %v", err)
	}

	grant.SetMemorySize(r.MemAmountToAllocate())
	grant.SetMemoryType(r.MemoryType())
	grant.SetMemoryZone(zone)
	grant.SetColdstart(r.ColdStart())

	return grant, updates, nil
}

// AllocateCPU allocates CPU for a grant from the supply.
func (cs *supply) AllocateCPU(r Request) (Grant, error) {
	var (
		exclusive cpuset.CPUSet
		err       error
	)

	cr := r.(*request)

	full := cr.full
	fraction := cr.fraction

	cpuType := cr.cpuType
	cpuClass := cr.cpuClass
	irqs := cr.irqs

	if cpuType == cpuReserved && full > 0 {
		log.Warnf("exclusive reserved CPUs not supported, allocating %d full CPUs as fractions", full)
		fraction += full * 1000
		full = 0
	}

	if cpuType == cpuReserved && fraction > 0 && cs.AllocatableReservedCPU() < fraction {
		log.Warnf("possible misconfiguration of reserved resources:")
		log.Warnf("  %s: allocatable %s", cs.GetNode().Name(), cs.DumpAllocatable())
		log.Warnf("  %s: needs %d reserved, only %d available",
			cr.GetContainer().PrettyName(), fraction, cs.AllocatableReservedCPU())
		log.Warnf("  falling back to using normal unreserved CPUs instead...")
		cpuType = cpuNormal
	}

	// allocate isolated exclusive CPUs or slice them off the sharable set
	exclusive, err = cs.pickExclusiveCPUs(r, full, false)
	if err != nil {
		return nil, err
	}

	grant := newGrant(cs.node, cr.GetContainer(), cpuType, cpuClass, exclusive, 0, 0, irqs, 0)
	grant.AccountAllocateCPU()

	// allocate the shared fraction of CPUs
	if fraction > 0 {
		_, err := cs.pickSharedFraction(r, fraction, cpuType, false)
		if err != nil {
			cs.ReleaseCPU(grant)
			return nil, err
		}
		grant.SetCPUPortion(fraction)
	}

	return grant, nil
}

func (cs *supply) GetCPUOffer(r Request) (cpuset.CPUSet, error) {
	return cs.pickExclusiveCPUs(r, r.FullCPUs(), true)
}

func (cs *supply) pickExclusiveCPUs(r Request, cnt int, dryRun bool) (cpuset.CPUSet, error) {
	var (
		cr   = r.(*request)
		none = cpuset.New()
	)

	switch {
	case cr.isolate && cs.isolated.Size() >= cnt:
		var (
			from = &cs.isolated
			pick cpuset.CPUSet
			err  error
		)

		if dryRun {
			copy := from.Clone()
			from = &copy
		}

		if cr.PickByHints() {
			pick, err = cs.takeCPUsByHints(from, cr.GetContainer().GetTopologyHints(), cnt, cr.CPUPrio())
		} else {
			pick, err = cs.takeCPUs(from, nil, cnt, cr.CPUPrio())
		}
		if err != nil {
			return none, policyError("internal error: "+
				"%s: can't take %d exclusive isolated CPUs from %s: %v",
				cs.node.Name(), cnt, *from, err)
		}
		return pick, nil

	case cs.AllocatableSharedCPU() >= 1000*cnt:
		var (
			slice, err = cs.SliceableCPUs()
			pick       cpuset.CPUSet
		)

		if err != nil {
			return none, policyError("internal error: "+
				"%s: can't take %d exclusive CPUs from shared %s: %v",
				cs.node.Name(), cnt, cs.SharableCPUs(), err)
		}

		log.Debugf("%s: sliceable cpuset is %s", cs.node.Name(), slice)

		if cr.PickByHints() {
			pick, err = cs.takeCPUsByHints(&slice, cr.GetContainer().GetTopologyHints(), cnt, cr.CPUPrio())
		} else {
			pick, err = cs.takeCPUs(&slice, nil, cnt, cr.CPUPrio())
		}
		if err != nil {
			return none, policyError("internal error: "+
				"%s: can't take %d exclusive CPUs from %s: %v",
				cs.node.Name(), cnt, slice, err)
		}

		if !dryRun {
			cs.sharable = cs.sharable.Difference(pick)
		}

		return pick, nil
	}

	return none, policyError("internal error: "+
		"%s: can't slice %d exclusive CPUs from %s, %dm available",
		cs.node.Name(), cnt, cs.sharable, cs.AllocatableSharedCPU())
}

func (cs *supply) pickSharedFraction(r Request, fraction int, cpuType cpuType, dryRun bool) (int, error) {
	if fraction < 0 {
		return 0, nil
	}

	if fraction > 0 {
		switch cpuType {
		case cpuNormal:
			// allocate requested portion of shared CPUs
			if cs.AllocatableSharedCPU() < fraction {
				return 0, policyError("internal error: "+
					"%s: not enough %dm sharable CPU for %dm, %dm available",
					cs.node.Name(), fraction, cs.sharable, cs.AllocatableSharedCPU())
			}
			if !dryRun {
				cs.grantedShared += fraction
			}
		case cpuReserved:
			// allocate requested portion of reserved CPUs
			if cs.AllocatableReservedCPU() < fraction {
				return 0, policyError("internal error: "+
					"%s: not enough reserved CPU: %dm requested, %dm available",
					cs.node.Name(), fraction, cs.AllocatableReservedCPU())
			}
			if !dryRun {
				cs.grantedReserved += fraction
			}
		}
	}

	return fraction, nil
}

func (cs *supply) ReleaseCPU(g Grant) {
	isolated := g.ExclusiveCPUs().Intersection(cs.node.GetSupply().IsolatedCPUs())
	sharable := g.ExclusiveCPUs().Difference(isolated)

	cs.isolated = cs.isolated.Union(isolated)
	cs.sharable = cs.sharable.Union(sharable)
	cs.grantedReserved -= g.ReservedPortion()
	cs.grantedShared -= g.SharedPortion()

	g.AccountReleaseCPU()
}

func (cs *supply) Reserve(g Grant, o *libmem.Offer) (map[string]libmem.NodeMask, error) {
	if g.CPUType() == cpuNormal {
		isolated := g.IsolatedCPUs()
		exclusive := g.ExclusiveCPUs().Difference(isolated)
		sharedPortion := g.SharedPortion()
		if !cs.isolated.Intersection(isolated).Equals(isolated) {
			return nil, policyError("can't reserve isolated CPUs (%s) of %s from %s",
				isolated.String(), g.String(), cs.DumpAllocatable())
		}
		if !cs.sharable.Intersection(exclusive).Equals(exclusive) {
			return nil, policyError("can't reserve exclusive CPUs (%s) of %s from %s",
				exclusive.String(), g.String(), cs.DumpAllocatable())
		}
		if cs.AllocatableSharedCPU() < 1000*exclusive.Size()+sharedPortion {
			return nil, policyError("can't reserve %d shared CPUs of %s from %s",
				sharedPortion, g.String(), cs.DumpAllocatable())
		}
		cs.isolated = cs.isolated.Difference(isolated)
		cs.sharable = cs.sharable.Difference(exclusive)
		cs.grantedShared += sharedPortion
	} else if g.CPUType() == cpuReserved {
		sharedPortion := 1000*g.ExclusiveCPUs().Size() + g.SharedPortion()
		if sharedPortion > 0 && cs.AllocatableReservedCPU() < sharedPortion {
			return nil, policyError("can't reserve %d reserved CPUs of %s from %s",
				sharedPortion, g.String(), cs.DumpAllocatable())
		}
		cs.grantedReserved += sharedPortion
	}

	g.AccountAllocateCPU()

	zone, updates, err := o.Commit()
	if err != nil {
		g.Release()
		return nil, policyError("failed to commit offer: %v", err)
	}

	g.SetMemoryZone(zone)
	return updates, nil
}

// takeCPUs takes up to cnt CPUs from a given CPU set to another.
func (cs *supply) takeCPUs(from, to *cpuset.CPUSet, cnt int, prio cpuPrio) (cpuset.CPUSet, error) {
	cset, err := cs.node.Policy().cpuAllocator.AllocateCpus(from, cnt, prio.Option())
	if err != nil {
		return cset, err
	}

	if to != nil {
		*to = to.Union(cset)
	}

	return cset, err
}

// takeCPUsByHints tries to allocate isolated or exclusive CPUs by topology hints.
func (cs *supply) takeCPUsByHints(from *cpuset.CPUSet, all topology.Hints, cnt int, prio cpuPrio) (cpuset.CPUSet, error) {
	hints := []*topology.Hint{}
	for provider, h := range all {
		if podresapi.IsPodResourceHint(provider) {
			hints = append(hints, &h)
		}
	}
	if len(hints) == 0 || len(hints) > cnt {
		return cs.takeCPUs(from, nil, cnt, prio)
	}

	total := cnt
	perHint := 1
	if len(hints) < total && total%len(hints) == 0 {
		perHint = total / len(hints)
	}

	free := (*from).Clone()
	cpus := cpuset.New()
	for _, h := range hints {
		cset := free.Intersection(cpuset.MustParse(h.CPUs))
		pick, err := cs.takeCPUs(&cset, nil, perHint, prio)
		if err != nil {
			log.Errorf("failed to allocate CPUs by topology hints: %v", err)
			return cs.takeCPUs(from, nil, cnt, prio)
		}
		cpus = cpus.Union(pick)
		free = free.Difference(pick)
		total -= perHint
	}

	if total > 0 {
		pick, err := cs.takeCPUs(&free, nil, total, prio)
		if err != nil {
			log.Errorf("failed to allocate CPUs by topology hints: %v", err)
			return cs.takeCPUs(from, nil, cnt, prio)
		}
		cpus = cpus.Union(pick)
		free = free.Difference(pick)
	}

	*from = free
	return cpus, nil
}

// DumpCapacity returns a printable representation of the supply's resource capacity.
func (cs *supply) DumpCapacity() string {
	cpu, mem, sep := "", "", ""

	if !cs.isolated.IsEmpty() {
		cpu = fmt.Sprintf("isolated:%s", kubernetes.ShortCPUSet(cs.isolated))
		sep = ", "
	}
	if !cs.reserved.IsEmpty() {
		cpu += sep + fmt.Sprintf("reserved:%s (%dm)", kubernetes.ShortCPUSet(cs.reserved),
			1000*cs.reserved.Size())
		sep = ", "
	}
	if !cs.sharable.IsEmpty() {
		cpu += sep + fmt.Sprintf("sharable:%s (%dm)", kubernetes.ShortCPUSet(cs.sharable),
			1000*cs.sharable.Size())
	}

	if amount := cs.node.Policy().poolZoneCapacity(cs.node, memoryAll); amount > 0 {
		mem = prettyMem(amount)
	}

	capacity := "<" + cs.node.Name() + " capacity: "

	if cpu == "" && mem == "" {
		capacity += "-"
	} else {
		sep = ""
		if cpu != "" {
			capacity += "CPU: " + cpu
			sep = ", "
		}
		if mem != "" {
			capacity += sep + "MemLimit: " + mem
		}
	}
	capacity += ">"

	return capacity
}

// DumpAllocatable returns a printable representation of the supply's resource capacity.
func (cs *supply) DumpAllocatable() string {
	cpu, mem, sep := "", "", ""

	if !cs.isolated.IsEmpty() {
		cpu = fmt.Sprintf("isolated:%s", kubernetes.ShortCPUSet(cs.isolated))
		sep = ", "
	}
	if !cs.reserved.IsEmpty() {
		cpu += sep + fmt.Sprintf("reserved:%s (allocatable: %dm)", kubernetes.ShortCPUSet(cs.reserved), cs.AllocatableReservedCPU())
		sep = ", "
		if cs.grantedReserved > 0 {
			cpu += sep + fmt.Sprintf("grantedReserved:%dm", cs.grantedReserved)
		}
	}
	local_grantedShared := cs.grantedShared
	total_grantedShared := cs.node.GrantedSharedCPU()
	if !cs.sharable.IsEmpty() {
		cpu += sep + fmt.Sprintf("sharable:%s (", kubernetes.ShortCPUSet(cs.sharable))
		sep = ""
		if local_grantedShared > 0 || total_grantedShared > 0 {
			cpu += "grantedShared:"
			kind := ""
			if local_grantedShared > 0 {
				cpu += fmt.Sprintf("%dm", local_grantedShared)
				kind = "local"
				sep = "/"
			}
			if total_grantedShared > 0 {
				cpu += sep + fmt.Sprintf("%dm", total_grantedShared)
				kind += sep + "subtree"
			}
			cpu += " " + kind
			sep = ", "
		}
		cpu += sep + fmt.Sprintf("allocatable:%dm)", cs.AllocatableSharedCPU(true))

		sliceable, _ := cs.SliceableCPUs()
		cpu += fmt.Sprintf("/sliceable:%s (%dm)", kubernetes.ShortCPUSet(sliceable),
			1000*sliceable.Size())
	}

	allocatable := "<" + cs.node.Name() + " allocatable: "

	if amount := cs.node.Policy().poolZoneFree(cs.node, memoryAll); amount > 0 {
		mem = prettyMem(amount)
	}

	if cpu == "" && mem == "" {
		allocatable += "-"
	} else {
		sep = ""
		if cpu != "" {
			allocatable += "CPU: " + cpu
			sep = ", "
		}
		if mem != "" {
			allocatable += sep + "MemLimit: " + mem
		}
	}
	allocatable += ">"

	return allocatable
}

// prettyMem formats the given amount as k, M, G, or T units.
func prettyMem(value int64) string {
	units := []string{"k", "M", "G", "T"}
	coeffs := []int64{1 << 10, 1 << 20, 1 << 30, 1 << 40}

	c, u := int64(1), ""
	for i := range units {
		if coeffs[i] > value {
			break
		}
		c, u = coeffs[i], units[i]
	}
	v := float64(value) / float64(c)

	return strconv.FormatFloat(v, 'f', 2, 64) + u
}

// newRequest creates a new request for the given container.
func (p *policy) newRequest(container cache.Container, types libmem.TypeMask) (Request, error) {
	pod, _ := container.GetPod()
	full, fraction, cpuLimit, isolate, cpuType, prio := cpuAllocationPreferences(pod, container)
	req, lim, mtype := memoryAllocationPreference(pod, container)
	coldStart := time.Duration(0)

	cpuClass, isCtrScoped, err := p.resolveCpuClass(container)
	switch {
	case err != nil:
		return nil, fmt.Errorf("failed to resolve CPU class: %w", err)
	case full == 0 && isCtrScoped && cpuClass != "":
		return nil, fmt.Errorf("CPU class (%q) invalid without exclusive CPUs", cpuClass)
	}

	log.Debugf("%s: CPU preferences: cpuType=%s, cpuClass=%s, full=%v, fraction=%v, isolate=%v, prio=%v",
		container.PrettyName(), cpuType, cpuClass, full, fraction, isolate, prio)

	irqs, isCtrScoped, err := irqAffinityPreference(container)
	switch {
	case err != nil:
		return nil, err
	case irqs != nil && full == 0 && isCtrScoped:
		return nil, fmt.Errorf("IRQ affinity (%v) invalid without exclusive CPUs", irqs)
	case irqs != nil:
		log.Debugf("%s: IRQ affinity preference: %v", container.PrettyName(), irqs)
	}

	if mtype == memoryUnspec {
		mtype = defaultMemoryType &^ memoryHBM
	}

	if mtype != memoryPreserve {
		mtype = memoryType(mtype.TypeMask().And(types))

		if coldStartOff {
			if mtype == memoryPMEM {
				mtype |= memoryDRAM
				log.Errorf("%s: coldstart disabled (movable non-DRAM memory zones present)",
					container.PrettyName())
			}
		} else {
			pref, err := coldStartPreference(pod, container)
			if err != nil {
				return nil, fmt.Errorf("failed to parse coldstart preference: %w", err)
			}
			coldStart = time.Duration(pref.Duration.Duration)
			if coldStart > 0 {
				mtype &^= memoryDRAM
			}
		}
	}

	return &request{
		container:   container,
		full:        full,
		fraction:    fraction,
		limit:       cpuLimit,
		isolate:     isolate,
		cpuType:     cpuType,
		cpuClass:    cpuClass,
		memReq:      req,
		memLim:      lim,
		memType:     mtype,
		coldStart:   coldStart,
		prio:        prio,
		pickByHints: pickByHintsPreference(pod, container),
		irqs:        irqs,
	}, nil
}

// GetContainer returns the container requesting CPU.
func (cr *request) GetContainer() cache.Container {
	return cr.container
}

// String returns aprintable representation of the CPU request.
func (cr *request) String() string {
	mem := fmt.Sprintf("<Memory request: limit: %s, req: %s>",
		prettyMem(cr.memLim), prettyMem(cr.memReq))
	isolated := map[bool]string{false: "", true: "isolated "}[cr.isolate]
	switch {
	case cr.full == 0 && cr.fraction == 0:
		return fmt.Sprintf("<%s CPU request %s: none> %s", cr.prio, cr.container.PrettyName(), mem)

	case cr.full > 0 && cr.fraction > 0:
		return fmt.Sprintf("<%s CPU request "+cr.container.PrettyName()+": "+
			"%sexclusive: %d, shared: %d>", cr.prio, isolated, cr.full, cr.fraction) + mem

	case cr.full > 0:
		return fmt.Sprintf("<%s CPU request %s: %sexclusive: %d> %s",
			cr.prio, cr.container.PrettyName(), isolated, cr.full, mem)

	default:
		return fmt.Sprintf("<%s CPU request %s: shared %d> %s",
			cr.prio, cr.container.PrettyName(), cr.fraction, mem)
	}
}

// CPUType returns the requested type of CPU for the grant.
func (cr *request) CPUType() cpuType {
	return cr.cpuType
}

func (cr *request) CPUClass() string {
	return cr.cpuClass
}

func (cr *request) CPUPrio() cpuPrio {
	return cr.prio
}

// SetCPUType sets the requested type of CPU for the grant.
func (cr *request) SetCPUType(cpuType cpuType) {
	cr.cpuType = cpuType
}

// FullCPUs return the number of full CPUs requested.
func (cr *request) FullCPUs() int {
	return cr.full
}

// CPUFraction returns the amount of fractional milli-CPU requested.
func (cr *request) CPUFraction() int {
	return cr.fraction
}

// CPULimit returns the amount of fractional milli-CPU limit.
func (cr *request) CPULimit() int {
	return cr.limit
}

// Isolate returns whether isolated CPUs are preferred for this request.
func (cr *request) Isolate() bool {
	return cr.isolate
}

// MemAmountToAllocate retuns how much memory we need to reserve for a request.
func (cr *request) MemAmountToAllocate() int64 {
	if cr.memReq != 0 {
		return cr.memReq
	}
	return cr.memLim
}

func (cr *request) MemoryLimit() int64 {
	return cr.memLim
}

// MemoryType returns the requested type of memory for the grant.
func (cr *request) MemoryType() memoryType {
	return cr.memType
}

func (cr *request) PickByHints() bool {
	return cr.pickByHints
}

// IrqAffinity returns the IRQ affinity for the request.
func (cr *request) IrqAffinity() *IrqAffinity {
	return cr.irqs
}

// ColdStart returns the cold start timeout (in milliseconds).
func (cr *request) ColdStart() time.Duration {
	return cr.coldStart
}

func (cr *request) verifyStrictPreferences(g Grant) error {
	if err := cr.verifyStrictTopologyHints(g); err != nil {
		return err
	}
	if err := cr.verifyStrictCPUPreferences(g); err != nil {
		return err
	}
	return nil
}

func (cr *request) verifyStrictTopologyHints(g Grant) error {
	if !cr.GetContainer().StrictTopologyHints() {
		return nil
	}

	for _, h := range cr.GetContainer().GetTopologyHints() {
		hint := topoutil.NewHint(g.GetCPUNode().System(), h)

		if g.SharedPortion() > 0 {
			if cpus := hint.MisalignedCPUSet(g.SharedCPUs()); cpus.Size() > 0 {
				return policyError("granted shared CPUs %q fail strict hint %v",
					cpus.String(), h)
			}
		}

		if g.ReservedPortion() > 0 {
			if cpus := hint.MisalignedCPUSet(g.ReservedCPUs()); cpus.Size() > 0 {
				return policyError("granted reserved CPUs %q fail strict hint %v",
					cpus.String(), h)
			}
		}

		if excl := g.ExclusiveCPUs(); excl.Size() > 0 {
			if cpus := hint.MisalignedCPUSet(excl); cpus.Size() > 0 {
				return policyError("granted exclusive CPUs %q fail strict hint %v",
					cpus.String(), h)
			}
		}

		if mems := hint.MisalignedMems(g.GetMemoryZone()); mems.Size() > 0 {
			return policyError("granted memory zones %s fail strict hint %v",
				mems.String(), h)
		}
	}

	return nil
}

func (cr *request) verifyStrictCPUPreferences(g Grant) error {
	full := cr.FullCPUs()

	if full < 1 || !cr.Isolate() {
		return nil
	}

	ctr := cr.GetContainer()
	pod, _ := ctr.GetPod()
	if !strictIsolatedCPUsPreference(pod, ctr) {
		return nil
	}

	if cpus := g.IsolatedCPUs(); cpus.Size() < full {
		return policyError("granted isolated CPUs %q less than requested %d",
			cpus.String(), full)
	}

	return nil
}

// Score collects data for scoring this supply wrt. the given request.
func (cs *supply) GetScore(req Request) Score {
	score := &score{
		supply: cs,
		req:    req,
		prio:   map[cpuPrio]int{},
	}

	cr := req.(*request)
	full, part := cr.full, cr.fraction
	if full == 0 && part == 0 {
		part = 1
	}

	score.reserved = cs.AllocatableReservedCPU()
	score.shared = cs.AllocatableSharedCPU()

	if cr.CPUType() == cpuReserved {
		// calculate free reserved capacity
		score.reserved -= part
	} else {
		p := cs.GetNode().Policy()

		// calculate isolated node capacity CPU
		if cr.isolate {
			score.isolated = cs.isolated.Size() - full
		}

		// if we don't want isolated or there is not enough, calculate sliceable capacity
		if !cr.isolate || score.isolated < 0 {
			score.shared -= 1000 * full
		}

		// calculate fractional capacity
		score.shared -= part

		lpCPUs := cs.GetNode().System().CoreKindCPUs(sysfs.EfficientCore)
		if lpCPUs.Size() == 0 {
			lpCPUs = p.cpuAllocator.GetCPUPriorities()[lowPrio]
		}
		lpCPUs = lpCPUs.Intersection(cs.SharableCPUs())
		lpCnt := lpCPUs.Size()
		score.prio[lowPrio] = lpCnt*1000 - (1000*full + part)

		hpCPUs := cs.GetNode().System().CoreKindCPUs(sysfs.PerformanceCore)
		if hpCPUs.Size() == 0 {
			hpCPUs = p.cpuAllocator.GetCPUPriorities()[highPrio]
		}
		hpCPUs = hpCPUs.Intersection(cs.SharableCPUs())
		hpCnt := hpCPUs.Size()
		score.prio[highPrio] = hpCnt*1000 - (1000*full + part)

		npCPUs := p.cpuAllocator.GetCPUPriorities()[normalPrio]
		npCPUs = npCPUs.Intersection(cs.SharableCPUs())
		npCnt := npCPUs.Size()
		score.prio[normalPrio] = npCnt*1000 - (1000*full + part)

		if cr.full > 0 && cr.cpuClass != "" && p.cpuClasses != nil {
			cpus, err := cs.GetCPUOffer(req)
			if err != nil {
				log.Errorf("%s: ignoring CPU class (%q), failed to get CPU offer: %v",
					req.GetContainer().PrettyName(), cr.cpuClass, err)
			} else {
				score.cpu = cpus
				hints := p.cpuClasses.Hints(cpuclass.AllocationIntent{
					ClassName:      cr.cpuClass,
					CurrentCpus:    cpuset.New(),
					FreeCpus:       cpus,
					RequestedCount: cr.full,
				})
				score.ccHints = &hints
			}
		}
	}

	// calculate colocation score
	for _, grant := range cs.node.Policy().allocations.grants {
		if cr.CPUType() == grant.CPUType() && grant.GetCPUNode().NodeID() == cs.node.NodeID() {
			score.colocated++
		}
	}

	// calculate real hint scores
	hints := cr.container.GetTopologyHints()
	hints.ResolvePartialHints(cs.GetNode().System().NodeHintToCPUs)
	score.hints = make(map[string]float64, len(hints))

	for provider, hint := range cr.container.GetTopologyHints() {
		log.Debugf(" - evaluating topology hint %s", hint)
		score.hints[provider] = cs.node.HintScore(hint)
	}

	node := cs.node
	if req.MemoryType() == memoryPreserve {
		node = cs.node.Policy().root
	}

	var (
		o   *libmem.Offer
		err error
	)

	if cr.PickByHints() {
		o, err = node.Policy().getMemOfferByHints(node, cr)
		if err != nil {
			log.Errorf("failed to get offer by hints: %v", err)
			o, err = node.Policy().getMemOffer(node, cr)
		}
	} else {
		o, err = node.Policy().getMemOffer(node, cr)
	}

	if err != nil {
		log.Errorf("failed to get offer for request %s: %v", req, err)
	} else {
		log.Debugf("got node %s offer for request %s: %s", node.Name(), req, o.NodeMask())
		score.mem = o
	}

	return score
}

// AllocatableReservedCPU calculates the allocatable amount of reserved CPU of this supply.
func (cs *supply) AllocatableReservedCPU() int {
	if cs.reserved.Size() == 0 {
		// This supply has no room for reserved (not even of zero-sized)
		return -1
	}
	reserved := 1000*cs.reserved.Size() - cs.node.GrantedReservedCPU()
	for node := cs.node.Parent(); !node.IsNil(); node = node.Parent() {
		pSupply := node.FreeSupply()
		pReserved := 1000*pSupply.ReservedCPUs().Size() - pSupply.GetNode().GrantedReservedCPU()
		if pReserved < reserved {
			reserved = pReserved
		}
	}
	return reserved
}

// AllocatableSharedCPU calculates the allocatable amount of shared CPU of this supply.
func (cs *supply) AllocatableSharedCPU(quiet ...bool) int {
	verbose := len(quiet) == 0 || !quiet[0]

	// Notes:
	//   Take into account the supplies/grants in all ancestors, making sure
	//   none of them gets overcommitted as the result of fulfilling this request.
	shared := 1000*cs.sharable.Size() - cs.node.GrantedSharedCPU()
	if verbose {
		log.Debugf("%s: unadjusted free shared CPU: %dm", cs.node.Name(), shared)
	}
	for node := cs.node.Parent(); !node.IsNil(); node = node.Parent() {
		pSupply := node.FreeSupply()
		pShared := 1000*pSupply.SharableCPUs().Size() - pSupply.GetNode().GrantedSharedCPU()
		if pShared < shared {
			if verbose {
				log.Debugf("%s: capping free shared CPU (%dm -> %dm) to avoid overcommit of %s",
					cs.node.Name(), shared, pShared, node.Name())
			}
			shared = pShared
		}
	}

	if verbose {
		log.Debugf("%s: ancestor-adjusted free shared CPU: %dm", cs.node.Name(), shared)
	}

	// If there are BestEffort or 0 CPU request Burstable containers in the node
	// or any of its children we need to account for them an extra milliCPU worth
	// of shared capacity.
	//
	// TODO(klihub): We might need to try speeding this up if it gets too slow.
	// Obvious optimization would be to 1) store grants per assigned node.
	hasZeroCpuReqs := false
	cs.node.BreadthFirst(func(n Node) bool {
		if cs.node.Policy().hasZeroCpuReqContainer(n) {
			hasZeroCpuReqs = true
		}
		return hasZeroCpuReqs
	})
	if hasZeroCpuReqs {
		shared--
		if verbose {
			log.Debugf("%s: 0 CPU req-adjusted free shared CPU: %dm",
				cs.node.Name(), shared)
		}
	}

	return shared
}

// SliceableCPUs calculates the shared cpuset we can slice exclusive CPUs off of.
func (cs *supply) SliceableCPUs() (cpuset.CPUSet, error) {
	var (
		sliceable = cpuset.New()
		errs      []error
	)

	// We need to avoid slicing any shared pool in child nodes below its
	// current allocation. To do so we go through the subtree collecting
	// CPUs from shared pools for allocation but leaving off enough full
	// CPUs in each for their current BestEffort and Burstable QoS class
	// shared allocations.

	cs.node.DepthFirst(func(n Node) (done bool) {
		if n.IsSameNode(cs.node) && !n.IsLeafNode() {
			return
		}

		ns := n.FreeSupply()
		if ns == nil {
			return false
		}

		cpus := ns.SharableCPUs()
		free := ns.AllocatableSharedCPU(true) / 1000

		// TODO(klihub): We should ideally take also into account the CPU
		// priority preference of any ongoing allocation, trying to slice
		// CPUs with a matching preference. We don't do that ATM.

		cset, err := cs.takeCPUs(&cpus, nil, free, nonePrio)
		if err != nil {
			errs = append(errs, err)
			return false
		}

		sliceable = sliceable.Union(cset)
		return false
	})

	if len(errs) > 0 {
		return cpuset.New(), errors.Join(errs...)
	}

	return sliceable, nil
}

// Eval...
func (score *score) Eval() float64 {
	return 1.0
}

func (score *score) Supply() Supply {
	return score.supply
}

func (score *score) Request() Request {
	return score.req
}

func (score *score) IsolatedCapacity() int {
	return score.isolated
}

func (score *score) ReservedCapacity() int {
	return score.reserved
}

func (score *score) SharedCapacity() int {
	return score.shared
}

func (score *score) Colocated() int {
	return score.colocated
}

func (score *score) HintScores() map[string]float64 {
	return score.hints
}

func (score *score) PrioCapacity(prio cpuPrio) int {
	return score.prio[prio]
}

func (score *score) CpuClassHints() *cpuclass.AllocationHints {
	return score.ccHints
}

func (score *score) CpuClassCpus() cpuset.CPUSet {
	return score.ccCpus
}

func (score *score) MemOffer() *libmem.Offer {
	return score.mem
}

func (score *score) CPUOffer() cpuset.CPUSet {
	return score.cpu
}

func (score *score) String() string {
	return fmt.Sprintf("<CPU score: node %s, isolated:%d, reserved:%d, shared:%d, colocated:%d, hints: %v>",
		score.supply.GetNode().Name(), score.isolated, score.reserved, score.shared, score.colocated, score.hints)
}

// newGrant creates a CPU grant from the given node for the container.
func newGrant(n Node, c cache.Container, cpuType cpuType, cpuCls string, exclusive cpuset.CPUSet, cpuPortion int, mt memoryType, irqs *IrqAffinity, coldstart time.Duration) Grant {
	grant := &grant{
		node:       n,
		container:  c,
		cpuType:    cpuType,
		cpuClass:   cpuCls,
		exclusive:  exclusive,
		cpuPortion: cpuPortion,
		memType:    mt,
		irqs:       irqs,
		coldStart:  coldstart,
	}
	return grant
}

// SetCPUPortion sets the fractional CPU portion for the grant.
func (cg *grant) SetCPUPortion(fraction int) {
	cg.cpuPortion = fraction
}

// SetMemoryType sets the memory type for the grant.
func (cg *grant) SetMemoryType(memType memoryType) {
	cg.memType = memType
}

// SetMemoryZone sets the memory zone for the grant.
func (cg *grant) SetMemoryZone(zone libmem.NodeMask) {
	cg.memZone = zone
}

// SetMemorySize sets the amount of memory to allocate.
func (cg *grant) SetMemorySize(size int64) {
	cg.memSize = size
}

// IrqAffinity returns the IRQ affinity for this grant.
func (cg *grant) IrqAffinity() *IrqAffinity {
	return cg.irqs
}

// SetColdstart sets coldstart period for the grant.
func (cg *grant) SetColdstart(period time.Duration) {
	cg.coldStart = period
}

// Clone creates a copy of this grant.
func (cg *grant) Clone() Grant {
	return &grant{
		node:       cg.GetCPUNode(),
		container:  cg.GetContainer(),
		exclusive:  cg.ExclusiveCPUs(),
		cpuType:    cg.CPUType(),
		cpuClass:   cg.CPUClass(),
		cpuPortion: cg.SharedPortion(),
		memType:    cg.MemoryType(),
		memZone:    cg.GetMemoryZone(),
		memSize:    cg.GetMemorySize(),
		coldStart:  cg.ColdStart(),
	}
}

// RefetchNodes updates the stored cpu and memory nodes of this grant by name.
func (cg *grant) RefetchNodes() error {
	node, ok := cg.node.Policy().nodes[cg.node.Name()]
	if !ok {
		return policyError("failed to refetch grant cpu node %s", cg.node.Name())
	}
	cg.node = node
	return nil
}

// GetContainer returns the container this grant is valid for.
func (cg *grant) GetContainer() cache.Container {
	return cg.container
}

// GetNode returns the Node this grant gets its CPU allocation from.
func (cg *grant) GetCPUNode() Node {
	return cg.node
}

// GetMemorySize returns the amount of memory allocated to this grant.
func (cg *grant) GetMemorySize() int64 {
	return cg.memSize
}

// GetMemoryZone returns the memory zone this grant is allocated to.
func (cg *grant) GetMemoryZone() libmem.NodeMask {
	return cg.memZone
}

// CPUType returns the requested type of CPU for the grant.
func (cg *grant) CPUType() cpuType {
	return cg.cpuType
}

// CPUClass returns the CPU class applicable to exclusive CPUs.
func (cg *grant) CPUClass() string {
	return cg.cpuClass
}

// SetCPUClass sets the CPU class applicable to exclusive CPUs.
func (cg *grant) SetCPUClass(class string) {
	cg.cpuClass = class
}

// CPUPortion returns granted milli-CPUs of non-full CPUs of CPUType().
func (cg *grant) CPUPortion() int {
	return cg.cpuPortion
}

// ExclusiveCPUs returns the non-isolated exclusive CPUSet in this grant.
func (cg *grant) ExclusiveCPUs() cpuset.CPUSet {
	return cg.exclusive
}

// ReservedCPUs returns the reserved CPUSet in the supply of this grant.
func (cg *grant) ReservedCPUs() cpuset.CPUSet {
	return cg.node.GetSupply().ReservedCPUs()
}

// ReservedPortion returns the milli-CPU allocation for the reserved CPUSet in this grant.
func (cg *grant) ReservedPortion() int {
	if cg.cpuType == cpuReserved {
		return cg.cpuPortion
	}
	return 0
}

// SharedCPUs returns the shared CPUSet in the supply of this grant.
func (cg *grant) SharedCPUs() cpuset.CPUSet {
	return cg.node.FreeSupply().SharableCPUs()
}

// SharedPortion returns the milli-CPU allocation for the shared CPUSet in this grant.
func (cg *grant) SharedPortion() int {
	if cg.cpuType == cpuNormal {
		return cg.cpuPortion
	}
	return 0
}

// ExclusiveCPUs returns the isolated exclusive CPUSet in this grant.
func (cg *grant) IsolatedCPUs() cpuset.CPUSet {
	return cg.node.GetSupply().IsolatedCPUs().Intersection(cg.exclusive)
}

// MemoryType returns the requested type of memory for the grant.
func (cg *grant) MemoryType() memoryType {
	return cg.memType
}

// String returns a printable representation of the CPU grant.
func (cg *grant) String() string {
	var cpuType, cpuClass, isolated, exclusive, reserved, shared string
	cpuType = fmt.Sprintf("cputype: %s", cg.cpuType)
	cpuClass = fmt.Sprintf(", cpuclass: %q", cg.cpuClass)
	isol := cg.IsolatedCPUs()
	if !isol.IsEmpty() {
		isolated = fmt.Sprintf(", isolated: %s", isol)
	}
	if !cg.exclusive.IsEmpty() {
		exclusive = fmt.Sprintf(", exclusive: %s", cg.exclusive)
	}
	if cg.ReservedPortion() > 0 || (cg.cpuType == cpuReserved && cg.SharedPortion() == 0) {
		reserved = fmt.Sprintf(", reserved: %s (%dm)",
			cg.node.FreeSupply().ReservedCPUs(), cg.ReservedPortion())
	} else {
		if cg.SharedPortion() > 0 || (isol.IsEmpty() && cg.exclusive.IsEmpty()) {
			shared = fmt.Sprintf(", shared: %s (%dm)",
				cg.node.FreeSupply().SharableCPUs(), cg.SharedPortion())
		}
	}

	mem := fmt.Sprintf(", memory: %s (%s)", cg.memZone, prettyMem(cg.memSize))

	return fmt.Sprintf("<grant for %s from %s: %s%s%s%s%s%s%s>",
		cg.container.PrettyName(), cg.node.Name(), cpuType, cpuClass, isolated, exclusive, reserved, shared, mem)
}

func (cg *grant) AccountAllocateCPU() {
	cg.node.DepthFirst(func(n Node) (done bool) {
		n.FreeSupply().AccountAllocateCPU(cg)
		return false
	})
	for node := cg.node.Parent(); !node.IsNil(); node = node.Parent() {
		node.FreeSupply().AccountAllocateCPU(cg)
	}
}

func (cg *grant) Release() {
	cg.GetCPUNode().FreeSupply().ReleaseCPU(cg)
	err := cg.node.Policy().releaseMem(cg.container.GetID())
	if err != nil {
		log.Errorf("releasing memory for %s failed: %v", cg.container.PrettyName(), err)
	}
	cg.StopTimer()
}

func (cg *grant) ReallocMemory(types libmem.TypeMask) error {
	zone, updates, err := cg.node.Policy().reallocMem(cg.container.GetID(), 0, types)
	if err != nil {
		return err
	}

	cg.SetMemoryZone(zone)
	if opt.PinMemory {
		cg.container.SetCpusetMems(zone.MemsetString())
	}

	for id, z := range updates {
		g, ok := cg.node.Policy().allocations.getGrant(id)
		if !ok {
			log.Errorf("offer commit returned zone update %s for unknown container %s", z, id)
		} else {
			log.Infof("updating memory allocation for %s to %s", g.GetContainer().PrettyName(), z)
			g.SetMemoryZone(z)
			if opt.PinMemory {
				g.GetContainer().SetCpusetMems(z.MemsetString())
			}
		}
	}

	return nil
}

func (cg *grant) AccountReleaseCPU() {
	cg.node.DepthFirst(func(n Node) (done bool) {
		n.FreeSupply().AccountReleaseCPU(cg)
		return false
	})
	for node := cg.node.Parent(); !node.IsNil(); node = node.Parent() {
		node.FreeSupply().AccountReleaseCPU(cg)
	}
}

func (cg *grant) ColdStart() time.Duration {
	return cg.coldStart
}

func (cg *grant) AddTimer(timer *time.Timer) {
	cg.coldStartTimer = timer
}

func (cg *grant) StopTimer() {
	if cg.coldStartTimer != nil {
		cg.coldStartTimer.Stop()
		cg.coldStartTimer = nil
	}
}

func (cg *grant) ClearTimer() {
	if cg.coldStartTimer != nil {
		cg.coldStartTimer = nil
	}
}
