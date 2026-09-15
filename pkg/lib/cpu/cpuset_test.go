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
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"

	"k8s.io/utils/cpuset"
)

// testCPUSet wraps *CpuMask but presents a different concrete type so that
// type assertions to *CpuMask fail, exercising the non-fast-path fallback
// code in Difference, Intersection, Equals, IsSubsetOf, and Union.
type testCPUSet struct {
	*CpuMask
}

var _ CPUSet = (*testCPUSet)(nil)

func newTestCPUSet(cpus ...int) *testCPUSet {
	return &testCPUSet{CpuMask: NewCpuMask(cpus...)}
}

// cpuRange returns a sorted []int with all integers from lo to hi inclusive.
func cpuRange(lo, hi int) []int {
	s := make([]int, hi-lo+1)
	for i := range s {
		s[i] = lo + i
	}
	return s
}

// maskListEqual reports whether two CPUSets contain exactly the same CPUs by
// comparing their sorted lists, avoiding any dependence on Equals itself.
func maskListEqual(a, b CPUSet) bool {
	return slices.Equal(a.List(), b.List())
}

// ---- TestNewCpuMask -------------------------------------------------------

func TestNewCpuMask(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected []int
	}{
		{
			name:     "empty",
			cpus:     []int{},
			expected: []int{},
		},
		{
			name:     "cpu-0-lowest-bit-word-0",
			cpus:     []int{0},
			expected: []int{0},
		},
		{
			name:     "cpu-63-highest-bit-word-0",
			cpus:     []int{63},
			expected: []int{63},
		},
		{
			name:     "cpu-64-lowest-bit-word-1",
			cpus:     []int{64},
			expected: []int{64},
		},
		{
			name:     "cpu-127-highest-bit-word-1",
			cpus:     []int{127},
			expected: []int{127},
		},
		{
			name:     "word-0-all-bits-set",
			cpus:     cpuRange(0, 63),
			expected: cpuRange(0, 63),
		},
		{
			name:     "two-words-all-bits-set",
			cpus:     cpuRange(0, 127),
			expected: cpuRange(0, 127),
		},
		{
			name:     "lowest-and-highest-in-word-0",
			cpus:     []int{0, 63},
			expected: []int{0, 63},
		},
		{
			name:     "straddles-word-boundary-63-and-64",
			cpus:     []int{63, 64},
			expected: []int{63, 64},
		},
		{
			name:     "cpu-1023-highest-bit-word-15",
			cpus:     []int{1023},
			expected: []int{1023},
		},
		{
			name:     "cpu-1024-lowest-bit-word-16",
			cpus:     []int{1024},
			expected: []int{1024},
		},
		{
			name:     "lowest-and-highest-across-16-words",
			cpus:     []int{0, 1023},
			expected: []int{0, 1023},
		},
		{
			name:     "sparse-word-boundary-cpus",
			cpus:     []int{0, 64, 128, 512, 960, 1023},
			expected: []int{0, 64, 128, 512, 960, 1023},
		},
		{
			name:     "large-contiguous-range-512-to-1023",
			cpus:     cpuRange(512, 1023),
			expected: cpuRange(512, 1023),
		},
		{
			name:     "duplicate-cpus-are-deduped",
			cpus:     []int{0, 0, 63, 63, 64, 64},
			expected: []int{0, 63, 64},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewCpuMask(tc.cpus...)
			got := m.List()
			if !slices.Equal(got, tc.expected) {
				t.Errorf("List() = %v, want %v", got, tc.expected)
			}
		})
	}
}

// ---- TestSetAndClear -------------------------------------------------------

func TestSetAndClear(t *testing.T) {
	type step struct {
		set   []int
		clear []int
	}
	tests := []struct {
		name     string
		initial  []int
		steps    []step
		expected []int
	}{
		{
			name:     "set-cpu-0-on-empty",
			steps:    []step{{set: []int{0}}},
			expected: []int{0},
		},
		{
			name:     "set-cpu-63-highest-bit-word-0",
			steps:    []step{{set: []int{63}}},
			expected: []int{63},
		},
		{
			name:     "set-cpu-64-first-bit-word-1",
			steps:    []step{{set: []int{64}}},
			expected: []int{64},
		},
		{
			name:     "set-cpu-1023",
			steps:    []step{{set: []int{1023}}},
			expected: []int{1023},
		},
		{
			name:     "set-cpu-1024",
			steps:    []step{{set: []int{1024}}},
			expected: []int{1024},
		},
		{
			name:     "set-is-idempotent",
			initial:  []int{0},
			steps:    []step{{set: []int{0}}},
			expected: []int{0},
		},
		{
			name:     "set-multiple-across-word-boundaries",
			steps:    []step{{set: []int{0, 63, 64, 1023}}},
			expected: []int{0, 63, 64, 1023},
		},
		{
			name:     "clear-middle-cpu",
			initial:  []int{0, 1, 2},
			steps:    []step{{clear: []int{1}}},
			expected: []int{0, 2},
		},
		{
			name:     "clear-highest-bit-in-word-0",
			initial:  cpuRange(0, 63),
			steps:    []step{{clear: []int{63}}},
			expected: cpuRange(0, 62),
		},
		{
			name:     "clear-lowest-bit-in-word-1",
			initial:  []int{0, 64},
			steps:    []step{{clear: []int{64}}},
			expected: []int{0},
		},
		{
			name:     "clear-cpu-1023",
			initial:  []int{0, 1023},
			steps:    []step{{clear: []int{1023}}},
			expected: []int{0},
		},
		{
			name:     "clear-nonexistent-is-noop",
			initial:  []int{0, 2},
			steps:    []step{{clear: []int{1}}},
			expected: []int{0, 2},
		},
		{
			name:    "clear-out-of-range-is-noop",
			initial: []int{0}, // mask covers only word 0
			// CPU 128 is word 2, well past the end — must not panic.
			steps:    []step{{clear: []int{128}}},
			expected: []int{0},
		},
		{
			name:    "clear-exactly-at-mask-length-boundary",
			initial: []int{0, 64}, // mask has 2 words (indices 0 and 1)
			// CPU 128 → word 2 == len(mask); must be a no-op, not a panic.
			steps:    []step{{clear: []int{128}}},
			expected: []int{0, 64},
		},
		{
			name:     "set-then-clear",
			steps:    []step{{set: []int{0, 1, 2}}, {clear: []int{1}}},
			expected: []int{0, 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewCpuMask(tc.initial...)
			for _, s := range tc.steps {
				for _, cpu := range s.set {
					m.Set(cpu)
				}
				for _, cpu := range s.clear {
					m.Clear(cpu)
				}
			}
			if got := m.List(); !slices.Equal(got, tc.expected) {
				t.Errorf("List() = %v, want %v", got, tc.expected)
			}
		})
	}

	// CpuMask caches String, Key and Size; Set and Clear must invalidate all
	// three, not just the one that happens to be read first.

	t.Run("caches-cleared-after-set", func(t *testing.T) {
		m := NewCpuMask(0)
		primedKey := m.Key()
		_, _ = m.String(), m.Size()

		m.Set(1)

		if got := m.String(); got != "0-1" {
			t.Errorf("String() not invalidated after Set: got %q, want %q", got, "0-1")
		}
		if got := m.Key(); got == primedKey {
			t.Errorf("Key() not invalidated after Set: still %q", got)
		}
		if got := m.Size(); got != 2 {
			t.Errorf("Size() not invalidated after Set: got %d, want 2", got)
		}
		if got, want := m.Key(), NewCpuMask(0, 1).Key(); got != want {
			t.Errorf("Key() = %q, want %q", got, want)
		}
	})

	t.Run("caches-cleared-after-clear", func(t *testing.T) {
		m := NewCpuMask(0, 1)
		primedKey := m.Key()
		_, _ = m.String(), m.Size()

		m.Clear(1)

		if got := m.String(); got != "0" {
			t.Errorf("String() not invalidated after Clear: got %q, want %q", got, "0")
		}
		if got := m.Key(); got == primedKey {
			t.Errorf("Key() not invalidated after Clear: still %q", got)
		}
		if got := m.Size(); got != 1 {
			t.Errorf("Size() not invalidated after Clear: got %d, want 1", got)
		}
		if got, want := m.Key(), NewCpuMask(0).Key(); got != want {
			t.Errorf("Key() = %q, want %q", got, want)
		}
	})

	t.Run("caches-cleared-when-emptied", func(t *testing.T) {
		m := NewCpuMask(0, 1)
		_, _, _ = m.String(), m.Key(), m.Size()

		m.Clear(0, 1)

		if got := m.String(); got != "" {
			t.Errorf("String() = %q, want %q", got, "")
		}
		if got := m.Key(); got != "" {
			t.Errorf("Key() = %q, want %q", got, "")
		}
		if got := m.Size(); got != 0 {
			t.Errorf("Size() = %d, want 0", got)
		}
	})

	// ---- variadic multi-arg Set / Clear calls --------------------------------

	t.Run("set-no-args-is-noop", func(t *testing.T) {
		m := NewCpuMask(5)
		m.Set()
		if got := m.List(); !slices.Equal(got, []int{5}) {
			t.Errorf("Set() no-op: got %v, want [5]", got)
		}
	})

	t.Run("clear-no-args-is-noop", func(t *testing.T) {
		m := NewCpuMask(5)
		m.Clear()
		if got := m.List(); !slices.Equal(got, []int{5}) {
			t.Errorf("Clear() no-op: got %v, want [5]", got)
		}
	})

	t.Run("set-multiple-same-word", func(t *testing.T) {
		m := NewCpuMask()
		m.Set(0, 1, 2, 3, 63)
		want := []int{0, 1, 2, 3, 63}
		if got := m.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("set-multiple-across-word-boundaries", func(t *testing.T) {
		m := NewCpuMask()
		m.Set(0, 63, 64, 127, 128, 1023)
		want := []int{0, 63, 64, 127, 128, 1023}
		if got := m.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("set-multiple-idempotent", func(t *testing.T) {
		m := NewCpuMask(0, 63, 64)
		m.Set(0, 63, 64, 64, 63, 0) // duplicates must not double-set
		want := []int{0, 63, 64}
		if got := m.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("set-multiple-large-range", func(t *testing.T) {
		m := NewCpuMask()
		args := cpuRange(512, 575) // 64 CPUs filling exactly word 8
		m.Set(args...)
		if got := m.List(); !slices.Equal(got, args) {
			t.Errorf("got %v, want %v", got, args)
		}
	})

	t.Run("clear-multiple-same-word", func(t *testing.T) {
		m := NewCpuMask(0, 1, 2, 3, 63)
		m.Clear(1, 2, 63)
		want := []int{0, 3}
		if got := m.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("clear-multiple-across-word-boundaries", func(t *testing.T) {
		m := NewCpuMask(0, 63, 64, 127, 128, 1023)
		m.Clear(63, 64, 1023)
		want := []int{0, 127, 128}
		if got := m.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("clear-multiple-some-absent", func(t *testing.T) {
		m := NewCpuMask(0, 2, 4)
		m.Clear(1, 2, 3) // 1 and 3 are not set — should be no-op for those
		want := []int{0, 4}
		if got := m.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("clear-multiple-out-of-range-mixed-with-valid", func(t *testing.T) {
		m := NewCpuMask(0, 64)
		// 512 is beyond the mask; clearing it must not panic and must be a no-op.
		m.Clear(64, 512)
		want := []int{0}
		if got := m.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("clear-all-via-variadic", func(t *testing.T) {
		cpus := []int{0, 63, 64, 127, 128, 255, 1023}
		m := NewCpuMask(cpus...)
		m.Clear(cpus...)
		if !m.IsEmpty() {
			t.Errorf("expected empty after clearing all CPUs, got %v", m.List())
		}
	})

	t.Run("set-then-clear-multi-arg", func(t *testing.T) {
		m := NewCpuMask()
		m.Set(0, 63, 64, 127, 512, 1023)
		m.Clear(63, 127, 1023)
		want := []int{0, 64, 512}
		if got := m.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// ---- TestClone ------------------------------------------------------------

func TestClone(t *testing.T) {
	t.Run("clone-of-empty", func(t *testing.T) {
		m := NewCpuMask()
		c := m.Clone()
		if !c.IsEmpty() {
			t.Errorf("clone of empty mask is not empty: %v", c.List())
		}
	})

	t.Run("clone-matches-original", func(t *testing.T) {
		cpus := []int{0, 63, 64, 127, 512, 1023}
		m := NewCpuMask(cpus...)
		c := m.Clone()
		if !slices.Equal(c.List(), cpus) {
			t.Errorf("clone content mismatch: want %v, got %v", cpus, c.List())
		}
	})

	t.Run("clone-is-independent-mutation-does-not-affect-original", func(t *testing.T) {
		m := NewCpuMask(0, 1, 2)
		c := m.Clone()
		c.Set(63)
		if m.Contains(63) {
			t.Error("mutating clone propagated to original")
		}
		if !c.Contains(63) {
			t.Error("mutation of clone did not take effect")
		}
	})

	t.Run("clone-of-sealed-mask-is-not-sealed", func(t *testing.T) {
		m := NewCpuMask(0)
		m.Seal()
		c := m.Clone()
		c.Set(1) // must not panic
		if !c.Contains(1) {
			t.Error("clone of sealed mask should be mutable")
		}
	})

	t.Run("clone-of-full-word-mask", func(t *testing.T) {
		m := NewCpuMask(cpuRange(0, 63)...)
		c := m.Clone()
		if !slices.Equal(c.List(), cpuRange(0, 63)) {
			t.Errorf("full-word clone mismatch: got %v", c.List())
		}
	})

	t.Run("clone-of-multi-word-high-cpu-mask", func(t *testing.T) {
		cpus := cpuRange(960, 1023)
		m := NewCpuMask(cpus...)
		c := m.Clone()
		if !slices.Equal(c.List(), cpus) {
			t.Errorf("multi-word clone mismatch: got %v", c.List())
		}
	})
}

// ---- TestSeal -------------------------------------------------------------

func TestSeal(t *testing.T) {
	t.Run("set-on-sealed-mask-panics", func(t *testing.T) {
		m := NewCpuMask(0)
		m.Seal()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from Set on sealed mask, got none")
			}
		}()
		m.Set(1)
	})

	t.Run("clear-on-sealed-mask-panics", func(t *testing.T) {
		m := NewCpuMask(0, 1)
		m.Seal()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from Clear on sealed mask, got none")
			}
		}()
		m.Clear(0)
	})

	t.Run("read-only-operations-work-on-sealed-mask", func(t *testing.T) {
		m := NewCpuMask(0, 63, 64, 1023)
		m.Seal()
		if !m.Contains(63) {
			t.Error("Contains should work on sealed mask")
		}
		if m.Size() != 4 {
			t.Errorf("Size should work on sealed mask: got %d, want 4", m.Size())
		}
		if m.IsEmpty() {
			t.Error("IsEmpty should work on sealed mask")
		}
		if !slices.Equal(m.List(), []int{0, 63, 64, 1023}) {
			t.Errorf("List should work on sealed mask: got %v", m.List())
		}
	})
}

// ---- TestIsDenseIsSparse ---------------------------------------------

func TestIsDenseIsSparse(t *testing.T) {
	t.Run("cpumask-is-dense-not-sparse", func(t *testing.T) {
		m := NewCpuMask(0, 1, 2)
		if !m.IsDense() {
			t.Error("CpuMask.IsDense() = false, want true")
		}
		if m.IsSparse() {
			t.Error("CpuMask.IsSparse() = true, want false")
		}
	})

	t.Run("cpuset-is-sparse-not-dense", func(t *testing.T) {
		s := NewCpuSet(0, 1, 2)
		if s.IsDense() {
			t.Error("CpuSet.IsDense() = true, want false")
		}
		if !s.IsSparse() {
			t.Error("CpuSet.IsSparse() = false, want true")
		}
	})
}

// ---- TestIsEmpty ------------------------------------------------------

func TestIsEmpty(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected bool
	}{
		{name: "empty", cpus: []int{}, expected: true},
		{name: "single-cpu-0", cpus: []int{0}, expected: false},
		{name: "single-cpu-63", cpus: []int{63}, expected: false},
		{name: "single-cpu-64", cpus: []int{64}, expected: false},
		{name: "word-0-all-bits-set", cpus: cpuRange(0, 63), expected: false},
		{name: "single-cpu-1023", cpus: []int{1023}, expected: false},
		{name: "multi-word-sparse", cpus: []int{0, 64, 512, 1023}, expected: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewCpuMask(tc.cpus...)
			if got := m.IsEmpty(); got != tc.expected {
				t.Errorf("IsEmpty() = %v, want %v", got, tc.expected)
			}
		})
	}

	t.Run("empty-after-clearing-all-bits", func(t *testing.T) {
		m := NewCpuMask(0, 1, 63, 64, 1023)
		for _, cpu := range m.List() {
			m.Clear(cpu)
		}
		if !m.IsEmpty() {
			t.Errorf("mask should be empty after clearing all bits, got %v", m.List())
		}
	})

	t.Run("empty-with-trailing-zero-words", func(t *testing.T) {
		// Set then clear a high CPU to leave trailing zero words in the mask array.
		m := NewCpuMask(0, 1023)
		m.Clear(1023)
		m.Clear(0)
		if !m.IsEmpty() {
			t.Errorf("mask with only zero words should report empty, got %v", m.List())
		}
	})
}

// ---- TestSize ---------------------------------------------------------

func TestSize(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected int
	}{
		{name: "empty", cpus: []int{}, expected: 0},
		{name: "single-cpu-0", cpus: []int{0}, expected: 1},
		{name: "single-cpu-63-highest-word-0", cpus: []int{63}, expected: 1},
		{name: "single-cpu-64-lowest-word-1", cpus: []int{64}, expected: 1},
		{name: "single-cpu-127-highest-word-1", cpus: []int{127}, expected: 1},
		{name: "word-0-all-64-bits", cpus: cpuRange(0, 63), expected: 64},
		{name: "two-words-all-128-bits", cpus: cpuRange(0, 127), expected: 128},
		{name: "single-cpu-1023", cpus: []int{1023}, expected: 1},
		{name: "single-cpu-1024", cpus: []int{1024}, expected: 1},
		{name: "large-range-512-to-1023", cpus: cpuRange(512, 1023), expected: 512},
		{name: "sparse-boundary-cpus", cpus: []int{0, 63, 64, 127, 512, 1023}, expected: 6},
		{name: "all-cpus-0-to-1023", cpus: cpuRange(0, 1023), expected: 1024},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewCpuMask(tc.cpus...)
			if got := m.Size(); got != tc.expected {
				t.Errorf("Size() = %d, want %d", got, tc.expected)
			}
		})
	}
}

// ---- TestContains -----------------------------------------------------

func TestContains(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		check    int
		expected bool
	}{
		{name: "empty-mask-check-cpu-0", cpus: []int{}, check: 0, expected: false},
		{name: "cpu-0-present", cpus: []int{0}, check: 0, expected: true},
		{name: "cpu-0-absent", cpus: []int{1}, check: 0, expected: false},
		{name: "cpu-63-present", cpus: []int{63}, check: 63, expected: true},
		{name: "cpu-63-absent", cpus: []int{62}, check: 63, expected: false},
		{name: "cpu-64-present", cpus: []int{64}, check: 64, expected: true},
		{name: "cpu-64-absent-only-63-set", cpus: []int{63}, check: 64, expected: false},
		{name: "cpu-63-absent-only-64-set", cpus: []int{64}, check: 63, expected: false},
		{name: "word-boundary-63-and-64-check-63", cpus: []int{63, 64}, check: 63, expected: true},
		{name: "word-boundary-63-and-64-check-64", cpus: []int{63, 64}, check: 64, expected: true},
		{name: "cpu-1023-present", cpus: []int{1023}, check: 1023, expected: true},
		{name: "cpu-1023-absent", cpus: []int{1022}, check: 1023, expected: false},
		{name: "check-beyond-mask-range", cpus: []int{0}, check: 1023, expected: false},
		{name: "middle-of-full-word", cpus: cpuRange(0, 63), check: 32, expected: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewCpuMask(tc.cpus...)
			if got := m.Contains(tc.check); got != tc.expected {
				t.Errorf("Contains(%d) = %v, want %v", tc.check, got, tc.expected)
			}
		})
	}

	// ---- variadic multi-arg / zero-arg Contains calls ------------------------

	t.Run("contains-no-args-is-vacuously-true", func(t *testing.T) {
		m := NewCpuMask(0, 1, 2)
		if !m.Contains() {
			t.Error("Contains() with no args should be true")
		}
		if got := NewCpuMask().Contains(); !got {
			t.Error("Contains() on empty mask with no args should be true")
		}
	})

	t.Run("contains-multiple-all-present", func(t *testing.T) {
		m := NewCpuMask(0, 63, 64, 1023)
		if !m.Contains(0, 63, 64, 1023) {
			t.Error("Contains(0, 63, 64, 1023) = false, want true")
		}
	})

	t.Run("contains-multiple-one-missing", func(t *testing.T) {
		m := NewCpuMask(0, 63, 64)
		if m.Contains(0, 63, 64, 1023) {
			t.Error("Contains(0, 63, 64, 1023) = true, want false (1023 absent)")
		}
	})

	t.Run("contains-multiple-duplicates", func(t *testing.T) {
		m := NewCpuMask(0, 64)
		if !m.Contains(0, 0, 64, 64) {
			t.Error("Contains with duplicate CPUs = false, want true")
		}
	})
}

// ---- TestString -------------------------------------------------------

func TestString(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected string
	}{
		{name: "empty", cpus: []int{}, expected: ""},
		{name: "single-cpu-0", cpus: []int{0}, expected: "0"},
		{name: "single-cpu-63", cpus: []int{63}, expected: "63"},
		{name: "single-cpu-64", cpus: []int{64}, expected: "64"},
		{name: "single-cpu-1023", cpus: []int{1023}, expected: "1023"},
		{name: "single-cpu-1024", cpus: []int{1024}, expected: "1024"},
		{name: "full-word-0", cpus: cpuRange(0, 63), expected: "0-63"},
		{name: "straddles-word-boundary-63-64", cpus: []int{63, 64}, expected: "63-64"},
		{name: "two-full-words-0-to-127", cpus: cpuRange(0, 127), expected: "0-127"},
		{name: "lowest-and-highest-of-two-words", cpus: []int{0, 63, 64, 127}, expected: "0,63-64,127"},
		{name: "sparse-word-boundary-cpus", cpus: []int{0, 64, 128, 192}, expected: "0,64,128,192"},
		{name: "low-and-very-high", cpus: []int{0, 1023}, expected: "0,1023"},
		{name: "large-contiguous-range-512-to-1023", cpus: cpuRange(512, 1023), expected: "512-1023"},
		{name: "mixed-ranges-across-words", cpus: []int{0, 1, 2, 64, 65, 128}, expected: "0-2,64-65,128"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewCpuMask(tc.cpus...)
			got := m.String()
			if got != tc.expected {
				t.Errorf("String() = %q, want %q", got, tc.expected)
			}
			// Second call must return the cached value unchanged.
			if got2 := m.String(); got2 != got {
				t.Errorf("cached String() = %q, first was %q", got2, got)
			}
		})
	}
}

// ---- TestListAndUnsortedList -----------------------------------------------

func TestListAndUnsortedList(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected []int
	}{
		{name: "empty", cpus: []int{}, expected: []int{}},
		{name: "single-cpu-0", cpus: []int{0}, expected: []int{0}},
		{name: "out-of-order-input-sorted-output", cpus: []int{3, 1, 2, 0}, expected: []int{0, 1, 2, 3}},
		{name: "multi-word-sparse", cpus: []int{0, 64, 127, 512, 1023}, expected: []int{0, 64, 127, 512, 1023}},
		{name: "full-word-0", cpus: cpuRange(0, 63), expected: cpuRange(0, 63)},
		{name: "large-range-960-to-1023", cpus: cpuRange(960, 1023), expected: cpuRange(960, 1023)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewCpuMask(tc.cpus...)
			if got := m.List(); !slices.Equal(got, tc.expected) {
				t.Errorf("List() = %v, want %v", got, tc.expected)
			}
			// UnsortedList only promises the same elements, not an order, so
			// sort before comparing. (CpuMask happens to return them sorted,
			// but nothing in the CPUSet contract requires that.)
			unsorted := m.UnsortedList()
			slices.Sort(unsorted)
			if !slices.Equal(unsorted, tc.expected) {
				t.Errorf("UnsortedList() (sorted) = %v, want %v", unsorted, tc.expected)
			}
		})
	}
}

// ---- TestForEachCpu -------------------------------------------------------

func TestForEachCpu(t *testing.T) {
	t.Run("empty-mask-f-never-called", func(t *testing.T) {
		m := NewCpuMask()
		called := false
		m.ForEachCpu(func(_ int) bool {
			called = true
			return true
		})
		if called {
			t.Error("ForEachCpu called f on empty mask")
		}
	})

	t.Run("single-cpu", func(t *testing.T) {
		m := NewCpuMask(42)
		var visited []int
		m.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		if !slices.Equal(visited, []int{42}) {
			t.Errorf("expected [42], got %v", visited)
		}
	})

	t.Run("ascending-order", func(t *testing.T) {
		want := []int{0, 7, 63, 64, 127, 512, 1023}
		m := NewCpuMask(want...)
		var visited []int
		m.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		if !slices.Equal(visited, want) {
			t.Errorf("expected %v, got %v", want, visited)
		}
	})

	t.Run("full-word-0-visits-all-64", func(t *testing.T) {
		m := NewCpuMask(cpuRange(0, 63)...)
		var visited []int
		m.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		if !slices.Equal(visited, cpuRange(0, 63)) {
			t.Errorf("expected CPUs 0-63, got %v", visited)
		}
	})

	t.Run("sparse-bits-within-one-word", func(t *testing.T) {
		// Several CPUs spread across a single word: iteration must skip the
		// zero bits between them and visit each set bit exactly once.
		want := []int{0, 16, 32, 48}
		m := NewCpuMask(want...)
		var visited []int
		m.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		if !slices.Equal(visited, want) {
			t.Errorf("expected %v, got %v", want, visited)
		}
	})

	t.Run("early-termination-stops-iteration", func(t *testing.T) {
		m := NewCpuMask(0, 63, 64, 1023)
		var visited []int
		m.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return len(visited) < 2 // stop after two CPUs
		})
		if !slices.Equal(visited, []int{0, 63}) {
			t.Errorf("expected [0, 63], got %v", visited)
		}
	})

	t.Run("high-cpu-numbers-960-to-1023", func(t *testing.T) {
		want := cpuRange(960, 1023)
		m := NewCpuMask(want...)
		var visited []int
		m.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		if !slices.Equal(visited, want) {
			t.Errorf("expected CPUs 960-1023, got %v", visited)
		}
	})

	t.Run("word-boundary-highest-bits", func(t *testing.T) {
		// Highest bit of each of the first four words.
		want := []int{63, 127, 191, 255}
		m := NewCpuMask(want...)
		var visited []int
		m.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		if !slices.Equal(visited, want) {
			t.Errorf("expected %v, got %v", want, visited)
		}
	})
}

// ---- TestDifference ---------------------------------------------------

func TestDifference(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected []int
	}{
		// *CpuMask fast path
		{
			name:     "mask: empty-minus-empty",
			a:        []int{},
			b:        NewCpuMask(),
			expected: []int{},
		},
		{
			name:     "mask: empty-minus-nonempty",
			a:        []int{},
			b:        NewCpuMask(0, 1, 2),
			expected: []int{},
		},
		{
			name:     "mask: nonempty-minus-empty",
			a:        []int{0, 1, 2},
			b:        NewCpuMask(),
			expected: []int{0, 1, 2},
		},
		{
			name:     "mask: a-minus-a-equals-empty",
			a:        []int{0, 1, 2},
			b:        NewCpuMask(0, 1, 2),
			expected: []int{},
		},
		{
			name:     "mask: disjoint-sets",
			a:        []int{0, 2, 4},
			b:        NewCpuMask(1, 3, 5),
			expected: []int{0, 2, 4},
		},
		{
			name:     "mask: superset-minus-subset",
			a:        []int{0, 1, 2, 3},
			b:        NewCpuMask(1, 2),
			expected: []int{0, 3},
		},
		{
			name:     "mask: subset-minus-superset-equals-empty",
			a:        []int{1, 2},
			b:        NewCpuMask(0, 1, 2, 3),
			expected: []int{},
		},
		{
			name:     "mask: a-longer-than-b-extra-a-words-kept",
			a:        []int{0, 64, 128, 1023},
			b:        NewCpuMask(64),
			expected: []int{0, 128, 1023},
		},
		{
			name:     "mask: b-longer-than-a-extra-b-words-ignored",
			a:        []int{0, 64},
			b:        NewCpuMask(64, 128, 1023),
			expected: []int{0},
		},
		{
			name:     "mask: full-word-0-minus-word-1-leaves-word-0-intact",
			a:        cpuRange(0, 63),
			b:        NewCpuMask(cpuRange(64, 127)...),
			expected: cpuRange(0, 63),
		},
		{
			name:     "mask: high-cpus-partial-difference",
			a:        cpuRange(960, 1023),
			b:        NewCpuMask(cpuRange(992, 1023)...),
			expected: cpuRange(960, 991),
		},
		// testCPUSet fallback path
		{
			name:     "non-mask: basic-difference",
			a:        []int{0, 1, 2, 3},
			b:        newTestCPUSet(1, 2),
			expected: []int{0, 3},
		},
		{
			name:     "non-mask: empty-a",
			a:        []int{},
			b:        newTestCPUSet(0, 1),
			expected: []int{},
		},
		{
			name:     "non-mask: empty-b",
			a:        []int{0, 1},
			b:        newTestCPUSet(),
			expected: []int{0, 1},
		},
		{
			name:     "non-mask: high-cpus",
			a:        cpuRange(512, 1023),
			b:        newTestCPUSet(cpuRange(768, 1023)...),
			expected: cpuRange(512, 767),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuMask(tc.a...)
			got := a.Difference(tc.b)
			exp := NewCpuMask(tc.expected...)
			if !maskListEqual(got, exp) {
				t.Errorf("Difference() = %v, want %v", got.List(), tc.expected)
			}
		})
	}
}

// ---- TestEquals -------------------------------------------------------

func TestEquals(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected bool
	}{
		// *CpuMask fast path
		{
			name:     "mask: both-empty",
			a:        []int{},
			b:        NewCpuMask(),
			expected: true,
		},
		{
			name:     "mask: same-single-cpu-0",
			a:        []int{0},
			b:        NewCpuMask(0),
			expected: true,
		},
		{
			name:     "mask: different-single-cpu",
			a:        []int{0},
			b:        NewCpuMask(1),
			expected: false,
		},
		{
			name:     "mask: one-empty-one-not",
			a:        []int{0},
			b:        NewCpuMask(),
			expected: false,
		},
		{
			name:     "mask: same-full-word-0",
			a:        cpuRange(0, 63),
			b:        NewCpuMask(cpuRange(0, 63)...),
			expected: true,
		},
		{
			name:     "mask: same-multi-word",
			a:        []int{0, 64, 128, 1023},
			b:        NewCpuMask(0, 64, 128, 1023),
			expected: true,
		},
		{
			name:     "mask: different-multi-word",
			a:        []int{0, 64},
			b:        NewCpuMask(0, 128),
			expected: false,
		},
		{
			name:     "mask: high-cpus-equal",
			a:        cpuRange(960, 1023),
			b:        NewCpuMask(cpuRange(960, 1023)...),
			expected: true,
		},
		{
			name:     "mask: high-cpus-differ-by-one",
			a:        cpuRange(960, 1022),
			b:        NewCpuMask(cpuRange(960, 1023)...),
			expected: false,
		},
		{
			// m (built from a) has fewer words than b; b's extra high word is
			// all zero, so the sets are still equal. Exercises the
			// `case c < len(other.mask)` branch in the fast path with a true
			// outcome.
			name: "mask: other-has-extra-all-zero-word-still-equal",
			a:    []int{0},
			b: func() CPUSet {
				o := NewCpuMask(0, 1024)
				o.Clear(1024)
				return o
			}(),
			expected: true,
		},
		{
			// Same as above, but b's extra high word has a bit set, so the
			// sets differ. Exercises the `case c < len(other.mask)` branch
			// with a false outcome.
			name:     "mask: other-has-extra-nonzero-word-not-equal",
			a:        []int{0},
			b:        NewCpuMask(0, 1024),
			expected: false,
		},
		// testCPUSet fallback path
		{
			name:     "non-mask: equal",
			a:        []int{0, 1, 2},
			b:        newTestCPUSet(0, 1, 2),
			expected: true,
		},
		{
			name:     "non-mask: not-equal-different-cpu",
			a:        []int{0, 1},
			b:        newTestCPUSet(0, 2),
			expected: false,
		},
		{
			name:     "non-mask: not-equal-different-size",
			a:        []int{0, 1, 2},
			b:        newTestCPUSet(0, 1),
			expected: false,
		},
		{
			name:     "non-mask: both-empty",
			a:        []int{},
			b:        newTestCPUSet(),
			expected: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuMask(tc.a...)
			if got := a.Equals(tc.b); got != tc.expected {
				t.Errorf("Equals() = %v, want %v", got, tc.expected)
			}
		})
	}

	t.Run("mask: trailing-zero-words-equal-shorter-mask", func(t *testing.T) {
		// Set CPU 64 then clear it, leaving a trailing zero word in the array.
		a := NewCpuMask(0, 64)
		a.Clear(64)        // a.mask = [0x1, 0x0]
		b := NewCpuMask(0) // b.mask = [0x1]
		if !a.Equals(b) {
			t.Error("mask with trailing zero word should equal shorter mask with same bits")
		}
		if !b.Equals(a) {
			t.Error("equality should be symmetric for trailing-zero case")
		}
	})
}

// ---- TestIntersection -------------------------------------------------

func TestIntersection(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected []int
	}{
		// *CpuMask fast path
		{
			name:     "mask: both-empty",
			a:        []int{},
			b:        NewCpuMask(),
			expected: []int{},
		},
		{
			name:     "mask: one-empty",
			a:        []int{0, 1, 2},
			b:        NewCpuMask(),
			expected: []int{},
		},
		{
			name:     "mask: no-overlap",
			a:        []int{0, 2},
			b:        NewCpuMask(1, 3),
			expected: []int{},
		},
		{
			name:     "mask: full-overlap-single-word",
			a:        cpuRange(0, 63),
			b:        NewCpuMask(cpuRange(0, 63)...),
			expected: cpuRange(0, 63),
		},
		{
			name:     "mask: partial-overlap-single-word",
			a:        []int{0, 1, 2},
			b:        NewCpuMask(1, 2, 3),
			expected: []int{1, 2},
		},
		{
			name:     "mask: a-shorter-than-b-extra-b-words-ignored",
			a:        []int{0},
			b:        NewCpuMask(0, 64),
			expected: []int{0},
		},
		{
			name:     "mask: b-shorter-than-a-extra-a-words-dropped",
			a:        []int{0, 64},
			b:        NewCpuMask(0),
			expected: []int{0},
		},
		{
			name:     "mask: multi-word-overlap",
			a:        []int{0, 64, 128, 512},
			b:        NewCpuMask(64, 128, 256, 512),
			expected: []int{64, 128, 512},
		},
		{
			name:     "mask: word-boundary-63-and-64",
			a:        []int{63, 64},
			b:        NewCpuMask(63, 64),
			expected: []int{63, 64},
		},
		{
			name:     "mask: high-cpus-partial-overlap",
			a:        cpuRange(512, 1023),
			b:        NewCpuMask(cpuRange(960, 1023)...),
			expected: cpuRange(960, 1023),
		},
		// testCPUSet fallback path
		{
			name:     "non-mask: partial-overlap",
			a:        []int{0, 1, 2},
			b:        newTestCPUSet(1, 2, 3),
			expected: []int{1, 2},
		},
		{
			name:     "non-mask: empty-a",
			a:        []int{},
			b:        newTestCPUSet(0, 1),
			expected: []int{},
		},
		{
			name:     "non-mask: empty-b",
			a:        []int{0, 1},
			b:        newTestCPUSet(),
			expected: []int{},
		},
		{
			name:     "non-mask: high-cpus",
			a:        cpuRange(512, 1023),
			b:        newTestCPUSet(cpuRange(960, 1023)...),
			expected: cpuRange(960, 1023),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuMask(tc.a...)
			got := a.Intersection(tc.b)
			exp := NewCpuMask(tc.expected...)
			if !maskListEqual(got, exp) {
				t.Errorf("Intersection() = %v, want %v", got.List(), tc.expected)
			}
		})
	}
}

// ---- TestIntersects ---------------------------------------------------

func TestIntersects(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected bool
	}{
		// *CpuMask fast path
		{name: "mask: both-empty", a: []int{}, b: NewCpuMask(), expected: false},
		{name: "mask: a-empty", a: []int{}, b: NewCpuMask(0, 1), expected: false},
		{name: "mask: b-empty", a: []int{0, 1}, b: NewCpuMask(), expected: false},
		{name: "mask: identical", a: []int{0, 1}, b: NewCpuMask(0, 1), expected: true},
		{name: "mask: single-common-cpu", a: []int{0, 2}, b: NewCpuMask(2, 4), expected: true},
		{name: "mask: no-overlap-same-word", a: []int{0, 2}, b: NewCpuMask(1, 3), expected: false},
		{name: "mask: overlap-only-in-word-0", a: []int{0, 64}, b: NewCpuMask(0, 128), expected: true},
		{name: "mask: overlap-only-in-high-word", a: []int{0, 1023}, b: NewCpuMask(1, 1023), expected: true},
		{name: "mask: word-boundary-63-vs-64", a: []int{63}, b: NewCpuMask(64), expected: false},
		{name: "mask: word-boundary-63-and-64", a: []int{63, 64}, b: NewCpuMask(64), expected: true},
		{name: "mask: disjoint-words-entirely", a: cpuRange(0, 63), b: NewCpuMask(cpuRange(64, 127)...), expected: false},
		// a is shorter than b: b's extra high words must not be consulted
		{name: "mask: a-shorter-no-overlap", a: []int{0}, b: NewCpuMask(64, 1023), expected: false},
		{name: "mask: a-shorter-with-overlap", a: []int{0}, b: NewCpuMask(0, 1023), expected: true},
		// a is longer than b: a's extra high words must not be consulted
		{name: "mask: a-longer-no-overlap", a: []int{64, 1023}, b: NewCpuMask(0), expected: false},
		{name: "mask: a-longer-with-overlap", a: []int{0, 1023}, b: NewCpuMask(0), expected: true},
		{name: "mask: high-cpus-partial-overlap", a: cpuRange(960, 1000), b: NewCpuMask(cpuRange(1000, 1023)...), expected: true},
		{name: "mask: high-cpus-no-overlap", a: cpuRange(960, 999), b: NewCpuMask(cpuRange(1000, 1023)...), expected: false},
		// testCPUSet fallback path
		{name: "non-mask: overlap", a: []int{0, 1, 2}, b: newTestCPUSet(2, 3), expected: true},
		{name: "non-mask: no-overlap", a: []int{0, 2}, b: newTestCPUSet(1, 3), expected: false},
		{name: "non-mask: b-empty", a: []int{0, 1}, b: newTestCPUSet(), expected: false},
		{name: "non-mask: a-empty", a: []int{}, b: newTestCPUSet(0, 1), expected: false},
		{name: "non-mask: high-cpus-overlap", a: cpuRange(512, 1023), b: newTestCPUSet(1023), expected: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuMask(tc.a...)
			if got := a.Intersects(tc.b); got != tc.expected {
				t.Errorf("Intersects() = %v, want %v", got, tc.expected)
			}
			// Intersects must agree with Intersection being non-empty.
			if got := !a.Intersection(tc.b).IsEmpty(); got != tc.expected {
				t.Errorf("Intersection().IsEmpty() implies %v, want %v", got, tc.expected)
			}
		})
	}

	t.Run("mask: trailing-zero-words-do-not-imply-overlap", func(t *testing.T) {
		// a keeps a trailing zero word after clearing its only high CPU.
		a := NewCpuMask(0, 64)
		a.Clear(64)
		if a.Intersects(NewCpuMask(64)) {
			t.Error("mask with a trailing zero word should not intersect CPU 64")
		}
		if !a.Intersects(NewCpuMask(0)) {
			t.Error("mask should still intersect CPU 0")
		}
	})
}

// ---- TestKey ----------------------------------------------------------

func TestKey(t *testing.T) {
	t.Run("equal-sets-share-a-key", func(t *testing.T) {
		tests := []struct {
			name string
			cpus []int
		}{
			{name: "empty", cpus: []int{}},
			{name: "single-cpu-0", cpus: []int{0}},
			{name: "single-cpu-63", cpus: []int{63}},
			{name: "single-cpu-64", cpus: []int{64}},
			{name: "full-word-0", cpus: cpuRange(0, 63)},
			{name: "straddles-word-boundary", cpus: []int{63, 64}},
			{name: "multi-word-sparse", cpus: []int{0, 64, 512, 1023}},
			{name: "high-cpus", cpus: cpuRange(960, 1023)},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				a, b := NewCpuMask(tc.cpus...), NewCpuMask(tc.cpus...)
				if a.Key() != b.Key() {
					t.Errorf("equal sets have different keys: %q vs %q", a.Key(), b.Key())
				}
				// The key must be stable across calls.
				if first, second := a.Key(), a.Key(); first != second {
					t.Errorf("Key() not stable: %q then %q", first, second)
				}
			})
		}
	})

	t.Run("different-sets-have-different-keys", func(t *testing.T) {
		tests := []struct {
			name string
			a, b []int
		}{
			{name: "adjacent-cpus", a: []int{0}, b: []int{1}},
			{name: "across-word-boundary", a: []int{63}, b: []int{64}},
			{name: "subset", a: []int{0, 1}, b: []int{0, 1, 2}},
			{name: "different-high-word", a: []int{0, 64}, b: []int{0, 128}},
			{name: "empty-vs-nonempty", a: []int{}, b: []int{0}},
			{name: "high-cpus-differ-by-one", a: cpuRange(960, 1022), b: cpuRange(960, 1023)},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				a, b := NewCpuMask(tc.a...), NewCpuMask(tc.b...)
				if a.Key() == b.Key() {
					t.Errorf("different sets share key %q", a.Key())
				}
			})
		}
	})

	t.Run("key-ignores-trailing-zero-words", func(t *testing.T) {
		// Equal sets whose masks have a different number of words must still
		// produce the same key.
		grown := NewCpuMask(0, 1023)
		grown.Clear(1023)
		if got, want := grown.Key(), NewCpuMask(0).Key(); got != want {
			t.Errorf("Key() = %q, want %q", got, want)
		}

		// Difference allocates len(receiver.mask) words and does not trim.
		diffed := NewCpuMask(0, 1023).Difference(NewCpuMask(1023))
		if got, want := diffed.Key(), NewCpuMask(0).Key(); got != want {
			t.Errorf("Key() after Difference = %q, want %q", got, want)
		}

		// An all-zero mask must key the same as a never-grown empty one.
		emptied := NewCpuMask(1023)
		emptied.Clear(1023)
		if got, want := emptied.Key(), NewCpuMask().Key(); got != want {
			t.Errorf("emptied Key() = %q, want %q", got, want)
		}
	})

	t.Run("empty-key-is-empty-string", func(t *testing.T) {
		if got := NewCpuMask().Key(); got != "" {
			t.Errorf("Key() = %q, want %q", got, "")
		}
	})
}

// ---- TestIsSubsetOf ---------------------------------------------------

func TestIsSubsetOf(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected bool
	}{
		// *CpuMask fast path
		{
			name:     "mask: empty-is-subset-of-empty",
			a:        []int{},
			b:        NewCpuMask(),
			expected: true,
		},
		{
			name:     "mask: empty-is-subset-of-nonempty",
			a:        []int{},
			b:        NewCpuMask(0, 1),
			expected: true,
		},
		{
			name:     "mask: nonempty-is-not-subset-of-empty",
			a:        []int{0},
			b:        NewCpuMask(),
			expected: false,
		},
		{
			name:     "mask: set-is-subset-of-itself",
			a:        []int{0, 1, 2},
			b:        NewCpuMask(0, 1, 2),
			expected: true,
		},
		{
			name:     "mask: proper-subset",
			a:        []int{0, 1},
			b:        NewCpuMask(0, 1, 2),
			expected: true,
		},
		{
			name:     "mask: not-subset-has-extra-cpu",
			a:        []int{0, 3},
			b:        NewCpuMask(0, 1, 2),
			expected: false,
		},
		{
			name:     "mask: word-0-all-bits-subset-of-0-to-127",
			a:        cpuRange(0, 63),
			b:        NewCpuMask(cpuRange(0, 127)...),
			expected: true,
		},
		{
			name:     "mask: word-0-not-subset-of-word-1",
			a:        cpuRange(0, 63),
			b:        NewCpuMask(cpuRange(64, 127)...),
			expected: false,
		},
		{
			name:     "mask: a-longer-extra-nonzero-word-not-subset",
			a:        []int{0, 128},
			b:        NewCpuMask(0, 64),
			expected: false,
		},
		{
			name:     "mask: high-cpus-proper-subset",
			a:        cpuRange(992, 1023),
			b:        NewCpuMask(cpuRange(960, 1023)...),
			expected: true,
		},
		{
			name:     "mask: high-cpu-outside-superset-not-subset",
			a:        []int{0, 1023},
			b:        NewCpuMask(cpuRange(960, 1023)...),
			expected: false,
		},
		// testCPUSet fallback path
		{
			name:     "non-mask: proper-subset",
			a:        []int{0, 1},
			b:        newTestCPUSet(0, 1, 2),
			expected: true,
		},
		{
			name:     "non-mask: not-subset",
			a:        []int{0, 3},
			b:        newTestCPUSet(0, 1, 2),
			expected: false,
		},
		{
			name:     "non-mask: empty-is-subset",
			a:        []int{},
			b:        newTestCPUSet(0, 1),
			expected: true,
		},
		{
			name:     "non-mask: high-cpus-subset",
			a:        cpuRange(992, 1023),
			b:        newTestCPUSet(cpuRange(960, 1023)...),
			expected: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuMask(tc.a...)
			if got := a.IsSubsetOf(tc.b); got != tc.expected {
				t.Errorf("IsSubsetOf() = %v, want %v", got, tc.expected)
			}
		})
	}

	t.Run("mask: a-longer-with-trailing-zero-words-is-still-subset", func(t *testing.T) {
		// a has trailing zero words; it should still report as a subset.
		a := NewCpuMask(0, 64)
		a.Clear(64) // a.mask = [0x1, 0x0]
		b := NewCpuMask(0, 1)
		if !a.IsSubsetOf(b) {
			t.Error("mask with trailing zero words should still be recognised as a subset")
		}
	})
}

// ---- TestUnion --------------------------------------------------------

func TestUnion(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		others   []CPUSet
		expected []int
	}{
		// *CpuMask fast path
		{
			name:     "mask: empty-union-empty",
			a:        []int{},
			others:   []CPUSet{NewCpuMask()},
			expected: []int{},
		},
		{
			name:     "mask: a-union-empty-returns-a",
			a:        []int{0, 1},
			others:   []CPUSet{NewCpuMask()},
			expected: []int{0, 1},
		},
		{
			name:     "mask: empty-union-b-returns-b",
			a:        []int{},
			others:   []CPUSet{NewCpuMask(0, 1)},
			expected: []int{0, 1},
		},
		{
			name:     "mask: disjoint-single-word",
			a:        []int{0},
			others:   []CPUSet{NewCpuMask(1)},
			expected: []int{0, 1},
		},
		{
			name:     "mask: overlapping",
			a:        []int{0, 1},
			others:   []CPUSet{NewCpuMask(1, 2)},
			expected: []int{0, 1, 2},
		},
		{
			name:     "mask: a-shorter-than-b-includes-b-words",
			a:        []int{0},
			others:   []CPUSet{NewCpuMask(64, 128)},
			expected: []int{0, 64, 128},
		},
		{
			name:     "mask: a-longer-than-b-keeps-extra-a-words",
			a:        []int{0, 64, 128},
			others:   []CPUSet{NewCpuMask(0)},
			expected: []int{0, 64, 128},
		},
		{
			name:     "mask: word-boundary-63-and-64",
			a:        []int{63},
			others:   []CPUSet{NewCpuMask(64)},
			expected: []int{63, 64},
		},
		{
			name:     "mask: multiple-others",
			a:        []int{0},
			others:   []CPUSet{NewCpuMask(64), NewCpuMask(128)},
			expected: []int{0, 64, 128},
		},
		{
			name:     "mask: large-range-union",
			a:        cpuRange(512, 767),
			others:   []CPUSet{NewCpuMask(cpuRange(768, 1023)...)},
			expected: cpuRange(512, 1023),
		},
		// zero-argument union must return a copy of m
		{
			name:     "mask: no-others-returns-copy-of-a",
			a:        []int{0, 63, 64, 1023},
			others:   nil,
			expected: []int{0, 63, 64, 1023},
		},
		{
			name:     "mask: no-others-empty-a-returns-empty",
			a:        []int{},
			others:   nil,
			expected: []int{},
		},
		// testCPUSet fallback path — m's CPUs must be included
		{
			name:     "non-mask: a-union-b",
			a:        []int{0, 1},
			others:   []CPUSet{newTestCPUSet(2, 3)},
			expected: []int{0, 1, 2, 3},
		},
		{
			name:     "non-mask: overlapping",
			a:        []int{0, 1},
			others:   []CPUSet{newTestCPUSet(1, 2)},
			expected: []int{0, 1, 2},
		},
		{
			name:     "non-mask: empty-b-returns-a",
			a:        []int{0, 1},
			others:   []CPUSet{newTestCPUSet()},
			expected: []int{0, 1},
		},
		{
			name:     "non-mask: empty-a-union-b-returns-b",
			a:        []int{},
			others:   []CPUSet{newTestCPUSet(0, 1)},
			expected: []int{0, 1},
		},
		{
			name:     "non-mask: high-cpus",
			a:        []int{0},
			others:   []CPUSet{newTestCPUSet(1023)},
			expected: []int{0, 1023},
		},
		// mixed CpuMask and testCPUSet in others
		{
			name:     "mixed: cpumask-and-non-mask-others",
			a:        []int{0},
			others:   []CPUSet{NewCpuMask(64), newTestCPUSet(128)},
			expected: []int{0, 64, 128},
		},
		{
			name:     "mixed: non-mask-then-cpumask-others",
			a:        []int{0},
			others:   []CPUSet{newTestCPUSet(64), NewCpuMask(128)},
			expected: []int{0, 64, 128},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuMask(tc.a...)
			got := a.Union(tc.others...)
			exp := NewCpuMask(tc.expected...)
			if !maskListEqual(got, exp) {
				t.Errorf("Union() = %v, want %v", got.List(), tc.expected)
			}
		})
	}

	// The zero-argument case is documented as returning a copy, so the result
	// must not share storage with the receiver.
	t.Run("mask: no-others-result-is-independent-of-a", func(t *testing.T) {
		a := NewCpuMask(0, 63, 64)
		got := a.Union()
		got.Set(1023)
		if a.Contains(1023) {
			t.Error("mutating the Union() result propagated to the receiver")
		}
		if !got.Contains(1023) {
			t.Error("mutating the Union() result had no effect")
		}
	})

	// Union must never modify its operands either.
	t.Run("mask: operands-are-not-modified", func(t *testing.T) {
		a := NewCpuMask(0, 1)
		b := NewCpuMask(64)
		_ = a.Union(b)
		if got, want := a.List(), []int{0, 1}; !slices.Equal(got, want) {
			t.Errorf("receiver modified: %v, want %v", got, want)
		}
		if got, want := b.List(), []int{64}; !slices.Equal(got, want) {
			t.Errorf("argument modified: %v, want %v", got, want)
		}
	})
}

// ---- TestParseCpuMask -------------------------------------------------

func TestParseCpuMask(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		err      bool
	}{
		{name: "single-high-cpu-1024", input: "1024", expected: "1024"},
		{name: "large-contiguous-range-512-to-1023", input: "512-1023", expected: "512-1023"},
		{name: "two-non-adjacent-ranges", input: "0-63,128-191", expected: "0-63,128-191"},
		{name: "word-boundary-range-63-to-64", input: "63-64", expected: "63-64"},
		{name: "degenerate-range-0-to-0", input: "0-0", expected: "0"},
		{name: "low-and-high-boundary", input: "0,1023", expected: "0,1023"},
		{name: "full-word-0", input: "0-63", expected: "0-63"},
		{name: "duplicate-cpus-deduped", input: "0,0,1,1,63,63", expected: "0-1,63"},
		{name: "unordered-cpus-sorted-in-output", input: "63,0,127,64", expected: "0,63-64,127"},
		{name: "all-cpus-0-to-1023", input: "0-1023", expected: "0-1023"},
		{name: "empty-string-returns-empty-mask", input: "", expected: ""},
		// error cases
		{name: "error-non-numeric", input: "abc", err: true},
		{name: "error-reversed-range", input: "5-3", err: true},
		{name: "error-bad-range-min", input: "a-3", err: true},
		{name: "error-bad-range-max", input: "3-a", err: true},
		{name: "error-empty-part-from-double-comma", input: "1,,2", err: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseCpuMask(tc.input)
			if tc.err {
				if err == nil {
					t.Errorf("ParseCpuMask(%q) expected error, got nil (mask=%v)", tc.input, m.List())
				}
				return
			}
			if err != nil {
				t.Errorf("ParseCpuMask(%q) unexpected error: %v", tc.input, err)
				return
			}
			if got := m.String(); got != tc.expected {
				t.Errorf("ParseCpuMask(%q).String() = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

// ===========================================================================
// CpuSet tests
//
// Binary-operation test cases are labelled "cpuset:" (fast path: both
// operands are *CpuSet, delegating to k8s cpuset methods) and "cpumask:"
// (fallback path: second operand is *CpuMask, exercising the iteration-based
// fallback in Difference, Intersection, Equals, IsSubsetOf, and Union).
// ===========================================================================

// ---- TestNewCpuSet --------------------------------------------------------

func TestNewCpuSet(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected []int
	}{
		{name: "empty", cpus: []int{}, expected: []int{}},
		{name: "single-cpu-0", cpus: []int{0}, expected: []int{0}},
		{name: "single-cpu-63", cpus: []int{63}, expected: []int{63}},
		{name: "single-cpu-64", cpus: []int{64}, expected: []int{64}},
		{name: "word-0-all-bits", cpus: cpuRange(0, 63), expected: cpuRange(0, 63)},
		{name: "straddles-word-boundary", cpus: []int{63, 64}, expected: []int{63, 64}},
		{name: "cpu-1023", cpus: []int{1023}, expected: []int{1023}},
		{name: "lowest-and-highest-multi-word", cpus: []int{0, 1023}, expected: []int{0, 1023}},
		{name: "sparse-word-boundaries", cpus: []int{0, 64, 128, 512, 1023}, expected: []int{0, 64, 128, 512, 1023}},
		{name: "large-contiguous-range", cpus: cpuRange(512, 1023), expected: cpuRange(512, 1023)},
		{name: "duplicate-cpus-deduped", cpus: []int{0, 0, 63, 63, 64, 64}, expected: []int{0, 63, 64}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCpuSet(tc.cpus...)
			if got := s.List(); !slices.Equal(got, tc.expected) {
				t.Errorf("List() = %v, want %v", got, tc.expected)
			}
		})
	}
}

// ---- TestParseCpuSet -------------------------------------------------

func TestParseCpuSet(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		err      bool
	}{
		{name: "empty-string-returns-empty-set", input: "", expected: ""},
		{name: "single-cpu-0", input: "0", expected: "0"},
		{name: "range-and-single", input: "0-3,5", expected: "0-3,5"},
		{name: "word-boundary-range", input: "63-64", expected: "63-64"},
		{name: "high-cpu-1023", input: "1023", expected: "1023"},
		// error cases
		{name: "error-non-numeric", input: "abc", err: true},
		{name: "error-bad-range", input: "3-a", err: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseCpuSet(tc.input)
			if tc.err {
				if err == nil {
					t.Errorf("ParseCpuSet(%q) expected error, got nil (set=%v)", tc.input, s.List())
				}
				return
			}
			if err != nil {
				t.Errorf("ParseCpuSet(%q) unexpected error: %v", tc.input, err)
				return
			}
			if got := s.String(); got != tc.expected {
				t.Errorf("ParseCpuSet(%q).String() = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

// ---- TestParsersAgree -----------------------------------------------------

// ParseCpuMask and ParseCpuSet are siblings with the same job, so they must
// accept and reject the same input and yield the same CPUs. It is easy to
// break that by touching only one of them.
func TestParsersAgree(t *testing.T) {
	inputs := []string{
		// accepted
		"", "0", "0-3", "0-3,5", "63-64", "1023", "1024",
		"0,0,1", "63,0,127,64", "0-0", "0-1023", "512-1023",
		// accepted, but only because strconv.Atoi is lenient
		"+1", "00",
		// rejected
		"abc", "5-3", "a-3", "3-a", "1,,2", ",", "-", "0-", "-1",
		"0--3", "1-2-3", "0x10", "1_0", "99999999999999999999",
		" 0-3", "0-3 ", "0, 1", "0 - 3",
	}
	for _, in := range inputs {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			m, merr := ParseCpuMask(in)
			s, serr := ParseCpuSet(in)

			if (merr != nil) != (serr != nil) {
				t.Fatalf("parsers disagree on validity: ParseCpuMask err=%v, ParseCpuSet err=%v",
					merr, serr)
			}
			if merr != nil {
				// Both rejected it. Neither may return a usable value
				// alongside the error.
				if m != nil {
					t.Errorf("ParseCpuMask returned non-nil mask %v with an error", m)
				}
				if s != nil {
					t.Errorf("ParseCpuSet returned non-nil set %v with an error", s)
				}
				return
			}
			if !m.Equals(s) {
				t.Errorf("parsers produced different sets: mask %q vs set %q",
					m.String(), s.String())
			}
			if got, want := m.String(), s.String(); got != want {
				t.Errorf("String() differs: mask %q vs set %q", got, want)
			}
		})
	}
}

// ---- TestCpuSetSetAndClear ------------------------------------------------

func TestCpuSetSetAndClear(t *testing.T) {
	type step struct {
		set   []int
		clear []int
	}
	tests := []struct {
		name     string
		initial  []int
		steps    []step
		expected []int
	}{
		{
			name:     "set-cpu-0-on-empty",
			steps:    []step{{set: []int{0}}},
			expected: []int{0},
		},
		{
			name:     "set-cpu-63",
			steps:    []step{{set: []int{63}}},
			expected: []int{63},
		},
		{
			name:     "set-cpu-64-crosses-word",
			steps:    []step{{set: []int{64}}},
			expected: []int{64},
		},
		{
			name:     "set-cpu-1023",
			steps:    []step{{set: []int{1023}}},
			expected: []int{1023},
		},
		{
			name:     "set-is-idempotent",
			initial:  []int{0},
			steps:    []step{{set: []int{0}}},
			expected: []int{0},
		},
		{
			name:     "set-multiple-cpus-in-one-call",
			steps:    []step{{set: []int{0, 63, 64, 1023}}},
			expected: []int{0, 63, 64, 1023},
		},
		{
			name:     "clear-middle-cpu",
			initial:  []int{0, 1, 2},
			steps:    []step{{clear: []int{1}}},
			expected: []int{0, 2},
		},
		{
			name:     "clear-multiple-cpus-in-one-call",
			initial:  []int{0, 1, 2, 3},
			steps:    []step{{clear: []int{1, 2}}},
			expected: []int{0, 3},
		},
		{
			name:     "clear-nonexistent-is-noop",
			initial:  []int{0, 2},
			steps:    []step{{clear: []int{1}}},
			expected: []int{0, 2},
		},
		{
			name:     "clear-out-of-range-is-noop",
			initial:  []int{0},
			steps:    []step{{clear: []int{1023}}},
			expected: []int{0},
		},
		{
			name:     "clear-cpu-1023",
			initial:  []int{0, 1023},
			steps:    []step{{clear: []int{1023}}},
			expected: []int{0},
		},
		{
			name:     "set-then-clear",
			steps:    []step{{set: []int{0, 1, 2}}, {clear: []int{1}}},
			expected: []int{0, 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCpuSet(tc.initial...)
			for _, step := range tc.steps {
				if len(step.set) > 0 {
					s.Set(step.set...)
				}
				if len(step.clear) > 0 {
					s.Clear(step.clear...)
				}
			}
			if got := s.List(); !slices.Equal(got, tc.expected) {
				t.Errorf("List() = %v, want %v", got, tc.expected)
			}
		})
	}

	// CpuSet caches String (and Key, which delegates to it); Set and Clear
	// must invalidate it. Size is not cached, it comes straight from the
	// underlying k8s cpuset.

	t.Run("caches-cleared-after-set", func(t *testing.T) {
		s := NewCpuSet(0)
		_, _ = s.String(), s.Key()

		s.Set(1)

		if got := s.String(); got != "0-1" {
			t.Errorf("String() after Set: got %q, want %q", got, "0-1")
		}
		if got := s.Key(); got != "0-1" {
			t.Errorf("Key() after Set: got %q, want %q", got, "0-1")
		}
		if got := s.Size(); got != 2 {
			t.Errorf("Size() after Set: got %d, want 2", got)
		}
	})

	t.Run("caches-cleared-after-clear", func(t *testing.T) {
		s := NewCpuSet(0, 1)
		_, _ = s.String(), s.Key()

		s.Clear(1)

		if got := s.String(); got != "0" {
			t.Errorf("String() after Clear: got %q, want %q", got, "0")
		}
		if got := s.Key(); got != "0" {
			t.Errorf("Key() after Clear: got %q, want %q", got, "0")
		}
		if got := s.Size(); got != 1 {
			t.Errorf("Size() after Clear: got %d, want 1", got)
		}
	})

	t.Run("caches-cleared-when-emptied", func(t *testing.T) {
		s := NewCpuSet(0, 1)
		_, _ = s.String(), s.Key()

		s.Clear(0, 1)

		if got := s.String(); got != "" {
			t.Errorf("String() = %q, want %q", got, "")
		}
		if got := s.Key(); got != "" {
			t.Errorf("Key() = %q, want %q", got, "")
		}
		if got := s.Size(); got != 0 {
			t.Errorf("Size() = %d, want 0", got)
		}
	})

	// ---- variadic zero-arg / multi-arg Set / Clear calls ---------------------

	t.Run("set-no-args-is-noop", func(t *testing.T) {
		s := NewCpuSet(5)
		s.Set()
		if got := s.List(); !slices.Equal(got, []int{5}) {
			t.Errorf("Set() no-op: got %v, want [5]", got)
		}
	})

	t.Run("clear-no-args-is-noop", func(t *testing.T) {
		s := NewCpuSet(5)
		s.Clear()
		if got := s.List(); !slices.Equal(got, []int{5}) {
			t.Errorf("Clear() no-op: got %v, want [5]", got)
		}
	})

	t.Run("set-multiple-across-word-boundaries", func(t *testing.T) {
		s := NewCpuSet()
		s.Set(0, 63, 64, 127, 128, 1023)
		want := []int{0, 63, 64, 127, 128, 1023}
		if got := s.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("clear-multiple-across-word-boundaries", func(t *testing.T) {
		s := NewCpuSet(0, 63, 64, 127, 128, 1023)
		s.Clear(63, 64, 1023)
		want := []int{0, 127, 128}
		if got := s.List(); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// ---- TestCpuSetSeal --------------------------------------------------

func TestCpuSetSeal(t *testing.T) {
	t.Run("set-on-sealed-set-panics", func(t *testing.T) {
		s := NewCpuSet(0)
		s.Seal()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from Set on sealed set, got none")
			}
		}()
		s.Set(1)
	})

	t.Run("clear-on-sealed-set-panics", func(t *testing.T) {
		s := NewCpuSet(0, 1)
		s.Seal()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from Clear on sealed set, got none")
			}
		}()
		s.Clear(0)
	})

	t.Run("read-only-operations-work-on-sealed-set", func(t *testing.T) {
		s := NewCpuSet(0, 63, 64, 1023)
		s.Seal()
		if !s.Contains(63) {
			t.Error("Contains should work on sealed set")
		}
		if s.Size() != 4 {
			t.Errorf("Size should work on sealed set: got %d, want 4", s.Size())
		}
		if s.IsEmpty() {
			t.Error("IsEmpty should work on sealed set")
		}
		if !slices.Equal(s.List(), []int{0, 63, 64, 1023}) {
			t.Errorf("List should work on sealed set: got %v", s.List())
		}
	})
}

// ---- TestCpuSetClone ------------------------------------------------------

func TestCpuSetClone(t *testing.T) {
	t.Run("clone-of-empty", func(t *testing.T) {
		s := NewCpuSet()
		if c := s.Clone(); !c.IsEmpty() {
			t.Errorf("clone of empty is not empty: %v", c.List())
		}
	})

	t.Run("clone-matches-original", func(t *testing.T) {
		cpus := []int{0, 63, 64, 512, 1023}
		s := NewCpuSet(cpus...)
		if c := s.Clone(); !slices.Equal(c.List(), cpus) {
			t.Errorf("clone content mismatch: want %v, got %v", cpus, c.List())
		}
	})

	t.Run("clone-is-independent", func(t *testing.T) {
		s := NewCpuSet(0, 1, 2)
		c := s.Clone()
		c.Set(63)
		if s.Contains(63) {
			t.Error("mutating clone propagated to original")
		}
		if !c.Contains(63) {
			t.Error("mutation on clone did not take effect")
		}
	})

	t.Run("clone-of-large-mask", func(t *testing.T) {
		cpus := cpuRange(512, 1023)
		s := NewCpuSet(cpus...)
		if c := s.Clone(); !slices.Equal(c.List(), cpus) {
			t.Errorf("large clone mismatch: got %v", c.List())
		}
	})
}

// ---- TestCpuSetIsEmpty ----------------------------------------------------

func TestCpuSetIsEmpty(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected bool
	}{
		{name: "empty", cpus: []int{}, expected: true},
		{name: "single-cpu-0", cpus: []int{0}, expected: false},
		{name: "single-cpu-63", cpus: []int{63}, expected: false},
		{name: "single-cpu-64", cpus: []int{64}, expected: false},
		{name: "word-0-all-bits", cpus: cpuRange(0, 63), expected: false},
		{name: "cpu-1023", cpus: []int{1023}, expected: false},
		{name: "multi-word-sparse", cpus: []int{0, 64, 512, 1023}, expected: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCpuSet(tc.cpus...)
			if got := s.IsEmpty(); got != tc.expected {
				t.Errorf("IsEmpty() = %v, want %v", got, tc.expected)
			}
		})
	}

	t.Run("empty-after-clearing-all", func(t *testing.T) {
		s := NewCpuSet(0, 63, 64, 1023)
		s.Clear(s.List()...)
		if !s.IsEmpty() {
			t.Errorf("should be empty after clearing all, got %v", s.List())
		}
	})
}

// ---- TestCpuSetSize -------------------------------------------------------

func TestCpuSetSize(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected int
	}{
		{name: "empty", cpus: []int{}, expected: 0},
		{name: "single-cpu-0", cpus: []int{0}, expected: 1},
		{name: "single-cpu-63", cpus: []int{63}, expected: 1},
		{name: "single-cpu-64", cpus: []int{64}, expected: 1},
		{name: "word-0-all-64-bits", cpus: cpuRange(0, 63), expected: 64},
		{name: "two-words-all-128-bits", cpus: cpuRange(0, 127), expected: 128},
		{name: "single-cpu-1023", cpus: []int{1023}, expected: 1},
		{name: "large-range-512-to-1023", cpus: cpuRange(512, 1023), expected: 512},
		{name: "sparse-boundary-cpus", cpus: []int{0, 63, 64, 127, 512, 1023}, expected: 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCpuSet(tc.cpus...)
			if got := s.Size(); got != tc.expected {
				t.Errorf("Size() = %d, want %d", got, tc.expected)
			}
		})
	}
}

// ---- TestCpuSetContains ---------------------------------------------------

func TestCpuSetContains(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		check    int
		expected bool
	}{
		{name: "empty-check-0", cpus: []int{}, check: 0, expected: false},
		{name: "cpu-0-present", cpus: []int{0}, check: 0, expected: true},
		{name: "cpu-0-absent", cpus: []int{1}, check: 0, expected: false},
		{name: "cpu-63-present", cpus: []int{63}, check: 63, expected: true},
		{name: "cpu-64-present", cpus: []int{64}, check: 64, expected: true},
		{name: "cpu-63-absent-only-64-set", cpus: []int{64}, check: 63, expected: false},
		{name: "cpu-64-absent-only-63-set", cpus: []int{63}, check: 64, expected: false},
		{name: "word-boundary-check-63", cpus: []int{63, 64}, check: 63, expected: true},
		{name: "word-boundary-check-64", cpus: []int{63, 64}, check: 64, expected: true},
		{name: "cpu-1023-present", cpus: []int{1023}, check: 1023, expected: true},
		{name: "cpu-1023-absent", cpus: []int{1022}, check: 1023, expected: false},
		{name: "check-beyond-set", cpus: []int{0}, check: 1023, expected: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCpuSet(tc.cpus...)
			if got := s.Contains(tc.check); got != tc.expected {
				t.Errorf("Contains(%d) = %v, want %v", tc.check, got, tc.expected)
			}
		})
	}

	// ---- variadic multi-arg / zero-arg Contains calls ------------------------

	t.Run("contains-no-args-is-vacuously-true", func(t *testing.T) {
		s := NewCpuSet(0, 1, 2)
		if !s.Contains() {
			t.Error("Contains() with no args should be true")
		}
		if got := NewCpuSet().Contains(); !got {
			t.Error("Contains() on empty set with no args should be true")
		}
	})

	t.Run("contains-multiple-all-present", func(t *testing.T) {
		s := NewCpuSet(0, 63, 64, 1023)
		if !s.Contains(0, 63, 64, 1023) {
			t.Error("Contains(0, 63, 64, 1023) = false, want true")
		}
	})

	t.Run("contains-multiple-one-missing", func(t *testing.T) {
		s := NewCpuSet(0, 63, 64)
		if s.Contains(0, 63, 64, 1023) {
			t.Error("Contains(0, 63, 64, 1023) = true, want false (1023 absent)")
		}
	})

	t.Run("contains-multiple-duplicates", func(t *testing.T) {
		s := NewCpuSet(0, 64)
		if !s.Contains(0, 0, 64, 64) {
			t.Error("Contains with duplicate CPUs = false, want true")
		}
	})
}

// ---- TestCpuSetString -----------------------------------------------------

func TestCpuSetString(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected string
	}{
		{name: "empty", cpus: []int{}, expected: ""},
		{name: "single-cpu-0", cpus: []int{0}, expected: "0"},
		{name: "single-cpu-63", cpus: []int{63}, expected: "63"},
		{name: "single-cpu-64", cpus: []int{64}, expected: "64"},
		{name: "full-word-0", cpus: cpuRange(0, 63), expected: "0-63"},
		{name: "straddles-word-boundary", cpus: []int{63, 64}, expected: "63-64"},
		{name: "two-full-words", cpus: cpuRange(0, 127), expected: "0-127"},
		{name: "lowest-and-highest-of-two-words", cpus: []int{0, 63, 64, 127}, expected: "0,63-64,127"},
		{name: "sparse-word-boundaries", cpus: []int{0, 64, 128, 192}, expected: "0,64,128,192"},
		{name: "low-and-very-high", cpus: []int{0, 1023}, expected: "0,1023"},
		{name: "large-range", cpus: cpuRange(512, 1023), expected: "512-1023"},
		{name: "complex", cpus: []int{0, 1, 2, 4, 5, 7}, expected: "0-2,4-5,7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCpuSet(tc.cpus...)
			got := s.String()
			if got != tc.expected {
				t.Errorf("String() = %q, want %q", got, tc.expected)
			}
			if got2 := s.String(); got2 != got {
				t.Errorf("cached String() differs: %q vs %q", got2, got)
			}
		})
	}
}

// ---- TestCpuSetList -------------------------------------------------------

func TestCpuSetList(t *testing.T) {
	tests := []struct {
		name     string
		cpus     []int
		expected []int
	}{
		{name: "empty", cpus: []int{}, expected: []int{}},
		{name: "single", cpus: []int{0}, expected: []int{0}},
		{name: "multi-word-sparse", cpus: []int{0, 64, 127, 512, 1023}, expected: []int{0, 64, 127, 512, 1023}},
		{name: "full-word-0", cpus: cpuRange(0, 63), expected: cpuRange(0, 63)},
		{name: "large-range", cpus: cpuRange(960, 1023), expected: cpuRange(960, 1023)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCpuSet(tc.cpus...)
			if got := s.List(); !slices.Equal(got, tc.expected) {
				t.Errorf("List() = %v, want %v", got, tc.expected)
			}
			// UnsortedList must contain exactly the same elements.
			unsorted := s.UnsortedList()
			slices.Sort(unsorted)
			if !slices.Equal(unsorted, tc.expected) {
				t.Errorf("UnsortedList() (sorted) = %v, want %v", unsorted, tc.expected)
			}
		})
	}
}

// ---- TestCpuSetForEachCpu --------------------------------------------

func TestCpuSetForEachCpu(t *testing.T) {
	// CpuSet.ForEachCpu iterates over UnsortedList(), which (unlike
	// CpuMask's bitmask-driven iteration) does not guarantee any
	// particular order, so these tests only assert on the set of visited
	// CPUs and call counts, not on ordering.

	t.Run("empty-set-f-never-called", func(t *testing.T) {
		s := NewCpuSet()
		called := false
		s.ForEachCpu(func(_ int) bool {
			called = true
			return true
		})
		if called {
			t.Error("ForEachCpu called f on empty set")
		}
	})

	t.Run("single-cpu", func(t *testing.T) {
		s := NewCpuSet(42)
		var visited []int
		s.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		if !slices.Equal(visited, []int{42}) {
			t.Errorf("expected [42], got %v", visited)
		}
	})

	t.Run("multiple-cpus-visits-all-exactly-once", func(t *testing.T) {
		want := []int{0, 7, 63, 64, 127, 512, 1023}
		s := NewCpuSet(want...)
		var visited []int
		s.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		slices.Sort(visited)
		if !slices.Equal(visited, want) {
			t.Errorf("expected %v (in any order), got %v", want, visited)
		}
	})

	t.Run("full-word-0-visits-all-64", func(t *testing.T) {
		want := cpuRange(0, 63)
		s := NewCpuSet(want...)
		var visited []int
		s.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		slices.Sort(visited)
		if !slices.Equal(visited, want) {
			t.Errorf("expected CPUs 0-63, got %v", visited)
		}
	})

	t.Run("high-cpu-numbers-960-to-1023", func(t *testing.T) {
		want := cpuRange(960, 1023)
		s := NewCpuSet(want...)
		var visited []int
		s.ForEachCpu(func(cpu int) bool {
			visited = append(visited, cpu)
			return true
		})
		slices.Sort(visited)
		if !slices.Equal(visited, want) {
			t.Errorf("expected CPUs 960-1023, got %v", visited)
		}
	})

	t.Run("early-termination-stops-iteration", func(t *testing.T) {
		s := NewCpuSet(0, 63, 64, 1023)
		calls := 0
		s.ForEachCpu(func(_ int) bool {
			calls++
			return false // stop after the very first CPU
		})
		if calls != 1 {
			t.Errorf("expected f to be called exactly once, got %d calls", calls)
		}
	})

	t.Run("partial-termination-stops-after-n", func(t *testing.T) {
		s := NewCpuSet(0, 1, 2, 3, 4)
		calls := 0
		s.ForEachCpu(func(_ int) bool {
			calls++
			return calls < 3 // stop after three CPUs
		})
		if calls != 3 {
			t.Errorf("expected f to be called exactly 3 times, got %d calls", calls)
		}
	})
}

// ---- TestCpuSetDifference -------------------------------------------------

func TestCpuSetDifference(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected []int
	}{
		// *CpuSet fast path
		{name: "cpuset: empty-minus-empty", a: []int{}, b: NewCpuSet(), expected: []int{}},
		{name: "cpuset: empty-minus-nonempty", a: []int{}, b: NewCpuSet(0, 1), expected: []int{}},
		{name: "cpuset: nonempty-minus-empty", a: []int{0, 1}, b: NewCpuSet(), expected: []int{0, 1}},
		{name: "cpuset: a-minus-a", a: []int{0, 1, 2}, b: NewCpuSet(0, 1, 2), expected: []int{}},
		{name: "cpuset: disjoint", a: []int{0, 2}, b: NewCpuSet(1, 3), expected: []int{0, 2}},
		{name: "cpuset: superset-minus-subset", a: []int{0, 1, 2, 3}, b: NewCpuSet(1, 2), expected: []int{0, 3}},
		{name: "cpuset: subset-minus-superset", a: []int{1, 2}, b: NewCpuSet(0, 1, 2, 3), expected: []int{}},
		{name: "cpuset: high-cpus", a: cpuRange(960, 1023), b: NewCpuSet(cpuRange(992, 1023)...), expected: cpuRange(960, 991)},
		// *CpuMask fallback path
		{name: "cpumask: basic", a: []int{0, 1, 2, 3}, b: NewCpuMask(1, 2), expected: []int{0, 3}},
		{name: "cpumask: empty-a", a: []int{}, b: NewCpuMask(0, 1), expected: []int{}},
		{name: "cpumask: empty-b", a: []int{0, 1}, b: NewCpuMask(), expected: []int{0, 1}},
		{name: "cpumask: disjoint", a: []int{0, 2}, b: NewCpuMask(1, 3), expected: []int{0, 2}},
		{name: "cpumask: high-cpus", a: cpuRange(512, 1023), b: NewCpuMask(cpuRange(768, 1023)...), expected: cpuRange(512, 767)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuSet(tc.a...)
			got := a.Difference(tc.b)
			if !maskListEqual(got, NewCpuSet(tc.expected...)) {
				t.Errorf("Difference() = %v, want %v", got.List(), tc.expected)
			}
		})
	}
}

// ---- TestCpuSetEquals -----------------------------------------------------

func TestCpuSetEquals(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected bool
	}{
		// *CpuSet fast path
		{name: "cpuset: both-empty", a: []int{}, b: NewCpuSet(), expected: true},
		{name: "cpuset: same-single", a: []int{0}, b: NewCpuSet(0), expected: true},
		{name: "cpuset: different-single", a: []int{0}, b: NewCpuSet(1), expected: false},
		{name: "cpuset: one-empty-one-not", a: []int{0}, b: NewCpuSet(), expected: false},
		{name: "cpuset: same-full-word-0", a: cpuRange(0, 63), b: NewCpuSet(cpuRange(0, 63)...), expected: true},
		{name: "cpuset: same-multi-word", a: []int{0, 64, 128, 1023}, b: NewCpuSet(0, 64, 128, 1023), expected: true},
		{name: "cpuset: different-multi-word", a: []int{0, 64}, b: NewCpuSet(0, 128), expected: false},
		{name: "cpuset: high-cpus-equal", a: cpuRange(960, 1023), b: NewCpuSet(cpuRange(960, 1023)...), expected: true},
		{name: "cpuset: high-cpus-differ", a: cpuRange(960, 1022), b: NewCpuSet(cpuRange(960, 1023)...), expected: false},
		// *CpuMask fallback path
		{name: "cpumask: equal", a: []int{0, 1, 2}, b: NewCpuMask(0, 1, 2), expected: true},
		{name: "cpumask: not-equal", a: []int{0, 1}, b: NewCpuMask(0, 2), expected: false},
		{name: "cpumask: size-mismatch", a: []int{0, 1, 2}, b: NewCpuMask(0, 1), expected: false},
		{name: "cpumask: both-empty", a: []int{}, b: NewCpuMask(), expected: true},
		{name: "cpumask: high-cpus-equal", a: cpuRange(512, 1023), b: NewCpuMask(cpuRange(512, 1023)...), expected: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuSet(tc.a...)
			if got := a.Equals(tc.b); got != tc.expected {
				t.Errorf("Equals() = %v, want %v", got, tc.expected)
			}
		})
	}
}

// ---- TestCpuSetIntersection -----------------------------------------------

func TestCpuSetIntersection(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected []int
	}{
		// *CpuSet fast path
		{name: "cpuset: both-empty", a: []int{}, b: NewCpuSet(), expected: []int{}},
		{name: "cpuset: one-empty", a: []int{0, 1}, b: NewCpuSet(), expected: []int{}},
		{name: "cpuset: no-overlap", a: []int{0, 2}, b: NewCpuSet(1, 3), expected: []int{}},
		{name: "cpuset: full-overlap", a: cpuRange(0, 63), b: NewCpuSet(cpuRange(0, 63)...), expected: cpuRange(0, 63)},
		{name: "cpuset: partial-overlap", a: []int{0, 1, 2}, b: NewCpuSet(1, 2, 3), expected: []int{1, 2}},
		{name: "cpuset: word-boundary", a: []int{63, 64}, b: NewCpuSet(63, 64), expected: []int{63, 64}},
		{name: "cpuset: high-cpus", a: cpuRange(512, 1023), b: NewCpuSet(cpuRange(960, 1023)...), expected: cpuRange(960, 1023)},
		// *CpuMask fallback path
		{name: "cpumask: partial-overlap", a: []int{0, 1, 2}, b: NewCpuMask(1, 2, 3), expected: []int{1, 2}},
		{name: "cpumask: no-overlap", a: []int{0, 2}, b: NewCpuMask(1, 3), expected: []int{}},
		{name: "cpumask: empty-b", a: []int{0, 1}, b: NewCpuMask(), expected: []int{}},
		{name: "cpumask: high-cpus", a: cpuRange(512, 1023), b: NewCpuMask(cpuRange(960, 1023)...), expected: cpuRange(960, 1023)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuSet(tc.a...)
			got := a.Intersection(tc.b)
			if !maskListEqual(got, NewCpuSet(tc.expected...)) {
				t.Errorf("Intersection() = %v, want %v", got.List(), tc.expected)
			}
		})
	}
}

// ---- TestCpuSetIsSubsetOf -------------------------------------------------

func TestCpuSetIsSubsetOf(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected bool
	}{
		// *CpuSet fast path
		{name: "cpuset: empty-subset-empty", a: []int{}, b: NewCpuSet(), expected: true},
		{name: "cpuset: empty-subset-nonempty", a: []int{}, b: NewCpuSet(0, 1), expected: true},
		{name: "cpuset: nonempty-not-subset-of-empty", a: []int{0}, b: NewCpuSet(), expected: false},
		{name: "cpuset: equal-sets", a: []int{0, 1, 2}, b: NewCpuSet(0, 1, 2), expected: true},
		{name: "cpuset: proper-subset", a: []int{0, 1}, b: NewCpuSet(0, 1, 2), expected: true},
		{name: "cpuset: not-subset", a: []int{0, 3}, b: NewCpuSet(0, 1, 2), expected: false},
		{name: "cpuset: word-0-subset-of-0-127", a: cpuRange(0, 63), b: NewCpuSet(cpuRange(0, 127)...), expected: true},
		{name: "cpuset: word-0-not-subset-of-word-1", a: cpuRange(0, 63), b: NewCpuSet(cpuRange(64, 127)...), expected: false},
		{name: "cpuset: high-cpus-subset", a: cpuRange(992, 1023), b: NewCpuSet(cpuRange(960, 1023)...), expected: true},
		{name: "cpuset: high-cpu-not-in-superset", a: []int{0, 1023}, b: NewCpuSet(cpuRange(960, 1023)...), expected: false},
		// *CpuMask fallback path
		{name: "cpumask: proper-subset", a: []int{0, 1}, b: NewCpuMask(0, 1, 2), expected: true},
		{name: "cpumask: not-subset", a: []int{0, 3}, b: NewCpuMask(0, 1, 2), expected: false},
		{name: "cpumask: empty-subset", a: []int{}, b: NewCpuMask(0, 1), expected: true},
		{name: "cpumask: high-cpus-subset", a: cpuRange(992, 1023), b: NewCpuMask(cpuRange(960, 1023)...), expected: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuSet(tc.a...)
			if got := a.IsSubsetOf(tc.b); got != tc.expected {
				t.Errorf("IsSubsetOf() = %v, want %v", got, tc.expected)
			}
		})
	}
}

// ---- TestCpuSetUnion ------------------------------------------------------

func TestCpuSetUnion(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		others   []CPUSet
		expected []int
	}{
		// *CpuSet fast path
		{name: "cpuset: empty-union-empty", a: []int{}, others: []CPUSet{NewCpuSet()}, expected: []int{}},
		{name: "cpuset: a-union-empty", a: []int{0, 1}, others: []CPUSet{NewCpuSet()}, expected: []int{0, 1}},
		{name: "cpuset: empty-union-b", a: []int{}, others: []CPUSet{NewCpuSet(0, 1)}, expected: []int{0, 1}},
		{name: "cpuset: disjoint", a: []int{0}, others: []CPUSet{NewCpuSet(1)}, expected: []int{0, 1}},
		{name: "cpuset: overlapping", a: []int{0, 1}, others: []CPUSet{NewCpuSet(1, 2)}, expected: []int{0, 1, 2}},
		{name: "cpuset: word-boundary", a: []int{63}, others: []CPUSet{NewCpuSet(64)}, expected: []int{63, 64}},
		{name: "cpuset: multiple-others", a: []int{0}, others: []CPUSet{NewCpuSet(64), NewCpuSet(128)}, expected: []int{0, 64, 128}},
		{name: "cpuset: large-range", a: cpuRange(512, 767), others: []CPUSet{NewCpuSet(cpuRange(768, 1023)...)}, expected: cpuRange(512, 1023)},
		// no-args union must return a copy of a
		{name: "cpuset: no-others-returns-copy", a: []int{0, 63, 64, 1023}, others: nil, expected: []int{0, 63, 64, 1023}},
		{name: "cpuset: no-others-empty", a: []int{}, others: nil, expected: []int{}},
		// *CpuMask fallback path
		{name: "cpumask: a-union-b", a: []int{0, 1}, others: []CPUSet{NewCpuMask(2, 3)}, expected: []int{0, 1, 2, 3}},
		{name: "cpumask: overlapping", a: []int{0, 1}, others: []CPUSet{NewCpuMask(1, 2)}, expected: []int{0, 1, 2}},
		{name: "cpumask: empty-b", a: []int{0, 1}, others: []CPUSet{NewCpuMask()}, expected: []int{0, 1}},
		{name: "cpumask: empty-a", a: []int{}, others: []CPUSet{NewCpuMask(0, 1)}, expected: []int{0, 1}},
		{name: "cpumask: high-cpus", a: []int{0}, others: []CPUSet{NewCpuMask(1023)}, expected: []int{0, 1023}},
		// mixed CpuSet and CpuMask in others
		{name: "mixed: cpuset-and-cpumask", a: []int{0}, others: []CPUSet{NewCpuSet(64), NewCpuMask(128)}, expected: []int{0, 64, 128}},
		{name: "mixed: cpumask-and-cpuset", a: []int{0}, others: []CPUSet{NewCpuMask(64), NewCpuSet(128)}, expected: []int{0, 64, 128}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuSet(tc.a...)
			got := a.Union(tc.others...)
			if !maskListEqual(got, NewCpuSet(tc.expected...)) {
				t.Errorf("Union() = %v, want %v", got.List(), tc.expected)
			}
		})
	}

	t.Run("cpuset: no-others-result-is-independent-of-a", func(t *testing.T) {
		a := NewCpuSet(0, 63, 64)
		got := a.Union()
		got.Set(1023)
		if a.Contains(1023) {
			t.Error("mutating the Union() result propagated to the receiver")
		}
		if !got.Contains(1023) {
			t.Error("mutating the Union() result had no effect")
		}
	})

	t.Run("cpuset: operands-are-not-modified", func(t *testing.T) {
		a := NewCpuSet(0, 1)
		b := NewCpuSet(64)
		_ = a.Union(b)
		if got, want := a.List(), []int{0, 1}; !slices.Equal(got, want) {
			t.Errorf("receiver modified: %v, want %v", got, want)
		}
		if got, want := b.List(), []int{64}; !slices.Equal(got, want) {
			t.Errorf("argument modified: %v, want %v", got, want)
		}
	})
}

// ---- TestCpuSetIntersects -------------------------------------------------

func TestCpuSetIntersects(t *testing.T) {
	tests := []struct {
		name     string
		a        []int
		b        CPUSet
		expected bool
	}{
		// *CpuSet operand
		{name: "cpuset: both-empty", a: []int{}, b: NewCpuSet(), expected: false},
		{name: "cpuset: a-empty", a: []int{}, b: NewCpuSet(0, 1), expected: false},
		{name: "cpuset: b-empty", a: []int{0, 1}, b: NewCpuSet(), expected: false},
		{name: "cpuset: identical", a: []int{0, 1}, b: NewCpuSet(0, 1), expected: true},
		{name: "cpuset: single-common-cpu", a: []int{0, 2}, b: NewCpuSet(2, 4), expected: true},
		{name: "cpuset: no-overlap", a: []int{0, 2}, b: NewCpuSet(1, 3), expected: false},
		{name: "cpuset: word-boundary-63-vs-64", a: []int{63}, b: NewCpuSet(64), expected: false},
		{name: "cpuset: word-boundary-63-and-64", a: []int{63, 64}, b: NewCpuSet(64), expected: true},
		{name: "cpuset: high-cpus-overlap", a: cpuRange(960, 1000), b: NewCpuSet(cpuRange(1000, 1023)...), expected: true},
		{name: "cpuset: high-cpus-no-overlap", a: cpuRange(960, 999), b: NewCpuSet(cpuRange(1000, 1023)...), expected: false},
		// *CpuMask operand
		{name: "cpumask: overlap", a: []int{0, 1, 2}, b: NewCpuMask(2, 3), expected: true},
		{name: "cpumask: no-overlap", a: []int{0, 2}, b: NewCpuMask(1, 3), expected: false},
		{name: "cpumask: b-empty", a: []int{0, 1}, b: NewCpuMask(), expected: false},
		{name: "cpumask: a-empty", a: []int{}, b: NewCpuMask(0, 1), expected: false},
		{name: "cpumask: overlap-in-high-word-only", a: []int{0, 1023}, b: NewCpuMask(1, 1023), expected: true},
		{name: "cpumask: high-cpus-overlap", a: cpuRange(512, 1023), b: NewCpuMask(1023), expected: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCpuSet(tc.a...)
			if got := a.Intersects(tc.b); got != tc.expected {
				t.Errorf("Intersects() = %v, want %v", got, tc.expected)
			}
			if got := !a.Intersection(tc.b).IsEmpty(); got != tc.expected {
				t.Errorf("Intersection().IsEmpty() implies %v, want %v", got, tc.expected)
			}
		})
	}
}

// ---- TestCpuSetKey --------------------------------------------------------

func TestCpuSetKey(t *testing.T) {
	// CpuSet.Key delegates to String, so the key is the cpuset string.
	t.Run("key-is-the-cpuset-string", func(t *testing.T) {
		for _, cpus := range [][]int{
			{}, {0}, {63}, {64}, {63, 64}, {0, 64, 512, 1023}, cpuRange(0, 63),
		} {
			s := NewCpuSet(cpus...)
			if got, want := s.Key(), s.String(); got != want {
				t.Errorf("%v: Key() = %q, want %q", cpus, got, want)
			}
		}
	})

	t.Run("equal-sets-share-a-key", func(t *testing.T) {
		for _, cpus := range [][]int{
			{}, {0}, {63, 64}, {0, 64, 512, 1023}, cpuRange(960, 1023),
		} {
			a, b := NewCpuSet(cpus...), NewCpuSet(cpus...)
			if a.Key() != b.Key() {
				t.Errorf("%v: equal sets have different keys: %q vs %q", cpus, a.Key(), b.Key())
			}
		}
	})

	t.Run("different-sets-have-different-keys", func(t *testing.T) {
		for _, tc := range []struct{ a, b []int }{
			{[]int{0}, []int{1}},
			{[]int{63}, []int{64}},
			{[]int{0, 1}, []int{0, 1, 2}},
			{[]int{}, []int{0}},
		} {
			a, b := NewCpuSet(tc.a...), NewCpuSet(tc.b...)
			if a.Key() == b.Key() {
				t.Errorf("%v and %v share key %q", tc.a, tc.b, a.Key())
			}
		}
	})

	t.Run("empty-key-is-empty-string", func(t *testing.T) {
		if got := NewCpuSet().Key(); got != "" {
			t.Errorf("Key() = %q, want %q", got, "")
		}
	})
}

// ===========================================================================
// Cross-cutting tests
//
// The tables above exercise one method at a time. The tests below cut across
// methods and implementations:
//
//   - cross-implementation dispatch, in *both* directions for every pair of
//     implementations. The fast paths type-assert to their own concrete type
//     and fall back to a generic loop otherwise, so a bug can hide in one
//     direction while the other stays correct.
//   - Seal semantics: a sealed set materializes its lazily cached values, so
//     that concurrent readers never write. Run with -race.
//   - the mask sizing heuristic in NewCpuMask.
//   - randomized cross-checks against an independent oracle.
// ===========================================================================

// setCtor builds a CPUSet of one particular implementation.
type setCtor struct {
	name string
	new  func(...int) CPUSet
}

// ctors covers every real implementation. rawCpuSet from cpuset-bench_test.go
// is deliberately excluded: it is a benchmark-only shim which assumes that the
// other set is a *rawCpuSet and panics otherwise.
var ctors = []setCtor{
	{"CpuMask", func(cpus ...int) CPUSet { return NewCpuMask(cpus...) }},
	{"CpuSet", func(cpus ...int) CPUSet { return NewCpuSet(cpus...) }},
	{"testCPUSet", func(cpus ...int) CPUSet { return newTestCPUSet(cpus...) }},
}

// setPair is an input pair for the cross-implementation tests. Between them
// they cover empty and non-empty operands, equal sets, both subset directions,
// disjoint sets, differences confined to a high word, and operands whose masks
// end up with different word counts.
type setPair struct {
	name string
	a    []int
	b    []int
}

var setPairs = []setPair{
	{"both-empty", nil, nil},
	{"empty-vs-one", nil, []int{0}},
	{"one-vs-empty", []int{0}, nil},
	{"equal-single", []int{0}, []int{0}},
	{"equal-multi-word", []int{0, 64, 128, 1023}, []int{0, 64, 128, 1023}},
	{"receiver-subset", []int{0, 1}, []int{0, 1, 2}},
	{"receiver-superset", []int{0, 1, 2}, []int{0, 1}},
	{"receiver-subset-high", []int{960}, []int{960, 1023}},
	{"receiver-subset-word-boundary", cpuRange(0, 62), cpuRange(0, 63)},
	{"disjoint-low", []int{0, 2}, []int{1, 3}},
	{"disjoint-words", []int{0, 64}, []int{128, 192}},
	{"overlap-high-word-only", []int{0, 1023}, []int{1023}},
	{"differ-across-words", []int{0, 64}, []int{0, 128}},
	{"full-word-vs-subset", cpuRange(0, 63), cpuRange(0, 31)},
}

// normalize returns the sorted, de-duplicated CPUs of s, never nil.
func normalize(s []int) []int {
	out := slices.Compact(slices.Sorted(slices.Values(s)))
	if out == nil {
		return []int{}
	}
	return out
}

// wantEquals etc. compute the expected results directly from the input
// slices, independently of any CPUSet method.
func wantEquals(a, b []int) bool { return slices.Equal(normalize(a), normalize(b)) }

func wantSubsetOf(a, b []int) bool {
	nb := normalize(b)
	for _, c := range normalize(a) {
		if !slices.Contains(nb, c) {
			return false
		}
	}
	return true
}

func wantIntersects(a, b []int) bool {
	nb := normalize(b)
	for _, c := range normalize(a) {
		if slices.Contains(nb, c) {
			return true
		}
	}
	return false
}

func wantUnion(a, b []int) []int { return normalize(append(slices.Clone(a), b...)) }

func wantIntersection(a, b []int) []int {
	nb, out := normalize(b), []int{}
	for _, c := range normalize(a) {
		if slices.Contains(nb, c) {
			out = append(out, c)
		}
	}
	return out
}

func wantDifference(a, b []int) []int {
	nb, out := normalize(b), []int{}
	for _, c := range normalize(a) {
		if !slices.Contains(nb, c) {
			out = append(out, c)
		}
	}
	return out
}

// forEachPairing runs fn for every (pair, receiver impl, argument impl)
// combination.
func forEachPairing(t *testing.T, fn func(t *testing.T, p setPair, a, b CPUSet)) {
	t.Helper()
	for _, p := range setPairs {
		for _, ac := range ctors {
			for _, bc := range ctors {
				name := fmt.Sprintf("%s/%s.op(%s)", p.name, ac.name, bc.name)
				t.Run(name, func(t *testing.T) {
					fn(t, p, ac.new(p.a...), bc.new(p.b...))
				})
			}
		}
	}
}

func TestCrossImplEquals(t *testing.T) {
	forEachPairing(t, func(t *testing.T, p setPair, a, b CPUSet) {
		want := wantEquals(p.a, p.b)
		if got := a.Equals(b); got != want {
			t.Errorf("Equals() = %v, want %v", got, want)
		}
		// Equality is symmetric regardless of which implementation is the
		// receiver.
		if got := b.Equals(a); got != want {
			t.Errorf("reversed Equals() = %v, want %v", got, want)
		}
	})
}

func TestCrossImplIsSubsetOf(t *testing.T) {
	forEachPairing(t, func(t *testing.T, p setPair, a, b CPUSet) {
		if got, want := a.IsSubsetOf(b), wantSubsetOf(p.a, p.b); got != want {
			t.Errorf("IsSubsetOf() = %v, want %v", got, want)
		}
		if got, want := b.IsSubsetOf(a), wantSubsetOf(p.b, p.a); got != want {
			t.Errorf("reversed IsSubsetOf() = %v, want %v", got, want)
		}
	})
}

func TestCrossImplIntersects(t *testing.T) {
	forEachPairing(t, func(t *testing.T, p setPair, a, b CPUSet) {
		want := wantIntersects(p.a, p.b)
		if got := a.Intersects(b); got != want {
			t.Errorf("Intersects() = %v, want %v", got, want)
		}
		// Intersects is symmetric.
		if got := b.Intersects(a); got != want {
			t.Errorf("reversed Intersects() = %v, want %v", got, want)
		}
		// and must agree with Intersection()
		if got := !NewAnyCPUSet(a).Intersection(b).IsEmpty(); got != want {
			t.Errorf("Intersects()=%v disagrees with Intersection()=%s", want, NewAnyCPUSet(a).Intersection(b))
		}
	})
}

func TestCrossImplUnion(t *testing.T) {
	forEachPairing(t, func(t *testing.T, p setPair, a, b CPUSet) {
		want := wantUnion(p.a, p.b)
		if got := NewAnyCPUSet(a).Union(b).List(); !slices.Equal(got, want) {
			t.Errorf("Union() = %v, want %v", got, want)
		}
		if got := NewAnyCPUSet(b).Union(a).List(); !slices.Equal(got, want) {
			t.Errorf("reversed Union() = %v, want %v", got, want)
		}
		// the operands must not be modified
		if got := a.List(); !slices.Equal(got, normalize(p.a)) {
			t.Errorf("Union() modified receiver: %v", got)
		}
		if got := b.List(); !slices.Equal(got, normalize(p.b)) {
			t.Errorf("Union() modified argument: %v", got)
		}
	})
}

func TestCrossImplIntersection(t *testing.T) {
	forEachPairing(t, func(t *testing.T, p setPair, a, b CPUSet) {
		if got, want := NewAnyCPUSet(a).Intersection(b).List(), wantIntersection(p.a, p.b); !slices.Equal(got, want) {
			t.Errorf("Intersection() = %v, want %v", got, want)
		}
		if got, want := NewAnyCPUSet(b).Intersection(a).List(), wantIntersection(p.b, p.a); !slices.Equal(got, want) {
			t.Errorf("reversed Intersection() = %v, want %v", got, want)
		}
	})
}

func TestCrossImplDifference(t *testing.T) {
	forEachPairing(t, func(t *testing.T, p setPair, a, b CPUSet) {
		if got, want := NewAnyCPUSet(a).Difference(b).List(), wantDifference(p.a, p.b); !slices.Equal(got, want) {
			t.Errorf("Difference() = %v, want %v", got, want)
		}
		if got, want := NewAnyCPUSet(b).Difference(a).List(), wantDifference(p.b, p.a); !slices.Equal(got, want) {
			t.Errorf("reversed Difference() = %v, want %v", got, want)
		}
	})
}

// ---- Key ------------------------------------------------------------------

// NewCpuMask must build the same mask from the same CPUs however they are
// ordered, mask word count included: it estimates that count from the first
// and last element and lets expand() grow it, so an unsorted argument takes a
// different route to the same result. The cross-implementation keying recipe in
// doc.go relies on this, since CpuSet.UnsortedList returns CPUs in no
// particular order.
func TestNewCpuMaskIsOrderIndependent(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, cpus := range [][]int{
		{}, {0}, {1023}, {0, 1023}, {7, 1023, 3}, {63, 64},
		{0, 64, 512, 1023}, {5, 5, 5}, cpuRange(0, 63), cpuRange(960, 1023),
	} {
		t.Run(fmt.Sprint(cpus), func(t *testing.T) {
			ref := NewCpuMask(cpus...)
			for range 200 {
				p := slices.Clone(cpus)
				rng.Shuffle(len(p), func(a, b int) { p[a], p[b] = p[b], p[a] })

				m := NewCpuMask(p...)
				if len(m.mask) != len(ref.mask) {
					t.Fatalf("%v: %d mask words, want %d", p, len(m.mask), len(ref.mask))
				}
				if !slices.Equal(m.mask, ref.mask) {
					t.Fatalf("%v: mask words differ from %v", p, cpus)
				}
				if m.Key() != ref.Key() {
					t.Fatalf("%v: Key() = %q, want %q", p, m.Key(), ref.Key())
				}
			}
		})
	}
}

// maskLikeKey is the recipe doc.go gives for a key which is comparable across
// implementations.
func maskLikeKey(s CPUSet) string {
	if m, ok := s.(*CpuMask); ok {
		return m.Key()
	}
	return NewCpuMask(s.UnsortedList()...).Key()
}

// Key is only comparable within one implementation, so a CpuMask and a CpuSet
// holding the same CPUs have different keys. Going through the CpuMask form
// restores the "equal keys exactly for equal sets" property across them.
func TestMaskLikeKeyIsComparableAcrossImplementations(t *testing.T) {
	// the mismatch the recipe exists for
	if m, s := NewCpuMask(0, 5), NewCpuSet(0, 5); m.Key() == s.Key() {
		t.Errorf("expected CpuMask and CpuSet keys to differ, both are %q", m.Key())
	}

	for _, p := range setPairs {
		t.Run(p.name, func(t *testing.T) {
			equal := NewCpuMask(p.a...).Equals(NewCpuMask(p.b...))
			for _, ac := range ctors {
				for _, bc := range ctors {
					k1 := maskLikeKey(ac.new(p.a...))
					k2 := maskLikeKey(bc.new(p.b...))
					if (k1 == k2) != equal {
						t.Errorf("%s/%s: keys %q and %q match=%v, want %v",
							ac.name, bc.name, k1, k2, k1 == k2, equal)
					}
				}
			}
		})
	}
}

// Key must be equal exactly when the sets are equal, within one
// implementation. TestKey and TestCpuSetKey cover the per-implementation
// details; this checks the invariant holds across the whole input matrix.
func TestKeyMatchesEquality(t *testing.T) {
	for _, c := range ctors {
		t.Run(c.name, func(t *testing.T) {
			for _, p := range setPairs {
				a, b := c.new(p.a...), c.new(p.b...)
				sameKey, equal := a.Key() == b.Key(), a.Equals(b)
				if sameKey != equal {
					t.Errorf("%s: Key() equality %v disagrees with Equals() %v (%q vs %q)",
						p.name, sameKey, equal, a.Key(), b.Key())
				}
			}
		})
	}
}

// ---- Seal -----------------------------------------------------------------

var sealCases = [][]int{
	nil, // empty: String() and Key() are "", which is also
	{0}, // the "not cached yet" sentinel
	{0, 1, 2},
	{5, 64, 1023},
	{63, 64}, // word boundary
	cpuRange(0, 63),
}

// Sealing must not change any observable value.
func TestSealPreservesValues(t *testing.T) {
	for _, c := range ctors {
		for _, cpus := range sealCases {
			t.Run(fmt.Sprintf("%s/%v", c.name, cpus), func(t *testing.T) {
				unsealed := c.new(cpus...)
				wStr, wKey, wSize := unsealed.String(), unsealed.Key(), unsealed.Size()

				sealed := c.new(cpus...)
				sealed.Seal()

				if got := sealed.String(); got != wStr {
					t.Errorf("String() = %q, want %q", got, wStr)
				}
				if got := sealed.Key(); got != wKey {
					t.Errorf("Key() = %q, want %q", got, wKey)
				}
				if got := sealed.Size(); got != wSize {
					t.Errorf("Size() = %d, want %d", got, wSize)
				}
				if got := sealed.List(); !slices.Equal(got, normalize(cpus)) {
					t.Errorf("List() = %v, want %v", got, normalize(cpus))
				}
			})
		}
	}
}

// Sealing must also work when the caches were already populated.
func TestSealAfterCachesPrimed(t *testing.T) {
	for _, c := range ctors {
		t.Run(c.name, func(t *testing.T) {
			s := c.new(3, 7)
			wStr, wKey, wSize := s.String(), s.Key(), s.Size()
			s.Seal()
			if s.String() != wStr || s.Key() != wKey || s.Size() != wSize {
				t.Errorf("after Seal: (%q,%q,%d), want (%q,%q,%d)",
					s.String(), s.Key(), s.Size(), wStr, wKey, wSize)
			}
		})
	}
}

// A sealed set materializes its caches, so concurrent readers never write to
// it. This only fails under -race.
func TestSealedConcurrentReadsAreRaceFree(t *testing.T) {
	for _, c := range ctors {
		for _, cpus := range sealCases {
			t.Run(fmt.Sprintf("%s/%v", c.name, cpus), func(t *testing.T) {
				s := c.new(cpus...)
				wStr, wKey, wSize := s.String(), s.Key(), s.Size()
				s.Seal()

				var wg sync.WaitGroup
				for range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for range 200 {
							if got := s.Size(); got != wSize {
								t.Errorf("Size() = %d, want %d", got, wSize)
							}
							if got := s.String(); got != wStr {
								t.Errorf("String() = %q, want %q", got, wStr)
							}
							if got := s.Key(); got != wKey {
								t.Errorf("Key() = %q, want %q", got, wKey)
							}
						}
					}()
				}
				wg.Wait()
			})
		}
	}
}

// A set that was emptied before sealing is the awkward case: "" is a valid
// String() and Key() for it, so the caches cannot be recognized as populated
// by their value alone.
func TestSealEmptiedSet(t *testing.T) {
	m := NewCpuMask(0, 1)
	m.Clear(0, 1)
	m.Seal()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if got := m.String(); got != "" {
					t.Errorf("String() = %q, want %q", got, "")
				}
				if got := m.Key(); got != "" {
					t.Errorf("Key() = %q, want %q", got, "")
				}
				if got := m.Size(); got != 0 {
					t.Errorf("Size() = %d, want 0", got)
				}
			}
		}()
	}
	wg.Wait()
}

// Clone returns an unsealed copy: it must be writable, must not disturb the
// original, and must be safe to share once sealed again.
func TestCloneOfSealedSet(t *testing.T) {
	for _, c := range ctors {
		t.Run(c.name, func(t *testing.T) {
			orig := c.new(0, 5)
			orig.Seal()

			clone := NewAnyCPUSet(orig).Clone()
			clone.Set(9)

			if got, want := clone.List(), []int{0, 5, 9}; !slices.Equal(got, want) {
				t.Errorf("clone.List() = %v, want %v", got, want)
			}
			if got, want := orig.List(), []int{0, 5}; !slices.Equal(got, want) {
				t.Errorf("original modified: List() = %v, want %v", got, want)
			}
			// the clone's caches must have been invalidated by Set
			if got, want := clone.String(), "0,5,9"; got != want {
				t.Errorf("clone.String() = %q, want %q", got, want)
			}

			// re-sealing the clone makes it shareable again
			clone.Seal()
			wStr, wKey, wSize := clone.String(), clone.Key(), clone.Size()
			var wg sync.WaitGroup
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 200 {
						if clone.String() != wStr || clone.Key() != wKey || clone.Size() != wSize {
							t.Error("resealed clone returned inconsistent values")
						}
					}
				}()
			}
			wg.Wait()
		})
	}
}

// ---- NewCpuMask sizing ----------------------------------------------------

// neededWords is the number of mask words the given CPUs actually require.
func neededWords(cpus []int) int {
	if len(cpus) == 0 {
		return 0
	}
	hi := cpus[0]
	for _, c := range cpus {
		if c > hi {
			hi = c
		}
	}
	return hi/64 + 1
}

// NewCpuMask estimates the mask size from the two endpoints of the input,
// which is exact for sorted input. expand() covers any shortfall, so this is
// about avoiding reallocation, not correctness.
func TestNewCpuMaskSizingIsExactForSortedInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		cpus []int
	}{
		{"empty", nil},
		{"single-0", []int{0}},
		{"single-1023", []int{1023}},
		{"dense-0-7", cpuRange(0, 7)},
		{"dense-0-63", cpuRange(0, 63)},
		{"dense-0-64", cpuRange(0, 64)},
		{"dense-0-79", cpuRange(0, 79)},
		{"dense-0-255", cpuRange(0, 255)},
		{"dense-512-575", cpuRange(512, 575)},
		{"sparse-0-and-1023", []int{0, 1023}},
		{"sparse-ascending", []int{0, 100, 500, 900}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewCpuMask(tc.cpus...)
			if got, want := len(m.mask), neededWords(tc.cpus); got != want {
				t.Errorf("mask has %d words, want exactly %d", got, want)
			}
		})
	}
}

// Unsorted or duplicate-bearing input may mis-size the initial mask, but the
// result must still be correct.
func TestNewCpuMaskUnsortedAndDuplicateInput(t *testing.T) {
	for _, cpus := range [][]int{
		{5, 5, 5},
		{0, 1023, 0},
		{7, 1023, 3},   // max in the middle: the estimate falls short
		{100, 1, 5000}, // max last
		{5000, 1, 100}, // max first
		{63, 0, 64},
	} {
		t.Run(fmt.Sprint(cpus), func(t *testing.T) {
			m := NewCpuMask(cpus...)
			if got, want := m.List(), normalize(cpus); !slices.Equal(got, want) {
				t.Errorf("List() = %v, want %v", got, want)
			}
			if got, want := m.Size(), len(normalize(cpus)); got != want {
				t.Errorf("Size() = %d, want %d", got, want)
			}
			if got, want := len(m.mask), neededWords(cpus); got < want {
				t.Errorf("mask has %d words, need at least %d", got, want)
			}
		})
	}
}

// Randomized cross-check: after any sequence of Set and Clear, the cached
// String, Key and Size must agree with the set's contents.
func TestRandomizedMutationKeepsCachesConsistent(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	m := NewCpuMask()
	tracked := map[int]bool{}

	for i := range 3000 {
		cpu := rng.Intn(300)
		if rng.Intn(2) == 0 {
			m.Set(cpu)
			tracked[cpu] = true
		} else {
			m.Clear(cpu)
			delete(tracked, cpu)
		}

		if rng.Intn(4) == 0 {
			// prime the caches part way through
			_, _, _ = m.Size(), m.String(), m.Key()
		}

		want := make([]int, 0, len(tracked))
		for c := range tracked {
			want = append(want, c)
		}
		slices.Sort(want)

		if got := m.List(); !slices.Equal(got, want) {
			t.Fatalf("iteration %d: List() = %v, want %v", i, got, want)
		}
		if got := m.Size(); got != len(want) {
			t.Fatalf("iteration %d: Size() = %d, want %d", i, got, len(want))
		}
		rt, err := ParseCpuMask(m.String())
		if err != nil {
			t.Fatalf("iteration %d: ParseCpuMask(%q): %v", i, m.String(), err)
		}
		if !rt.Equals(m) {
			t.Fatalf("iteration %d: round trip of %q gave %q", i, m.String(), rt.String())
		}
	}
}

// The two implementations must be interchangeable: the same inputs and
// operations must produce the same CPUs whichever one is used.
func TestImplementationsAgreeOnRandomOperations(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	randomCpus := func() []int {
		n := rng.Intn(20)
		cpus := make([]int, n)
		for i := range cpus {
			cpus[i] = rng.Intn(200)
		}
		return cpus
	}

	for i := range 500 {
		x, y := randomCpus(), randomCpus()

		mx, my := NewCpuMask(x...), NewCpuMask(y...)
		sx, sy := NewCpuSet(x...), NewCpuSet(y...)

		for _, op := range []struct {
			name string
			fn   func(a, b CPUSet) CPUSet
		}{
			{"Union", func(a, b CPUSet) CPUSet { return NewAnyCPUSet(a).Union(b) }},
			{"Intersection", func(a, b CPUSet) CPUSet { return NewAnyCPUSet(a).Intersection(b) }},
			{"Difference", func(a, b CPUSet) CPUSet { return NewAnyCPUSet(a).Difference(b) }},
		} {
			gotMask := op.fn(mx, my).List()
			gotSet := op.fn(sx, sy).List()
			if !slices.Equal(gotMask, gotSet) {
				t.Fatalf("iteration %d: %s: CpuMask gave %v, CpuSet gave %v (a=%v b=%v)",
					i, op.name, gotMask, gotSet, x, y)
			}
			// and mixing implementations must agree too
			if mixed := op.fn(mx, sy).List(); !slices.Equal(mixed, gotMask) {
				t.Fatalf("iteration %d: %s: CpuMask.op(CpuSet) gave %v, want %v",
					i, op.name, mixed, gotMask)
			}
			if mixed := op.fn(sx, my).List(); !slices.Equal(mixed, gotSet) {
				t.Fatalf("iteration %d: %s: CpuSet.op(CpuMask) gave %v, want %v",
					i, op.name, mixed, gotSet)
			}
		}

		for _, op := range []struct {
			name string
			fn   func(a, b CPUSet) bool
		}{
			{"Equals", func(a, b CPUSet) bool { return a.Equals(b) }},
			{"IsSubsetOf", func(a, b CPUSet) bool { return a.IsSubsetOf(b) }},
			{"Intersects", func(a, b CPUSet) bool { return a.Intersects(b) }},
		} {
			want := op.fn(mx, my)
			for _, c := range []struct {
				name string
				a, b CPUSet
			}{
				{"CpuSet/CpuSet", sx, sy},
				{"CpuMask/CpuSet", mx, sy},
				{"CpuSet/CpuMask", sx, my},
			} {
				if got := op.fn(c.a, c.b); got != want {
					t.Fatalf("iteration %d: %s on %s = %v, want %v (a=%v b=%v)",
						i, op.name, c.name, got, want, x, y)
				}
			}
		}
	}
}

// AsCpuMask and AsCpuSet hand back the set they were given when it already is
// of the wanted type, and convert only when it is not.
func TestAsCpuMaskAndAsCpuSet(t *testing.T) {
	var (
		cpus  = []int{0, 5, 70}
		mask  = NewCpuMask(cpus...)
		set   = NewCpuSet(cpus...)
		other = newTestCPUSet(cpus...)
	)

	t.Run("AsCpuMask", func(t *testing.T) {
		if got := AsCpuMask(mask); got != mask {
			t.Error("AsCpuMask did not return the CpuMask it was given")
		}
		for _, from := range []struct {
			name string
			cpus CPUSet
		}{{"CpuSet", set}, {"testCPUSet", other}} {
			got := AsCpuMask(from.cpus)
			if !got.IsDense() {
				t.Errorf("AsCpuMask(%s) is not dense", from.name)
			}
			if !got.Equals(mask) {
				t.Errorf("AsCpuMask(%s) = %s, want %s", from.name, got, mask)
			}
		}
	})

	t.Run("AsCpuSet", func(t *testing.T) {
		if got := AsCpuSet(set); got != set {
			t.Error("AsCpuSet did not return the CpuSet it was given")
		}
		for _, from := range []struct {
			name string
			cpus CPUSet
		}{{"CpuMask", mask}, {"testCPUSet", other}} {
			got := AsCpuSet(from.cpus)
			if !got.IsSparse() {
				t.Errorf("AsCpuSet(%s) is not sparse", from.name)
			}
			if !got.Equals(set) {
				t.Errorf("AsCpuSet(%s) = %s, want %s", from.name, got, set)
			}
		}
	})
}

// An operation through an AnyCPUSet keeps the implementation it was performed
// on, so that wrapping a sparse set does not quietly turn it dense. A set from
// neither implementation here has no representation to keep and comes back as a
// CpuMask.
func TestAnyCPUSetKeepsImplementation(t *testing.T) {
	other := NewCpuMask(1)

	for _, tc := range []struct {
		name  string
		cpus  CPUSet
		dense bool
	}{
		{"CpuMask", NewCpuMask(0, 1), true},
		{"CpuSet", NewCpuSet(0, 1), false},
		{"testCPUSet", newTestCPUSet(0, 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAnyCPUSet(tc.cpus)
			for name, got := range map[string]AnyCPUSet{
				"Clone":        a.Clone(),
				"Union":        a.Union(other),
				"Difference":   a.Difference(other),
				"Intersection": a.Intersection(other),
			} {
				if got.IsDense() != tc.dense {
					t.Errorf("%s: IsDense() = %v, want %v",
						name, got.IsDense(), tc.dense)
				}
			}
		})
	}
}

// WrapCpuSet takes a k8s.io/utils/cpuset.CPUSet over without copying it. The
// two then share a set neither of them modifies in place, so what matters is
// that changing one leaves the other alone.
func TestWrapCpuSet(t *testing.T) {
	raw := cpuset.New(0, 5, 70)

	t.Run("holds-the-same-cpus", func(t *testing.T) {
		s := WrapCpuSet(raw)
		if got, want := s.List(), []int{0, 5, 70}; !slices.Equal(got, want) {
			t.Errorf("List() = %v, want %v", got, want)
		}
		if !s.IsSparse() {
			t.Error("a wrapped cpuset.CPUSet is not sparse")
		}
		if got, want := s.String(), raw.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	})

	t.Run("wrapper-is-unsealed", func(t *testing.T) {
		s := WrapCpuSet(raw)
		s.Set(9) // must not panic
		if !s.Contains(9) {
			t.Error("Set() on a wrapped set did not take effect")
		}
	})

	t.Run("mutating-the-wrapper-leaves-the-raw-set-alone", func(t *testing.T) {
		s := WrapCpuSet(raw)
		s.Set(9)
		s.Clear(0)

		if got, want := raw.List(), []int{0, 5, 70}; !slices.Equal(got, want) {
			t.Errorf("the raw set changed to %v, want %v", got, want)
		}
	})

	t.Run("composes-with-AnyCPUSet", func(t *testing.T) {
		got := NewAnyCPUSet(WrapCpuSet(raw)).Difference(NewCpuMask(5))
		if want := []int{0, 70}; !slices.Equal(got.List(), want) {
			t.Errorf("Difference() = %v, want %v", got.List(), want)
		}
		if !got.IsSparse() {
			t.Error("the result of an operation on a wrapped set is not sparse")
		}
	})
}

func TestEmptyIfNil(t *testing.T) {
	t.Run("a nil mask reads as empty", func(t *testing.T) {
		var m *CpuMask

		// the point of the helper: this is a method call on a nil pointer, which
		// is legal, and everything after it is a call on a real set
		cpus := m.EmptyIfNil()
		if cpus == nil {
			t.Fatal("EmptyIfNil returned nil")
		}
		if !cpus.IsEmpty() || cpus.Size() != 0 || cpus.String() != "" {
			t.Errorf("expected an empty mask, got %q", cpus)
		}
		if cpus.Union(NewCpuMask(1)).String() != "1" {
			t.Error("the empty mask does not work as an operand")
		}
	})

	t.Run("a real mask is itself", func(t *testing.T) {
		m := NewCpuMask(1, 2)
		if got := m.EmptyIfNil(); got != m {
			t.Errorf("EmptyIfNil returned %q, not the mask it was called on", got)
		}
	})

	t.Run("a nil set reads as empty", func(t *testing.T) {
		var s *CpuSet

		cpus := s.EmptyIfNil()
		if cpus == nil {
			t.Fatal("EmptyIfNil returned nil")
		}
		if !cpus.IsEmpty() || cpus.Size() != 0 {
			t.Errorf("expected an empty set, got %q", cpus)
		}
	})

	t.Run("a real set is itself", func(t *testing.T) {
		s := NewCpuSet(1, 2)
		if got := s.EmptyIfNil(); got != s {
			t.Errorf("EmptyIfNil returned %q, not the set it was called on", got)
		}
	})

	t.Run("the empty sets are sealed", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func()
		}{
			{"EmptyCpuMask", func() { EmptyCpuMask.Set(0) }},
			{"EmptyCpuSet", func() { EmptyCpuSet.Set(0) }},
		} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("modifying %s did not panic", tc.name)
					}
				}()
				tc.mutate()
			}()
		}
	})
}

func TestMustParse(t *testing.T) {
	t.Run("a good string parses", func(t *testing.T) {
		if got := MustParseCpuMask("0-3,7").String(); got != "0-3,7" {
			t.Errorf("MustParseCpuMask gave %q", got)
		}
		if got := MustParseCpuSet("0-3,7").String(); got != "0-3,7" {
			t.Errorf("MustParseCpuSet gave %q", got)
		}
	})

	for _, tc := range []struct {
		name  string
		parse func()
	}{
		{"MustParseCpuMask", func() { MustParseCpuMask("nonsense") }},
		{"MustParseCpuSet", func() { MustParseCpuSet("nonsense") }},
	} {
		t.Run(tc.name+" panics on a bad string", func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", tc.name)
				}
			}()
			tc.parse()
		})
	}
}

// TestNilReadsAsEmpty checks the rule the package promises: a nil set is the
// empty set for everything which does not modify it, whether it is the receiver
// or an operand, and modifying one is an error which says what to do instead.
func TestNilReadsAsEmpty(t *testing.T) {
	var (
		nilMask *CpuMask
		nilSet  *CpuSet
		none    CPUSet // a nil interface, not a nil set inside one
	)

	t.Run("reading a nil mask", func(t *testing.T) {
		if got := nilMask.Size(); got != 0 {
			t.Errorf("Size() = %d", got)
		}
		if !nilMask.IsEmpty() {
			t.Error("IsEmpty() is false")
		}
		if got := nilMask.String(); got != "" {
			t.Errorf("String() = %q", got)
		}
		if got := nilMask.Key(); got != "" {
			t.Errorf("Key() = %q", got)
		}
		if got := nilMask.List(); len(got) != 0 {
			t.Errorf("List() = %v", got)
		}
		if got := nilMask.UnsortedList(); len(got) != 0 {
			t.Errorf("UnsortedList() = %v", got)
		}
		if nilMask.Contains(0) {
			t.Error("Contains(0) is true")
		}
		if !nilMask.IsDense() || nilMask.IsSparse() {
			t.Error("a nil mask is not dense")
		}
		nilMask.ForEachCpu(func(int) bool {
			t.Error("ForEachCpu called f")
			return false
		})
	})

	t.Run("reading a nil set", func(t *testing.T) {
		if got := nilSet.Size(); got != 0 {
			t.Errorf("Size() = %d", got)
		}
		if !nilSet.IsEmpty() {
			t.Error("IsEmpty() is false")
		}
		if got := nilSet.String(); got != "" {
			t.Errorf("String() = %q", got)
		}
		if got := nilSet.Key(); got != "" {
			t.Errorf("Key() = %q", got)
		}
		if got := nilSet.List(); len(got) != 0 {
			t.Errorf("List() = %v", got)
		}
		if got := nilSet.UnsortedList(); len(got) != 0 {
			t.Errorf("UnsortedList() = %v", got)
		}
		if nilSet.Contains(0) {
			t.Error("Contains(0) is true")
		}
		if nilSet.IsDense() || !nilSet.IsSparse() {
			t.Error("a nil set is not sparse")
		}
		nilSet.ForEachCpu(func(int) bool {
			t.Error("ForEachCpu called f")
			return false
		})
	})

	t.Run("set algebra on a nil receiver", func(t *testing.T) {
		one := NewCpuMask(1)
		if got := nilMask.Union(one).String(); got != "1" {
			t.Errorf("Union = %q", got)
		}
		if got := nilMask.Intersection(one).String(); got != "" {
			t.Errorf("Intersection = %q", got)
		}
		if got := nilMask.Difference(one).String(); got != "" {
			t.Errorf("Difference = %q", got)
		}
		if nilMask.Intersects(one) {
			t.Error("Intersects is true")
		}
		if nilMask.Equals(one) {
			t.Error("Equals a non-empty set")
		}
		if !nilMask.IsSubsetOf(one) {
			t.Error("the empty set is a subset of everything")
		}

		oneSparse := NewCpuSet(1)
		if got := nilSet.Union(oneSparse).String(); got != "1" {
			t.Errorf("sparse Union = %q", got)
		}
		if got := nilSet.Intersection(oneSparse).String(); got != "" {
			t.Errorf("sparse Intersection = %q", got)
		}
		if got := nilSet.Difference(oneSparse).String(); got != "" {
			t.Errorf("sparse Difference = %q", got)
		}
		if nilSet.Intersects(oneSparse) {
			t.Error("sparse Intersects is true")
		}
		if !nilSet.IsSubsetOf(oneSparse) {
			t.Error("the empty sparse set is a subset of everything")
		}
	})

	t.Run("nothing as an operand", func(t *testing.T) {
		// three ways to say nothing, each of which used to panic
		for name, nothing := range map[string]CPUSet{
			"a nil interface": none,
			"a nil mask":      nilMask,
			"a nil set":       nilSet,
		} {
			t.Run(name, func(t *testing.T) {
				m := NewCpuMask(1, 2)
				if got := m.Union(nothing).String(); got != "1-2" {
					t.Errorf("mask Union = %q", got)
				}
				if got := m.Difference(nothing).String(); got != "1-2" {
					t.Errorf("mask Difference = %q", got)
				}
				if got := m.Intersection(nothing).String(); got != "" {
					t.Errorf("mask Intersection = %q", got)
				}
				if m.Intersects(nothing) {
					t.Error("mask Intersects nothing")
				}
				if m.Equals(nothing) {
					t.Error("mask Equals nothing")
				}
				if m.IsSubsetOf(nothing) {
					t.Error("mask is a subset of nothing")
				}
				if !NewCpuMask().Equals(nothing) {
					t.Error("an empty mask does not equal nothing")
				}

				s := NewCpuSet(1, 2)
				if got := s.Union(nothing).String(); got != "1-2" {
					t.Errorf("set Union = %q", got)
				}
				if got := s.Difference(nothing).String(); got != "1-2" {
					t.Errorf("set Difference = %q", got)
				}
				if got := s.Intersection(nothing).String(); got != "" {
					t.Errorf("set Intersection = %q", got)
				}
				if s.Intersects(nothing) {
					t.Error("set Intersects nothing")
				}
				if s.Equals(nothing) {
					t.Error("set Equals nothing")
				}
				if s.IsSubsetOf(nothing) {
					t.Error("set is a subset of nothing")
				}
			})
		}
	})

	t.Run("nothing converted", func(t *testing.T) {
		for name, nothing := range map[string]CPUSet{
			"a nil interface": none,
			"a nil mask":      nilMask,
			"a nil set":       nilSet,
		} {
			t.Run(name, func(t *testing.T) {
				if m := AsCpuMask(nothing); m == nil || !m.IsEmpty() {
					t.Errorf("AsCpuMask gave %v", m)
				}
				if s := AsCpuSet(nothing); s == nil || !s.IsEmpty() {
					t.Errorf("AsCpuSet gave %v", s)
				}
				if a := NewAnyCPUSet(nothing); !a.IsEmpty() || a.Size() != 0 {
					t.Errorf("NewAnyCPUSet gave %v", a)
				}
			})
		}
	})

	t.Run("a clone of nothing is writable", func(t *testing.T) {
		m := nilMask.Clone()
		m.Set(3)
		if got := m.String(); got != "3" {
			t.Errorf("mask clone = %q", got)
		}
		s := nilSet.Clone()
		s.Set(3)
		if got := s.String(); got != "3" {
			t.Errorf("set clone = %q", got)
		}
	})

	t.Run("EmptyIfNil is writable", func(t *testing.T) {
		m := nilMask.EmptyIfNil()
		m.Set(4)
		if got := m.String(); got != "4" {
			t.Errorf("mask = %q", got)
		}
		s := nilSet.EmptyIfNil()
		s.Set(4)
		if got := s.String(); got != "4" {
			t.Errorf("set = %q", got)
		}
		// and a set which is there comes back untouched
		real := NewCpuMask(5)
		if got := real.EmptyIfNil(); got != real {
			t.Error("EmptyIfNil replaced a set which was there")
		}
		realSparse := NewCpuSet(5)
		if got := realSparse.EmptyIfNil(); got != realSparse {
			t.Error("EmptyIfNil replaced a sparse set which was there")
		}
	})

	// EmptyIfNil answers for nil and nothing else, which is the reason Clone
	// exists beside it: a sealed set comes back sealed and still cannot be
	// modified, while a clone of one always can.
	t.Run("EmptyIfNil does not unseal, Clone does", func(t *testing.T) {
		sealedMask := NewCpuMask(0, 5)
		sealedMask.Seal()
		sealedSet := NewCpuSet(0, 5)
		sealedSet.Seal()

		for name, modify := range map[string]func(){
			"mask": func() { sealedMask.EmptyIfNil().Set(9) },
			"set":  func() { sealedSet.EmptyIfNil().Set(9) },
		} {
			t.Run(name+" stays sealed", func(t *testing.T) {
				defer func() {
					if recover() == nil {
						t.Error("modifying it through EmptyIfNil did not panic")
					}
				}()
				modify()
			})
		}

		if clone := sealedMask.Clone(); func() bool {
			clone.Set(9)
			return !clone.Contains(9)
		}() {
			t.Error("a clone of a sealed mask did not take the CPU")
		}
		if clone := sealedSet.Clone(); func() bool {
			clone.Set(9)
			return !clone.Contains(9)
		}() {
			t.Error("a clone of a sealed set did not take the CPU")
		}
	})

	t.Run("modifying nothing says what to do", func(t *testing.T) {
		for name, modify := range map[string]func(){
			"mask Set":   func() { nilMask.Set(0) },
			"mask Clear": func() { nilMask.Clear(0) },
			"mask Seal":  func() { nilMask.Seal() },
			"set Set":    func() { nilSet.Set(0) },
			"set Clear":  func() { nilSet.Clear(0) },
			"set Seal":   func() { nilSet.Seal() },
		} {
			t.Run(name, func(t *testing.T) {
				defer func() {
					r := recover()
					if r == nil {
						t.Fatal("did not panic")
					}
					if msg, ok := r.(string); !ok ||
						!strings.Contains(msg, "EmptyIfNil") {
						t.Errorf("panic does not say what to do: %v", r)
					}
				}()
				modify()
			})
		}
	})
}
