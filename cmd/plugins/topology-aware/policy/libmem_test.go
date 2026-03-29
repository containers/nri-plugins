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
	"os"
	"path"
	"strings"
	"testing"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils"
)

// setupTestPolicy creates a policy from the server sysfs testdata.
func setupTestPolicy(t *testing.T) (*policy, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nri-libmem-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	if err := utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			t.Logf("failed to remove temp dir %q: %v", dir, rerr)
		}
		t.Fatalf("failed to uncompress testdata: %v", err)
	}

	sysPath := path.Join(dir, "sysfs", "server", "sys")
	sys, err := system.DiscoverSystemAt(sysPath)
	if err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			t.Logf("failed to remove temp dir %q: %v", dir, rerr)
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
		if rerr := os.RemoveAll(dir); rerr != nil {
			t.Logf("failed to remove temp dir %q: %v", dir, rerr)
		}
		t.Fatalf("failed to setup policy: %v", err)
	}
	return p, dir
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
