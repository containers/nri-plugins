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

package fsm

import (
	"sync"
	"sync/atomic"
)

// Rows are the rows of the tables that Apply applied while they were
// counted (CountRows): what the tests of a package exercised.
type Rows struct {
	mu      sync.Mutex
	applied map[string]map[int]bool
}

// counting is the Rows that Apply counts its rows in, nil when no one
// counts.
var counting atomic.Pointer[Rows]

// CountRows makes every Apply of the process count its row in a new Rows
// and returns it; the rows that Apply applied before are not in it.
func CountRows() *Rows {
	r := &Rows{applied: map[string]map[int]bool{}}
	counting.Store(r)
	return r
}

// add counts row of the table of field of object.
func (r *Rows) add(object, field string, row int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := object + "." + field
	if r.applied[name] == nil {
		r.applied[name] = map[int]bool{}
	}
	r.applied[name][row] = true
}

// Applied tells whether Apply applied the row of the table of field of
// object.
func (r *Rows) Applied(object, field string, row int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied[object+"."+field][row]
}

// Unapplied returns the indexes of the rows of the table that Apply did not
// apply.
func (r *Rows) Unapplied(t TableText) []int {
	var out []int
	for i := range t.Rows {
		if !r.Applied(t.Object, t.Field, i) {
			out = append(out, i)
		}
	}
	return out
}
