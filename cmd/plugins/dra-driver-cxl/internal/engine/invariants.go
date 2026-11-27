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
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// invariant is a predicate over the records and a kernel snapshot, taken
// between requests.
type invariant struct {
	Name  string
	Text  string
	check func(c *checker)
}

// checkOptions are what the invariants need besides the records and the
// snapshot.
type checkOptions struct {
	// Sources are the sources whose records the engine owns, nil for all.
	Sources []state.Source
	// MountsDirs are the directories of the node mounts of the sources:
	// the lease binds that I8 checks are those of the node mounts there.
	MountsDirs []string
	// LeaseFsRoot is the directory of the lease-fs directories.
	LeaseFsRoot string
	// Fenced are the NUMA nodes that the fence holds, nil when fencing is
	// not checked (fencing none).
	Fenced []int
}

// invariants are the invariants of the records and the kernel together,
// rendered into docs/memory/cxl/states.md (I11 is the containers'); the
// document invariants of package state are checked with them.
var invariants = []invariant{
	{Name: "I1", Text: "Every region of ours is exactly one Region record in state OWNED, KEPT or HOLLOW_OWNED that the " +
		"kernel has on the memdevs of its Targets (enabled when OWNED, or disabled while a grant of it is RELEASING; " +
		"hollow or gone when HOLLOW_OWNED); a region on a memdev of ours that no record names is FOREIGN.", check: checkI1},
	{Name: "I2", Text: "Every dax device of a region of ours that is not the seed is a record of the region with a user " +
		"that is REALISED or RELEASING, or the Whole device (daxN.0) of a kept region.", check: checkI2},
	{Name: "I3", Text: "The objects of a REALISED grant of a memdev that is not GONE exist with the recorded names and " +
		"numbers: its region on its memdev, its dax device, its block device, its node mount.", check: checkI3},
	{Name: "I5", Text: "With fencing allowed-mems, memory of a region of ours is online only on a NUMA node that the " +
		"fence holds.", check: checkI5},
	{Name: "I6", Text: "A node mount is read-write iff a writer grant of its memdev uses it (but while a user of it is " +
		"RELEASING).", check: checkI6},
	{Name: "I7", Text: "The block device of a persistent region of ours with prepared grants is read-only iff no writer " +
		"grant uses it.", check: checkI7},
	{Name: "I8", Text: "No mount or bind under the driver's directories without a REALISED or REALISED_UNRESTORED grant " +
		"of a prepared claim that lists it, except the Stale entries of its memdev, and the node mount and lease bind " +
		"of a RELEASING grant of an UNPREPARING or KEPT claim (a release that failed, which kubelet's retry " +
		"finishes).", check: checkI8},
	{Name: "I9", Text: "Every object that the driver created lies inside a journaled container (the document side is the " +
		"\"journaled\" invariant of package state).", check: checkI9},
	{Name: "I10", Text: "No new local grant on a serial whose provenance is POOL or HELD.", check: checkI10},
	{Name: "I12", Text: "No lend of a READY lease on a NUMA node whose memory is offline (a releasing or failed lease " +
		"is revoked before the memory of its companion goes offline, and keeps its lend record until it is gone).",
		check: checkI12},
}

// checker is one check of the invariants.
type checker struct {
	r    *state.Records
	s    *kernel.Snapshot
	opts checkOptions
	inv  string
	out  []state.Violation
}

// add adds a violation of the invariant being checked.
func (c *checker) add(record, format string, args ...any) {
	c.out = append(c.out, state.Violation{Invariant: c.inv, Record: record, Text: fmt.Sprintf(format, args...)})
}

// owned tells whether the engine owns the records of the source.
func (c *checker) owned(source state.Source) bool {
	return c.opts.Sources == nil || slices.Contains(c.opts.Sources, source)
}

// memdevOwned tells whether the engine owns the record of the memdev with
// the serial.
func (c *checker) memdevOwned(serial state.Serial) bool {
	m := c.r.Memdev(serial)
	return m != nil && c.owned(m.Source)
}

// check returns the violations of the document invariants, but the lease
// claims one, and of invariants by the records r and the kernel snapshot
// s, of the records of the sources that the engine owns.
func check(r *state.Records, s *kernel.Snapshot, opts checkOptions) []state.Violation {
	c := &checker{r: r, s: s, opts: opts}
	for _, v := range state.Check(r) {
		if v.Invariant != state.InvariantLeaseClaims && (v.Source == "" || c.owned(v.Source)) {
			c.out = append(c.out, v)
		}
	}
	for _, inv := range invariants {
		c.inv = inv.Name
		inv.check(c)
	}
	return c.out
}

// ownRegions returns the region records of ours of owned memdevs.
func (c *checker) ownRegions() []*state.Region {
	var out []*state.Region
	for i := range c.r.Regions {
		reg := &c.r.Regions[i]
		if reg.Ours() && len(reg.Targets) > 0 && c.memdevOwned(reg.Targets[0]) {
			out = append(out, reg)
		}
	}
	return out
}

// releasing tells whether a grant of a memdev of the region is RELEASING.
func (c *checker) releasing(reg *state.Region) bool {
	for i := range c.r.Grants {
		if g := &c.r.Grants[i]; g.State == state.GrantReleasing && reg.TargetsSerial(g.Serial) {
			return true
		}
	}
	return false
}

// releasingUser tells whether one of the users is RELEASING.
func (c *checker) releasingUser(users []state.GrantRef) bool {
	for _, u := range users {
		if g := c.r.Grant(u); g != nil && g.State == state.GrantReleasing {
			return true
		}
	}
	return false
}

// kernelRegion returns the observation of a region, and whether it exists.
func (c *checker) kernelRegion(name string) (kernel.RegionInfo, bool) {
	ri := c.s.Region(name)
	return ri, ri.State != kernel.RegionAbsent
}

func checkI1(c *checker) {
	for _, reg := range c.ownRegions() {
		checkI1Record(c, reg)
	}
	checkI1Unnamed(c)
}

// checkI1Record adds a region record of ours whose region the kernel does
// not have in the record's state on its targets.
func checkI1Record(c *checker, reg *state.Region) {
	name := string(reg.Ref())
	ri, ok := c.kernelRegion(name)
	switch reg.State {
	case state.RegionOwned:
		// a region being released may be disabled
		if !ok || (ri.State != kernel.RegionEnabled && (ri.State != kernel.RegionDisabled || !c.releasing(reg))) {
			c.add("region "+name, "OWNED, the kernel has it %s", ri.State)
			return
		}
	case state.RegionKept:
		if !ok || ri.State == kernel.RegionHollow {
			c.add("region "+name, "KEPT, the kernel has it %s", ri.State)
			return
		}
	case state.RegionHollowOwned:
		if ok && ri.State != kernel.RegionHollow {
			c.add("region "+name, "HOLLOW_OWNED, the kernel has it %s", ri.State)
		}
		return
	default:
		c.add("region "+name, "%s between requests", reg.State)
		return
	}
	for _, serial := range reg.Targets {
		if !slices.Contains(ri.Serials, uint64(serial)) {
			c.add("region "+name, "does not target memdev %s in the kernel", serial)
		}
	}
}

// checkI1Unnamed adds the regions on memdevs of ours that no record names.
func checkI1Unnamed(c *checker) {
	for _, m := range c.r.Memdevs {
		if !c.owned(m.Source) {
			continue
		}
		for _, ri := range c.s.RegionsOf(uint64(m.Serial)) {
			if c.r.Region(state.RegionRef(ri.Name)) == nil {
				c.add("memdev "+m.Serial.String(), "the kernel has %s on it that no record names", ri.Name)
			}
		}
	}
}

func checkI2(c *checker) {
	for _, reg := range c.ownRegions() {
		if reg.State != state.RegionOwned && reg.State != state.RegionKept {
			continue
		}
		for _, d := range c.s.DaxDevicesOf(string(reg.Ref())) {
			if d.State != kernel.DaxSeed {
				checkI2Device(c, reg, d)
			}
		}
	}
}

// checkI2Device adds a dax device of a region of ours that no record names,
// or whose record has no user that is REALISED or RELEASING and is not the
// Whole device of a kept or set up region.
func checkI2Device(c *checker, reg *state.Region, d kernel.DaxDeviceInfo) {
	name := string(reg.Ref())
	rec := reg.DaxDevice(d.Name)
	switch {
	case rec == nil && (reg.State == state.RegionKept || c.r.UnfinishedRegion(reg)) && strings.HasSuffix(d.Name, ".0"):
		// the Whole device of a region kept, or of a companion kept at a
		// start, before it was set up
	case rec == nil:
		c.add("region "+name, "the kernel has %s (%s) that no record names", d.Name, d.State)
	case (reg.State == state.RegionKept || c.r.UnfinishedRegion(reg)) && !reg.SetUp && strings.HasSuffix(d.Name, ".0"):
		// the Whole device of a kept region, or of a companion kept at a
		// start, enabled again before its set-up
	case rec.Whole && (c.r.KeptRegion(reg) || len(rec.Users) > 0 || reg.SetUp || c.r.UnfinishedRegion(reg)):
	case !c.r.InUse(rec) && !c.r.UnfinishedUser(rec):
		c.add("region "+name, "%s has no REALISED or RELEASING user", d.Name)
	}
}

func checkI3(c *checker) {
	for i := range c.r.Grants {
		g := &c.r.Grants[i]
		if g.State != state.GrantRealised || !c.owned(g.Asked.Source) || g.Got.Region == "" {
			continue
		}
		if m := c.r.Memdev(g.Serial); m != nil && m.State == state.MemdevGone {
			// kept for kubelet's Unprepare
			continue
		}
		checkI3Grant(c, g)
	}
}

// checkI3Grant adds the objects of a REALISED grant that the kernel does
// not have as recorded.
func checkI3Grant(c *checker, g *state.Grant) {
	id := "grant " + g.Ref.String()
	ri, ok := c.kernelRegion(string(g.Got.Region))
	if !ok || !slices.Contains(ri.Serials, uint64(g.Serial)) {
		c.add(id, "%s is not on memdev %s in the kernel", g.Got.Region, g.Serial)
		return
	}
	checkI3Dax(c, g, id)
	checkI3Block(c, g, id)
	if path := mountPathOf(c.r, g); path != "" && kernel.FindMount(c.s.Mounts, path) == nil {
		c.add(id, "%s is not mounted", path)
	}
}

// checkI3Dax adds the dax device of a grant that the kernel does not have
// with the recorded region and numbers.
func checkI3Dax(c *checker, g *state.Grant, id string) {
	if g.Got.Dax == "" {
		return
	}
	d := c.s.DaxDevice(g.Got.Dax)
	dev := c.r.DaxDevOf(g)
	switch {
	case d.State == kernel.DaxAbsent || d.Region != string(g.Got.Region):
		c.add(id, "%s is not a dax device of %s", g.Got.Dax, g.Got.Region)
	case !dev.IsZero() && (d.Major != dev.Major || d.Minor != dev.Minor):
		c.add(id, "%s is %d:%d, recorded %d:%d", g.Got.Dax, d.Major, d.Minor, dev.Major, dev.Minor)
	}
}

// checkI3Block adds the block device of a grant that the kernel does not
// have with the recorded numbers.
func checkI3Block(c *checker, g *state.Grant, id string) {
	bd := g.Got.BlockDevice
	if bd == nil {
		return
	}
	if dev, ok := c.s.BlockDevice(bd.Name); !ok || dev.Major != bd.Dev.Major || dev.Minor != bd.Dev.Minor {
		c.add(id, "block device %s (%d:%d) is not in the kernel", bd.Name, bd.Dev.Major, bd.Dev.Minor)
	}
}

func checkI5(c *checker) {
	if c.opts.Fenced == nil {
		return
	}
	for _, reg := range c.ownRegions() {
		ri, ok := c.kernelRegion(string(reg.Ref()))
		if !ok || ri.Range.Size() == 0 || c.s.BlockSize == 0 {
			continue
		}
		for _, b := range c.s.OnlineBlocks(ri.Range.Blocks(c.s.BlockSize)) {
			if node := c.s.Block(b).NUMANode; node >= 0 && !slices.Contains(c.opts.Fenced, node) {
				c.add("region "+string(reg.Ref()), "memory%d is online on NUMA node %d, which is not fenced", b, node)
			}
		}
	}
}

func checkI6(c *checker) {
	for _, reg := range c.ownRegions() {
		for _, m := range reg.Mounts {
			if m.State != state.MountedRO && m.State != state.MountedRW || c.releasingUser(m.Users) {
				// a release of a user may have changed it already
				continue
			}
			mi := kernel.FindMount(c.s.Mounts, m.Path)
			if mi == nil {
				c.add("region "+string(reg.Ref()), "mount %s is not mounted", m.Path)
				continue
			}
			writer := mountWriter(c.r, &m)
			if mi.SuperReadOnly() == writer {
				c.add("region "+string(reg.Ref()), "mount %s: superblock read-only %v with writer %v", m.Path, mi.SuperReadOnly(), writer)
			}
		}
	}
}

func checkI7(c *checker) {
	for _, reg := range c.ownRegions() {
		if reg.Pmem == nil || reg.Pmem.BlockDevice == nil || reg.State != state.RegionOwned {
			continue
		}
		writer, uses, releasing := false, 0, false
		for i := range c.r.Grants {
			g := &c.r.Grants[i]
			if g.Got.Region != reg.Ref() {
				continue
			}
			// a release that failed leaves the flag as it found it
			releasing = releasing || g.State == state.GrantReleasing
			if g.State == state.GrantRealised {
				uses++
				writer = writer || !g.Asked.ReadOnly
			}
		}
		if uses == 0 || releasing {
			continue
		}
		dev, ok := c.s.BlockDevice(reg.Pmem.BlockDevice.Name)
		if ok && dev.ReadOnly == writer {
			c.add("region "+string(reg.Ref()), "%s read-only %v with writer %v", dev.Path, dev.ReadOnly, writer)
		}
	}
}

func checkI8(c *checker) {
	listed := c.listedMounts()
	for _, m := range c.r.Memdevs {
		for _, path := range m.StaleBinds {
			listed[path] = true
		}
	}
	inMountsDir := func(target string) bool {
		return slices.Contains(c.opts.MountsDirs, filepath.Dir(target))
	}
	// a lease bind is of a node mount of the sources (its source is that
	// of a mount in their mounts directories)
	own := map[string]bool{}
	for _, mi := range c.s.Mounts {
		if inMountsDir(mi.Target) {
			own[mi.Source] = true
		}
	}
	under := func(mi *kernel.MountInfo) bool {
		return inMountsDir(mi.Target) ||
			c.opts.LeaseFsRoot != "" && strings.HasPrefix(mi.Target, c.opts.LeaseFsRoot+"/") && own[mi.Source]
	}
	for i := range c.s.Mounts {
		if mi := &c.s.Mounts[i]; under(mi) && !listed[mi.Target] {
			c.add("mount "+mi.Target, "no realised grant of a prepared claim lists it")
		}
	}
}

// listedMounts returns the node mounts and lease binds of the realised
// grants of prepared claims, and of the RELEASING grants of UNPREPARING or
// KEPT claims (a release that failed).
func (c *checker) listedMounts() map[string]bool {
	listed := map[string]bool{}
	for i := range c.r.Grants {
		g := &c.r.Grants[i]
		p := c.r.PreparationOf(g.Ref)
		if p == nil {
			continue
		}
		releasing := g.State == state.GrantReleasing &&
			(p.State == state.PreparationUnpreparing || p.State == state.PreparationKept)
		if !releasing && (!prepared(g) || !p.Prepared()) {
			continue
		}
		listed[mountPathOf(c.r, g)] = true
		if g.Got.Bind != nil {
			listed[g.Got.Bind.Target] = true
		}
	}
	return listed
}

func checkI9(c *checker) {
	for _, ri := range c.s.Regions {
		reg := c.r.Region(state.RegionRef(ri.Name))
		if reg == nil || !reg.Ours() {
			continue
		}
		for _, serial := range reg.Targets {
			if c.r.Memdev(serial) == nil {
				c.add("region "+ri.Name, "its memdev %s has no record", serial)
			}
		}
	}
}

func checkI10(c *checker) {
	for i := range c.r.Grants {
		g := &c.r.Grants[i]
		if g.Asked.Source != state.SourceLocal || (g.State != state.GrantJournaled && g.State != state.GrantRealising) {
			continue
		}
		if p := c.r.ProvenanceOf(g.Serial); p != nil && (p.State == state.ProvenancePool || p.State == state.ProvenanceHeld) {
			c.add("grant "+g.Ref.String(), "a new local grant on serial %s of provenance %s", g.Serial, p.State)
		}
	}
}

func checkI12(c *checker) {
	for _, l := range c.r.Leases {
		if l.State != state.LeaseReady || l.Lend == nil || l.Lend.NUMANode == nil {
			continue
		}
		if n, ok := c.s.NUMANode(*l.Lend.NUMANode); !ok || !n.HasMemory {
			c.add("lease "+l.ID, "lends NUMA node %d, which has no memory online", *l.Lend.NUMANode)
		}
	}
}

// Check returns the violations of the invariants by the records of the
// store and the kernel now, for the sources that the engine owns.
func (e *Engine) Check(snap *kernel.Snapshot, sources []state.Source, fenced []int) []state.Violation {
	var dirs []string
	for s, d := range e.opts.MountsDirs {
		if sources == nil || slices.Contains(sources, s) {
			dirs = append(dirs, d)
		}
	}
	return check(e.records(), snap, checkOptions{Sources: sources, MountsDirs: dirs, LeaseFsRoot: e.opts.LeaseFsRoot, Fenced: fenced})
}
