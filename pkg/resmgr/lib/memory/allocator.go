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

package libmem

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// Allocator implements a topology aware but largely policy agnostic scheme
// for memory accounting and allocation.
type Allocator struct {
	nodes    map[ID]*Node
	requests map[string]*Request
	zones    map[NodeMask]*Zone
	users    map[string]NodeMask
	masks    *MaskCache
	version  int64
	journal  *journal
	custom   CustomFunctions
}

// Journal records reversible changes to an allocator.
type journal struct {
	updates map[string]NodeMask
	reverts map[string]NodeMask
}

// Offer represents a possible memory allocation for a request. Valid offers
// can be committed and turned into actual allocations. Allocating memory or
// releasing memory invalidates all uncommitted offers.
type Offer struct {
	a       *Allocator
	version int64
	req     *Request
	updates map[string]NodeMask
}

type (
	// ID is the unique ID of a memory node, its NUMA node ID.
	ID = idset.ID
)

const (
	// ForeachDone as a return value terminates iteration by a Foreach* function.
	ForeachDone = false
	// ForeachMore as a return value continues iteration by a Foreach* function.
	ForeachMore = !ForeachDone
)

// AllocatorOption is an opaque option for an Allocator.
type AllocatorOption func(*Allocator) error

// WithSystemNodes is an option to request an allocator to perform
// automatic NUMA node discovery using the given sysfs instance.
func WithSystemNodes(sys sysfs.System) AllocatorOption {
	return func(a *Allocator) error {
		nodes := []*Node{}

		for _, id := range sys.NodeIDs() {
			sysNode := sys.Node(id)
			info, err := sysNode.MemoryInfo()
			if err != nil {
				return fmt.Errorf("failed to discover system node #%d: %w", id, err)
			}

			var (
				memType   = TypeForSysfs(sysNode.GetMemoryType())
				capacity  = int64(info.MemTotal)
				isNormal  = sysNode.HasNormalMemory()
				closeCPUs = sysNode.CPUSet()
				distance  = sysNode.Distance()
			)

			n, err := NewNode(id, memType, capacity, isNormal, closeCPUs, distance)
			if err != nil {
				return fmt.Errorf("failed to create node #%d: %w", id, err)
			}

			nodes = append(nodes, n)
		}

		return WithNodes(nodes)(a)
	}
}

// WithNodes is an option to assign the given memory nodes to an allocator.
func WithNodes(nodes []*Node) AllocatorOption {
	return func(a *Allocator) error {
		if len(a.nodes) > 0 {
			return fmt.Errorf("allocator already has nodes set")
		}

		for _, n := range nodes {
			if _, ok := a.nodes[n.id]; ok {
				return fmt.Errorf("multiple nodes with id #%d", n.id)
			}
			a.nodes[n.id] = n
		}

		return nil
	}
}

// NewAllocator creates a new allocator instance and configures it with
// the given options.
func NewAllocator(options ...AllocatorOption) (*Allocator, error) {
	return newAllocator(options...)
}

// Masks returns the memory node and type mask cache for the allocator.
func (a *Allocator) Masks() *MaskCache {
	return a.masks
}

// CPUSetAffinity returns the mask of closest nodes for the given cpuset.
func (a *Allocator) CPUSetAffinity(cpus cpuset.CPUSet) NodeMask {
	nodes := NodeMask(0)
	a.ForeachNode(a.masks.nodes.all, func(n *Node) bool {
		if !cpus.Intersection(n.cpus).IsEmpty() {
			nodes |= n.Mask()
		}
		return ForeachMore
	})
	return nodes
}

// AssignedZone returns the assigned nodes for the given allocation and
// whether such an allocation was found.
func (a *Allocator) AssignedZone(id string) (NodeMask, bool) {
	if zone, ok := a.users[id]; ok {
		return zone, true
	}
	return 0, false
}

// ForeachNode calls the given function with each node present in the mask.
// It stops iterating early if the function returns false.
func (a *Allocator) ForeachNode(nodes NodeMask, fn func(*Node) bool) {
	(nodes & a.masks.nodes.all).Foreach(func(id ID) bool {
		return fn(a.nodes[id])
	})
}

// ForeachRequest calls the given function with each active allocation in
// the allocator. It stops iterating early if the functions returns false.
func (a *Allocator) ForeachRequest(req *Request, fn func(*Request) bool) {
	for _, req := range SortRequests(a.requests, nil, RequestsByAge) {
		if !fn(req) {
			return
		}
	}
}

// GetOffer returns an offer for the given request. The offer can be turned
// into an actual allocation by Commit(). Multiple offers can coexist for a
// request. Committing any offer invalidates all other offers. Allocating
// or releasing memory likewise invalidates all offers.
func (a *Allocator) GetOffer(req *Request) (*Offer, error) {
	log.Debug("get offer for %s", req)
	defer a.validateState("GetOffer")

	err := a.allocate(req)
	if err != nil {
		return nil, err
	}

	updates, err := a.revertJournal(req)
	if err != nil {
		return nil, err
	}

	return a.newOffer(req, updates), nil
}

// Allocate allocates memory for the given request. It is equivalent to
// committing an acquired offer for the request. Allocate returns the
// nodes used to satisfy the request, together with any updates made to
// other existing allocations to fulfill the request. The caller must
// ensure these updates are properly enforced.
func (a *Allocator) Allocate(req *Request) (NodeMask, map[string]NodeMask, error) {
	log.Debug("allocate %s memory for %s", req.types, req)
	defer a.validateState("Allocate")
	defer a.cleanupUnusedZones()

	err := a.allocate(req)
	if err != nil {
		return 0, nil, err
	}

	return req.zone, a.commitJournal(req), nil
}

// Realloc updates an existing allocation with the given extra affinity
// and memory types. Realloc is semantically either expansive or no-op.
// In particular, Realloc cannot be used to remove assigned nodes and
// types from an allocation, and it cannot be used to shrink the amount
// of memory assigned to an allocation. A non-zero affinity and 0 types
// cause the types implied by affinity to be used. A non-zero types is
// used to filter non-matching nodes from the affinity used. A call with
// 0 affinity and types is a no-op. A call with both affinity and types
// matching the existing allocation is also a no-op. Realloc returns the
// updated nodes for the allocation, together with any updates made to
// other existing allocations to fulfill the request. The caller must
// ensure these updates are properly enforced.
func (a *Allocator) Realloc(id string, affinity NodeMask, types TypeMask) (NodeMask, map[string]NodeMask, error) {
	log.Debug("reallocate to add %s memory affine to %s for %s", types, affinity, id)

	req, ok := a.requests[id]
	if !ok {
		return 0, nil, fmt.Errorf("%w: no request with ID %s", ErrUnknownRequest, id)
	}

	defer a.validateState("Realloc")

	return a.realloc(req, affinity, types)
}

// Release releases the allocation with the given ID.
func (a *Allocator) Release(id string) error {
	req, ok := a.requests[id]
	if !ok {
		return fmt.Errorf("%w: no request with ID %s", ErrUnknownRequest, id)
	}

	log.Debug("release memory for %s", req)

	defer a.validateState("Release")
	defer a.cleanupUnusedZones()

	return a.release(req)
}

// Reset resets the state of the allocator, releasing all allocations
// and invalidating all offers.
func (a *Allocator) Reset() {
	log.Debug("reset allocations")
	a.reset()
}

func newAllocator(options ...AllocatorOption) (*Allocator, error) {
	a := &Allocator{
		nodes: make(map[ID]*Node),
		masks: NewMaskCache(),
	}

	a.reset()

	for _, o := range options {
		if err := o(a); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrFailedOption, err)
		}
	}

	for id, n := range a.nodes {
		if len(n.distance.vector) != len(a.nodes) {
			return nil, fmt.Errorf("%w: node #%d has %d distances for %d nodes", ErrInvalidNode,
				id, len(n.distance.vector), len(a.nodes))
		}
		a.masks.addNode(n)
	}

	a.DumpConfig()

	return a, nil
}

func (a *Allocator) allocate(req *Request) (retErr error) {
	a.DumpState()

	if err := a.validateRequest(req); err != nil {
		return err
	}

	if err := a.findInitialZone(req); err != nil {
		return err
	}

	if err := a.ensureNormalMemory(req); err != nil {
		return err
	}

	if err := a.startJournal(); err != nil {
		return err
	}

	defer func() {
		if retErr != nil {
			_, err := a.revertJournal(req)
			if err != nil {
				log.Warn("failed to revert journal on error: %v", err)
			}
		}
	}()

	a.requests[req.ID()] = req
	a.zoneAssign(req.zone, req)

	return a.handleOvercommit(req.zone)
}

func (a *Allocator) realloc(req *Request, nodes NodeMask, types TypeMask) (zone NodeMask, updates map[string]NodeMask, retErr error) {
	var (
		done bool
		err  error
	)

	if nodes, types, done, err = a.validateRealloc(req, nodes, types); err != nil {
		return 0, nil, err
	}

	if done {
		return req.Zone(), nil, nil
	}

	log.Debug("reallocate to add %s memory affine to %s for %s", types, nodes, req)

	if err = a.startJournal(); err != nil {
		return 0, nil, err
	}

	defer func() {
		if retErr != nil {
			_, err := a.revertJournal(nil)
			if err != nil {
				log.Warn("failed to revert journal on error: %v", err)
			}
		}
	}()

	newNodes, newTypes := a.expand(req.zone|nodes, types)
	if newNodes == 0 {
		return 0, nil, fmt.Errorf("%w: failed to reallocate, can't find new %s nodes",
			ErrNoMem, types)
	}

	a.zoneMove(req.zone|nodes|newNodes, req)

	if err := a.handleOvercommit(req.zone | nodes | newNodes); err != nil {
		req.zone = a.users[req.ID()]
		return 0, nil, fmt.Errorf("%w: failed to reallocate: %w", ErrNoMem, err)
	}

	req.zone |= nodes | newNodes
	req.types |= newTypes

	return req.zone, a.commitJournal(req), nil
}

func (a *Allocator) release(req *Request) error {
	zone, ok := a.users[req.ID()]
	if !ok {
		return fmt.Errorf("%w: no assigned zone for %s", ErrNoZone, req)
	}

	a.zoneRemove(zone, req.ID())
	delete(a.requests, req.ID())
	a.invalidateOffers()

	return nil
}

func (a *Allocator) reset() {
	a.zones = make(map[NodeMask]*Zone)
	a.users = make(map[string]NodeMask)
	a.requests = make(map[string]*Request)
	a.invalidateOffers()
}

func (a *Allocator) invalidateOffers() {
	a.version++
}

func (a *Allocator) validateRequest(req *Request) error {
	if _, ok := a.requests[req.ID()]; ok {
		return ErrAlreadyExists
	}

	if (req.affinity & a.masks.nodes.all) != req.affinity {
		unknown := req.affinity &^ a.masks.nodes.all
		return fmt.Errorf("%w: unknown nodes requested (%s)", ErrInvalidNode, unknown)
	}

	if (req.types&a.masks.types) != req.types && req.IsStrict() {
		unavailable := req.types &^ a.masks.types
		return fmt.Errorf("%w: unavailable types requested (%s)", ErrInvalidType, unavailable)
	}

	if req.affinity == 0 {
		return fmt.Errorf("%w: request without affinity", ErrInvalidNodeMask)
	}

	req.types &= a.masks.types
	if req.types == 0 {
		req.types = a.zoneType(req.affinity)
	}

	return nil
}

func (a *Allocator) validateRealloc(req *Request, nodes NodeMask, types TypeMask) (NodeMask, TypeMask, bool, error) {
	// neither new nodes nor types requested, nothing to do
	if nodes == 0 && types == 0 {
		return 0, 0, true, nil
	}

	// requested nodes and types are not new, nothing to do
	if req.affinity == nodes && req.types == types {
		return nodes, types, true, nil
	}

	if (req.affinity & a.masks.nodes.all) != req.affinity {
		unknown := req.affinity &^ a.masks.nodes.all
		return 0, 0, false, fmt.Errorf("%w: unknown nodes requested (%s)", ErrInvalidNode, unknown)
	}

	if (req.types&a.masks.types) != req.types && req.IsStrict() {
		unavailable := req.types &^ a.masks.types
		return 0, 0, false, fmt.Errorf("%w: unavailable types requested (%s)", ErrInvalidType, unavailable)
	}

	// assigned zone already has requested nodes and types, nothing to do
	if (req.zone&nodes) == nodes && (a.zoneType(req.zone)&types) == types {
		return nodes, types, true, nil
	}

	if types == 0 {
		// if only nodes are given, use type mask of those nodes
		types = a.ZoneType(nodes)
	} else {
		// if both nodes and types are given, mask out nodes of other types
		nodes &= a.masks.nodes.byTypes[types]
	}

	return nodes, types, false, nil
}

func (a *Allocator) validateState(where string) {
	for _, req := range a.requests {
		if assigned, ok := a.users[req.ID()]; ok {
			if assigned != req.Zone() {
				log.Error("internal error: %s: %s assigned to %s, but has zone set to %s",
					where, req, assigned, req.Zone())
			}
		} else {
			log.Error("internal error: %s: %s not assigned, but has zone set to %s",
				where, req, req.Zone())
		}
	}

	for zone, z := range a.zones {
		for id, zReq := range z.users {
			req, ok := a.requests[id]
			if !ok {
				log.Error("internal error: %s: %s present in zone %s, but has no assignment",
					where, zReq, zone)
				continue
			}
			if req.Zone() != zone {
				log.Error("internal error %s: %s assigned to %s, also present in zone %s",
					where, req, req.Zone(), zone)
			}
		}
	}
}

func (a *Allocator) findInitialZone(req *Request) error {
	// Find an initial zone for the request.
	//
	// The initial zone is the request affinity expanded to contain nodes
	// for all the requested memory types. For strict requests fulfilling
	// all requested types is a mandatory requirement, for others only a
	// preference.
	//
	// Note that we mask out non-requested types from the expanded initial
	// zone at the end. This allows a preference like 'I want only HBM memory
	// close to node #0' be expressed by setting affinity to NewNodeMask(0)
	// and type to TypeMaskHBM, even if node #0 itself is not of type HBM.

	var (
		zone = req.affinity & a.masks.nodes.all
		miss = req.types &^ a.zoneType(zone)
	)

	if miss != 0 {
		log.Debug("- find initial zone (start at %s, expand with %s)", zone, miss)

		nodes, _ := a.expand(zone, miss)
		zone |= nodes
	}

	if req.IsStrict() {
		zone &= a.masks.nodes.byTypes[req.types]
		miss = req.types &^ a.zoneType(zone)
		if miss != 0 {
			return fmt.Errorf("no initial nodes of type %s", miss)
		}
	} else {
		if prefer := zone & a.masks.nodes.byTypes[req.types]; prefer != 0 {
			zone = prefer
		}
	}

	req.zone = zone

	return nil
}

func (a *Allocator) ensureNormalMemory(req *Request) error {
	// Make sure that request has some initial normal memory.
	//
	// We expand the zone for the request until the expanded zone has normal
	// (IOW non-movable) memory. For strict requests we fail the request if
	// we don't find a normal node of some of the requested types. For other
	// requests we force even unrequested DRAM, PMEM, or HBM nodes if those
	// provide normal memory.

	if (req.zone & a.masks.nodes.normal) != 0 {
		return nil
	}

	var (
		normal = a.zoneType(a.masks.nodes.normal)
		zone   = req.zone
		types  = req.types & normal
	)

	if types == 0 {
		if req.IsStrict() {
			return fmt.Errorf("no normal memory (of %s types)", req.types)
		}

		switch {
		case (normal & TypeMaskDRAM) != 0:
			types = TypeMaskDRAM
		case (normal & TypeMaskPMEM) != 0:
			types = TypeMaskPMEM
		case (normal & TypeMaskHBM) != 0:
			types = TypeMaskHBM
		}

		// shouldn't happen, we can't even boot without normal memory...
		if types == 0 {
			return fmt.Errorf("no normal memory (of any type)")
		}
	}

	log.Debug("- ensure normal memory for %s (with %s types)", zone, types)

	for n, _ := a.expand(zone, types); n != 0; n, _ = a.expand(zone, types) {
		zone |= n

		if (zone & a.masks.nodes.normal) != 0 {
			req.zone = zone
			req.types |= types

			return nil
		}
	}

	return fmt.Errorf("no normal memory (of any type %s)", types)
}

func (a *Allocator) newOffer(req *Request, updates map[string]NodeMask) *Offer {
	return &Offer{
		a:       a,
		req:     req,
		updates: updates,
		version: a.version,
	}
}

func (a *Allocator) expand(zone NodeMask, types TypeMask) (NodeMask, TypeMask) {
	var nodes NodeMask

	if a.custom.ExpandZone != nil {
		nodes = a.custom.ExpandZone(zone, types, &customAllocator{a}) &^ zone
		types = a.zoneType(nodes)
	} else {
		nodes, types = a.defaultExpand(zone, types)
	}

	if nodes != 0 {
		log.Debug("  + %s expanded by %s %s to %s", zoneName(zone), types, nodes, zone|nodes)
	}

	return nodes, types
}

func (a *Allocator) defaultExpand(zone NodeMask, types TypeMask) (NodeMask, TypeMask) {
	// The default zone expansion algorithm expands the zone by adding
	// the closest set of new nodes for each allowed type, doing a single
	// pass over all types and nodes already present in the zone.
	//
	// TODO(klihub): When looking for the closest set of nodes for a type
	// this implementation only considers distances from any of the nodes
	// already present in the original zone. We might need to reconsider
	// this. Depending on the distance matrix, this can give unintuitive
	// results. We could do a final pass on the new nodes and add, for
	// each type other than that of the new node, neighbors which are not
	// further than the closest newly discovered nodes of that type.

	var (
		newNodes NodeMask
		newTypes TypeMask
	)

	types.Foreach(func(t Type) bool {
		if n := a.newCloseNodesOfType(zone, t); n != 0 {
			newNodes |= n
			newTypes |= t.Mask()
		}
		return true
	})

	if newNodes != 0 {
		details.Debug("expanded nodes %s by types %s to %s %s", zone, types, newTypes, newNodes)
	}

	return newNodes, newTypes
}

func (a *Allocator) newCloseNodesOfType(zone NodeMask, t Type) NodeMask {
	var (
		close NodeMask
		max   = math.MaxInt
	)

	a.ForeachNode(zone, func(node *Node) bool {
		node.ForeachDistance(func(dist int, nodes NodeMask) bool {
			nodes &= a.masks.nodes.byTypes[t.Mask()] &^ zone
			if nodes == 0 {
				return true
			}
			if dist <= max {
				max = dist
				close |= nodes
			}
			return false
		})
		return true
	})

	return close
}

func (a *Allocator) handleOvercommit(nodes NodeMask) error {
	oc, spill := a.checkOvercommit(nodes)
	if len(oc) == 0 {
		return nil
	}

	if a.custom.HandleOvercommit != nil {
		return a.custom.HandleOvercommit(spill, &customAllocator{a})
	} else {
		return a.defaultHandleOvercommit(nodes, oc, spill)
	}
}

func (a *Allocator) defaultHandleOvercommit(nodes NodeMask, oc []NodeMask, spill map[NodeMask]int64) error {
	// The default zone overcommit resolution algorithm tries to shrink
	// overcommitted zones by moving requests out to expanded zones. It
	// processes zones in a loop in order of decreasing number of zone
	// users (requests assigned to the zone). It tries to avoid moving
	// high priority requests by moving requests in increasing priority
	// order. When expanding zones, it first tries to expand zones with
	// their existing memory types, then by DRAM, PMEM, and HBM in this
	// order. It rechecks overcommit after each zone expansion and stops
	// once no zones are overcommitted. Resolution fails once we can't
	// shrink any zones (IOW move any requests).

	var (
		allowedPrios = []Priority{Burstable, Guaranteed, Preserved}
		expandTypes  = []TypeMask{0, TypeMaskDRAM, TypeMaskPMEM, TypeMaskHBM}
	)

	for {
		a.dumpOvercommit("- resolving overcommit for zones:", oc, spill)

		moved := int64(0)
		for _, prio := range allowedPrios {
			types := TypeMask(0)
			for _, extra := range expandTypes {
				if extra != 0 {
					extra &= a.masks.types
					if extra == 0 {
						continue
					}
					types |= extra
				}

				for _, z := range oc {
					amount, ok := spill[z]
					if !ok {
						continue
					}
					m := a.zoneShrinkUsage(z, amount, prio, types)
					moved += m
				}

				if oc, spill = a.checkOvercommit(nodes); len(oc) == 0 {
					return nil
				}
			}
		}
		if moved == 0 {
			break
		}
	}

	var (
		failed = []string{}
		total  = int64(0)
	)

	for z, amount := range spill {
		failed = append(failed, z.String())
		total += amount
	}

	return fmt.Errorf("%w: failed to resolve overcommit, zones %s overcommit by %s",
		ErrNoMem, strings.Join(failed, ","), prettySize(total))
}

func (a *Allocator) checkOvercommit(nodes NodeMask) ([]NodeMask, map[NodeMask]int64) {
	var (
		zones = make([]NodeMask, 0, len(a.zones))
		spill = make(map[NodeMask]int64)
	)

	for z := range a.zones {
		if nodes == 0 || (z&nodes) != 0 {
			if free := a.zoneFree(z); free < 0 {
				zones = append(zones, z)
				spill[z] = -free
			}
		}
	}

	slices.SortFunc(zones, a.ZonesByUsersSubzonesFirst)

	return zones, spill
}

func (a *Allocator) cleanupUnusedZones() {
	for z, zone := range a.zones {
		if len(zone.users) == 0 {
			log.Debug("removing unused zone %s...", z)
			delete(a.zones, z)
		}
	}
}

func (a *Allocator) startJournal() error {
	if a.journal != nil {
		return fmt.Errorf("%w: failed, allocator journal already active", ErrInternalError)
	}

	a.journal = &journal{
		updates: make(map[string]NodeMask),
		reverts: make(map[string]NodeMask),
	}

	return nil
}

func (a *Allocator) commitJournal(req *Request) map[string]NodeMask {
	j := a.journal
	a.journal = nil

	delete(j.updates, req.ID())
	if len(j.updates) == 0 {
		j.updates = nil
	}

	return j.updates
}

func (a *Allocator) revertJournal(req *Request) (map[string]NodeMask, error) {
	if a.journal == nil {
		return nil, nil
	}

	log.Debug("reverting journal...")

	j := a.journal
	a.journal = nil
	for id, zone := range j.reverts {
		r, ok := a.requests[id]
		if !ok {
			if req == nil || req.ID() != id {
				return nil, fmt.Errorf("%w: revert failed, can't find request %s",
					ErrInternalError, id)
			}
			r = req
		}
		current, ok := a.users[id]
		if !ok {
			return nil, fmt.Errorf("%w: revert failed, no zone for request #%s",
				ErrInternalError, id)
		}
		a.zoneRemove(current, id)
		if zone != 0 {
			a.zoneAssign(zone, r)
		}
	}

	if req != nil {
		delete(a.requests, req.ID())
	}

	a.DumpState()

	return j.updates, nil
}

func (j *journal) assign(zone NodeMask, id string) {
	if j == nil {
		return
	}

	j.updates[id] = zone
	if _, ok := j.reverts[id]; ok {
		return
	}
	j.reverts[id] = 0
}

func (j *journal) delete(zone NodeMask, id string) {
	if j == nil {
		return
	}
	if _, ok := j.reverts[id]; ok {
		return
	}
	j.reverts[id] = zone
}

// Commit the given offer, turning it into an allocation. Any other
// offers are invalidated.
func (o *Offer) Commit() (NodeMask, map[string]NodeMask, error) {
	if !o.IsValid() {
		return 0, nil, fmt.Errorf("%w: version %d != %d", ErrExpiredOffer, o.version, o.a.version)
	}

	o.a.validateState("pre-Commit")
	defer o.a.validateState("post-Commit")
	defer o.a.DumpState()
	defer o.a.cleanupUnusedZones()

	for id, zone := range o.updates {
		if id == o.req.ID() {
			o.a.zoneAssign(zone, o.req)
			o.a.requests[o.req.ID()] = o.req
		} else {
			req, ok := o.a.requests[id]
			if ok {
				o.a.zoneMove(zone, req)
				req.zone = zone
			}
		}
	}

	o.a.invalidateOffers()

	log.Debug("committed offer %s to %s", o.req, o.req.Zone())

	return o.NodeMask(), o.Updates(), nil
}

func (o *Offer) IsValid() bool {
	return o.version == o.a.version
}

func (o *Offer) NodeMask() NodeMask {
	return o.updates[o.req.ID()]
}

func (o *Offer) Updates() map[string]NodeMask {
	u := maps.Clone(o.updates)
	delete(u, o.req.ID())
	if len(u) == 0 {
		return nil
	}
	return u
}
