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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestNodeMask(t *testing.T) {
	for _, tc := range []struct {
		nodes []int
		want  []uint64
	}{
		{nodes: []int{0}, want: []uint64{1}},
		{nodes: []int{2, 3}, want: []uint64{0b1100}},
		{nodes: []int{0, 63}, want: []uint64{1 | 1<<63}},
		{nodes: []int{1, 64, 130}, want: []uint64{2, 1, 4}},
	} {
		if got := nodeMask(tc.nodes); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("nodeMask(%v) = %#x, want %#x", tc.nodes, got, tc.want)
		}
	}
}

func TestCheckSubset(t *testing.T) {
	have := []int{0, 1, 64, 65}
	if err := checkSubset("CPU", []int{1, 64}, have, "the allowed CPUs"); err != nil {
		t.Errorf("subset: %v", err)
	}
	err := checkSubset("CPU", []int{1, 3, 65}, have, "the allowed CPUs")
	if err == nil || !strings.Contains(err.Error(), "CPU 3 is not available (the allowed CPUs: 0-1,64-65)") {
		t.Errorf("CPU 3: error %v", err)
	}
}

func TestMemoryNodes(t *testing.T) {
	sysfs := t.TempDir()
	if _, err := memoryNodes(sysfs); err == nil || !strings.Contains(err.Error(), "cannot read the NUMA nodes with memory") {
		t.Errorf("no sysfs: error %v", err)
	}
	dir := filepath.Join(sysfs, "devices/system/node")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "has_memory"), []byte("0-1,4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := memoryNodes(sysfs)
	if err != nil || !reflect.DeepEqual(got, []int{0, 1, 4}) {
		t.Errorf("memoryNodes = %v, %v, want [0 1 4]", got, err)
	}
}

// numaNode returns a NUMA node with memory on which a bound mapping works
// here, or skips the test.
func numaNode(t *testing.T) int {
	t.Helper()
	nodes, err := memoryNodes("/sys")
	if err != nil || len(nodes) == 0 {
		t.Skipf("no NUMA nodes with memory: %v", err)
	}
	buf, err := unix.Mmap(-1, 0, 1<<20, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Munmap(buf) }()
	if err := bindMemory(buf, nodes[:1]); err != nil {
		t.Skipf("memory cannot be bound here: %v", err)
	}
	return nodes[0]
}

func TestBindAndQuery(t *testing.T) {
	node := numaNode(t)
	size := 1 << 20
	buf, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Munmap(buf) }()
	if err := bindMemory(buf, []int{node}); err != nil {
		t.Fatal(err)
	}
	var addrs []uintptr
	for off := 0; off < size; off += unix.Getpagesize() {
		buf[off] = 1
		addrs = append(addrs, uintptr(unsafe.Pointer(&buf[off])))
	}
	status, err := pageNodes(addrs)
	if errors.Is(err, errNotPermitted) {
		t.Skipf("the node of a page cannot be queried here: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range status {
		if int(got) != node {
			t.Fatalf("page %d is on node %d, want the bound node %d", i, got, node)
		}
	}
}

func TestBindRefusesAMissingNode(t *testing.T) {
	numaNode(t)
	buf, err := unix.Mmap(-1, 0, 1<<20, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Munmap(buf) }()
	if err := bindMemory(buf, []int{1000}); err == nil || !strings.Contains(err.Error(), "mbind to nodes 1000") {
		t.Errorf("bind to node 1000: error %v", err)
	}
}

func TestPageNodesWithoutPages(t *testing.T) {
	status, err := pageNodes(nil)
	if err != nil || len(status) != 0 {
		t.Errorf("pageNodes(nil) = %v, %v, want none", status, err)
	}
}

func TestFreeMemory(t *testing.T) {
	sysfs := t.TempDir()
	for node, free := range map[int]string{2: "1000", 3: "2500"} {
		dir := filepath.Join(sysfs, fmt.Sprintf("devices/system/node/node%d", node))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meminfo := fmt.Sprintf("Node %d MemTotal:  9999 kB\nNode %d MemFree:   %s kB\nNode %d MemUsed: 1 kB\n", node, node, free, node)
		if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(meminfo), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := freeMemory(sysfs, []int{2, 3})
	if err != nil || got != 3500<<10 {
		t.Errorf("freeMemory of nodes 2,3 = %d, %v, want %d", got, err, 3500<<10)
	}
	if _, err := freeMemory(sysfs, []int{4}); err == nil || !strings.Contains(err.Error(), "cannot read the free memory of node 4") {
		t.Errorf("freeMemory of node 4: error %v", err)
	}
	bad := filepath.Join(sysfs, "devices/system/node/node5")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "meminfo"), []byte("Node 5 MemTotal: 1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := freeMemory(sysfs, []int{5}); err == nil || !strings.Contains(err.Error(), "no MemFree line") {
		t.Errorf("freeMemory of node 5: error %v", err)
	}
}
