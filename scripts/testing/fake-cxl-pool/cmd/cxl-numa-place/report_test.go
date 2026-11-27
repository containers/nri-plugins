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

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// mib1 is the number of 4 KiB pages of 1 MiB.
const mib1 = 256

// fakeMap returns a fileMap of size bytes with 4 KiB pages and no mapping.
func fakeMap(path string, size int64) *fileMap {
	return &fileMap{path: path, size: size, pageSize: 4096}
}

// statuses returns n copies of s.
func statuses(n int, s int32) []int32 {
	out := make([]int32, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestNewPlacement(t *testing.T) {
	status := append(statuses(3*mib1, 5), statuses(mib1, 6)...)
	status = append(status, -int32(unix.ENOENT), -int32(unix.ENOENT), -int32(unix.EFAULT))
	p := newPlacement(fakeMap("/pool/llm0.raw", 8<<20+1), status)
	want := &Placement{
		File:      "/pool/llm0.raw",
		Size:      8<<20 + 1,
		PageSize:  4096,
		Pages:     8*mib1 + 1,
		Resident:  4*mib1 + 3,
		Nodes:     map[int]int{5: 3 * mib1, 6: mib1},
		Unlocated: map[string]int{"ENOENT": 2, "EFAULT": 1},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("newPlacement = %+v, want %+v", p, want)
	}
	if empty := newPlacement(fakeMap("/pool/empty", 0), nil); empty.Pages != 0 || empty.Resident != 0 || len(empty.Nodes) != 0 || empty.Unlocated != nil {
		t.Errorf("newPlacement of an empty file = %+v", empty)
	}
}

func TestRuns(t *testing.T) {
	got := runs([]int{0, 1, 2, 5, 7, 8})
	want := []pageRun{{0, 2}, {5, 5}, {7, 8}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runs = %v, want %v", got, want)
	}
	if got := runs(nil); got != nil {
		t.Errorf("runs(nil) = %v", got)
	}
}

func TestShare(t *testing.T) {
	for _, tc := range []struct {
		pages, total int
		want         string
	}{
		{0, 10, "-"},
		{3 * mib1, 4 * mib1, "3.0 (75%)"},
		{3, 9, "3 pages (33%)"},
		{13, 13, "0.1 (100%)"},
	} {
		if got := share(tc.pages, tc.total, 4096); got != tc.want {
			t.Errorf("share(%d, %d) = %q, want %q", tc.pages, tc.total, got, tc.want)
		}
	}
}

func TestWriteTable(t *testing.T) {
	a := newPlacement(fakeMap("/pool/llm0.raw", 8<<20), append(statuses(3*mib1, 5), statuses(mib1, 6)...))
	b := newPlacement(fakeMap("/pool/llm0.lsa", 256<<10), append(statuses(2, 2), -int32(unix.ENOENT)))
	var out bytes.Buffer
	if err := writeTable(&out, []*Placement{a, b}); err != nil {
		t.Fatal(err)
	}
	want := `FILE            SIZE MiB  RESIDENT MiB  NODE 2         NODE 5     NODE 6     ENOENT
/pool/llm0.raw  8.0       4.0           -              3.0 (75%)  1.0 (25%)  -
/pool/llm0.lsa  0.2       0.0           2 pages (67%)  -          -          1 page (33%)
(the E... columns: resident pages without a node, by the error of move_pages)
`
	if got := out.String(); got != want {
		t.Errorf("writeTable:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteJSON(t *testing.T) {
	p := newPlacement(fakeMap("/pool/llm0.raw", 8<<20), statuses(mib1, 5))
	var out bytes.Buffer
	if err := writeJSON(&out, []*Placement{p}); err != nil {
		t.Fatal(err)
	}
	var back []*Placement
	if err := json.Unmarshal(out.Bytes(), &back); err != nil {
		t.Fatalf("writeJSON wrote %q: %v", out.String(), err)
	}
	if len(back) != 1 || !reflect.DeepEqual(back[0], p) {
		t.Errorf("writeJSON round trip = %+v, want %+v", back, p)
	}
	if !strings.Contains(out.String(), `"nodes": {`+"\n"+`      "5": 256`) {
		t.Errorf("writeJSON: no nodes by number in %s", out.String())
	}
}
