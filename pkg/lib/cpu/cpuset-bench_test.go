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

package libcpu

import (
	"flag"
	"fmt"
	"os"
	"testing"
	"text/tabwriter"

	"k8s.io/utils/cpuset"
)

// This file benchmarks our dense [CpuMask] and sparse [CpuSet] against each
// other and against the raw k8s.io/utils/cpuset.CPUSet which CpuSet wraps.
// Every operation is measured for several CPU set sizes and densities, since
// both the number of CPUs in the set and the highest CPU number in it affect
// the implementations differently.
//
// Benchmarks are named <operation>/<scenario>/<implementation>, so
//
//	go test -bench 'BenchmarkCPUSet/Contains'
//	go test -bench 'BenchmarkCPUSet/.*/1024cpus'
//	go test -bench 'BenchmarkCPUSet/.*/CpuMask'
//
// all pick out a useful slice of the matrix. For a single table which shows
// the fastest implementation per operation and scenario, run
//
//	CPUSET_BENCH_COMPARE=1 go test -run TestCompareImplementations -v
//
// Note which operations are measured how. New, Parse, Clone, Union,
// Intersection and Difference are called on the concrete type, so the raw
// cpuset.CPUSet is measured as it would be used directly, with no adapter of
// ours in the way, and our own types are measured as their callers now use
// them. See benchDirect below.
//
// The rest are driven through the [CPUSet] interface, which adds the same
// non-inlinable indirection to every implementation. That is what a caller
// taking the interface pays anyway, and it keeps the comparison like for like,
// but it does mean the very cheapest operations are measured with a constant
// overhead included. For those the raw type is reached through [rawCpuSet],
// whose own cost is a single interface call, as it is for the other two.

// rawCpuSet exposes the raw k8s.io/utils/cpuset.CPUSet through the [CPUSet]
// interface so that it can be benchmarked side by side with our own types. It
// is deliberately as thin as possible: no string or key caching, no seal
// checks. Comparing it against [CpuSet] therefore shows what our wrapper adds
// on top of it, and comparing it against [CpuMask] shows the cost of the
// sparse representation itself.
//
// It is only used for the operations driven through the interface. The ones in
// benchDirect below reach the embedded cpuset.CPUSet instead, so that a result
// which allocates does not also pay for allocating one of these.
//
// The embedded cpuset.CPUSet provides Size, IsEmpty, List, UnsortedList and
// String as is. The rest need adapting, mostly because they take or return
// our CPUSet instead of a cpuset.CPUSet. Those methods assume the other set
// is a *rawCpuSet, which is all the benchmarks below ever pass them.
type rawCpuSet struct {
	cpuset.CPUSet
}

// rawCpuSet should implement CPUSet.
var _ CPUSet = (*rawCpuSet)(nil)

func newRawCpuSet(cpus ...int) CPUSet {
	return &rawCpuSet{CPUSet: cpuset.New(cpus...)}
}

func parseRawCpuSet(s string) (CPUSet, error) {
	cpus, err := cpuset.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrParseFailed, err)
	}
	return &rawCpuSet{CPUSet: cpus}, nil
}

func (s *rawCpuSet) Clone() CPUSet {
	return &rawCpuSet{CPUSet: s.CPUSet.Clone()}
}

func (s *rawCpuSet) Set(cpus ...int) {
	s.CPUSet = s.CPUSet.Union(cpuset.New(cpus...))
}

func (s *rawCpuSet) Clear(cpus ...int) {
	s.CPUSet = s.CPUSet.Difference(cpuset.New(cpus...))
}

func (s *rawCpuSet) Difference(other CPUSet) CPUSet {
	return &rawCpuSet{CPUSet: s.CPUSet.Difference(other.(*rawCpuSet).CPUSet)}
}

func (s *rawCpuSet) Intersection(other CPUSet) CPUSet {
	return &rawCpuSet{CPUSet: s.CPUSet.Intersection(other.(*rawCpuSet).CPUSet)}
}

func (s *rawCpuSet) Intersects(other CPUSet) bool {
	return !s.CPUSet.Intersection(other.(*rawCpuSet).CPUSet).IsEmpty()
}

func (s *rawCpuSet) Union(others ...CPUSet) CPUSet {
	r := s.CPUSet
	for _, other := range others {
		r = r.Union(other.(*rawCpuSet).CPUSet)
	}
	return &rawCpuSet{CPUSet: r}
}

func (s *rawCpuSet) Contains(cpus ...int) bool {
	for _, cpu := range cpus {
		if !s.CPUSet.Contains(cpu) {
			return false
		}
	}
	return true
}

func (s *rawCpuSet) Equals(other CPUSet) bool {
	return s.CPUSet.Equals(other.(*rawCpuSet).CPUSet)
}

func (s *rawCpuSet) IsSubsetOf(other CPUSet) bool {
	return s.CPUSet.IsSubsetOf(other.(*rawCpuSet).CPUSet)
}

func (s *rawCpuSet) Key() string {
	return s.String()
}

func (s *rawCpuSet) Seal() {}

func (*rawCpuSet) IsDense() bool {
	return false
}

func (*rawCpuSet) IsSparse() bool {
	return true
}

func (s *rawCpuSet) ForEachCpu(f func(cpu int) bool) {
	for _, cpu := range s.UnsortedList() {
		if !f(cpu) {
			return
		}
	}
}

// benchDirect holds the operations of one case which are measured by calling
// them on the concrete type, bound to the data they run on.
//
// Two reasons an operation belongs here rather than in the shared table below.
//
// Clone, Union, Intersection and Difference are not part of CPUSet: each
// implementation returns its own type, which no single interface signature can
// describe. Binding them per implementation is what production code does too,
// so this measures a direct call rather than the type switch an [AnyCPUSet]
// would add on top.
//
// New, Parse and those four also allocate their result, and for the raw
// cpuset.CPUSet the [rawCpuSet] adapter would allocate a second time to wrap it.
// Bound here, the raw implementation is measured unwrapped, which is the honest
// baseline to hold our own types against: it is the code someone would write
// using k8s.io/utils/cpuset directly.
type benchDirect struct {
	newSet       func()
	parseSet     func() error
	clone        func()
	union        func()
	intersection func()
	difference   func()
}

// benchImpl is one implementation under test.
type benchImpl struct {
	name  string
	new   func(cpus ...int) CPUSet
	parse func(s string) (CPUSet, error)
	// direct binds the operations measured on the concrete type. cpus and str
	// are the case's inputs for New and Parse, a and b its two sets.
	direct func(cpus []int, str string, a, b CPUSet) benchDirect
}

var benchImpls = []benchImpl{
	{
		name:  "CpuMask",
		new:   func(cpus ...int) CPUSet { return NewCpuMask(cpus...) },
		parse: func(s string) (CPUSet, error) { return ParseCpuMask(s) },
		direct: func(cpus []int, str string, a, b CPUSet) benchDirect {
			x, y := a.(*CpuMask), b.(*CpuMask)
			return benchDirect{
				newSet:       func() { sinkMask = NewCpuMask(cpus...) },
				parseSet:     func() (err error) { sinkMask, err = ParseCpuMask(str); return },
				clone:        func() { sinkMask = x.Clone() },
				union:        func() { sinkMask = x.Union(y) },
				intersection: func() { sinkMask = x.Intersection(y) },
				difference:   func() { sinkMask = x.Difference(y) },
			}
		},
	},
	{
		name:  "CpuSet",
		new:   func(cpus ...int) CPUSet { return NewCpuSet(cpus...) },
		parse: func(s string) (CPUSet, error) { return ParseCpuSet(s) },
		direct: func(cpus []int, str string, a, b CPUSet) benchDirect {
			x, y := a.(*CpuSet), b.(*CpuSet)
			return benchDirect{
				newSet:       func() { sinkSet = NewCpuSet(cpus...) },
				parseSet:     func() (err error) { sinkSet, err = ParseCpuSet(str); return },
				clone:        func() { sinkSet = x.Clone() },
				union:        func() { sinkSet = x.Union(y) },
				intersection: func() { sinkSet = x.Intersection(y) },
				difference:   func() { sinkSet = x.Difference(y) },
			}
		},
	},
	{
		// The raw k8s type, measured without the rawCpuSet adapter wherever the
		// adapter would show up in the result. This is the baseline our own two
		// implementations are worth comparing against.
		name:  "cpuset.CPUSet",
		new:   newRawCpuSet,
		parse: parseRawCpuSet,
		direct: func(cpus []int, str string, a, b CPUSet) benchDirect {
			x, y := a.(*rawCpuSet).CPUSet, b.(*rawCpuSet).CPUSet
			return benchDirect{
				newSet:       func() { sinkRaw = cpuset.New(cpus...) },
				parseSet:     func() (err error) { sinkRaw, err = cpuset.Parse(str); return },
				clone:        func() { sinkRaw = x.Clone() },
				union:        func() { sinkRaw = x.Union(y) },
				intersection: func() { sinkRaw = x.Intersection(y) },
				difference:   func() { sinkRaw = x.Difference(y) },
			}
		},
	},
}

// benchScenario describes a CPU set to benchmark with. count CPUs are picked
// stride apart, so count says how much data an operation has to chew through
// and stride how thinly it is spread: the sparse implementations care about
// count only, the dense one about count*stride, the highest CPU in the set.
type benchScenario struct {
	name   string
	count  int
	stride int
	// otherCount is how many CPUs the second operand has; zero means as many
	// as the first one. The scenarios which set it are there for the binary
	// operations that iterate whichever operand is smaller: with two sets of
	// the same size those look no different from a naive implementation.
	otherCount int
}

var benchScenarios = []benchScenario{
	{name: "1cpu", count: 1, stride: 1},
	{name: "8cpus", count: 8, stride: 1},
	{name: "8cpus-spread", count: 8, stride: 128},
	{name: "64cpus", count: 64, stride: 1},
	{name: "64cpus-spread", count: 64, stride: 16},
	{name: "256cpus", count: 256, stride: 1},
	{name: "256cpus-spread", count: 256, stride: 4},
	{name: "1024cpus", count: 1024, stride: 1},
	// asymmetric: a large set against a small one and the other way round
	{name: "1024cpus-vs-8", count: 1024, stride: 1, otherCount: 8},
	{name: "8cpus-vs-1024", count: 8, stride: 1, otherCount: 1024},
}

// asymmetric reports whether the scenario's two operands differ in size.
func (sc benchScenario) asymmetric() bool {
	return sc.otherCount != 0 && sc.otherCount != sc.count
}

// others is the number of CPUs in the second operand.
func (sc benchScenario) others() int {
	if sc.otherCount == 0 {
		return sc.count
	}
	return sc.otherCount
}

// cpus returns the CPUs of the scenario.
func (sc benchScenario) cpus() []int {
	return strided(0, sc.count, sc.stride)
}

// otherCpus returns a second set of CPUs of the same shape, overlapping the
// first one by half. It is the second operand for the set operations.
func (sc benchScenario) otherCpus() []int {
	return strided(max(1, sc.count/2)*sc.stride, sc.others(), sc.stride)
}

// overlappingCpus returns a set of CPUs of the same shape which is guaranteed
// to share at least one CPU with cpus(). It is otherCpus() for every scenario
// but the single-CPU one, where otherCpus() shifts clear of cpus() entirely
// and would not overlap; there this is cpus() itself.
func (sc benchScenario) overlappingCpus() []int {
	return strided(sc.count/2*sc.stride, sc.others(), sc.stride)
}

// disjointCpus returns a set of CPUs of the same shape which shares no CPU
// with cpus(). It starts just past the last CPU of cpus(), so it is also the
// operand that forces a full scan out of the operations which can bail out as
// soon as they find a CPU in common.
func (sc benchScenario) disjointCpus() []int {
	return strided(sc.count*sc.stride, sc.others(), sc.stride)
}

// strided returns count CPUs starting at first, stride apart.
func strided(first, count, stride int) []int {
	cpus := make([]int, count)
	for i := range cpus {
		cpus[i] = first + i*stride
	}
	return cpus
}

// benchCase is everything an operation needs to run: an implementation, and a
// scenario pre-built with it.
type benchCase struct {
	impl   benchImpl
	cpus   []int       // CPUs in set a
	str    string      // string representation of set a
	a, b   CPUSet      // two sets of the same shape, overlapping by half
	direct benchDirect // operations measured on the concrete type
	o      CPUSet      // a set of the same shape guaranteed to overlap a
	d      CPUSet      // a set of the same shape sharing no CPU with a
	hi     int         // highest CPU in a, always present in it
	absnt  int         // lowest CPU not in a
}

func newBenchCase(impl benchImpl, sc benchScenario) *benchCase {
	cpus := sc.cpus()
	a := impl.new(cpus...)
	b := impl.new(sc.otherCpus()...)

	absent := 0
	for a.Contains(absent) {
		absent++
	}

	return &benchCase{
		impl:   impl,
		cpus:   cpus,
		str:    a.String(),
		a:      a,
		b:      b,
		o:      impl.new(sc.overlappingCpus()...),
		d:      impl.new(sc.disjointCpus()...),
		hi:     cpus[len(cpus)-1],
		absnt:  absent,
		direct: impl.direct(cpus, a.String(), a, b),
	}
}

// Sinks for results we do not otherwise use. Without them the compiler is free
// to elide the allocation of a result which never escapes, which it can do for
// some implementations and not others -- the map-based ones came out at zero
// allocations per New. Assigning to a package-level variable makes the result
// escape, as keeping it would in real code. One sink per concrete type, so that
// nothing is boxed into an interface on the way.
var (
	sinkStr  string
	sinkMask *CpuMask
	sinkSet  *CpuSet
	sinkRaw  cpuset.CPUSet
)

// benchOps are the operations we measure. Every one of them leaves its sets
// unchanged, so that repeated iterations all do the same amount of work.
var benchOps = []struct {
	name string
	run  func(b *testing.B, c *benchCase)
}{
	{"New", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.direct.newSet()
		}
	}},
	{"Parse", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			if err := c.direct.parseSet(); err != nil {
				b.Fatal(err)
			}
		}
	}},
	{"Clone", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.direct.clone()
		}
	}},
	// Set adds a CPU which is already in the set, Clear removes one which is
	// not, both leaving the set as it was. Note that for a densely packed
	// scenario the cleared CPU falls beyond the last word of a CpuMask, which
	// CpuMask can reject with a bounds check alone.
	{"Set", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.Set(c.hi)
		}
	}},
	{"Clear", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.Clear(c.absnt)
		}
	}},
	{"Contains-hit", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.Contains(c.hi)
		}
	}},
	{"Contains-miss", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.Contains(c.absnt)
		}
	}},
	{"Size", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.Size()
		}
	}},
	{"IsEmpty", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.IsEmpty()
		}
	}},
	{"Union", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.direct.union()
		}
	}},
	{"Intersection", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.direct.intersection()
		}
	}},
	// Intersects returns as soon as it finds a CPU in common, so measure both
	// the early exit, against a set overlapping a half way in, and the full
	// scan, against a set sharing no CPU with it. The miss is the worst case
	// and the one directly comparable to Intersection above.
	{"Intersects-hit", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.Intersects(c.o)
		}
	}},
	{"Intersects-miss", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.Intersects(c.d)
		}
	}},
	{"Difference", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.direct.difference()
		}
	}},
	// Equals and IsSubsetOf are given a set equal to a, the worst case for
	// both: they cannot bail out early.
	{"Equals", func(b *testing.B, c *benchCase) {
		o := c.impl.new(c.cpus...)
		for b.Loop() {
			c.a.Equals(o)
		}
	}},
	{"IsSubsetOf", func(b *testing.B, c *benchCase) {
		o := c.impl.new(c.cpus...)
		for b.Loop() {
			c.a.IsSubsetOf(o)
		}
	}},
	{"List", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.List()
		}
	}},
	// Note that these measure repeated calls on an unmodified set, which is
	// the case the string and key caches exist for. CpuSet caches String,
	// CpuMask caches Key, and the raw cpuset.CPUSet caches neither.
	{"String", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			sinkStr = c.a.String()
		}
	}},
	{"Key", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			sinkStr = c.a.Key()
		}
	}},
	// Both CpuMask and CpuSet cache String, so the op above only ever measures
	// a cache hit for them. This one builds a fresh set for every iteration to
	// measure the cold path too. Subtract the New row from it to get the cost
	// of generating the string itself.
	{"String-uncached", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			sinkStr = c.impl.new(c.cpus...).String()
		}
	}},
	{"ForEachCpu", func(b *testing.B, c *benchCase) {
		for b.Loop() {
			c.a.ForEachCpu(func(int) bool { return true })
		}
	}},
}

// takesSecondOperand reports whether the named operation is measured against
// one of the scenario's second operands, and so whether an asymmetric scenario
// tells us anything a symmetric one does not. Equals and IsSubsetOf are not in
// the list: they deliberately build an operand equal to the first one, so
// their cost does not depend on the scenario's second operand either.
func takesSecondOperand(op string) bool {
	switch op {
	case "Union", "Intersection", "Difference", "Intersects-hit", "Intersects-miss":
		return true
	}
	return false
}

// skip reports whether an operation and scenario combination is worth
// measuring. An asymmetric scenario only says something new about the
// operations which look at a second operand; for the rest it would simply
// repeat the row of the symmetric scenario with the same count.
func skip(op string, sc benchScenario) bool {
	return sc.asymmetric() && !takesSecondOperand(op)
}

func BenchmarkCPUSet(b *testing.B) {
	for _, op := range benchOps {
		b.Run(op.name, func(b *testing.B) {
			for _, sc := range benchScenarios {
				if skip(op.name, sc) {
					continue
				}
				b.Run(sc.name, func(b *testing.B) {
					for _, impl := range benchImpls {
						b.Run(impl.name, func(b *testing.B) {
							op.run(b, newBenchCase(impl, sc))
						})
					}
				})
			}
		})
	}
}

// TestBenchOperands checks the relationships between the operands that the
// benchmark cases rely on. Getting one of these wrong does not fail any
// benchmark, it just silently measures something other than the row's name --
// which is exactly what happened when overlappingCpus() did not yet exist and
// Intersects-hit used otherCpus(), disjoint from cpus() for a single-CPU set.
func TestBenchOperands(t *testing.T) {
	for _, sc := range benchScenarios {
		t.Run(sc.name, func(t *testing.T) {
			var (
				a = NewCpuMask(sc.cpus()...)
				o = NewCpuMask(sc.overlappingCpus()...)
				d = NewCpuMask(sc.disjointCpus()...)
			)

			if got := a.Size(); got != sc.count {
				t.Errorf("cpus() has %d CPUs, want %d", got, sc.count)
			}
			if !a.Intersects(o) {
				t.Errorf("overlappingCpus() %s does not intersect cpus() %s", o, a)
			}
			if a.Intersects(d) {
				t.Errorf("disjointCpus() %s intersects cpus() %s", d, a)
			}
			if o.Size() != sc.others() || d.Size() != sc.others() {
				t.Errorf("second operands have %d and %d CPUs, want %d each",
					o.Size(), d.Size(), sc.others())
			}
			if b := NewCpuMask(sc.otherCpus()...); b.Size() != sc.others() {
				t.Errorf("otherCpus() has %d CPUs, want %d", b.Size(), sc.others())
			}

			// hi must be in the set and absnt must not, for Contains-hit,
			// Contains-miss, Set and Clear to measure what they claim.
			for _, impl := range benchImpls {
				c := newBenchCase(impl, sc)
				if !c.a.Contains(c.hi) {
					t.Errorf("%s: a does not contain hi=%d", impl.name, c.hi)
				}
				if c.a.Contains(c.absnt) {
					t.Errorf("%s: a contains absnt=%d", impl.name, c.absnt)
				}
			}
		})
	}
}

// TestCompareImplementations runs the full benchmark matrix and prints a
// table of ns/op per implementation. The last column names the fastest one
// for each operation and scenario, and how much faster it is than the runner
// up. It is a test rather than a benchmark because it needs to compare
// results against each other.
//
// Because it runs every case, it is opt-in:
//
//	CPUSET_BENCH_COMPARE=1 go test -run TestCompareImplementations -v
//
// Each case gets a short run by default, enough to rank the implementations
// but not to trust the absolute numbers. Pass an explicit -benchtime for a
// more accurate, and much slower, table.
func TestCompareImplementations(t *testing.T) {
	if os.Getenv("CPUSET_BENCH_COMPARE") == "" {
		t.Skip("set CPUSET_BENCH_COMPARE=1 to run the implementation comparison")
	}

	if f := flag.Lookup("test.benchtime"); f != nil && f.Value.String() == "1s" {
		if err := f.Value.Set("20ms"); err != nil {
			t.Fatalf("failed to shorten benchmark time: %v", err)
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	defer w.Flush() // nolint:errcheck

	fmt.Fprint(w, "operation\tCPUs\t") // nolint:errcheck
	for _, impl := range benchImpls {
		fmt.Fprintf(w, "%s\t", impl.name) // nolint:errcheck
	}
	fmt.Fprint(w, "fastest (vs 2nd)\t\n") // nolint:errcheck

	for _, op := range benchOps {
		for _, sc := range benchScenarios {
			if skip(op.name, sc) {
				continue
			}

			var (
				best   = benchImpl{}
				bestNs = 0.0
				next   = 0.0
			)

			fmt.Fprintf(w, "%s\t%s\t", op.name, sc.name) // nolint:errcheck

			for _, impl := range benchImpls {
				c := newBenchCase(impl, sc)
				r := testing.Benchmark(func(b *testing.B) { op.run(b, c) })
				ns := float64(r.T.Nanoseconds()) / float64(r.N)

				fmt.Fprintf(w, "%.1f\t", ns) // nolint:errcheck

				switch {
				case bestNs == 0 || ns < bestNs:
					best, bestNs, next = impl, ns, bestNs
				case next == 0 || ns < next:
					next = ns
				}
			}

			fmt.Fprintf(w, "%s (%.1fx)\t\n", best.name, next/bestNs) // nolint:errcheck
		}
		fmt.Fprint(w, "\t\t\t\t\t\t\n") // nolint:errcheck
	}
}
