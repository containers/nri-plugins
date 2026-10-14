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

package e2eish

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/stretchr/testify/require"
)

// Set is a set of topology.py names, like the sets of the variables e2e
// verify expressions see. testify asserts work on it as on any map.
type Set map[string]struct{}

// NewSet returns a set of the names.
func NewSet(names ...string) Set {
	s := Set{}
	for _, n := range names {
		s[n] = struct{}{}
	}
	return s
}

func (s Set) String() string {
	return "{" + strings.Join(slices.Sorted(maps.Keys(s)), ", ") + "}"
}

// Has tells if the set has the name.
func (s Set) Has(name string) bool {
	_, ok := s[name]
	return ok
}

// Equal tells if the sets have the same names.
func (s Set) Equal(o Set) bool {
	return len(s) == len(o) && s.IsSubset(o)
}

// IsSubset tells if o has every name of the set.
func (s Set) IsSubset(o Set) bool {
	for n := range s {
		if !o.Has(n) {
			return false
		}
	}
	return true
}

// Union returns the names of any of the sets.
func Union(sets ...Set) Set {
	out := Set{}
	for _, s := range sets {
		maps.Copy(out, s)
	}
	return out
}

// Intersection returns the names of all the sets.
func Intersection(sets ...Set) Set {
	if len(sets) == 0 {
		return Set{}
	}
	out := maps.Clone(sets[0])
	for _, s := range sets[1:] {
		maps.DeleteFunc(out, func(n string, _ struct{}) bool { return !s.Has(n) })
	}
	return out
}

// Difference returns the names of a which b does not have.
func Difference(a, b Set) Set {
	out := maps.Clone(a)
	maps.DeleteFunc(out, func(n string, _ struct{}) bool { return b.Has(n) })
	return out
}

// Disjoint is disjoint_sets: no two of the sets share a name.
func Disjoint(sets ...Set) bool {
	return shared(sets) == ""
}

// shared tells which two of the sets share which name, if any do.
func shared(sets []Set) string {
	seen := map[string]int{}
	for i, s := range sets {
		for _, n := range slices.Sorted(maps.Keys(s)) {
			if j, ok := seen[n]; ok {
				return "sets #" + strconv.Itoa(j) + " " + sets[j].String() + " and #" +
					strconv.Itoa(i) + " " + s.String() + " share " + n
			}
			seen[n] = i
		}
	}
	return ""
}

// RequireDisjoint requires that no two of the sets share a name.
func RequireDisjoint(t require.TestingT, sets ...Set) {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper()
	}
	if why := shared(sets); why != "" {
		require.Fail(t, why)
	}
}

var nameNumber = regexp.MustCompile(`^[a-z]+([0-9]+)$`)

// IDs is the *_ids helpers: the numbers of the names, sorted. Every name must
// end in a number of its own, like cpu03, node1 or package0, which is what the
// names of cpus, nodes, mems and packages do. The composite names of cores,
// threads and dies, like node1/core3, have no number of their own and panic.
func IDs(s Set) []int {
	ids := []int{}
	for n := range s {
		m := nameNumber.FindStringSubmatch(n)
		if m == nil {
			panic("e2eish: IDs of " + n + ", not a name ending in a number")
		}
		id, err := strconv.Atoi(m[1])
		if err != nil {
			panic("e2eish: IDs of " + n + ": " + err.Error())
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
