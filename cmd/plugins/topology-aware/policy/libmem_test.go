// Copyright 2026 Intel Corporation. All Rights Reserved.
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

package topologyaware

import (
	"flag"
	"fmt"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"testing"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils"
	"github.com/go-logr/logr"
	m "github.com/ozhuraki/gofmbt/gofmbt"
	"k8s.io/klog/v2"
)

// LibmemState is the abstract model state for TestLibmemGofmbt2. It
// tracks how many bytes are free and which named allocations are live.
type LibmemState struct {
	freeBytes int64
	allocs    map[string]int64 // abstract name -> allocated size
}

// setupTestPolicy creates a policy from the server sysfs testdata.
// If testdata/sysfs/server/sys already exists in the current directory it is
// used directly and the returned dir is empty (caller must not delete it).
// Otherwise the tarball is unpacked into a temp dir and that dir is returned
// so the caller can clean it up with removeAll.
func setupTestPolicy(t *testing.T) (*policy, string) {
	t.Helper()

	const preUnpacked = "testdata/sysfs/server/sys"
	var sysPath string
	var dir string

	if _, err := os.Stat(preUnpacked); err == nil {
		sysPath = preUnpacked
	} else {
		var err error
		dir, err = os.MkdirTemp("", "nri-libmem-test-")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
		if err := utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
			if rerr := os.RemoveAll(dir); rerr != nil {
				t.Logf("failed to remove temp dir %q: %v", dir, rerr)
			}
			t.Fatalf("failed to uncompress testdata: %v", err)
		}
		sysPath = path.Join(dir, "sysfs", "server", "sys")
	}

	sys, err := system.DiscoverSystemAt(sysPath)
	if err != nil {
		if dir != "" {
			if rerr := os.RemoveAll(dir); rerr != nil {
				t.Logf("failed to remove temp dir %q: %v", dir, rerr)
			}
		}
		t.Fatalf("failed to discover system: %v", err)
	}

	p := New().(*policy)
	if err := p.Setup(&policyapi.BackendOptions{
		Cache:  &mockCache{},
		System: sys,
		Config: &cfgapi.Config{
			ReservedResources: cfgapi.Constraints{cfgapi.CPU: "750m"},
		},
	}); err != nil {
		if dir != "" {
			if rerr := os.RemoveAll(dir); rerr != nil {
				t.Logf("failed to remove temp dir %q: %v", dir, rerr)
			}
		}
		t.Fatalf("failed to setup policy: %v", err)
	}
	printSystemDRAM(sys)
	return p, dir
}

// printSystemDRAM prints DRAM capacity per NUMA node and the total.
func printSystemDRAM(sys system.System) {
	var total uint64
	for _, id := range sys.NodeIDs() {
		n := sys.Node(id)
		if n.GetMemoryType() != system.MemoryTypeDRAM {
			continue
		}
		info, err := n.MemoryInfo()
		if err != nil || info == nil {
			continue
		}
		fmt.Printf("  NUMA node %d DRAM: %s\n", id, formatBytes(info.MemTotal))
		total += info.MemTotal
	}
	fmt.Printf("  DRAM total: %s\n", formatBytes(total))
}

// formatBytes formats a byte count in a human-readable form (GiB/MiB/KiB/B).
func formatBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// TestLibmemGetMemOfferByHintsMemoryPreserve verifies that getMemOfferByHints
// returns an error immediately when memoryPreserve is requested.
func TestLibmemGetMemOfferByHintsMemoryPreserve(t *testing.T) {
	p, dir := setupTestPolicy(t)
	defer removeAll(t, dir)

	pool := p.pools[0]
	req := &request{
		memType:   memoryPreserve,
		container: &mockContainer{},
	}

	_, err := p.getMemOfferByHints(pool, req)
	if err == nil {
		t.Fatal("expected error for memoryPreserve, got nil")
	}
	if !strings.Contains(err.Error(), "memoryPreserve") {
		t.Errorf("expected 'memoryPreserve' in error, got: %v", err)
	}
}

// TestLibmemGetMemOfferByHintsNoHints verifies that getMemOfferByHints returns an
// error when the container has no pod resource API topology hints.
func TestLibmemGetMemOfferByHintsNoHints(t *testing.T) {
	p, dir := setupTestPolicy(t)
	defer removeAll(t, dir)

	// Find a leaf NUMA node with DRAM.
	var pool Node
	for _, n := range p.pools {
		if n.IsLeafNode() && n.HasMemoryType(memoryDRAM) {
			pool = n
			break
		}
	}
	if pool == nil {
		t.Fatal("no leaf DRAM node found in test system")
	}

	req := &request{
		memType:   memoryDRAM,
		container: &mockContainer{}, // GetTopologyHints() returns empty map
	}

	_, err := p.getMemOfferByHints(pool, req)
	if err == nil {
		t.Fatal("expected error when no hints provided, got nil")
	}
	if !strings.Contains(err.Error(), "no pod resource API hints") {
		t.Errorf("expected 'no pod resource API hints' in error, got: %v", err)
	}
}

// mallocSeq is used to generate unique container IDs in malloc.
var mallocSeq int

// mallocSizeByID tracks the allocated size per container ID so free() can print it.
var mallocSizeByID = map[string]int64{}

// malloc allocates memory of the given size on a leaf DRAM node of the policy
// and returns the container ID of the committed allocation.
func malloc(p *policy, size int64) (string, error) {
	mallocSeq++
	id := fmt.Sprintf("%d", mallocSeq)

	fmt.Printf("malloc(%dGB)", size>>30)
	if fmbtV >= 1 {
		fmt.Printf(" id=%s", id)
	}
	for i := 1; i <= callerDepth; i++ {
		pc, _, _, ok := runtime.Caller(i)
		if !ok {
			break
		}
		full := runtime.FuncForPC(pc).Name()
		short := full[strings.LastIndex(full, "/")+1:]
		short = short[strings.Index(short, ".")+1:]
		fmt.Printf(" %s()", short)
	}
	fmt.Println()

	var pool Node
	for _, n := range p.pools {
		if n.IsLeafNode() && n.HasMemoryType(memoryDRAM) {
			pool = n
			break
		}
	}
	if pool == nil {
		return "", fmt.Errorf("no leaf DRAM node found in test system")
	}
	ctr := &mockContainer{returnValueForGetID: id}
	req := &request{
		memType:   memoryDRAM,
		memReq:    size,
		container: ctr,
	}
	offer, err := p.getMemOffer(pool, req)
	if err != nil {
		return "", fmt.Errorf("getMemOffer failed: %w", err)
	}
	if _, _, err := offer.Commit(); err != nil {
		return "", fmt.Errorf("Offer.Commit() failed: %w", err)
	}
	mallocSizeByID[id] = size
	return id, nil
}

// free releases a previously committed memory allocation for the given container ID.
func free(p *policy, id string) error {
	fmt.Printf("free(%dGB)", mallocSizeByID[id]>>30)
	if fmbtV >= 1 {
		fmt.Printf(" id=%s", id)
	}
	for i := 1; i <= callerDepth; i++ {
		pc, _, _, ok := runtime.Caller(i)
		if !ok {
			break
		}
		full := runtime.FuncForPC(pc).Name()
		short := full[strings.LastIndex(full, "/")+1:]
		short = short[strings.Index(short, ".")+1:]
		fmt.Printf(" %s()", short)
	}
	fmt.Println()
	err := p.releaseMem(id)
	if err == nil {
		delete(mallocSizeByID, id)
	}
	return err
}

// TestLibmemReleaseMem verifies that releaseMem releases a previously committed
// memory allocation, and returns an error for an unknown ID.
func TestLibmemReleaseMem(t *testing.T) {
	p, dir := setupTestPolicy(t)
	defer removeAll(t, dir)

	id, err := malloc(p, 64*1024*1024) // 64 MiB
	if err != nil {
		t.Fatalf("malloc failed: %v", err)
	}

	if err := free(p, id); err != nil {
		t.Errorf("free failed for known ID: %v", err)
	}

	// Releasing the same ID again should return an error (unknown request).
	if err := free(p, id); err == nil {
		t.Error("expected error releasing unknown ID, got nil")
	}
}

// TestLibmemPoolZoneCapacityAndFree verifies that poolZoneCapacity returns a
// positive value and that poolZoneFree does not exceed it.
func TestLibmemPoolZoneCapacityAndFree(t *testing.T) {
	p, dir := setupTestPolicy(t)
	defer removeAll(t, dir)

	var pool Node
	for _, n := range p.pools {
		if n.IsLeafNode() && n.HasMemoryType(memoryDRAM) {
			pool = n
			break
		}
	}
	if pool == nil {
		t.Fatal("no leaf DRAM node found in test system")
	}

	capacity := p.poolZoneCapacity(pool, memoryDRAM)
	free := p.poolZoneFree(pool, memoryDRAM)

	if capacity <= 0 {
		t.Errorf("expected positive DRAM capacity, got %d", capacity)
	}
	if free < 0 || free > capacity {
		t.Errorf("expected 0 <= free (%d) <= capacity (%d)", free, capacity)
	}
}

func (s *LibmemState) String() string {
	names := make([]string, 0, len(s.allocs))
	for name := range s.allocs {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("[free:%dGB allocs:[%s]]", s.freeBytes>>30, strings.Join(names, " "))
}

var (
	maxLibmem2Steps int
	libmem2Search   int
	callerDepth     int
	fmbtV           int
)

// init registers flags and switches CommandLine to ContinueOnError so that
// flags passed via -args on the command line are accepted.
func init() {
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	flag.IntVar(&maxLibmem2Steps, "libmem2-steps", 1000, "number of test steps for TestLibmemGofmbt2")
	flag.IntVar(&libmem2Search, "libmem2-search-depth", 4, "look-ahead depth for TestLibmemGofmbt2")
	flag.IntVar(&callerDepth, "caller-depth", 1, "number of caller frames printed by malloc() and free()")
	flag.IntVar(&fmbtV, "fmbt-v", 0, "verbosity for TestLibmemGofmbt2: 1=basic, 2=include caller info in mallocFn/freeFn")
}

// TestLibmemGofmbt uses gofmbt model-based testing to drive malloc/free
// sequences against the policy, verifying that all operations succeed.
func TestLibmemGofmbt(t *testing.T) {
	klog.SetLogger(logr.Discard())
	p, dir := setupTestPolicy(t)
	klog.ClearLogger()
	defer removeAll(t, dir)

	allocNames := []string{"a0", "a1", "a2", "a3", "a4"}
	allocSizes := map[string]int64{
		"a0": 2 << 30,
		"a1": 4 << 30,
		"a2": 8 << 30,
		"a3": 16 << 30,
		"a4": 32 << 30,
	}

	var totalAllocBytes int64
	for _, size := range allocSizes {
		totalAllocBytes += size
	}

	allocIDs := map[string]string{} // abstract name -> real container ID
	var execute bool                // true only during step execution; guards doMalloc/doFree from BestPath exploration calls

	doMalloc := func(name string) (string, error) {
		if !execute {
			return "", nil
		}
		id, err := malloc(p, allocSizes[name])
		if err == nil {
			allocIDs[name] = id
		}
		return id, err
	}

	doFree := func(name string) error {
		if !execute {
			return nil
		}
		id, ok := allocIDs[name]
		if !ok {
			return nil
		}
		err := free(p, id)
		if err == nil {
			delete(allocIDs, name)
		}
		return err
	}

	mallocFn := func(name string, size int64) m.StateChange {
		return func(curr m.State) m.State {
			s := curr.(*LibmemState)
			if _, ok := s.allocs[name]; ok || s.freeBytes < size {
				return nil
			}
			newAllocs := make(map[string]int64, len(s.allocs)+1)
			for k, v := range s.allocs {
				newAllocs[k] = v
			}
			newAllocs[name] = size
			if fmbtV >= 2 {
				pc, _, _, _ := runtime.Caller(1)
				fmt.Printf("mallocFn(%dGB) called from %s\n", size>>30, runtime.FuncForPC(pc).Name())
			} else if fmbtV == 1 {
				fmt.Printf("mallocFn(%dGB)\n", size>>30)
			}
			return &LibmemState{freeBytes: s.freeBytes - size, allocs: newAllocs}
		}
	}

	freeFn := func(name string) m.StateChange {
		return func(curr m.State) m.State {
			s := curr.(*LibmemState)
			size, ok := s.allocs[name]
			if !ok {
				return nil
			}
			newAllocs := make(map[string]int64, len(s.allocs))
			for k, v := range s.allocs {
				if k != name {
					newAllocs[k] = v
				}
			}
			if fmbtV >= 2 {
				pc, _, _, _ := runtime.Caller(1)
				fmt.Printf("freeFn(%dGB) called from %s\n", size>>30, runtime.FuncForPC(pc).Name())
			} else if fmbtV == 1 {
				fmt.Printf("freeFn(%dGB)\n", size>>30)
			}
			return &LibmemState{freeBytes: s.freeBytes + size, allocs: newAllocs}
		}
	}

	model := m.NewModel()

	model.From(func(curr m.State) []*m.Transition {
		s := curr.(*LibmemState)
		var ts []*m.Transition
		for _, name := range allocNames {
			if _, ok := s.allocs[name]; !ok && s.freeBytes >= allocSizes[name] {
				ts = append(ts, m.OnAction("malloc %s", name).Register(doMalloc, name).Do(mallocFn(name, allocSizes[name]))...)
			}
		}
		return ts
	})

	model.From(func(curr m.State) []*m.Transition {
		s := curr.(*LibmemState)
		var ts []*m.Transition
		for _, name := range allocNames {
			if _, ok := s.allocs[name]; ok {
				ts = append(ts, m.OnAction("free %s", name).Register(doFree, name).Do(freeFn(name))...)
			}
		}
		return ts
	})

	coverer := m.NewCoverer()
	coverer.CoverActionCombinations(3)

	state := m.State(&LibmemState{
		freeBytes: totalAllocBytes,
		allocs:    map[string]int64{},
	})

	testStep := 0
	for testStep < maxLibmem2Steps {
		path, covStats := coverer.BestPath(model, state, libmem2Search)
		if len(path) == 0 {
			break
		}
		for i := 0; i <= covStats.MaxStep; i++ {
			testStep++
			step := path[i]
			fmt.Printf("step:%d coverage:%d state:%v\n", testStep, coverer.Coverage(), state)
			pc, _, _, _ := runtime.Caller(0)
			full := runtime.FuncForPC(pc).Name()
			short := full[strings.LastIndex(full, "/")+1:]
			short = short[strings.Index(short, ".")+1:]
			fmt.Printf("%s %s()\n", step.Action(), short)
			execute = true
			results := step.Action().Execute()
			execute = false
			if len(results) > 0 {
				if err, _ := results[len(results)-1].(error); err != nil {
					t.Errorf("step %d: %s failed: %v", testStep, step.Action(), err)
				}
			}
			state = step.EndState()
			coverer.MarkCovered(step)
			coverer.UpdateCoverage()
			if testStep >= maxLibmem2Steps {
				break
			}
		}
	}
}
