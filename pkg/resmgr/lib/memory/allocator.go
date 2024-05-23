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
	idset "github.com/intel/goresctrl/pkg/utils"
)

// Allocator implements policy agnostic memory accounting and allocation.
type Allocator struct {
	nodes    map[ID]*Node
	zones    map[NodeMask]*Zone
	requests map[string]*Request
	users    map[string]NodeMask
	masks    *MaskCache
	version  int64
	record   *recording
	custom   CustomFunctions
}

// Offer is a possible memory allocation for a request. A valid offer can be
// committed which turns it into an actual allocation. An offer is invalidated
// by memory allocations (including offer commits) and release.
type Offer struct {
	a       *Allocator
	version int64
	req     *Request
	updates map[string]NodeMask
}

type (
	ID = idset.ID
)

// AllocatorOption is an opaque option for an Allocator.
type AllocatorOption func(*Allocator) error

// WithSystemNodes returns an option to assign memory to an allocator.
// It uses the given sysfs instance to discover memory nodes.
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

// WithNodes returns an option to assign the given memory to an allocator.
func WithNodes(nodes []*Node) AllocatorOption {
	return func(a *Allocator) error {
		if len(a.nodes) > 0 {
			return fmt.Errorf("allocator already has nodes set")
		}

		for _, n := range nodes {
			if _, ok := a.nodes[n.id]; ok {
				return fmt.Errorf("allocator already has node #%d", n.id)
			}
			a.nodes[n.id] = n
		}

		return nil
	}
}

// NewAllocator returns a new allocator instance for the given options.
func NewAllocator(options ...AllocatorOption) (*Allocator, error) {
	return newAllocator(options...)
}

// GetOffer returns an allocation offer for the given request. Until the
// offer expires it can be Commit()ed, turning into an allocation. The
// offer expires if any memory is allocated (directly or by committing
// any other unexpired offer) or released.
func (a *Allocator) GetOffer(req *Request) (*Offer, error) {
	return a.getOffer(req)
}

// Allocate allocates memory for the given request. It is equivalent to
// a successful GetOffer() followed by a commit of the returned offer.
func (a *Allocator) Allocate(req *Request) (NodeMask, map[string]NodeMask, error) {
	return a.allocate(req)
}

// Realloc adds the given extra affinity and memory types to an allocation.
func (a *Allocator) Realloc(id string, affinity NodeMask, types TypeMask) (NodeMask, map[string]NodeMask, error) {
	return a.realloc(id, affinity&a.masks.nodes.all, types&a.masks.types)
}

// Release releases the allocation with the given ID.
func (a *Allocator) Release(id string) error {
	return a.release(id)
}

// Reset resets the state of the allocator. Effectively it releases all existing
// allocations and invalidates any uncommitted offers.
func (a *Allocator) Reset() {
	a.reset()
	a.invalidateOffers()
}

// AssignedZone returns the assigned nodes for the given allocation if it exists.
func (a *Allocator) AssignedZone(id string) (NodeMask, bool) {
	if zone, ok := a.users[id]; ok {
		return zone, true
	}
	return 0, false
}

// Masks returns the cache of node and type masks for the allocator.
func (a *Allocator) Masks() *MaskCache {
	return a.masks
}

// ForeachNode calls the given function with each node present in the mask,
// until the function returns false.
func (a *Allocator) ForeachNode(nodes NodeMask, fn func(n *Node) bool) {
	(nodes & a.masks.nodes.all).Foreach(func(id ID) bool {
		return fn(a.nodes[id])
	})
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
		a.masks.AddNode(n)
	}

	a.DumpConfig()

	return a, nil
}

func (a *Allocator) getOffer(req *Request) (*Offer, error) {
	if err := a.validateRequest(req); err != nil {
		return nil, err
	}

	if err := a.findInitialZone(req); err != nil {
		return nil, err
	}

	if err := a.ensureNormalMemory(req); err != nil {
		return nil, err
	}

	if err := a.recordChanges(); err != nil {
		return nil, err
	}

	defer func() {
		a.revertRecord(req)
	}()

	a.requests[req.ID()] = req
	a.zoneAssign(req.zone, req)

	if err := a.resolveOverflow(req.zone); err != nil {
		return nil, err
	}

	updates, err := a.revertRecord(req)
	if err != nil {
		return nil, err
	}

	return a.newOffer(req, updates), nil
}

func (a *Allocator) allocate(req *Request) (NodeMask, map[string]NodeMask, error) {
	o, err := a.getOffer(req)
	if err != nil {
		return 0, nil, err
	}
	return o.Commit()
}

func (a *Allocator) realloc(id string, nodes NodeMask, types TypeMask) (NodeMask, map[string]NodeMask, error) {
	req, ok := a.requests[id]
	if !ok {
		return 0, nil, fmt.Errorf("%w: no request with ID %s", ErrUnknownRequest, id)
	}

	if nodes == 0 && types == 0 {
		return req.zone, nil, nil
	}

	if err := a.recordChanges(); err != nil {
		return 0, nil, err
	}

	defer func() {
		a.revertRecord(nil)
	}()

	newNodes, newTypes := a.expand(req.zone|nodes, req.types|types)
	if newNodes == 0 {
		return 0, nil, fmt.Errorf("%w: failed to reallocate, can't find new %s nodes",
			ErrNoMem, types)
	}

	a.zoneMove(req.zone|newNodes, req)

	if err := a.resolveOverflow(req.zone | newNodes); err != nil {
		req.zone = a.users[req.ID()]
		return 0, nil, fmt.Errorf("%w: failed to reallocate: %w", ErrNoMem, err)
	}

	req.zone |= newNodes
	req.types |= newTypes

	return req.zone, a.commitRecord(), nil
}

func (a *Allocator) release(id string) error {
	zone, ok := a.users[id]
	if !ok {
		return fmt.Errorf("%w: no request with ID %s", ErrUnknownRequest, id)
	}

	a.zoneRemove(zone, id)
	delete(a.requests, id)
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
		return fmt.Errorf("%w", ErrAlreadyExists)
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
		return fmt.Errorf("%w: request without affinity", ErrNoMem)
	}

	req.types &= a.masks.types
	if req.types == 0 {
		req.types = a.zoneType(req.affinity)
	}

	return nil
}

func (a *Allocator) findInitialZone(req *Request) error {
	//
	// Find an initial zone for the request.
	//
	// The initial zone is the request affinity expanded to contain nodes
	// for all the preferred types. For strict requests this is a mandatory
	// requirement.
	//
	// Note that we only mask out non-preferred types from the expanded
	// initial zone at the end. This allows expressing a preference like
	// 'I want only HBM memory close to node #0' by simply setting affinity
	// to NewNodeMask(0) and type to TypeMaskHBM, even if node #0 itself is
	// of some other type than HBM.
	//

	req.zone = req.affinity & a.masks.nodes.all
	missing := req.types &^ a.zoneType(req.zone)

	if missing != 0 {
		nodes, types := a.expand(req.zone, missing)
		if types != missing && req.IsStrict() {
			return fmt.Errorf("failed to find initial nodes of type %s", missing&^types)
		}

		req.zone |= nodes
	}

	if req.IsStrict() {
		req.zone &= a.masks.nodes.byTypes[req.types]
		missing = req.types &^ a.zoneType(req.zone)
		if missing != 0 {
			return fmt.Errorf("failed find initial nodes of type %s", missing)
		}
	} else {
		if req.zone&a.masks.nodes.byTypes[req.types] != 0 {
			req.zone &= a.masks.nodes.byTypes[req.types]
		}
	}

	return nil
}

func (a *Allocator) ensureNormalMemory(req *Request) error {
	//
	// Make sure that request has some initial normal memory.
	//
	// We assume that we always have some normal DRAM present and therefore
	// only force DRAM into the allowed types of non-strict requests.
	//

	if (req.zone & a.masks.nodes.normal) != 0 {
		return nil
	}

	zone := req.zone
	types := req.types
	if !req.IsStrict() {
		types |= TypeMaskDRAM
	}

	for {
		newNodes, newTypes := a.expand(zone, types)
		if newNodes == 0 {
			return fmt.Errorf("failed to find normal memory (of any type %s)", types)
		}

		zone |= newNodes
		types |= newTypes

		if (zone & a.masks.nodes.normal) != 0 {
			req.zone = zone
			req.types = types
			return nil
		}
	}
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
	return nodes, types
}

func (a *Allocator) defaultExpand(nodes NodeMask, types TypeMask) (NodeMask, TypeMask) {
	var (
		newNodes NodeMask
		newTypes TypeMask
		minDist  = math.MaxInt
		distIDs  = map[int]NodeMask{}
	)

	//
	// This is our default zone/node expansion algorithm.
	//
	//   - for each type T
	//     - for each node N in the zone
	//       - for each distance for N
	//         - take the closest set of new nodes of type T
	//

	types.Foreach(func(t Type) bool {
		a.ForeachNode(nodes, func(n *Node) bool {
			n.ForeachDistance(func(dist int, distNodes NodeMask) bool {
				if dist > minDist {
					return false
				}

				extra := distNodes & a.masks.nodes.byTypes[t.Mask()] &^ nodes
				if extra == 0 {
					return dist < minDist // if we're not at minDist yet, check next distance
				}

				distIDs[dist] |= extra
				minDist = dist

				return false // picked nodes, we're done here (next dist > minDist)
			})

			return true
		})

		return true
	})

	if minDist < math.MaxInt {
		newNodes = distIDs[minDist]
		newTypes = a.zoneType(newNodes)
	}

	if newNodes != 0 {
		log.Debug("%s expanded to %s %s", zoneName(nodes), types|newTypes, nodes|newNodes)
	}

	return newNodes, newTypes
}

func (a *Allocator) checkOverflow(nodes NodeMask) ([]NodeMask, map[NodeMask]int64) {
	var (
		zones = []NodeMask{}
		spill = map[NodeMask]int64{}
	)

	for z := range a.zones {
		if nodes == 0 || (z&nodes) != 0 {
			if free := a.zoneFree(z); free < 0 {
				zones = append(zones, z)
				spill[z] = -free
			}
		}
	}

	slices.SortFunc(zones, func(z1, z2 NodeMask) int {
		l1, l2 := len(a.zones[z1].users), len(a.zones[z2].users)
		if l1 != 0 && l2 == 0 {
			return -1
		}
		if l1 == 0 && l2 != 0 {
			return 1
		}
		if (z1 & z2) == z1 {
			return -1
		}
		if (z1 & z2) == z2 {
			return 1
		}
		if diff := z2.Size() - z1.Size(); diff < 0 {
			return -1
		} else if diff > 0 {
			return 1
		}
		return int(z2 - z1)
	})

	if log.DebugEnabled() && len(zones) > 0 {
		log.Debug("overflowing zones:")
		for _, z := range zones {
			log.Debug("  %s: %s", zoneName(z), prettySize(spill[z]))
		}
	}

	return zones, spill
}

func (a *Allocator) resolveOverflow(nodes NodeMask) error {
	zones, spill := a.checkOverflow(nodes)
	if len(zones) == 0 {
		return nil
	}

	return a.handleOverflow(nodes, zones, spill)
}

func (a *Allocator) handleOverflow(nodes NodeMask, zones []NodeMask, spill map[NodeMask]int64) error {
	if a.custom.HandleOverflow != nil {
		return a.custom.HandleOverflow(spill, &customAllocator{a})
	} else {
		return a.defaultHandleOverflow(nodes, zones, spill)
	}
}

func (a *Allocator) defaultHandleOverflow(nodes NodeMask, zones []NodeMask, spill map[NodeMask]int64) error {
	//
	// This is our default zone overflow resolution algorithm.
	//
	//   - find all zones which overflow, sort them subzones first
	//   - move requests, start with low inertia first to expanded zones
	//   - expand zones first with existing types, then with DRAM, PMEM and HBM,
	//   - repeat allowing higher inertia (Guaranteed, then Preserved)
	//

	for zones, spill := a.checkOverflow(nodes); len(zones) != 0; zones, spill = a.checkOverflow(nodes) {
		moved := int64(0)
		for _, inertia := range []Inertia{Burstable, Guaranteed, Preserved} {
			types := TypeMask(0)
			for i, extra := range []TypeMask{0, TypeMaskDRAM, TypeMaskPMEM, TypeMaskHBM} {
				extra &= a.masks.types
				if i > 0 && (types|extra) == types {
					continue
				}
				types |= extra

				for _, z := range zones {
					//if i == 0 || (a.zoneType(z)&types) != types {
					if reduce, ok := spill[z]; ok {
						m := a.zoneShrinkUsage(z, reduce, inertia, types)
						reduce -= m
						moved += m
					}
					//}
				}

				zones, spill = a.checkOverflow(nodes)
				if len(zones) == 0 {
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

	return fmt.Errorf("%w: failed to resolve overflow, zones %s overflow by %s",
		ErrNoMem, strings.Join(failed, ","), prettySize(total))
}

type recording struct {
	updates map[string]NodeMask
	reverts map[string]NodeMask
}

func (a *Allocator) recordChanges() error {
	if a.record != nil {
		return fmt.Errorf("%w: failed, allocator is already recording", ErrInternalError)
	}

	a.record = &recording{
		updates: make(map[string]NodeMask),
		reverts: make(map[string]NodeMask),
	}

	return nil
}

func (a *Allocator) commitRecord() map[string]NodeMask {
	r := a.record
	a.record = nil

	return r.updates
}

func (a *Allocator) revertRecord(req *Request) (map[string]NodeMask, error) {
	if a.record == nil {
		return nil, nil
	}

	r := a.record
	a.record = nil

	for id, zone := range r.reverts {
		if zone == 0 {
			z, ok := a.users[id]
			if !ok {
				return nil, fmt.Errorf("%w: revert failed, zone not found for request #%s",
					ErrInternalError, id)
			}
			a.zoneRemove(z, id)
		} else {
			req, ok := a.requests[id]
			if !ok {
				return nil, fmt.Errorf("%w: revert failed, request #%s not found",
					ErrInternalError, id)
			}
			a.zoneAssign(zone, req)
		}
	}

	if req != nil {
		delete(a.requests, req.ID())
	}

	return r.updates, nil
}

func (r *recording) assign(zone NodeMask, id string) {
	if r != nil {
		r.updates[id] = zone
		if _, ok := r.reverts[id]; !ok {
			r.reverts[id] = 0
		}
	}
}

func (r *recording) delete(zone NodeMask, id string) {
	if r != nil {
		if _, ok := r.reverts[id]; !ok {
			r.reverts[id] = zone
		}
	}
}

func (o *Offer) Commit() (NodeMask, map[string]NodeMask, error) {
	if !o.IsValid() {
		return 0, nil, fmt.Errorf("%w: version %d != %d", ErrExpiredOffer, o.version, o.a.version)
	}
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
