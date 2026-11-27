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

package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// UnknownTransition is the start of the debug log line of an observed
// change of a kernel object that is not a row of its table.
const UnknownTransition = "kernel transition not in table: "

// Observe returns a new observation of the kernel objects. A region of
// ours that is hollow is logged once while it stays hollow, with the
// command that deletes it. Every change of the state of a kernel object since the last
// observation of the engine that its table does not have is logged at klog
// V(4) (UnknownTransition); it is never an error.
func (e *Engine) Observe(ctx context.Context) (*kernel.Snapshot, error) {
	snap, err := e.k.Observe(ctx)
	if err != nil {
		return nil, err
	}
	e.qmu.Lock()
	prev := e.last
	e.last, e.observed = snap, e.started
	e.qmu.Unlock()
	if prev != nil {
		lines := unknownTransitions(prev, snap)
		for _, line := range lines {
			klog.V(4).Info(UnknownTransition + line)
		}
		e.addUnknown(lines)
	}
	e.logHollow(snap)
	return snap, nil
}

// unknownKept is how many of the latest unknown changes of kernel objects
// the engine keeps until UnknownSeen.
const unknownKept = 32

// addUnknown keeps the unknown changes lines, the latest unknownKept of
// them.
func (e *Engine) addUnknown(lines []string) {
	if len(lines) == 0 {
		return
	}
	e.qmu.Lock()
	defer e.qmu.Unlock()
	e.unknown = append(e.unknown, lines...)
	if n := len(e.unknown) - unknownKept; n > 0 {
		e.unknown = slices.Delete(e.unknown, 0, n)
	}
}

// UnknownSeen returns the changes of kernel objects that no row of their
// tables has that Observe logged since the last call (the latest
// unknownKept of them), and forgets them.
func (e *Engine) UnknownSeen() []string {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	out := e.unknown
	e.unknown = nil
	return out
}

// requestKey is the key of the engine of a request in its context.
type requestKey struct{}

// Request marks a request of a kubelet plugin in progress (a Prepare or an
// Unprepare of a claim, whose calls of the engine get the context that it
// returns) until the function that it returns is called. The invariants of
// the records and the kernel hold between requests: at the end of a request
// they are logged at klog V(4) ("engine invariant: ...") when no other
// request is in progress. A call of PrepareGrants or ReleaseGrants outside
// a request is a request of its own.
func (e *Engine) Request(ctx context.Context) (context.Context, func()) {
	if ctx.Value(requestKey{}) == e {
		return ctx, func() {}
	}
	e.qmu.Lock()
	e.running++
	e.started++
	seq := e.started
	e.qmu.Unlock()
	return context.WithValue(ctx, requestKey{}, e), func() {
		e.checkBetween(seq)
		e.qmu.Lock()
		e.running--
		e.qmu.Unlock()
	}
}

// checkBetween logs the violations of the invariants at the end of the
// request seq at klog V(4), by the latest observation of the kernel, when no
// other request is in progress or starts meanwhile and the request observed
// the kernel (the observation is of its end).
func (e *Engine) checkBetween(seq int) {
	if !klog.V(4).Enabled() {
		return
	}
	e.qmu.Lock()
	snap, started, others, observed := e.last, e.started, e.running-1, e.observed
	e.qmu.Unlock()
	switch {
	case snap == nil || observed < seq:
		return
	case others > 0:
		klog.V(5).Infof("engine: invariants not checked: %d other requests in progress", others)
		return
	}
	violations := e.Check(snap, e.opts.Sources, nil)
	e.qmu.Lock()
	again := e.started != started || e.running > 1
	e.qmu.Unlock()
	if again {
		klog.V(5).Info("engine: invariants not checked: another request started")
		return
	}
	for _, v := range violations {
		klog.V(4).Info("engine invariant: " + v.String())
	}
}

// verify is the verify step after a Prepare or a release of grants: a new
// observation (Observe), the FOREIGN records of the regions that are gone
// removed, and the objects of the grants refs that are REALISED checked in
// it (an error logged for one that does not verify). The end of the
// request checks the invariants (Request).
func (e *Engine) verify(ctx context.Context, refs []state.GrantRef, trigger state.Trigger) {
	snap, err := e.Observe(ctx)
	if err != nil {
		klog.Errorf("engine: observing the kernel: %v", err)
		return
	}
	if err := e.syncForeignRegions(ctx, nil, snap.Region, trigger); err != nil {
		klog.Errorf("engine: %v", err)
	}
	c := &checker{r: e.records(), s: snap, opts: checkOptions{Sources: e.opts.Sources}, inv: "I3"}
	for _, ref := range refs {
		if g := c.r.Grant(ref); g != nil && g.State == state.GrantRealised {
			checkI3Grant(c, g)
		}
	}
	for _, v := range c.out {
		klog.Errorf("engine: a grant does not verify: %s", v)
	}
}

// logHollow logs each hollow region of ours of the snapshot once while it
// stays hollow, with the command that deletes it. A name that is no longer
// hollow is forgotten: the kernel reuses region names.
func (e *Engine) logHollow(snap *kernel.Snapshot) {
	r := e.records()
	hollow := map[string]bool{}
	for _, ri := range snap.Regions {
		if ri.State != kernel.RegionHollow {
			continue
		}
		reg := r.Region(state.RegionRef(ri.Name))
		if reg == nil || !reg.Ours() {
			continue
		}
		hollow[ri.Name] = true
		e.qmu.Lock()
		logged := e.hollowLogged[ri.Name]
		e.hollowLogged[ri.Name] = true
		e.qmu.Unlock()
		if logged {
			continue
		}
		decoder := ri.RootDecoder
		if decoder == "" {
			decoder = "<its root decoder>"
		}
		klog.Warningf("engine: %s is hollow (NUMA node %d): it is deleted when its memory blocks are offline: echo %s > /sys/bus/cxl/devices/%s/delete_region",
			ri.Name, ri.NUMANode, ri.Name, decoder)
	}
	e.qmu.Lock()
	for name := range e.hollowLogged {
		if !hollow[name] {
			delete(e.hollowLogged, name)
		}
	}
	e.qmu.Unlock()
}

// unknownTransitions returns the changes of the states of the kernel
// objects from prev to cur that the kernel's tables do not have, in words:
// "<table> <object>: FROM -> TO: transition not in the table".
func unknownTransitions(prev, cur *kernel.Snapshot) []string {
	var out []string
	add := func(name string, err error) {
		if err != nil {
			out = append(out, name+": "+err.Error())
		}
	}
	memdevs := map[uint64][2]kernel.MemdevState{}
	for _, m := range prev.Memdevs {
		memdevs[m.Serial] = [2]kernel.MemdevState{m.State, kernel.MemdevAbsent}
	}
	for _, m := range cur.Memdevs {
		s, ok := memdevs[m.Serial]
		if !ok {
			s[0] = kernel.MemdevAbsent
		}
		s[1] = m.State
		memdevs[m.Serial] = s
	}
	for _, serial := range slices.Sorted(maps.Keys(memdevs)) {
		s := memdevs[serial]
		add(fmt.Sprintf("memdev 0x%x", serial), kernel.MemdevTransitions.Check(s[0], s[1], fsm.AnyTrigger))
	}
	diff(&out, "region", kernel.RegionTransitions, kernel.RegionAbsent, prev.Regions, cur.Regions,
		func(r kernel.RegionInfo) (string, kernel.RegionState) { return r.Name, r.State })
	diff(&out, "dax device", kernel.DaxTransitions, kernel.DaxAbsent, prev.DaxDevices, cur.DaxDevices,
		func(d kernel.DaxDeviceInfo) (string, kernel.DaxState) { return d.Name, d.State })
	diff(&out, "memory block", kernel.BlockTransitions, kernel.BlockAbsent, prev.Blocks, cur.Blocks,
		func(b kernel.MemoryBlock) (string, kernel.BlockState) {
			return "memory" + strconv.Itoa(b.Index), b.State
		})
	diff(&out, "nd region", kernel.NdRegionTransitions, kernel.NdAbsent, prev.Pmem, cur.Pmem,
		func(n kernel.NdRegion) (string, kernel.NdRegionState) { return n.Name, n.State })
	diff(&out, "namespace", kernel.NamespaceTransitions, kernel.NsAbsent, namespaces(prev), namespaces(cur),
		func(n kernel.Namespace) (string, kernel.NamespaceState) { return n.Name, n.State })
	return out
}

// diff adds the changes of the objects from prev to cur that the table does
// not have; an object that is missing has the state absent.
func diff[T any, S comparable](out *[]string, kind string, table fsm.Table[S], absent S, prev, cur []T, key func(T) (string, S)) {
	states := map[string][2]S{}
	for _, o := range prev {
		name, s := key(o)
		states[name] = [2]S{s, absent}
	}
	for _, o := range cur {
		name, s := key(o)
		st, ok := states[name]
		if !ok {
			st[0] = absent
		}
		st[1] = s
		states[name] = st
	}
	for _, name := range slices.Sorted(maps.Keys(states)) {
		st := states[name]
		if err := table.Check(st[0], st[1], fsm.AnyTrigger); err != nil {
			*out = append(*out, kind+" "+name+": "+err.Error())
		}
	}
}

// namespaces returns the namespaces of the nd regions of a snapshot.
func namespaces(s *kernel.Snapshot) []kernel.Namespace {
	var out []kernel.Namespace
	for _, nd := range s.Pmem {
		out = append(out, nd.Namespaces...)
	}
	return out
}

// syncForeignRegions records the regions names that no record names
// (FOREIGN), and removes the FOREIGN records of the regions that are gone
// from their targets. lookup returns what the kernel has of a region; the
// commit is made only when the records change.
func (e *Engine) syncForeignRegions(ctx context.Context, names []string, lookup func(string) kernel.RegionInfo, trigger state.Trigger) error {
	r := e.records()
	seen := map[string]kernel.RegionInfo{}
	changed := false
	for _, name := range names {
		if _, ok := seen[name]; !ok {
			seen[name] = lookup(name)
			changed = changed || seen[name].State != kernel.RegionAbsent && r.Region(state.RegionRef(name)) == nil
		}
	}
	for _, reg := range r.Regions {
		if reg.State != state.RegionForeign {
			continue
		}
		name := string(reg.Ref())
		if _, ok := seen[name]; !ok {
			seen[name] = lookup(name)
		}
		changed = changed || !foreignStays(&reg, seen[name])
	}
	if !changed {
		return nil
	}
	return e.commit(ctx, func(r *state.Records) error { return syncForeign(r, seen, names, trigger) })
}

// memdevRegions returns the names of the regions of the snapshot on the
// memdevs of the engine's records.
func (e *Engine) memdevRegions(snap *kernel.Snapshot) []string {
	var names []string
	for _, m := range e.records().Memdevs {
		if e.owns(m.Source) {
			for _, ri := range snap.RegionsOf(uint64(m.Serial)) {
				names = append(names, ri.Name)
			}
		}
	}
	return names
}

// kernelRegion returns what the kernel has of the region name, in state
// RegionAbsent when it has none.
func (e *Engine) kernelRegion(name string) kernel.RegionInfo {
	ri, err := e.k.Region(name)
	if err != nil {
		return kernel.RegionInfo{Name: name, State: kernel.RegionAbsent, NUMANode: -1}
	}
	return ri
}

// syncForeign removes the FOREIGN records whose region seen does not have
// on their targets (NONE), and records the regions names of seen that no
// record names (FOREIGN).
func syncForeign(r *state.Records, seen map[string]kernel.RegionInfo, names []string, trigger state.Trigger) error {
	for _, ref := range regionRefs(r) {
		if reg := r.Region(ref); reg.State == state.RegionForeign && !foreignStays(reg, seen[string(ref)]) {
			if err := forgetForeign(r, ref, trigger); err != nil {
				return err
			}
		}
	}
	for _, name := range names {
		ri := seen[name]
		if ri.State == kernel.RegionAbsent || r.Region(state.RegionRef(name)) != nil {
			continue
		}
		reg := state.Region{Name: name, Mode: state.RegionMode(ri.Mode), Owner: state.OwnerForeign, State: state.RegionNone}
		for _, serial := range ri.Serials {
			reg.Targets = append(reg.Targets, state.Serial(serial))
		}
		if ri.Range.End > 0 {
			reg.Size = ri.Range.Size()
		}
		if ri.NUMANode >= 0 {
			node := ri.NUMANode
			reg.NUMANode = &node
		}
		if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionForeign, trigger); err != nil {
			return err
		}
		r.Regions = append(r.Regions, reg)
	}
	return nil
}

// foreignStays tells whether the kernel has the region of a FOREIGN record
// on the record's targets.
func foreignStays(reg *state.Region, ri kernel.RegionInfo) bool {
	if ri.State == kernel.RegionAbsent || len(ri.Serials) != len(reg.Targets) {
		return false
	}
	for _, serial := range reg.Targets {
		if !slices.Contains(ri.Serials, uint64(serial)) {
			return false
		}
	}
	return true
}

// forgetForeign removes the FOREIGN record ref, if there is one.
func forgetForeign(r *state.Records, ref state.RegionRef, trigger state.Trigger) error {
	reg := r.Region(ref)
	if reg == nil || reg.State != state.RegionForeign {
		return nil
	}
	if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionNone, trigger); err != nil {
		return err
	}
	removeRecord(&r.Regions, func(x *state.Region) bool { return x.Ref() == ref })
	return nil
}
