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
	"fmt"
	"math"
	"sort"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	corev1 "k8s.io/api/core/v1"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// buildPoolsByTopology builds a hierarchical tree of pools based on HW topology.
func (p *policy) buildPoolsByTopology() error {
	if err := p.checkHWTopology(); err != nil {
		return err
	}

	// We build a tree of pools by examining sockets, dies, and NUMA nodes.
	// If we have multiple sockets, we create an extra virtual root for the
	// whole system, otherwise we use the socket as the root of the tree.
	// Under sockets, we create die nodes, and under dies, we create NUMA
	// nodes. We omit a die pool if is is the only die in a socket. We omit
	// a NUMA node if it is the only child pool under its parent.
	//
	// We discover pool resources by first assigning the closest CPUs to
	// the pool, then the set of NUMA nodes closest to any of the CPUs in
	// the pool. Then those NUMA nodes without CPU locality get assigned
	// to the pool for which any of the closest neighbor with CPU locality
	// is already assigned to the pool. For the root node, we also assign
	// the system-wide memory resources without CPU locality. Finally, any
	// NUMA node otherwise unassigned is assigned to the root pool (which
	// is done by simply assigning all NUMA nodes without CPU locality to
	// the root pool).

	log.Info("building pools by HW topology...")

	p.nodes = make(map[string]Node)
	p.pools = make([]Node, 0)

	p.buildRootPool()
	p.enumeratePools()

	p.root.Dump("<pool-setup>")

	return nil
}

func (p *policy) enumeratePools() {
	log.Info("enumerating pools...")

	id := 0
	p.root.DepthFirst(func(n Node) {
		n.(*node).id = id
		id++
		log.Info("  #%d: pool %s (depth %d)", n.NodeID(), n.Name(), n.RootDistance())
		if p.depth < n.RootDistance() {
			p.depth = n.RootDistance()
		}
		p.pools = append(p.pools, n)
	})
}

func (p *policy) buildRootPool() {
	var (
		root  = nilnode
		vroot *virtualnode
	)

	if p.sys.SocketCount() > 1 {
		vroot = p.NewVirtualNode("root", nilnode)
		p.nodes[vroot.Name()] = vroot

		p.root = vroot
		root = vroot

		log.Info("+ created pool %s", vroot.Name())

		cpus := p.sys.CPUSet()
		vroot.noderes, vroot.freeres = p.getCpuSupply(vroot, cpus)
		vroot.mem, vroot.pMem, vroot.hbm = p.getMemSupply(vroot, cpus)
	} else {
		log.Info("- omitted virtual root pool (single socket HW)")
	}

	for _, socketID := range p.sys.PackageIDs() {
		p.buildSocketPool(socketID, root)
	}
}

func (p *policy) buildSocketPool(socketID idset.ID, root Node) {
	socket := p.NewSocketNode(socketID, root)
	p.nodes[socket.Name()] = socket
	socket.depth = socket.RootDistance()

	if p.root == nil {
		p.root = socket
	}

	log.Info("+ created pool %s", socket.Name())

	cpus := p.sys.Package(socketID).CPUSet()
	socket.noderes, socket.freeres = p.getCpuSupply(socket, cpus)
	socket.mem, socket.pMem, socket.hbm = p.getMemSupply(socket, cpus)

	if dieIDs := p.sys.Package(socketID).DieIDs(); len(dieIDs) > 1 {
		for _, dieID := range dieIDs {
			p.buildDiePool(socketID, dieID, socket)
		}
	} else {
		if nodeIDs := p.sys.Package(socketID).NodeIDs(); len(nodeIDs) > 1 {
			for _, nodeID := range nodeIDs {
				p.buildNumaNodePool(socketID, nodeID, socket)
			}
		}
	}
}

func (p *policy) buildDiePool(socketID, dieID idset.ID, socket Node) {
	die := p.NewDieNode(dieID, socket)
	p.nodes[die.Name()] = die
	die.depth = die.RootDistance()

	log.Info("+ created pool %s", die.Name())

	cpus := p.sys.Package(socketID).DieCPUSet(dieID)
	die.noderes, die.freeres = p.getCpuSupply(die, cpus)
	die.mem, die.pMem, die.hbm = p.getMemSupply(die, cpus)

	if nodeIDs := p.sys.Package(socketID).DieNodeIDs(dieID); len(nodeIDs) > 1 {
		for _, nodeID := range nodeIDs {
			p.buildNumaNodePool(socketID, nodeID, die)
		}
	}
}

func (p *policy) buildNumaNodePool(socketID, nodeID idset.ID, parent Node) {
	if mi, _ := p.sys.Node(nodeID).MemoryInfo(); mi != nil && mi.MemTotal == 0 {
		// Notes:
		//   We only get called for NUMA nodes with some CPU locality. Then
		//   if we have no attached memory, we have here a bunch of CPUs for
		//   which we can't allocate memory with the lowest possible latency.
		//   Hence we omit this node, effectively folding its CPUs to parent,
		//   making the CPUs less appealing for allocation than a sibling NUMA
		//   node with some memory attached (and some CPU locality).
		log.Info("- omitted pool %s%d (no memory attached)", "NUMA node #", nodeID)
		return
	}

	node := p.NewNumaNode(nodeID, parent)
	p.nodes[node.Name()] = node
	node.depth = node.RootDistance()

	log.Info("+ created pool %s", node.Name())

	cpus := p.sys.Node(nodeID).CPUSet()
	node.noderes, node.freeres = p.getCpuSupply(node, cpus)
	node.mem, node.pMem, node.hbm = p.getMemSupply(node, cpus)
}

func (p *policy) getCpuSupply(node Node, cpus cpuset.CPUSet) (Supply, Supply) {
	var (
		allowed  = cpus.Intersection(p.allowed)
		isolated = allowed.Intersection(p.isolated)
		reserved = allowed.Intersection(p.reserved)
		sharable = allowed.Difference(isolated).Difference(reserved)
	)

	s := newSupply(node, isolated, reserved, sharable, 0, 0)

	log.Info("    %s CPU: %s", node.Name(), s.DumpCapacity())

	return s, s.Clone()
}

func (p *policy) getMemSupply(node Node, cpus cpuset.CPUSet) (dram, pmem, hbm idset.IDSet) {
	if p.root == node {
		dram, pmem, hbm = p.splitMemsByType(p.getAllMems())
		if dram.Size() > 0 {
			log.Info("    %s all system DRAM: %s", node.Name(), dram)
		}
		if pmem.Size() > 0 {
			log.Info("    %s all system PMEM: %s", node.Name(), pmem)
		}
		if hbm.Size() > 0 {
			log.Info("    %s all system HBM: %s", node.Name(), hbm)
		}
	} else {
		mems := p.getMemsForCpus(cpus)
		dram, pmem, hbm = p.splitMemsByType(mems)

		if dram.Size() > 0 {
			log.Info("    %s DRAM by CPU locality: %s", node.Name(), dram)
		}
		if pmem.Size() > 0 {
			log.Info("    %s PMEM by CPU locality: %s", node.Name(), pmem)
		}
		if hbm.Size() > 0 {
			log.Info("    %s HBM by CPU locality: %s", node.Name(), hbm)
		}

		dm, pm, hm := p.splitMemsByType(p.getClosestSpecialMem(mems))
		if dm.Size() > 0 {
			log.Info("    %s closest DRAM without CPU locality: %s", node.Name(), dm)
		}
		if pm.Size() > 0 {
			log.Info("    %s closest PMEM without CPU locality: %s", node.Name(), pm)
		}
		if hm.Size() > 0 {
			log.Info("    %s closest HBM without CPU locality: %s", node.Name(), hm)
		}
		dram.Add(dm.Members()...)
		pmem.Add(pm.Members()...)
		hbm.Add(hm.Members()...)
	}

	if dram.Size() > 0 {
		log.Info("    %s DRAM: %s", node.Name(), dram.String())
	}
	if pmem.Size() > 0 {
		log.Info("    %s PMEM: %s", node.Name(), pmem.String())
	}
	if hbm.Size() > 0 {
		log.Info("    %s HBM: %s", node.Name(), hbm.String())
	}
	return dram, pmem, hbm
}

func (p *policy) getMemsForCpus(cpus cpuset.CPUSet) idset.IDSet {
	mems := idset.NewIDSet()

	for _, nodeID := range p.sys.NodeIDs() {
		node := p.sys.Node(nodeID)
		if !node.CPUSet().Intersection(cpus).IsEmpty() {
			mems.Add(nodeID)
		}
	}

	return mems
}

func (p *policy) getClosestSpecialMem(mems idset.IDSet) idset.IDSet {
	var (
		special = idset.NewIDSet()
		nodeIDs = p.sys.NodeIDs()

		pmemNoCPU = []system.NodeFilter{
			system.NodeOfPMEMType,
			system.NodeHasMemory,
			system.NodeHasNoLocalCPUs,
		}

		hbmNoCPU = []system.NodeFilter{
			system.NodeOfHBMType,
			system.NodeHasMemory,
			system.NodeHasNoLocalCPUs,
		}
	)

	for _, id := range p.sys.FilterNodes(nodeIDs, pmemNoCPU...).Members() {
		closest, _ := p.sys.ClosestNodes(id, system.NodeOfDRAMType, system.NodeHasLocalCPUs)
		if len(closest) > 0 {
			for _, cid := range closest[0].Members() {
				if mems.Has(cid) {
					special.Add(id)
				}
			}
		}
	}

	for _, id := range p.sys.FilterNodes(nodeIDs, hbmNoCPU...).Members() {
		closest, _ := p.sys.ClosestNodes(id, system.NodeOfDRAMType, system.NodeHasLocalCPUs)
		if len(closest) > 0 {
			for _, cid := range closest[0].Members() {
				if mems.Has(cid) {
					special.Add(id)
				}
			}
		}
	}

	return special
}

func (p *policy) getAllMems() idset.IDSet {
	return p.sys.FilterNodes(p.sys.NodeIDs(), system.NodeHasMemory)
}

func (p *policy) splitMemsByType(ids idset.IDSet) (dram, pmem, hbm idset.IDSet) {
	dram, pmem, hbm = idset.NewIDSet(), idset.NewIDSet(), idset.NewIDSet()

	for _, id := range ids.Members() {
		node := p.sys.Node(id)
		switch node.GetMemoryType() {
		case system.MemoryTypeDRAM:
			dram.Add(id)
		case system.MemoryTypePMEM:
			pmem.Add(id)
		case system.MemoryTypeHBM:
			hbm.Add(id)
		}
	}

	return dram, pmem, hbm
}

// checkHWTopology verifies our otherwise implicit assumptions about the HW.
func (p *policy) checkHWTopology() error {
	// NUMA distance matrix should be symmetric.
	for _, from := range p.sys.NodeIDs() {
		for _, to := range p.sys.NodeIDs() {
			d1 := p.sys.NodeDistance(from, to)
			d2 := p.sys.NodeDistance(to, from)
			if d1 != d2 {
				log.Error("asymmetric NUMA distance (#%d, #%d): %d != %d",
					from, to, d1, d2)
				return policyError("asymmetric NUMA distance (#%d, #%d): %d != %d",
					from, to, d1, d2)
			}
		}
	}

	return nil
}

// Pick a pool and allocate resource from it to the container.
func (p *policy) allocatePool(container cache.Container, poolHint string) (Grant, error) {
	var (
		pool  Node
		offer *libmem.Offer
	)

	request := newRequest(container, p.memAllocator.Masks().AvailableTypes())

	if p.root.FreeSupply().ReservedCPUs().IsEmpty() && request.CPUType() == cpuReserved {
		// Fallback to allocating reserved CPUs from the shared pool
		// if there are no reserved CPUs.
		request.SetCPUType(cpuNormal)
	}

	if request.CPUType() == cpuReserved || request.CPUType() == cpuPreserve {
		pool = p.root
		o, err := p.getMemOffer(pool, request)
		if err != nil {
			return nil, policyError("failed to get offer for request %s: %v", request, err)
		}
		offer = o
	} else {
		affinity, err := p.calculatePoolAffinities(request.GetContainer())

		if err != nil {
			return nil, policyError("failed to calculate affinity for container %s: %v",
				container.PrettyName(), err)
		}

		scores, pools := p.sortPoolsByScore(request, affinity)

		if log.DebugEnabled() {
			log.Debug("* node fitting for %s", request)
			for idx, n := range pools {
				log.Debug("    - #%d: node %s, score %s, affinity: %d",
					idx, n.Name(), scores[n.NodeID()], affinity[n.NodeID()])
			}
		}

		if len(pools) == 0 {
			return nil, policyError("no suitable pool found for container %s",
				container.PrettyName())
		}

		if poolHint != "" {
			for idx, p := range pools {
				if p.Name() == poolHint {
					log.Debug("* using hinted pool %q (#%d best fit)", poolHint, idx+1)
					pool = p
					break
				}
			}
			if pool == nil {
				log.Debug("* cannot use hinted pool %q", poolHint)
			}
		}

		if pool == nil {
			pool = pools[0]
		}

		offer = scores[pool.NodeID()].Offer()
		if offer == nil {
			return nil, policyError("failed to get offer for request %s", request)
		}
	}

	supply := pool.FreeSupply()
	grant, updates, err := supply.Allocate(request, offer)
	if err != nil {
		return nil, policyError("failed to allocate %s from %s: %v",
			request, supply.DumpAllocatable(), err)
	}

	for id, z := range updates {
		g, ok := p.allocations.grants[id]
		if !ok {
			log.Error("offer commit returned zone update %s for unknown container %s", z, id)
		} else {
			log.Info("updating memory allocation for %s to %s", g.GetContainer().PrettyName(), z)
			g.SetMemoryZone(z)
			if opt.PinMemory {
				g.GetContainer().SetCpusetMems(z.MemsetString())
			}
		}
	}

	log.Debug("allocated req '%s' to memory zone %s", container.PrettyName(),
		grant.GetMemoryZone())

	p.allocations.grants[container.GetID()] = grant

	p.saveAllocations()

	return grant, nil
}

// setPreferredCpusetCpus pins container's CPUs according to what has been
// allocated for it, taking into account if the container should run
// with hyperthreads hidden.
func (p *policy) setPreferredCpusetCpus(container cache.Container, allocated cpuset.CPUSet, info string) {
	allow := allocated
	hidingInfo := ""
	pod, ok := container.GetPod()
	if ok && hideHyperthreadsPreference(pod, container) {
		allow = p.sys.SingleThreadForCPUs(allocated)
		if allow.Size() != allocated.Size() {
			hidingInfo = fmt.Sprintf(" (hide %d hyperthreads, remaining cpuset: %s)", allocated.Size()-allow.Size(), allow)
		} else {
			hidingInfo = " (no hyperthreads to hide)"
		}
	}
	log.Info("%s%s", info, hidingInfo)
	container.SetCpusetCpus(allow.String())
}

// Apply the result of allocation to the requesting container.
func (p *policy) applyGrant(grant Grant) {
	log.Info("* applying grant %s", grant)

	container := grant.GetContainer()
	cpuType := grant.CPUType()
	exclusive := grant.ExclusiveCPUs()
	reserved := grant.ReservedCPUs()
	shared := grant.SharedCPUs()
	cpuPortion := grant.SharedPortion()

	cpus := cpuset.New()
	kind := ""
	switch cpuType {
	case cpuNormal:
		if exclusive.IsEmpty() {
			cpus = shared
			kind = "shared"
		} else {
			kind = "exclusive"
			if cpuPortion > 0 {
				kind += "+shared"
				cpus = exclusive.Union(shared)
			} else {
				cpus = exclusive
			}
		}
	case cpuReserved:
		kind = "reserved"
		cpus = reserved
		cpuPortion = grant.ReservedPortion()
	case cpuPreserve:
		// Will skip CPU pinning, may still pin memory.
	default:
		log.Debug("unsupported granted cpuType %s", cpuType)
		return
	}

	mems := libmem.NodeMask(0)
	if opt.PinMemory {
		mems = grant.GetMemoryZone()
	}

	if opt.PinCPU {
		if cpuType == cpuPreserve {
			log.Info("  => preserving %s cpuset %s", container.PrettyName(), container.GetCpusetCpus())
		} else {
			if cpus.Size() > 0 {
				p.setPreferredCpusetCpus(container, cpus,
					fmt.Sprintf("  => pinning %s to (%s) cpuset %s",
						container.PrettyName(), kind, cpus))
			} else {
				log.Info("  => not pinning %s CPUs, cpuset is empty...", container.PrettyName())
				container.SetCpusetCpus("")
			}
		}

		// Notes:
		//     It is extremely important to ensure that the exclusive subset of mixed
		//     CPU allocations are really exclusive at the level of the whole system
		//     and not just the orchestration. This is something we can't really do
		//     from here reliably ATM.
		//
		//     We set the CPU scheduling weight for the whole container (all processes
		//     within the container) according to container's partial allocation.
		//     This is typically a sub-CPU allocation (< 1000 mCPU) which is meant to be
		//     consumed by an 'infra/mgmt' process within the container from the shared subset
		//     of CPUs assigned to the container. The container entry point or the processes
		//     within the container are supposed to arrange so that the 'infra' process(es)
		//     are pinned to the shared CPUs and the 'data/performance critical' critical'
		//     process(es) to the exclusive CPU(s).
		//
		//     With this setup the kernel will slice out the correct amount of CPU from
		//     the shared pool for the 'infra' process as it competes with other workloads'
		//     processes in the same pool. Also the 'data' process should run fine, since
		//     it does not need to compete for CPU with any other processes in the system
		//     as long as that allocation is genuinely system-wide exclusive.
		milliCPU := cpuPortion
		if milliCPU == 0 {
			milliCPU = 1000 * grant.ExclusiveCPUs().Size()
		}
		container.SetCPUShares(int64(cache.MilliCPUToShares(int64(milliCPU))))
	}

	if grant.MemoryType() == memoryPreserve {
		log.Debug("  => preserving %s memory pinning %s", container.PrettyName(), container.GetCpusetMems())
	} else {
		if mems != libmem.NodeMask(0) {
			log.Debug("  => pinning %s to memory %s", container.PrettyName(), mems)
		} else {
			log.Debug("  => not pinning %s memory, memory set is empty...", container.PrettyName())
		}
		container.SetCpusetMems(mems.MemsetString())
	}
}

// Release resources allocated by this grant.
func (p *policy) releasePool(container cache.Container) (Grant, bool) {
	log.Info("* releasing resources allocated to %s", container.PrettyName())

	grant, ok := p.allocations.grants[container.GetID()]
	if !ok {
		log.Info("  => no grant found, nothing to do...")
		return nil, false
	}

	log.Info("  => releasing grant %s...", grant)

	// Remove the grant from all supplys it uses.
	grant.Release()

	delete(p.allocations.grants, container.GetID())
	p.saveAllocations()

	return grant, true
}

// Update shared allocations effected by agrant.
func (p *policy) updateSharedAllocations(grant *Grant) {
	if grant != nil {
		log.Info("* updating shared allocations affected by %s", (*grant).String())
		if (*grant).CPUType() == cpuReserved {
			log.Info("  this grant uses reserved CPUs, does not affect shared allocations")
			return
		}
	} else {
		log.Info("* updating all shared allocations")
	}

	for _, other := range p.allocations.grants {
		if grant != nil {
			if other.GetContainer().GetID() == (*grant).GetContainer().GetID() {
				continue
			}
		}

		if other.CPUType() == cpuReserved {
			log.Info("  => %s not affected (only reserved CPUs)...", other)
			continue
		}

		if other.CPUType() == cpuPreserve {
			log.Info("  => %s not affected (preserving CPU pinning)", other)
			continue
		}

		if other.SharedPortion() == 0 && !other.ExclusiveCPUs().IsEmpty() {
			log.Info("  => %s not affected (only exclusive CPUs)...", other)
			continue
		}

		if opt.PinCPU {
			shared := other.GetCPUNode().FreeSupply().SharableCPUs()
			exclusive := other.ExclusiveCPUs()
			if exclusive.IsEmpty() {
				p.setPreferredCpusetCpus(other.GetContainer(), shared,
					fmt.Sprintf("  => updating %s with shared CPUs of %s: %s...",
						other, other.GetCPUNode().Name(), shared.String()))
			} else {
				p.setPreferredCpusetCpus(other.GetContainer(), exclusive.Union(shared),
					fmt.Sprintf("  => updating %s with exclusive+shared CPUs of %s: %s+%s...",
						other, other.GetCPUNode().Name(), exclusive.String(), shared.String()))
			}
		}
	}
}

// Score pools against the request and sort them by score.
func (p *policy) sortPoolsByScore(req Request, aff map[int]int32) (map[int]Score, []Node) {
	scores := make(map[int]Score, p.nodeCnt)

	p.root.DepthFirst(func(n Node) {
		scores[n.NodeID()] = n.GetScore(req)
	})

	// Filter out pools which don't have enough uncompressible resources
	// (memory) to satisfy the request.
	//filteredPools := p.filterInsufficientResources(req, p.pools)
	filteredPools := make([]Node, len(p.pools))
	copy(filteredPools, p.pools)

	sort.Slice(filteredPools, func(i, j int) bool {
		return p.compareScores(req, filteredPools, scores, aff, i, j)
	})

	return scores, filteredPools
}

// Compare two pools by scores for allocation preference.
func (p *policy) compareScores(request Request, pools []Node, scores map[int]Score,
	affinity map[int]int32, i int, j int) bool {
	node1, node2 := pools[i], pools[j]
	depth1, depth2 := node1.RootDistance(), node2.RootDistance()
	id1, id2 := node1.NodeID(), node2.NodeID()
	score1, score2 := scores[id1], scores[id2]
	cpuType := request.CPUType()
	isolated1, reserved1, shared1 := score1.IsolatedCapacity(), score1.ReservedCapacity(), score1.SharedCapacity()
	isolated2, reserved2, shared2 := score2.IsolatedCapacity(), score2.ReservedCapacity(), score2.SharedCapacity()
	a1 := affinityScore(affinity, node1)
	a2 := affinityScore(affinity, node2)
	o1, o2 := score1.Offer(), score2.Offer()

	log.Debug("comparing scores for %s and %s", node1.Name(), node2.Name())
	log.Debug("  %s: %s, affinity score %f", node1.Name(), score1.String(), a1)
	log.Debug("  %s: %s, affinity score %f", node2.Name(), score2.String(), a2)

	//
	// Notes:
	//
	// Our scoring/score sorting algorithm is:
	//
	//   - insufficient isolated, reserved or shared capacity loses
	//   - if we have affinity, the higher affinity score wins
	//   - if we have topology hints
	//       * better hint score wins
	//       * for a tie, prefer the lower node then the smaller id
	//   - if we have a better matching memory offer, it wins
	//   - if we have a burstable container, sufficient capacity for the limit wins
	//   - if we have or tighter fitting memory offer, it wins
	//   - if only one node matches the memory type request, it wins
	//   - for low-prio and high-prio CPU preference, if only one node has such CPUs, it wins
	//   - if a node is lower in the tree it wins
	//   - for reserved allocations
	//       * more unallocated reserved capacity per colocated container wins
	//   - for (non-reserved) isolated allocations
	//       * more isolated capacity wins
	//       * for a tie, prefer the smaller id
	//   - for (non-reserved) exclusive allocations
	//       * more slicable (shared) capacity wins
	//       * for a tie, prefer the smaller id
	//   - for (non-reserved) shared-only allocations
	//       * fewer colocated containers win
	//       * for a tie prefer more shared capacity
	//   - lower id wins
	//
	// Before this comparison is reached, nodes with insufficient uncompressible resources
	// (memory) have been filtered out.

	// a node with insufficient isolated or shared capacity loses
	switch {
	case cpuType == cpuNormal && ((isolated2 < 0 && isolated1 >= 0) || (shared2 <= 0 && shared1 > 0)):
		log.Debug("  => %s loses, insufficent isolated or shared", node2.Name())
		return true
	case cpuType == cpuNormal && ((isolated1 < 0 && isolated2 >= 0) || (shared1 <= 0 && shared2 > 0)):
		log.Debug("  => %s loses, insufficent isolated or shared", node1.Name())
		return false
	case cpuType == cpuReserved && reserved2 < 0 && reserved1 >= 0:
		log.Debug("  => %s loses, insufficent reserved", node2.Name())
		return true
	case cpuType == cpuReserved && reserved1 < 0 && reserved2 >= 0:
		log.Debug("  => %s loses, insufficent reserved", node1.Name())
		return false
	}

	log.Debug("  - isolated/reserved/shared insufficiency is a TIE")

	// higher affinity score wins
	if a1 > a2 {
		log.Debug("  => %s loses on affinity", node2.Name())
		return true
	}
	if a2 > a1 {
		log.Debug("  => %s loses on affinity", node1.Name())
		return false
	}

	log.Debug("  - affinity is a TIE")

	// better topology hint score wins
	hScores1 := score1.HintScores()
	if len(hScores1) > 0 {
		hScores2 := score2.HintScores()
		hs1, nz1 := combineHintScores(hScores1)
		hs2, nz2 := combineHintScores(hScores2)

		if hs1 > hs2 {
			log.Debug("  => %s WINS on hints", node1.Name())
			return true
		}
		if hs2 > hs1 {
			log.Debug("  => %s WINS on hints", node2.Name())
			return false
		}

		log.Debug("  - hints are a TIE")

		if hs1 == 0 {
			if nz1 > nz2 {
				log.Debug("  => %s WINS on non-zero hints", node1.Name())
				return true
			}
			if nz2 > nz1 {
				log.Debug("  => %s WINS on non-zero hints", node2.Name())
				return false
			}

			log.Debug("  - non-zero hints are a TIE")
		}

		// for a tie, prefer lower nodes and smaller ids
		if hs1 == hs2 && nz1 == nz2 && (hs1 != 0 || nz1 != 0) {
			if depth1 > depth2 {
				log.Debug("  => %s WINS as it is lower", node1.Name())
				return true
			}
			if depth1 < depth2 {
				log.Debug("  => %s WINS as it is lower", node2.Name())
				return false
			}

			log.Debug("  => %s WINS based on equal hint socres, lower id",
				map[bool]string{true: node1.Name(), false: node2.Name()}[id1 < id2])

			return id1 < id2
		}
	}

	// better matching offer wins
	switch {
	case o1 != nil && o2 == nil:
		log.Debug("  => %s loses on memory offer (failed offer)", node2.Name())
		return true
	case o1 == nil && o2 != nil:
		log.Debug("  => %s loses on memory offer (failed offer)", node1.Name())
		return false
	case o1 == nil && o2 == nil:
		log.Debug("  - memory offer is a TIE (both failed)")
	default:
		m1, m2 := o1.NodeMask(), o2.NodeMask()
		t1, t2 := p.memZoneType(m1), p.memZoneType(m2)
		memType := request.MemoryType()

		if t1 == memType.TypeMask() && t2 != memType.TypeMask() {
			log.Debug("   - %s loses on mis-matching type (%s != %s)", node2.Name(), t2, memType)
			return true
		}
		if t1 != memType.TypeMask() && t2 == memType.TypeMask() {
			log.Debug("   - %s loses on mis-matching type (%s != %s)", node1.Name(), t1, memType)
			return false
		}
		log.Debug("  - offer memory types are a tie (%s vs %s)", t1, t2)

		if req, lim := request.MemAmountToAllocate(), request.MemoryLimit(); req != lim {
			capa1, capa2 := p.poolZoneCapacity(node1, memType), p.poolZoneCapacity(node2, memType)
			if (lim != 0 && capa1 >= lim && capa2 < lim) || (lim == 0 && capa1 > capa2) {
				log.Debug("   - %s loses on memory offer burstability", node2.Name())
				return true
			}
			if (lim != 0 && capa1 < lim && capa2 >= lim) || (lim == 0 && capa2 > capa1) {
				log.Debug("   - %s loses on memory offer burstability", node1.Name())
				return false
			}
			log.Debug("  - memory offers burstability are a TIE")
		}
	}

	if request.GetContainer().GetQOSClass() == corev1.PodQOSBurstable {
		var (
			limit = request.CPULimit()
			b1    = score1.Supply().AllocatableSharedCPU()
			b2    = score2.Supply().AllocatableSharedCPU()
			r1    = b1 - limit
			r2    = b2 - limit
		)

		log.Debug("  - CPU burstability %s=%d, %s=%d, limit=%d",
			node1.Name(), b1, node2.Name(), b2, limit)

		if limit != unlimitedCPU {
			// prefer pool with enough burstable capacity
			switch {
			case r1 >= 0 && r2 < 0:
				log.Debug("  - %s loses on insufficient CPU burstability (%d vs. %d for limit %d)",
					node2.Name(), b1, b2, limit)
				return true
			case r2 >= 0 && r1 < 0:
				log.Debug("  - %s loses on insufficient CPU burstability", node1.Name())
				return false
			default:
				log.Debug("  - CPU burstability is a TIE")
			}
		} else {
			// prefer pool with more burstable capacity
			switch {
			case b1 > b2:
				log.Debug("  - %s WINS on more CPU burstability", node1.Name())
				return true
			case b2 > b1:
				log.Debug("  - %s WINS on more CPU burstability", node2.Name())
				return false
			default:
				log.Debug("  - CPU burstability is a TIE")
			}
		}
	}

	// tighter memory offer wins
	m1, m2 := o1.NodeMask(), o2.NodeMask()
	if m1.Size() < m2.Size() {
		log.Debug("   - %s loses on memory offer (%s less tight than %s)",
			node2.Name(), m2, m1)
		return true
	}
	if m2.Size() < m1.Size() {
		log.Debug("   - %s loses on memory offer (%s less tight than %s)",
			node1.Name(), m1, m2)
		return false
	}
	if m2.Size() == m1.Size() {
		log.Debug("  - memory offers are a TIE (%s vs. %s)", m1, m2)
	}

	// matching memory type wins
	if reqType := request.MemoryType(); reqType != memoryUnspec && reqType != memoryPreserve {
		if node1.HasMemoryType(reqType) && !node2.HasMemoryType(reqType) {
			log.Debug("  => %s WINS on memory type", node1.Name())
			return true
		}
		if !node1.HasMemoryType(reqType) && node2.HasMemoryType(reqType) {
			log.Debug("  => %s WINS on memory type", node2.Name())
			return false
		}

		log.Debug("  - memory type is a TIE")
	}

	// for low-prio and high-prio CPU preference, the only fulfilling node wins
	log.Debug("  - preferred CPU priority is %s", request.CPUPrio())
	switch request.CPUPrio() {
	case lowPrio:
		lp1, lp2 := score1.PrioCapacity(lowPrio), score2.PrioCapacity(lowPrio)
		log.Debug("  - lp1 %d vs. lp2 %d", lp1, lp2)
		switch {
		case lp1 == lp2:
			log.Debug("  - low-prio CPU capacity is a TIE")
		case lp1 >= 0 && lp2 < 0:
			log.Debug("  => %s WINS based on low-prio CPU capacity", node1.Name())
			return true
		case lp1 < 0 && lp2 >= 0:
			log.Debug("  => %s WINS based on low-prio CPU capacity", node2.Name())
			return false
		}

	case highPrio:
		hp1, hp2 := score1.PrioCapacity(highPrio), score2.PrioCapacity(highPrio)
		switch {
		case hp1 == hp2:
			log.Debug("  - HighPrio CPU capacity is a TIE")
		case hp1 >= 0 && hp2 < 0:
			log.Debug("  => %s WINS based on high-prio CPU capacity", node1.Name())
			return true
		case hp1 < 0 && hp2 >= 0:
			log.Debug("  => %s WINS based on high-prio CPU capacity", node2.Name())
			return false
		}
	}

	// a lower node wins
	if depth1 > depth2 {
		log.Debug("  => %s WINS on depth", node1.Name())
		return true
	}
	if depth1 < depth2 {
		log.Debug("  => %s WINS on depth", node2.Name())
		return false
	}

	log.Debug("  - depth is a TIE")

	if request.CPUType() == cpuReserved {
		// if requesting reserved CPUs, more reserved
		// capacity per colocated container wins. Reserved
		// CPUs cannot be precisely accounted as they run
		// also BestEffort containers that do not carry
		// information on their CPU needs.
		if reserved1/(score1.Colocated()+1) > reserved2/(score2.Colocated()+1) {
			return true
		}
		if reserved2/(score2.Colocated()+1) > reserved1/(score1.Colocated()+1) {
			return false
		}
		log.Debug("  - reserved capacity is a TIE")
	} else if request.CPUType() == cpuNormal {
		// more isolated capacity wins
		if request.Isolate() && (isolated1 > 0 || isolated2 > 0) {
			if isolated1 > isolated2 {
				return true
			}
			if isolated2 > isolated1 {
				return false
			}

			log.Debug("  => %s WINS based on equal isolated capacity, lower id",
				map[bool]string{true: node1.Name(), false: node2.Name()}[id1 < id2])

			return id1 < id2
		}

		// for normal-prio CPU preference, the only fulfilling node wins
		log.Debug("  - preferred CPU priority is %s", request.CPUPrio())
		if request.CPUPrio() == normalPrio {
			np1, np2 := score1.PrioCapacity(normalPrio), score2.PrioCapacity(normalPrio)
			switch {
			case np1 == np2:
				log.Debug("  - normal-prio CPU capacity is a TIE")
			case np1 >= 0 && np2 < 0:
				log.Debug("  => %s WINS based on normal-prio CPU capacity", node1.Name())
				return true
			case np1 < 0 && np2 >= 0:
				log.Debug("  => %s WINS based on normal-prio capacity", node2.Name())
				return false
			}
		}

		// more slicable shared capacity wins
		if request.FullCPUs() > 0 && (shared1 > 0 || shared2 > 0) {
			if shared1 > shared2 {
				log.Debug("  => %s WINS on more slicable capacity", node1.Name())
				return true
			}
			if shared2 > shared1 {
				log.Debug("  => %s WINS on more slicable capacity", node2.Name())
				return false
			}

			log.Debug("  => %s WINS based on equal slicable capacity, lower id",
				map[bool]string{true: node1.Name(), false: node2.Name()}[id1 < id2])

			return id1 < id2
		}

		// fewer colocated containers win
		if score1.Colocated() < score2.Colocated() {
			log.Debug("  => %s WINS on colocation score", node1.Name())
			return true
		}
		if score2.Colocated() < score1.Colocated() {
			log.Debug("  => %s WINS on colocation score", node2.Name())
			return false
		}

		log.Debug("  - colocation score is a TIE")

		// more shared capacity wins
		if shared1 > shared2 {
			log.Debug("  => %s WINS on more shared capacity", node1.Name())
			return true
		}
		if shared2 > shared1 {
			log.Debug("  => %s WINS on more shared capacity", node2.Name())
			return false
		}
	}

	// lower id wins
	log.Debug("  => %s WINS based on lower id",
		map[bool]string{true: node1.Name(), false: node2.Name()}[id1 < id2])

	return id1 < id2
}

// affinityScore calculate the 'goodness' of the affinity for a node.
func affinityScore(affinities map[int]int32, node Node) float64 {
	Q := 0.75

	// Calculate affinity for every node as a combination of
	// affinities of the nodes on the path from the node to
	// the root and the nodes in the subtree under the node.
	//
	// The combined affinity for node n is Sum_x(A_x*D_x),
	// where for every node x, A_x is the affinity for x and
	// D_x is Q ** (number of links from node to x). IOW, the
	// effective affinity is the sum of the affinity of n and
	// the affinity of each node x of the above mentioned set
	// diluted proprotionally to the distance of x to n, with
	// Q being 0.75.

	var score float64
	for n, q := node.Parent(), Q; !n.IsNil(); n, q = n.Parent(), q*Q {
		a := affinities[n.NodeID()]
		score += q * float64(a)
	}
	node.BreadthFirst(func(n Node) {
		diff := float64(n.RootDistance() - node.RootDistance())
		q := math.Pow(Q, diff)
		a := affinities[n.NodeID()]
		score += q * float64(a)
	})
	return score
}

// hintScores calculates combined full and zero-filtered hint scores.
func combineHintScores(scores map[string]float64) (float64, float64) {
	if len(scores) == 0 {
		return 0.0, 0.0
	}

	combined, filtered := 1.0, 0.0
	for _, score := range scores {
		combined *= score
		if score != 0.0 {
			if filtered == 0.0 {
				filtered = score
			} else {
				filtered *= score
			}
		}
	}
	return combined, filtered
}
