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

package main

// Completeness of the canonical key (canon.go): the trees that the
// planner walks with and without interning are the same in everything
// that the search reads, and states of equal keys behave the same.

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"

	m "github.com/askervin/gofmbt/gofmbt"
)

// searchView is a copy of a state without the fields that the key
// leaves out (canon.go).
func searchView(s *State) *State {
	v := s.clone() // (no Alts, Truncated, Wide, Narrow, memos)
	v.Steps, v.Last = 0, Act{}
	v.HeldAt = [nDevs]int64{}
	v.UnblockAt, v.LeaseIDs, v.SlotData = nil, nil, nil
	v.Data = [nDevs]Token{}
	for x := range v.PDev {
		v.PDev[x].Tok, v.PDev[x].Raw, v.PDev[x].UUID = Token{}, Token{}, ""
		v.PDev[x].LSAHash, v.PDev[x].HeadHash = "", ""
	}
	for d := range v.Drv {
		v.Drv[d].KillAtAt = 0
	}
	for i := range v.Slots {
		v.Slots[i].Gen = 0
	}
	for d := range v.Drv {
		v.Drv[d].Pod, v.Drv[d].PodBefore, v.Drv[d].PodGen, v.Drv[d].Inc = "", "", 0, 0
		v.Drv[d].Boot, v.Drv[d].BootBefore, v.Drv[d].Reboots = "", "", 0
	}
	if len(v.Ev) == 0 {
		v.Ev = nil
	}
	return v
}

// noisyTwin is a copy of a state with random values in the fields that
// the key leaves out.
func noisyTwin(s *State, rng *rand.Rand) *State {
	n := s.clone()
	n.Steps = int32(rng.Intn(1000))
	n.Last = Act{Verb: Verb(rng.Intn(int(nVerbs))), Slot: int16(rng.Intn(len(s.Slots))), VM: VM(rng.Intn(nVMs)), Pool: Pool(rng.Intn(nPools))}
	for d := range n.HeldAt {
		n.HeldAt[d] = rng.Int63()
	}
	n.UnblockAt = make([]int64, len(s.Slots))
	n.LeaseIDs = make([]string, len(s.Slots))
	n.SlotData = make([]Token, len(s.Slots))
	for i := range n.Slots {
		n.Slots[i].Gen = uint16(rng.Intn(100))
		n.UnblockAt[i] = rng.Int63()
		n.LeaseIDs[i] = fmt.Sprintf("lease-%d", rng.Intn(100))
		n.SlotData[i] = Token{Known: rng.Intn(2) == 0, Text: fmt.Sprintf("mbt-%d", rng.Intn(100))}
	}
	for x := range n.PDev {
		n.PDev[x].Tok = Token{Known: rng.Intn(2) == 0, Text: fmt.Sprintf("mbt-%d", rng.Intn(100))}
		n.PDev[x].Raw = Token{Known: rng.Intn(2) == 0, Text: fmt.Sprintf("mbt-%d", rng.Intn(100))}
		n.PDev[x].UUID = fmt.Sprintf("u%d", rng.Intn(100))
	}
	for d := range n.Data {
		n.Data[d] = Token{Known: rng.Intn(2) == 0, Text: fmt.Sprintf("mbt-%d", rng.Intn(100)), OrZero: rng.Intn(2) == 0}
	}
	for v := range n.Drv {
		n.Drv[v].Pod = fmt.Sprintf("pod-%d", rng.Intn(100))
		n.Drv[v].PodBefore = fmt.Sprintf("pod-%d", rng.Intn(100))
		n.Drv[v].PodGen = uint16(rng.Intn(100))
		n.Drv[v].Inc = uint16(rng.Intn(100))
		n.Drv[v].Boot = fmt.Sprintf("boot-%d", rng.Intn(100))
		n.Drv[v].BootBefore = fmt.Sprintf("boot-%d", rng.Intn(100))
		n.Drv[v].Reboots = uint8(rng.Intn(100))
	}
	n.Alts = []*State{s}
	n.Truncated = rng.Intn(2) == 0
	n.Wide, n.Narrow = s, fmt.Sprintf("slot-%d:vm%d", rng.Intn(100), rng.Intn(3))
	return n
}

// stepStrings are the C1 (weight 3), C2 and C4 strings of a step.
func stepStrings(cfg *Config, from *State, a Act, to *State) []string {
	out := append([]string(nil), c1Strings(cfg, a, 3)...)
	out = append(out, leaseChances(to)...)
	out = append(out, "|")
	out = append(out, faultContext(from, a)...)
	out = append(out, "|")
	return append(out, to.abstract()...)
}

// sameStates fails if two states differ in what the search reads.
func sameStates(t *testing.T, what string, x, y *State) {
	t.Helper()
	if !reflect.DeepEqual(searchView(x), searchView(y)) {
		t.Fatalf("%s: states differ:\n%s\n%s", what, searchView(x).dump(), searchView(y).dump())
	}
	if x.key() != y.key() {
		t.Fatalf("%s: equal states, keys differ", what)
	}
}

// sameBehaviour fails if two states of the same key differ in next() or
// in the coverage strings of any action.
func sameBehaviour(t *testing.T, mm *mbtModel, what string, x, y *State) {
	t.Helper()
	if x.key() != y.key() {
		t.Fatalf("%s: keys differ", what)
	}
	if x.String() != y.String() || x.forced() != y.forced() {
		t.Fatalf("%s: abstraction or forced differs", what)
	}
	for _, a := range mm.acts {
		nx, ny := x.next(a), y.next(a)
		if (nx == nil) != (ny == nil) {
			t.Fatalf("%s: %s enabled %v and %v\n%s\n%s", what, a.name(mm.cfg), nx != nil, ny != nil, x.dump(), y.dump())
		}
		if x.allowed(a) != y.allowed(a) {
			t.Fatalf("%s: %s allowed differs", what, a.name(mm.cfg))
		}
		if nx == nil {
			continue
		}
		sameStates(t, what+" "+a.name(mm.cfg), nx, ny)
		if sx, sy := stepStrings(mm.cfg, x, a, nx), stepStrings(mm.cfg, y, a, ny); !slices.Equal(sx, sy) {
			t.Fatalf("%s %s: coverage strings differ:\n%q\n%q", what, a.name(mm.cfg), sx, sy)
		}
	}
}

// randomState walks n random enabled steps from the initial state.
func randomState(mm *mbtModel, n int, rng *rand.Rand) *State {
	s := newState(mm.cfg)
	s = s.apply(Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool})
	for i := 0; i < n; i++ {
		s = s.apply(randomEnabled(mm, s, rng))
	}
	return s
}

type canonWalk struct {
	t      *testing.T
	mm     *mbtModel
	w      *planWalk
	groups map[string][]*State
	nodes  int
	width  int // below the root, follow this many random steps (0: all)
	rng    *rand.Rand
}

// compare walks the uncached tree from x and the cached one from y in
// lock step.
func (c *canonWalk) compare(path string, x, y *State, d int) {
	c.nodes++
	if g := c.groups[x.key()]; len(g) < 3 {
		c.groups[x.key()] = append(g, x)
	}
	if d == 0 {
		return
	}
	xs, ys := c.mm.StepsFrom(x), c.w.StepsFrom(y)
	if len(xs) != len(ys) {
		c.t.Fatalf("%s: %d steps without interning, %d with", path, len(xs), len(ys))
	}
	for j := range xs {
		if xs[j].Action() != ys[j].Action() {
			c.t.Fatalf("%s: step %d: %s without interning, %s with", path, j, xs[j].Action(), ys[j].Action())
		}
		if ys[j].StartState() != m.State(y) {
			c.t.Fatalf("%s: a cached step does not start from its state", path)
		}
		a := c.mm.acts[c.mm.actIndex(xs[j])]
		p := path + " / " + a.name(c.mm.cfg)
		xe, ye := xs[j].EndState().(*State), ys[j].EndState().(*State)
		sameStates(c.t, p, xe, ye)
		if sx, sy := stepStrings(c.mm.cfg, x, a, xe), stepStrings(c.mm.cfg, y, a, ye); !slices.Equal(sx, sy) {
			c.t.Fatalf("%s: coverage strings differ:\n%q\n%q", p, sx, sy)
		}
		if c.width == 0 || x == y || c.rng.Intn(len(xs)) < c.width {
			c.compare(p, xe, ye, d-1)
		}
	}
}

func TestCanonicalKeyComplete(t *testing.T) {
	type run struct {
		profile    string
		online     bool
		seed       int64
		depth, max int // max: groups whose behaviour is compared
		width      int // canonWalk.width
		reboot     bool
	}
	var runs []run
	for _, online := range []bool{false, true} {
		for seed := int64(1); seed <= int64(size(4, 1)); seed++ {
			runs = append(runs, run{"full", online, seed, 2, 30, 0, false})
		}
		runs = append(runs, run{"small", online, 7, 3, 30, 0, false})
		for seed := int64(8); seed <= 9 && !testing.Short(); seed++ {
			runs = append(runs, run{"full", online, seed, 3, 30, 6, false})
		}
		// node-reboot (A42)
		for seed := int64(10); seed <= int64(size(11, 10)); seed++ {
			runs = append(runs, run{"full", online, seed, 2, 30, 0, true})
		}
	}
	for _, r := range runs {
		cfg := defaultConfig()
		cfg.PlanStates = maxPlanStates
		cfg.Online = r.online
		if r.reboot {
			cfg.Reboot = RebootGuest
		}
		if err := cfg.loadProfile(r.profile); err != nil {
			t.Fatal(err)
		}
		mm := newMbtModel(cfg)
		rng := rand.New(rand.NewSource(r.seed))
		root := randomState(mm, 5+rng.Intn(40), rng)
		c := &canonWalk{t: t, mm: mm, w: newPlanWalk(mm, root), groups: map[string][]*State{}, width: r.width, rng: rng}
		c.compare(fmt.Sprintf("%s online=%v seed %d", r.profile, r.online, r.seed), root, root, r.depth)
		merged, compared := 0, 0
		for _, g := range c.groups {
			if len(g) < 2 {
				continue
			}
			merged++
			if compared >= r.max {
				continue
			}
			compared++
			for _, x := range g[1:] {
				sameStates(t, "equal keys", g[0], x)
				sameBehaviour(t, mm, "equal keys", g[0], x)
			}
		}
		// the fields left out of the key do not matter
		twins := 0
		for _, g := range c.groups {
			if twins >= 20 {
				break
			}
			twins++
			sameBehaviour(t, mm, "noisy twin", g[0], noisyTwin(g[0], rng))
		}
		states, expanded := c.w.size()
		t.Logf("%s online=%v seed %d depth %d: %d nodes, %d keys (%d with several nodes), cache %d states, %d expanded",
			r.profile, r.online, r.seed, r.depth, c.nodes, len(c.groups), merged, states, expanded)
		if r.depth == 3 && merged == 0 {
			t.Fatalf("no states merged: the test does not test interning")
		}
	}
}

// The search reorders the steps it gets: the cache must hand out copies.
func TestPlanWalkReturnsCopies(t *testing.T) {
	cfg := testConfig(t, "small", true)
	cfg.PlanStates = maxPlanStates
	mm := newMbtModel(cfg)
	s := steps(t, newState(cfg), "create plain@vm0#0")
	w := newPlanWalk(mm, s)
	a := w.StepsFrom(s)
	want := make([]string, len(a))
	for i := range a {
		want[i] = a[i].Action().String()
	}
	slices.Reverse(a)
	a[0] = nil
	b := w.StepsFrom(s)
	got := make([]string, len(b))
	for i := range b {
		got[i] = b[i].Action().String()
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cached steps changed by the caller:\n%s\n%s", strings.Join(got, ", "), strings.Join(want, ", "))
	}
	// equal states are one object
	x, y := b[0].EndState(), w.StepsFrom(s)[0].EndState()
	if x != y {
		t.Fatalf("end states of the same step are different objects")
	}
}

// The fields of the key fit their bits.
func TestKeyLayoutBounds(t *testing.T) {
	switch {
	case nPhases > 16:
		t.Fatalf("phase does not fit 4 bits")
	case nVMs > 3:
		t.Fatalf("node+1 does not fit 2 bits")
	case 3+2*devsPerPool*nVMs > 32:
		t.Fatalf("controller does not fit 32 bits")
	case nDevs+2 > 16:
		t.Fatalf("stack key+1 does not fit 4 bits")
	case nBridges > 4:
		t.Fatalf("fence bits do not fit 4 bits")
	case nStacks+1 > 8:
		t.Fatalf("event stack+1 does not fit 3 bits")
	case int(SteerAny) > 3 || int(LeaseBusy) > 3 || int(nModes) > 8 || int(SlicesUnknown) > 3 || int(TriMaybe) > 3 || len(markerNames) > 7:
		t.Fatalf("an enum does not fit its bits")
	}
	// a user (a slot index, or nSlots + a slot index) fits a byte
	for name := range profiles {
		if cfg := testConfig(t, name, true); 2*len(cfg.Slots) > 256 {
			t.Fatalf("profile %s: the users of %d slots do not fit a byte", name, len(cfg.Slots))
		}
	}
	// escaped counters keep keys distinct
	cfg := testConfig(t, "small", true)
	s := newState(cfg)
	keys := map[string]bool{}
	for _, r := range []uint8{0, 1, 14, 15, 16, 200} {
		for _, sl := range []uint8{0, 15, 255} {
			n := s.clone()
			n.Slots[0].Restarts, n.Slots[0].Slack = r, sl
			keys[n.key()] = true
		}
	}
	if len(keys) != 18 {
		t.Fatalf("%d distinct keys of 18 counter values", len(keys))
	}
}

// A full cache walks the same steps.
func TestPlanWalkBound(t *testing.T) {
	cfg := defaultConfig()
	cfg.PlanStates = maxPlanStates
	if err := cfg.loadProfile("full"); err != nil {
		t.Fatal(err)
	}
	mm := newMbtModel(cfg)
	root := randomState(mm, 20, rand.New(rand.NewSource(3)))
	w := newPlanWalk(mm, root)
	w.max = 50
	c := &canonWalk{t: t, mm: mm, w: w, groups: map[string][]*State{}}
	c.compare("bound", root, root, 2)
	if states, _ := w.size(); states != 50 {
		t.Fatalf("%d states cached, want the bound 50", states)
	}
}
