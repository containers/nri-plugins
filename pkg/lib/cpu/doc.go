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

// Package libcpu provides set types for CPU ids.
//
// [CPUSet] is the interface. There are two implementations of it, and which
// one fits depends on the sets you keep:
//
//   - [CpuMask] is dense: a bitmask with one bit per CPU. Set algebra runs a
//     word at a time and comparisons take a few nanoseconds whatever the size,
//     which makes it the right choice for sets that are large, long lived, or
//     combined often.
//   - [CpuSet] is sparse: it wraps k8s.io/utils/cpuset.CPUSet, so a map. It
//     suits small sets, and code which mostly passes cpuset strings around and
//     rarely does set algebra.
//
// cpuset-bench_test.go measures the two against each other and against the
// bare k8s.io/utils/cpuset.CPUSet. For a table of the whole matrix run
//
//	CPUSET_BENCH_COMPARE=1 go test -run TestCompareImplementations -v
//
// Summarised: CpuMask wins almost everything, for large sets by two or three
// orders of magnitude, and trails only marginally on Size and IsEmpty.
//
// # The interface, and what is deliberately not in it
//
// [CPUSet] carries the operations which answer a question about a set: Contains,
// Equals, Size, Intersects, List, String and the rest. Take it to accept any set
// of CPUs.
//
// The operations which produce a new set -- Clone, Union, Difference,
// Intersection -- are not in it. They are on [CpuMask] and [CpuSet], and each
// returns its own type. Go has no covariant returns, so a method returning
// *CpuMask cannot satisfy an interface method declared to return CPUSet; a
// single interface therefore cannot describe them without forcing every caller
// which knows what it holds to assert on the result. Since a caller almost
// always does know, they are left off, following the usual advice to accept
// interfaces and return concrete types.
//
// The cost is that a caller holding only a CPUSet cannot use it as the receiver
// of those four. Wrap it in an [AnyCPUSet] when that happens -- rarely, and
// visibly, at the cost of a type switch per operation. [AsCpuMask] and
// [AsCpuSet] convert a set of unknown implementation to a known one.
//
// # Coming in from k8s.io/utils/cpuset
//
// [WrapCpuSet] takes a cpuset.CPUSet over as a [CpuSet] without copying it, for
// one allocation and no dependence on how many CPUs are in it. Listing the
// members and rebuilding is the alternative, and it is not close: at a hundred
// or so CPUs wrapping is two orders of magnitude cheaper. Use it wherever a set
// arrives from an upstream interface, and [NewAnyCPUSet] on top of it if the set
// algebra is needed as well.
//
// Going back out is free: the embedded field of a [CpuSet] is the cpuset.CPUSet
// itself.
//
// # Nil sets
//
// A nil *CpuMask or *CpuSet is the empty set for everything which does not
// modify it. Size, IsEmpty, String, Key, List, Contains, ForEachCpu and the set
// algebra all read one as empty, whether it is the receiver or an operand, so a
// set which came from a map with no such key, or a struct field nobody assigned,
// needs no guarding:
//
//	free := byNode[id]              // nothing there
//	fmt.Println(free.Size())        // 0
//	fmt.Println(all.Difference(free)) // all of them
//
// This is Go's own rule for nil maps and slices: readable, not writable. The
// operations which do modify a set -- Set, Clear and Seal -- panic on a nil one,
// because no method can allocate a set and store it back into the caller's
// variable. Assign it first. There are two idioms for that, and which one you
// want depends on what you know about the set:
//
//	cpus = cpus.EmptyIfNil()  // nothing becomes an empty set, the rest is kept
//	cpus = cpus.Clone()       // always your own set, unsealed, and a copy
//
// EmptyIfNil only answers for nil. It hands back the set itself when there is
// one, which costs nothing, but a *sealed* set comes back sealed and the Set
// after it still panics. Reach for it when the set is one you own and know is
// unsealed, for instance a field of your own you may not have filled in yet.
//
// Clone answers for both, at the price of a copy: nil or not, sealed or not, what
// comes back is an unsealed set with no other owner. Reach for it for a set of
// unknown provenance -- anything the hardware package hands out is sealed -- and
// whenever you are about to modify something you were given rather than made.
//
// # Concurrency
//
// Nothing here is safe for concurrent use without external synchronisation,
// and the reason is less obvious than it looks: reading is not enough to make
// it safe. String, Key and Size fill their caches on first use, so a call that
// only reads a set can still write to it.
//
// [CPUSet.Seal] is the way out. It marks a set immutable, so that Set and
// Clear panic from then on, and it materialises every cached value up front. A
// sealed set can be read from any number of goroutines. So for a set that is
// built once and then shared:
//
//	cpus := libcpu.NewCpuMask(ids...)
//	cpus.Seal()
//	// ...safe to hand out now
//
// Clone deliberately returns an *unsealed* copy, since cloning is how you
// obtain a set you are allowed to modify. A clone of a sealed set is therefore
// not safe to share until it has been sealed again.
//
// # Gotchas
//
// Keys are per implementation. [CPUSet.Key] returns equal strings for equal
// sets of the same implementation only. CpuMask keys on its hex mask words and
// CpuSet on the cpuset string, so the set {0,5} keys as "21" through the one
// and "0,5" through the other. Never mix implementations in a single keyed
// map.
//
// If you do need one key for both, take the CpuMask form of it:
//
//	func key(s libcpu.CPUSet) string {
//		if m, ok := s.(*libcpu.CpuMask); ok {
//			return m.Key()	// already the right form, and cached
//		}
//		return libcpu.NewCpuMask(s.UnsortedList()...).Key()
//	}
//
// That is deterministic, even though CpuSet.UnsortedList returns CPUs in no
// particular order: NewCpuMask builds the same mask from the same CPUs however
// they are ordered, and CpuMask.Key is a function of the mask alone.
//
// It is not cheap though, and nothing caches it: for a set of a thousand CPUs
// it costs some tens of microseconds and a few dozen allocations, against about
// a nanosecond and none for either Key. Key it once and keep the string if you
// need it more than once.
//
// A CpuMask costs memory in proportion to its highest CPU id, not to how many
// CPUs it holds. One holding only CPU 1023 takes 16 words, as much as one
// holding all of 0-1023. A sparse set with high ids is the case where CpuSet
// may be the cheaper representation.
//
// Mixing implementations works, but it is slower. Every binary operation has a
// fast path for its own type and a fallback for everything else, and the
// fallback crosses the interface once per CPU. Mixing is supported so that the
// results are correct, not because it is fast: prefer a single implementation
// throughout any one data structure.
//
// CPU ids must not be negative. A negative id is not rejected, it aliases:
// NewCpuMask(-1) yields the set {63}. Others, Set(-64) among them, panic. The
// parsers do reject negative input.
//
// [CPUSet.UnsortedList] means what its name says. CpuMask happens to return
// CPUs in increasing order and CpuSet does not; rely on neither. Use
// [CPUSet.List] when the order matters, or [CPUSet.ForEachCpu] to walk a
// CpuMask without allocating a slice at all.
//
// [CPUSet.Contains] is variadic and means "all of", which makes Contains()
// with no arguments true. Equals depends on that.
//
// [ParseCpuMask] and [ParseCpuSet] both take the Linux cpuset list format
// ("0-3,8") and agree on what counts as valid, down to the leniency they
// inherit from strconv.Atoi: "+1" parses as 1 and "00" as 0. Neither accepts
// surrounding whitespace.
package libcpu
