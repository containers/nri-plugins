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
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"

	"k8s.io/utils/cpuset"
)

// CPUSet represents an unordered set of CPUs. It is the common
// interface we expect from every data type that represents a set
// of CPUs. We provide two implementations: a dense [CpuMask] and
// a sparse [CpuSet] which just wraps k8s.io/utils/cpuset.CPUSet.
//
// Take this interface to accept any set of CPUs. Note that it does
// not carry the operations which produce a new set: Clone, Union,
// Difference and Intersection are on the implementations, and each
// returns its own type rather than this interface, so that a caller
// holding one does not have to assert on what it already knows.
// Wrap a set in an [AnyCPUSet] to perform those with a set of
// unknown type as the receiver.
type CPUSet interface {
	// Set adds the given CPUs to an unsealed set.
	Set(cpus ...int)
	// Clear removes the given CPUs from an unsealed set.
	Clear(cpus ...int)
	// Intersects returns true if the two sets have any common CPUs.
	Intersects(other CPUSet) bool
	// Contains returns true if all the given CPUs are in the set.
	Contains(cpus ...int) bool
	// Equals returns true if the two sets contain the same CPUs.
	Equals(other CPUSet) bool
	// Size returns the number of CPUs in the set.
	Size() int
	// IsEmpty returns true if the set contains no CPUs.
	IsEmpty() bool
	// IsSubsetOf returns true if all CPUs in this set are also in the other set.
	IsSubsetOf(other CPUSet) bool
	// List returns the list of all CPUs in the set in increasing order.
	List() []int
	// UnsortedList returns an unsorted list of all CPUs in the set.
	UnsortedList() []int
	// String returns a string representation of the set, in a Linux kernel
	// cpuset compatible format.
	String() string
	// Key returns a string usable as a map key for the set. Key() is
	// guaranteed to return the same string for two sets of the same
	// implementation if and only if the two sets are equal. Note that two
	// different implementations may return different keys for sets which
	// are equal, and [CpuMask] and [CpuSet] do.
	Key() string
	// Seal the CPUSet. Any attempt to modify a sealed set will panic.
	Seal()
	// IsDense returns true if the set implementation is dense.
	IsDense() bool
	// IsSparse returns true if the set implementation is sparse.
	IsSparse() bool
	// ForEachCpu calls the given function for each CPU in the set. Iteration
	// stops early if the function returns false.
	ForEachCpu(f func(cpu int) bool)
}

var (
	// ErrParseFailed is returned when a CPUSet string cannot be parsed.
	ErrParseFailed = errors.New("failed to parse CPU set")
)

var (
	// EmptyCpuMask is a CpuMask with no CPUs in it, for [CpuMask.EmptyIfNil] to
	// answer with. It is sealed: it is shared by everyone who asks, so modifying
	// it panics instead of changing what every other caller sees.
	EmptyCpuMask = sealedEmptyCpuMask()

	// EmptyCpuSet is the same for [CpuSet.EmptyIfNil].
	EmptyCpuSet = sealedEmptyCpuSet()
)

func sealedEmptyCpuMask() *CpuMask {
	cpus := NewCpuMask()
	cpus.Seal()
	return cpus
}

func sealedEmptyCpuSet() *CpuSet {
	cpus := NewCpuSet()
	cpus.Seal()
	return cpus
}

// CpuMask is a dense implementation of CPUSet. It uses bitmasks
// to store CPUs and is a good choice for large CPU sets with many
// CPUs.
type CpuMask struct {
	mask []uint64
	seal bool
	size int
	str  string
	key  string
}

// CpuMask should implement CPUSet.
var _ CPUSet = (*CpuMask)(nil)

// NewCpuMask returns a new CpuMask containing the given CPUs.
func NewCpuMask(cpus ...int) *CpuMask {
	words := 0
	if cnt := len(cpus); cnt > 0 {
		hi := max(cpus[0], cpus[cnt-1])
		words = hi/64 + 1
	}
	mask := make([]uint64, words)
	for _, cpu := range cpus {
		w, b := cpu/64, cpu&63
		mask = expand(mask, w)
		mask[w] |= 1 << b
	}
	return newCpuMask(mask)
}

func newCpuMask(mask []uint64) *CpuMask {
	return &CpuMask{mask: mask, size: -1}
}

// ParseCpuMask parses the given string representation of a CPU set
// and returns a corresponding new CpuMask.
func ParseCpuMask(s string) (*CpuMask, error) {
	m := NewCpuMask()
	if s == "" {
		return m, nil
	}

	for _, part := range strings.Split(s, ",") {
		if !strings.Contains(part, "-") {
			cpu, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrParseFailed, err)
			}
			m.Set(cpu)
			continue
		}

		rng := strings.SplitN(part, "-", 2)
		min, err := strconv.Atoi(rng[0])
		if err != nil {
			return nil, fmt.Errorf("%w: invalid range start %q: %w",
				ErrParseFailed, rng[0], err)
		}
		max, err := strconv.Atoi(rng[1])
		if err != nil {
			return nil, fmt.Errorf("%w: invalid range end %q: %w",
				ErrParseFailed, rng[1], err)
		}
		if min > max {
			return nil, fmt.Errorf("%w: invalid range %q", ErrParseFailed, part)
		}

		for cpu := min; cpu <= max; cpu++ {
			m.Set(cpu)
		}
	}

	return m, nil
}

// MustParseCpuMask is [ParseCpuMask] for a string which is known to be a CPU
// set: a constant in a test, or a value some other component has already
// validated. It panics if the string does not parse.
func MustParseCpuMask(s string) *CpuMask {
	cpus, err := ParseCpuMask(s)
	if err != nil {
		panic(err)
	}
	return cpus
}

// words returns the mask's bitmap words, and none for a nil mask. Every read
// below goes through this, which is what lets a nil mask be read as the empty
// set: ranging over no words yields nothing and len() of them is zero.
func (m *CpuMask) words() []uint64 {
	if m == nil {
		return nil
	}
	return m.mask
}

// isNothing reports whether a set is nothing at all: a nil interface, or a nil
// CpuMask or CpuSet inside one. Such a set reads as the empty set, and this is
// how the operations below tell.
func isNothing(cpus CPUSet) bool {
	switch o := cpus.(type) {
	case nil:
		return true
	case *CpuMask:
		return o == nil
	case *CpuSet:
		return o == nil
	}
	return false
}

// operandMask resolves a set given to one of the operations below. The second
// result says whether the first is usable as the operand's bitmap: a CpuMask, or
// a set which is nothing at all. Anything else the caller has to read through
// the CPUSet interface.
//
// Note the order. The type assertion comes first because it is the fast path and
// it already answers for a nil CpuMask, whose words() are none; isNothing is a
// type switch, and asking it first cost a couple of nanoseconds on the cheapest
// operations, which is most of what they take.
func operandMask(other CPUSet) (*CpuMask, bool) {
	if m, ok := other.(*CpuMask); ok {
		return m, true
	}
	return nil, isNothing(other)
}

// Clone returns a copy of the mask which is safe to modify: unsealed, and with
// no other owner. A nil mask clones into a new empty one, so
//
//	cpus = cpus.Clone()
//
// is the way to get something writable out of a set of unknown provenance,
// whether it is nil, sealed, or shared with whoever handed it over.
// [CpuMask.EmptyIfNil] is the cheaper answer when only nil is in question.
func (m *CpuMask) Clone() *CpuMask {
	if m == nil {
		return NewCpuMask()
	}
	return &CpuMask{
		mask: slices.Clone(m.mask),
		str:  m.str,
		key:  m.key,
		size: m.size,
	}
}

// EmptyIfNil returns the mask, or a new empty one if it is nil.
//
// Reading a nil mask needs no help: every operation which does not modify one
// treats it as the empty set. This is for the ones which do. A nil mask cannot
// be modified, and no method can fix that by itself, since it cannot store a
// mask it just made back into the caller's variable. So assign it first:
//
//	cpus = cpus.EmptyIfNil()
//	cpus.Set(0, 1)
//
// The new mask is the caller's own, not the shared [EmptyCpuMask], which is
// sealed and would panic on the Set.
//
// This answers for nil and nothing else: a mask which is there comes back as it
// is, sealed included, and modifying that still panics. [CpuMask.Clone] is the
// one which always returns something writable.
func (m *CpuMask) EmptyIfNil() *CpuMask {
	if m == nil {
		return NewCpuMask()
	}
	return m
}

func (m *CpuMask) Set(cpus ...int) {
	m.panicIfNilOrSealed()

	for _, cpu := range cpus {
		w, b := cpu/64, cpu&63
		m.mask = expand(m.mask, w)
		m.mask[w] |= 1 << b
	}

	m.invalidateCached()
}

func (m *CpuMask) Clear(cpus ...int) {
	m.panicIfNilOrSealed()

	for _, cpu := range cpus {
		w, b := cpu/64, cpu&63
		if w < len(m.mask) {
			m.mask[w] &^= 1 << b
		}
	}

	m.invalidateCached()
}

func (m *CpuMask) Difference(other CPUSet) *CpuMask {
	o, ok := operandMask(other)
	if !ok {
		o = NewCpuMask(other.UnsortedList()...)
	}

	mine, theirs := m.words(), o.words()
	r := make([]uint64, len(mine))

	for w, v := range mine {
		if w < len(theirs) {
			r[w] = v &^ theirs[w]
		} else {
			r[w] = v
		}
	}

	return newCpuMask(r)
}

func (m *CpuMask) Intersection(other CPUSet) *CpuMask {
	o, ok := operandMask(other)
	if !ok {
		r := NewCpuMask()
		for _, cpu := range other.UnsortedList() {
			if m.Contains(cpu) {
				r.Set(cpu)
			}
		}
		return r
	}

	mine, theirs := m.words(), o.words()
	r := make([]uint64, min(len(mine), len(theirs)))
	for w := range r {
		r[w] = mine[w] & theirs[w]
	}

	return newCpuMask(r)
}

func (m *CpuMask) Intersects(other CPUSet) bool {
	o, ok := operandMask(other)
	if !ok {
		for _, cpu := range other.UnsortedList() {
			if m.Contains(cpu) {
				return true
			}
		}
		return false
	}

	if m == nil || o == nil {
		return false
	}

	c := min(len(m.mask), len(o.mask))
	for w := 0; w < c; w++ {
		if m.mask[w]&o.mask[w] != 0 {
			return true
		}
	}

	return false
}

func (m *CpuMask) Union(others ...CPUSet) *CpuMask {
	r := newCpuMask(slices.Clone(m.words()))

	for _, other := range others {
		o, ok := operandMask(other)
		if !ok {
			for _, cpu := range other.UnsortedList() {
				r.Set(cpu)
			}
			continue
		}

		theirs := o.words()
		r.mask = expand(r.mask, len(theirs)-1)
		for w, v := range theirs {
			r.mask[w] |= v
		}
	}

	return r
}

func (m *CpuMask) Contains(cpus ...int) bool {
	mine := m.words()
	for _, cpu := range cpus {
		w, b := cpu/64, cpu&63
		if w >= len(mine) || (mine[w]&(1<<b)) == 0 {
			return false
		}
	}
	return true
}

func (m *CpuMask) Equals(other CPUSet) bool {
	o, ok := operandMask(other)
	if !ok {
		if m.Size() != other.Size() {
			return false
		}

		for _, cpu := range other.UnsortedList() {
			if !m.Contains(cpu) {
				return false
			}
		}

		return true
	}

	if m == nil {
		return o.IsEmpty()
	}
	if o == nil {
		return m.IsEmpty()
	}

	c := min(len(m.mask), len(o.mask))
	for w := 0; w < c; w++ {
		if m.mask[w] != o.mask[w] {
			return false
		}
	}

	switch {
	case c < len(m.mask):
		for w := c; w < len(m.mask); w++ {
			if m.mask[w] != 0 {
				return false
			}
		}
	case c < len(o.mask):
		for w := c; w < len(o.mask); w++ {
			if o.mask[w] != 0 {
				return false
			}
		}
	}

	return true
}

func (m *CpuMask) Size() int {
	if m == nil {
		return 0
	}
	if m.seal || m.size >= 0 {
		return m.size
	}

	size := 0
	for _, v := range m.mask {
		size += bits.OnesCount64(v)
	}
	m.size = size

	return size
}

func (m *CpuMask) IsEmpty() bool {
	for _, v := range m.words() {
		if v != 0 {
			return false
		}
	}

	return true
}

func (m *CpuMask) IsSubsetOf(other CPUSet) bool {
	o, ok := operandMask(other)
	if !ok {
		return other.Contains(m.UnsortedList()...)
	}

	if m == nil {
		return true
	}
	if o == nil {
		return m.IsEmpty()
	}

	c := min(len(m.mask), len(o.mask))
	for w := 0; w < c; w++ {
		if m.mask[w]&^o.mask[w] != 0 {
			return false
		}
	}

	if c < len(m.mask) {
		for w := c; w < len(m.mask); w++ {
			if m.mask[w] != 0 {
				return false
			}
		}
	}

	return true
}

func (m *CpuMask) List() []int {
	cpus := make([]int, 0, m.Size())

	m.ForEachCpu(func(cpu int) bool {
		cpus = append(cpus, cpu)
		return true
	})

	return cpus
}

func (m *CpuMask) UnsortedList() []int {
	return m.List()
}

func (m *CpuMask) String() string {
	if m == nil {
		return ""
	}
	if m.seal || m.str != "" {
		return m.str
	}

	var (
		str        strings.Builder
		rangeStart = -1
		prev       = -1
	)

	flush := func(end int) {
		if rangeStart < 0 {
			return
		}
		if str.Len() > 0 {
			str.WriteString(",")
		}
		str.WriteString(strconv.Itoa(rangeStart))
		if end > rangeStart {
			str.WriteString("-")
			str.WriteString(strconv.Itoa(end))
		}
		rangeStart = -1
	}

	m.ForEachCpu(func(cpu int) bool {
		if rangeStart >= 0 && cpu == prev+1 {
			prev = cpu
			return true
		}
		flush(prev)
		rangeStart = cpu
		prev = cpu
		return true
	})
	flush(prev)

	m.str = str.String()

	return m.str
}

func (m *CpuMask) Key() string {
	if m == nil {
		return ""
	}
	if m.seal || m.key != "" {
		return m.key
	}

	// Trailing all-zero words must not contribute to the key, otherwise
	// two equal sets with differently sized masks get different keys.
	mask := m.mask
	for len(mask) > 0 && mask[len(mask)-1] == 0 {
		mask = mask[:len(mask)-1]
	}

	buf, sep := strings.Builder{}, ""
	for _, w := range mask {
		buf.WriteString(sep)
		buf.WriteString(strconv.FormatUint(w, 16))
		sep = "-"
	}
	m.key = buf.String()

	return m.key
}

func (m *CpuMask) Seal() {
	if m == nil {
		panic("cannot seal a nil CpuMask: assign cpus = cpus.EmptyIfNil() first")
	}
	_ = m.Key()
	_ = m.String()
	_ = m.Size()
	m.seal = true
}

func (*CpuMask) IsDense() bool {
	return true
}

func (*CpuMask) IsSparse() bool {
	return false
}

func (m *CpuMask) ForEachCpu(f func(cpu int) bool) {
	for w, v := range m.words() {
		for v != 0 {
			if !f(w*64 + bits.TrailingZeros64(v)) {
				return
			}
			v &= v - 1
		}
	}
}

func (m *CpuMask) panicIfNilOrSealed() {
	if m == nil {
		panic("cannot modify a nil CpuMask: assign cpus = cpus.EmptyIfNil() first")
	}
	if m.seal {
		panic("CpuMask is sealed")
	}
}

func (m *CpuMask) invalidateCached() {
	m.str = ""
	m.key = ""
	m.size = -1
}

func expand(mask []uint64, w int) []uint64 {
	if w < len(mask) {
		return mask
	}
	for len(mask) <= w {
		mask = append(mask, 0)
	}
	return mask
}

// CpuSet is a sparse implementation of CPUSet. Internally it
// wraps k8s.io/utils/cpuset.CPUSet and is a good choice for
// representing CPU sets which contain a few CPUs.
type CpuSet struct {
	cpuset.CPUSet
	seal bool
	str  string
}

// CpuSet should implement CPUSet.
var _ CPUSet = (*CpuSet)(nil)

// NewCpuSet returns a new CpuSet containing the given CPUs.
func NewCpuSet(cpus ...int) *CpuSet {
	s := &CpuSet{
		CPUSet: cpuset.New(cpus...),
	}
	return s
}

// WrapCpuSet returns a CpuSet backed by the given k8s.io/utils/cpuset.CPUSet,
// without copying it.
//
// It is the cheap way in from code which speaks the k8s type -- one allocation
// for the wrapper, against the list-and-rebuild that a NewCpuSet of its members
// costs -- and is worth reaching for where a set arrives from an upstream
// interface. Put a [NewAnyCPUSet] around the result to get the set-producing
// operations as well.
//
// Sharing the set is safe in both directions. cpuset.CPUSet is immutable, and
// Set and Clear here replace the embedded value rather than modify it, so
// neither side disturbs the other. The embedded field stays reachable, so the
// raw set can be had back without a copy too.
func WrapCpuSet(cpus cpuset.CPUSet) *CpuSet {
	return &CpuSet{
		CPUSet: cpus,
	}
}

// ParseCpuSet parses the given string representation of a CPU set
// and returns a corresponding new CpuSet.
func ParseCpuSet(s string) (*CpuSet, error) {
	cpus, err := cpuset.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrParseFailed, err)
	}
	return &CpuSet{
		CPUSet: cpus,
	}, nil
}

// MustParseCpuSet is [MustParseCpuMask] for the sparse implementation.
func MustParseCpuSet(s string) *CpuSet {
	cpus, err := ParseCpuSet(s)
	if err != nil {
		panic(err)
	}
	return cpus
}

// emptyCpuSetValue is what a nil set reads as. The k8s type is immutable -- its
// methods all return new sets -- so one shared empty is safe to hand around.
var emptyCpuSetValue = cpuset.New()

// cpus returns the set's CPUs, and none for a nil set. This is [CpuMask.words]
// for the sparse implementation: every read below goes through it, which is what
// lets a nil set be read as the empty one.
func (s *CpuSet) cpus() cpuset.CPUSet {
	if s == nil {
		return emptyCpuSetValue
	}
	return s.CPUSet
}

// operandCpuSet is [operandMask] for the sparse implementation, in the same
// order and for the same reason.
func operandCpuSet(other CPUSet) (*CpuSet, bool) {
	if s, ok := other.(*CpuSet); ok {
		return s, true
	}
	return nil, isNothing(other)
}

// Clone returns a copy of the set which is safe to modify: unsealed, and with no
// other owner. A nil set clones into a new empty one, as [CpuMask.Clone] does,
// and for the same purpose.
func (s *CpuSet) Clone() *CpuSet {
	if s == nil {
		return NewCpuSet()
	}
	return &CpuSet{
		CPUSet: s.CPUSet.Clone(),
	}
}

// EmptyIfNil returns the set, or a new empty one if it is nil. It is
// [CpuMask.EmptyIfNil] for the sparse implementation, with the same purpose and
// the same limit: it answers for nil, not for sealed. Reading a nil set needs no
// help, and [CpuSet.Clone] is the one which always returns something writable.
func (s *CpuSet) EmptyIfNil() *CpuSet {
	if s == nil {
		return NewCpuSet()
	}
	return s
}

func (s *CpuSet) Set(cpus ...int) {
	s.panicIfNilOrSealed()
	s.CPUSet = s.CPUSet.Union(cpuset.New(cpus...))
	s.str = ""
}

func (s *CpuSet) Clear(cpus ...int) {
	s.panicIfNilOrSealed()
	s.CPUSet = s.CPUSet.Difference(cpuset.New(cpus...))
	s.str = ""
}

func (s *CpuSet) Difference(other CPUSet) *CpuSet {
	o, ok := operandCpuSet(other)
	if ok {
		return &CpuSet{
			CPUSet: s.cpus().Difference(o.cpus()),
		}
	}

	cpus := make([]int, 0, s.cpus().Size())
	for _, cpu := range s.cpus().UnsortedList() {
		if !other.Contains(cpu) {
			cpus = append(cpus, cpu)
		}
	}
	return NewCpuSet(cpus...)
}

func (s *CpuSet) Intersection(other CPUSet) *CpuSet {
	o, ok := operandCpuSet(other)
	if ok {
		return &CpuSet{
			CPUSet: s.cpus().Intersection(o.cpus()),
		}
	}

	cpus := make([]int, 0, s.cpus().Size())
	for _, cpu := range s.cpus().UnsortedList() {
		if other.Contains(cpu) {
			cpus = append(cpus, cpu)
		}
	}
	return NewCpuSet(cpus...)
}

func (s *CpuSet) Intersects(other CPUSet) bool {
	if o, ok := operandCpuSet(other); ok {
		a, b := s.cpus(), o.cpus()
		if b.Size() < a.Size() {
			a, b = b, a
		}
		for _, cpu := range a.UnsortedList() {
			if b.Contains(cpu) {
				return true
			}
		}
		return false
	}

	if other.Size() < s.cpus().Size() {
		for _, cpu := range other.UnsortedList() {
			if s.cpus().Contains(cpu) {
				return true
			}
		}
		return false
	}

	for _, cpu := range s.cpus().UnsortedList() {
		if other.Contains(cpu) {
			return true
		}
	}
	return false
}

func (s *CpuSet) Union(others ...CPUSet) *CpuSet {
	r := s.cpus().Clone()
	for _, other := range others {
		o, ok := operandCpuSet(other)
		if ok {
			r = r.Union(o.cpus())
			continue
		}

		r = r.Union(cpuset.New(other.UnsortedList()...))
	}
	return &CpuSet{CPUSet: r}
}

func (s *CpuSet) Contains(cpus ...int) bool {
	mine := s.cpus()
	for _, cpu := range cpus {
		if !mine.Contains(cpu) {
			return false
		}
	}
	return true
}

func (s *CpuSet) Equals(other CPUSet) bool {
	o, ok := operandCpuSet(other)
	if ok {
		return s.cpus().Equals(o.cpus())
	}

	return other.Contains(s.UnsortedList()...) && s.Contains(other.UnsortedList()...)
}

func (s *CpuSet) Size() int {
	return s.cpus().Size()
}

func (s *CpuSet) IsEmpty() bool {
	return s.cpus().IsEmpty()
}

func (s *CpuSet) IsSubsetOf(other CPUSet) bool {
	o, ok := operandCpuSet(other)
	if ok {
		return s.cpus().IsSubsetOf(o.cpus())
	}

	return other.Contains(s.cpus().UnsortedList()...)
}

func (s *CpuSet) List() []int {
	return s.cpus().List()
}

func (s *CpuSet) UnsortedList() []int {
	return s.cpus().UnsortedList()
}

func (s *CpuSet) String() string {
	if s == nil {
		return ""
	}
	if s.seal || s.str != "" {
		return s.str
	}

	s.str = s.CPUSet.String()

	return s.str
}

func (s *CpuSet) Key() string {
	return s.String()
}

func (s *CpuSet) Seal() {
	if s == nil {
		panic("cannot seal a nil CpuSet: assign cpus = cpus.EmptyIfNil() first")
	}
	_ = s.String()
	s.seal = true
}

func (*CpuSet) IsDense() bool {
	return false
}

func (*CpuSet) IsSparse() bool {
	return true
}

func (m *CpuSet) ForEachCpu(f func(cpu int) bool) {
	for _, cpu := range m.UnsortedList() {
		if !f(cpu) {
			return
		}
	}
}

func (s *CpuSet) panicIfNilOrSealed() {
	if s == nil {
		panic("cannot modify a nil CpuSet: assign cpus = cpus.EmptyIfNil() first")
	}
	if s.seal {
		panic("CpuSet is sealed")
	}
}

//
// Working with a set of unknown implementation
//

// AsCpuMask returns cpus as a [CpuMask]: cpus itself if it already is one, a new
// dense set holding the same CPUs otherwise. Note that converting a sparse set
// which holds a few high-numbered CPUs allocates a mask large enough to reach
// them.
func AsCpuMask(cpus CPUSet) *CpuMask {
	if m, ok := operandMask(cpus); ok {
		return m.EmptyIfNil()
	}
	return NewCpuMask(cpus.UnsortedList()...)
}

// AsCpuSet returns cpus as a [CpuSet]: cpus itself if it already is one, a new
// sparse set holding the same CPUs otherwise.
func AsCpuSet(cpus CPUSet) *CpuSet {
	if s, ok := operandCpuSet(cpus); ok {
		return s.EmptyIfNil()
	}
	return NewCpuSet(cpus.UnsortedList()...)
}

// AnyCPUSet is a [CPUSet] of unknown implementation with the set-producing
// operations put back: Clone, Union, Difference and Intersection are on the
// implementations, so a caller which only has the interface cannot use one as
// the receiver of those. Wrapping it here makes that possible.
//
// Reach for this only when the implementation genuinely is not known. Holding a
// [CpuMask] or a [CpuSet], call the operations directly and get the same type
// back; that is the common case and it costs nothing. This one pays for a type
// switch and an interface call per operation.
//
// The result of an operation keeps the implementation it was performed on, so a
// wrapped sparse set stays sparse. A set from neither implementation here is
// answered as a [CpuMask], there being no better guess. The embedded CPUSet
// provides everything else, and an AnyCPUSet is itself a CPUSet.
type AnyCPUSet struct {
	CPUSet
}

// NewAnyCPUSet wraps cpus so that the set-producing operations can be performed
// with it as the receiver.
func NewAnyCPUSet(cpus CPUSet) AnyCPUSet {
	// Nothing wrapped is the empty set, so that a wrapper reads like the sets it
	// wraps. Sealed: it is shared, and has no owner to modify it.
	if isNothing(cpus) {
		return AnyCPUSet{CPUSet: EmptyCpuMask}
	}
	return AnyCPUSet{CPUSet: cpus}
}

// Clone returns a new unsealed copy of the set.
func (a AnyCPUSet) Clone() AnyCPUSet {
	switch s := a.CPUSet.(type) {
	case *CpuMask:
		return AnyCPUSet{CPUSet: s.Clone()}
	case *CpuSet:
		return AnyCPUSet{CPUSet: s.Clone()}
	}
	return AnyCPUSet{CPUSet: AsCpuMask(a.CPUSet).Clone()}
}

// Union returns a new set with all CPUs in this set or in at least one of the
// other sets.
func (a AnyCPUSet) Union(others ...CPUSet) AnyCPUSet {
	switch s := a.CPUSet.(type) {
	case *CpuMask:
		return AnyCPUSet{CPUSet: s.Union(others...)}
	case *CpuSet:
		return AnyCPUSet{CPUSet: s.Union(others...)}
	}
	return AnyCPUSet{CPUSet: AsCpuMask(a.CPUSet).Union(others...)}
}

// Difference returns a new set with all CPUs in this set which are not in the
// other set.
func (a AnyCPUSet) Difference(other CPUSet) AnyCPUSet {
	switch s := a.CPUSet.(type) {
	case *CpuMask:
		return AnyCPUSet{CPUSet: s.Difference(other)}
	case *CpuSet:
		return AnyCPUSet{CPUSet: s.Difference(other)}
	}
	return AnyCPUSet{CPUSet: AsCpuMask(a.CPUSet).Difference(other)}
}

// Intersection returns a new set with the CPUs which are in both sets.
func (a AnyCPUSet) Intersection(other CPUSet) AnyCPUSet {
	switch s := a.CPUSet.(type) {
	case *CpuMask:
		return AnyCPUSet{CPUSet: s.Intersection(other)}
	case *CpuSet:
		return AnyCPUSet{CPUSet: s.Intersection(other)}
	}
	return AnyCPUSet{CPUSet: AsCpuMask(a.CPUSet).Intersection(other)}
}

// AnyCPUSet should implement CPUSet.
var _ CPUSet = AnyCPUSet{}
