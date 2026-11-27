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
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"golang.org/x/sys/unix"
)

// Placement is where the resident pages of one file are.
type Placement struct {
	File     string `json:"file"`
	Size     int64  `json:"size"`
	PageSize int    `json:"pageSize"`
	Pages    int    `json:"pages"`
	Resident int    `json:"resident"`
	// Nodes is the number of resident pages on each NUMA node.
	Nodes map[int]int `json:"nodes"`
	// Unlocated is the number of resident pages without a node, by the
	// errno name of move_pages (ENOENT: not mapped).
	Unlocated map[string]int `json:"unlocated,omitempty"`
}

// newPlacement returns the placement of the resident pages of m from
// their move_pages status.
func newPlacement(m *fileMap, status []int32) *Placement {
	p := &Placement{
		File:     m.path,
		Size:     m.size,
		PageSize: m.pageSize,
		Pages:    m.pages(),
		Resident: len(status),
		Nodes:    map[int]int{},
	}
	for _, s := range status {
		if s >= 0 {
			p.Nodes[int(s)]++
			continue
		}
		if p.Unlocated == nil {
			p.Unlocated = map[string]int{}
		}
		p.Unlocated[errnoName(-s)]++
	}
	return p
}

// errnoName returns the name of the errno e, like "ENOENT".
func errnoName(e int32) string {
	if name := unix.ErrnoName(syscall.Errno(e)); name != "" {
		return name
	}
	return fmt.Sprintf("errno %d", e)
}

// mib returns the size of pages pages of pageSize bytes in MiB, with one
// decimal.
func mib(pages, pageSize int) string {
	return fmt.Sprintf("%.1f", float64(pages)*float64(pageSize)/(1<<20))
}

// writeTable writes the placements as a table, sizes in MiB: one row per
// file, one column per node and per errno of the unlocated pages.
func writeTable(w io.Writer, ps []*Placement) error {
	nodes, errnos := columns(ps)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := []string{"FILE", "SIZE MiB", "RESIDENT MiB"}
	for _, n := range nodes {
		head = append(head, fmt.Sprintf("NODE %d", n))
	}
	head = append(head, errnos...)
	// The writes to tw fail at its Flush.
	_, _ = fmt.Fprintln(tw, strings.Join(head, "\t"))
	for _, p := range ps {
		row := []string{p.File, fmt.Sprintf("%.1f", float64(p.Size)/(1<<20)), mib(p.Resident, p.PageSize)}
		for _, n := range nodes {
			row = append(row, share(p.Nodes[n], p.Resident, p.PageSize))
		}
		for _, e := range errnos {
			row = append(row, share(p.Unlocated[e], p.Resident, p.PageSize))
		}
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(errnos) > 0 {
		if _, err := fmt.Fprintln(w, "(the E... columns: resident pages without a node, by the error of move_pages)"); err != nil {
			return err
		}
	}
	return nil
}

// columns returns the nodes and the errno names of the placements,
// sorted.
func columns(ps []*Placement) ([]int, []string) {
	nodeSet := map[int]bool{}
	errnoSet := map[string]bool{}
	for _, p := range ps {
		for n := range p.Nodes {
			nodeSet[n] = true
		}
		for e := range p.Unlocated {
			errnoSet[e] = true
		}
	}
	nodes := make([]int, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	errnos := make([]string, 0, len(errnoSet))
	for e := range errnoSet {
		errnos = append(errnos, e)
	}
	sort.Strings(errnos)
	return nodes, errnos
}

// share returns pages pages of pageSize bytes in MiB and as a percentage
// of total pages, like "610.8 (50%)", in pages if that is below 0.1 MiB,
// like "3 pages (1%)", or "-" for none.
func share(pages, total, pageSize int) string {
	if pages == 0 {
		return "-"
	}
	size := mib(pages, pageSize)
	if float64(pages)*float64(pageSize) < 0.05*(1<<20) {
		size = pageCount(pages)
	}
	return fmt.Sprintf("%s (%.0f%%)", size, 100*float64(pages)/float64(total))
}

// pageCount returns n pages in words, like "1 page" or "3 pages".
func pageCount(n int) string {
	if n == 1 {
		return "1 page"
	}
	return fmt.Sprintf("%d pages", n)
}

// writeJSON writes the placements as a JSON array.
func writeJSON(w io.Writer, ps []*Placement) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(ps)
}
