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

package statetest

import (
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/config"
)

const (
	// Node is the Kubernetes node and the DRA pool of cxl.nri.io.
	Node = "node-1"
	// BootID is the boot of the records.
	BootID = "boot-1"
	// LocalDriver and PoolDriver are the drivers of the preparations.
	LocalDriver = "cxl.nri.io"
	PoolDriver  = "cxl-pool.nri.io"
	// LocalDir and PoolDir are the plugin data directories in the paths
	// of the records.
	LocalDir = "/var/lib/kubelet/plugins/cxl.nri.io"
	PoolDir  = "/var/lib/kubelet/plugins/cxl-pool.nri.io"

	// Serials of the memdevs.
	SerialLocal      state.Serial = 0xc100e2e1
	SerialShared     state.Serial = 0xc100e2e2
	SerialPersistent state.Serial = 0xc1ae2001
	SerialPoolFs     state.Serial = 0xc1ae2002
	SerialPoolNUMA   state.Serial = 0xc1ae2003
	SerialPoolDax    state.Serial = 0xc1ae2004
	SerialHeld       state.Serial = 0xc1ae2005
	SerialForeign    state.Serial = 0xc100e2e5
	SerialStale      state.Serial = 0xc100e2e9

	// Claim UIDs.
	ClaimNUMA       = "11111111-0000-0000-0000-000000000001"
	ClaimDax        = "11111111-0000-0000-0000-000000000002"
	ClaimShared     = "11111111-0000-0000-0000-000000000003"
	ClaimMixed      = "11111111-0000-0000-0000-000000000004"
	ClaimKept       = "11111111-0000-0000-0000-000000000005"
	ClaimFs         = "11111111-0000-0000-0000-000000000006"
	ClaimLease      = "11111111-0000-0000-0000-000000000007"
	ClaimStale      = "11111111-0000-0000-0000-000000000008"
	ClaimCompanion  = "22222222-0000-0000-0000-000000000001"
	ClaimPoolNUMA   = "33333333-0000-0000-0000-000000000001"
	ClaimPoolFs     = "33333333-0000-0000-0000-000000000002"
	ClaimPoolDax    = "33333333-0000-0000-0000-000000000003"
	ClaimPoolLeaseC = "44444444-0000-0000-0000-000000000001"

	// Lease ids.
	LeaseNUMA = "0a1b2c3d"
	LeaseFs   = "1b2c3d4e"
)

const (
	mi = uint64(1) << 20
	gi = uint64(1) << 30
)

// Records returns a node's records: claims of every kind of both drivers,
// a KEPT holder, a stale claim, a local numa lease and a pool fs lease that
// is held busy, consistent by state.Check.
func Records() state.Records {
	created := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	node := state.NUMANodePtr
	ref := func(claim string, n int) state.GrantRef { return state.GrantRef{Claim: claim, Result: n} }
	requester := state.PodRef{Namespace: "demo", Name: "requester", UID: "55555555-0000-0000-0000-000000000001"}
	steer := config.ResultSettings{
		Container: &config.Container{Steering: &config.Steering{Policy: "auto"}},
		Origins:   map[string]string{"container.steering.policy": "claim"},
	}
	fsDir := LocalDir + "/lease-fs/" + ClaimLease
	bindTarget := fsDir + "/mbt"
	poolFsMount := PoolDir + "/mounts/c1ae2002-cached"
	localFsMount := LocalDir + "/mounts/c1ae2001-direct"
	numaLend := &state.GrantLend{Lease: LeaseNUMA, Pod: requester, Container: "main", NUMANode: node(2)}
	fsLend := &state.GrantLend{Lease: LeaseFs, Pod: requester, Container: "main", FSDir: fsDir, Name: "mbt"}
	grantResult := func(claim string, n int, request, pool, device string) state.Result {
		r := ref(claim, n)
		return state.Result{Index: n, Kind: state.ResultGrant, Request: request, Pool: pool, Device: device, Grant: &r}
	}

	r := state.Records{
		Memdevs: []state.Memdev{
			{
				Serial: SerialLocal, Name: "mem0", Source: state.SourceLocal, State: state.MemdevActive,
				Facts:   state.MemdevFacts{NUMAAffinity: node(0)},
				Regions: []state.RegionRef{"region0"},
				Grants:  []state.GrantRef{ref(ClaimNUMA, 0), ref(ClaimDax, 0), ref(ClaimKept, 0), ref(ClaimCompanion, 0)},
			},
			{
				Serial: SerialShared, Name: "mem1", Source: state.SourceLocal, State: state.MemdevActive,
				Facts:   state.MemdevFacts{Shared: true, NUMAAffinity: node(0)},
				Regions: []state.RegionRef{"region1"},
				Grants:  []state.GrantRef{ref(ClaimShared, 0), ref(ClaimMixed, 1)},
			},
			{
				Serial: SerialPersistent, Name: "mem2", Source: state.SourceLocal, State: state.MemdevActive,
				Facts:   state.MemdevFacts{Persistent: true},
				Regions: []state.RegionRef{"region2"},
				Grants:  []state.GrantRef{ref(ClaimFs, 0)},
			},
			{
				Serial: SerialPoolFs, Name: "mem3", Source: state.SourcePool, State: state.MemdevActive,
				Facts:      state.MemdevFacts{Persistent: true, Shared: true},
				Regions:    []state.RegionRef{"region3"},
				StaleBinds: []string{bindTarget},
				Grants:     []state.GrantRef{ref(ClaimPoolFs, 0), ref(ClaimPoolLeaseC, 0)},
			},
			{
				Serial: SerialPoolNUMA, Name: "mem4", Source: state.SourcePool, State: state.MemdevActive,
				Facts:   state.MemdevFacts{},
				Regions: []state.RegionRef{"region4"},
				Grants:  []state.GrantRef{ref(ClaimPoolNUMA, 0)},
			},
			{
				Serial: SerialPoolDax, Name: "mem5", Source: state.SourcePool, State: state.MemdevActive,
				Facts:   state.MemdevFacts{Shared: true},
				Regions: []state.RegionRef{"region6"},
				Grants:  []state.GrantRef{ref(ClaimPoolDax, 0)},
			},
		},
		Regions: []state.Region{
			{
				Name: "region0", NotedName: "region0", Mode: state.RegionRAM, Targets: []state.Serial{SerialLocal},
				Owner: state.OwnerOurs, State: state.RegionOwned, SetUp: true, NUMANode: node(2), Size: gi,
				DaxDevices: []state.DaxDevice{
					{Name: "dax0.0", Dev: state.DevNumbers{Major: 252, Minor: 0}, State: state.DaxSeed, Whole: true},
					{
						Name: "dax0.1", Dev: state.DevNumbers{Major: 252, Minor: 1}, Size: 128 * mi, Driver: state.DriverKmem,
						State: state.DaxKmemOnline, Users: []state.GrantRef{ref(ClaimNUMA, 0)},
					},
					{
						Name: "dax0.2", Dev: state.DevNumbers{Major: 252, Minor: 2}, Size: 128 * mi, Driver: state.DriverDeviceDax,
						State: state.DaxDeviceDax, Users: []state.GrantRef{ref(ClaimDax, 0)},
					},
					{
						Name: "dax0.3", Dev: state.DevNumbers{Major: 252, Minor: 3}, Size: 128 * mi, Driver: state.DriverKmem,
						State: state.DaxKmemOnline, Users: []state.GrantRef{ref(ClaimCompanion, 0)},
					},
				},
			},
			{
				Name: "region1", NotedName: "region1", Mode: state.RegionRAM, Targets: []state.Serial{SerialShared},
				Owner: state.OwnerOurs, State: state.RegionOwned, SetUp: true, Zeroed: true, NUMANode: node(3), Size: 512 * mi,
				DaxDevices: []state.DaxDevice{{
					Name: "dax1.0", Dev: state.DevNumbers{Major: 252, Minor: 4}, Size: 512 * mi, Driver: state.DriverDeviceDax,
					State: state.DaxDeviceDax, Whole: true, Users: []state.GrantRef{ref(ClaimShared, 0), ref(ClaimMixed, 1)},
				}},
			},
			{
				Name: "region2", NotedName: "region2", Mode: state.RegionPmem, Targets: []state.Serial{SerialPersistent},
				Owner: state.OwnerOurs, State: state.RegionOwned, SetUp: true, Size: gi,
				Pmem: &state.Pmem{
					NdRegion: "region7", Namespace: "namespace7.0",
					BlockDevice: &state.BlockDevice{Name: "/dev/pmem7", Dev: state.DevNumbers{Major: 259, Minor: 0}},
				},
				Mounts: []state.Mount{{
					Access: state.AccessDirect, Path: localFsMount, State: state.MountedRO, FSType: "erofs",
					Users: []state.GrantRef{ref(ClaimFs, 0)},
				}},
			},
			{
				Name: "region3", NotedName: "region3", Mode: state.RegionPmem, Targets: []state.Serial{SerialPoolFs},
				Owner: state.OwnerOurs, State: state.RegionOwned, SetUp: true, Size: gi,
				Pmem: &state.Pmem{
					NdRegion: "region8", Namespace: "namespace8.0",
					BlockDevice: &state.BlockDevice{Name: "/dev/pmem8", Dev: state.DevNumbers{Major: 259, Minor: 1}},
				},
				Mounts: []state.Mount{{
					Access: state.AccessCached, Path: poolFsMount, State: state.MountedRO, Loop: "/dev/loop0", FSType: "erofs",
					Users: []state.GrantRef{ref(ClaimPoolFs, 0), ref(ClaimPoolLeaseC, 0)},
				}},
			},
			{
				Name: "region4", NotedName: "region4", Mode: state.RegionRAM, Targets: []state.Serial{SerialPoolNUMA},
				Owner: state.OwnerOurs, State: state.RegionOwned, SetUp: true, NUMANode: node(4), Size: gi,
				DaxDevices: []state.DaxDevice{{
					Name: "dax4.0", Dev: state.DevNumbers{Major: 252, Minor: 5}, Size: gi, Driver: state.DriverKmem,
					State: state.DaxKmemOnline, Whole: true, Users: []state.GrantRef{ref(ClaimPoolNUMA, 0)},
				}},
			},
			{
				Name: "region5", Mode: state.RegionRAM, Targets: []state.Serial{SerialForeign},
				Owner: state.OwnerForeign, State: state.RegionForeign, NUMANode: node(5), Size: 2 * gi,
			},
			{
				Name: "region6", NotedName: "region6", Mode: state.RegionRAM, Targets: []state.Serial{SerialPoolDax},
				Owner: state.OwnerOurs, State: state.RegionOwned, SetUp: true, NUMANode: node(6), Size: gi,
				DaxDevices: []state.DaxDevice{{
					Name: "dax6.0", Dev: state.DevNumbers{Major: 252, Minor: 6}, Size: gi, Driver: state.DriverDeviceDax,
					State: state.DaxDeviceDax, Whole: true, Users: []state.GrantRef{ref(ClaimPoolDax, 0)},
				}},
			},
		},
		Grants: []state.Grant{
			{
				Ref: ref(ClaimNUMA, 0), Serial: SerialLocal, State: state.GrantRealised,
				Asked:  state.Asked{Source: state.SourceLocal, Kind: state.KindLocal, Mode: state.ModeNUMA, Size: 128 * mi},
				Intent: state.Intent{Region: "region0"},
				Got:    state.Got{Region: "region0", Dax: "dax0.1", NUMANode: node(2), Size: 128 * mi},
			},
			{
				Ref: ref(ClaimDax, 0), Serial: SerialLocal, State: state.GrantRealised,
				Asked:  state.Asked{Source: state.SourceLocal, Kind: state.KindLocal, Mode: state.ModeDax, ReadOnly: true, Size: 128 * mi},
				Intent: state.Intent{Region: "region0"},
				Got:    state.Got{Region: "region0", Dax: "dax0.2", Size: 128 * mi},
			},
			{
				Ref: ref(ClaimKept, 0), Serial: SerialLocal, State: state.GrantJournaled,
				Asked:  state.Asked{Source: state.SourceLocal, Kind: state.KindLocal, Mode: state.ModeNUMA, Size: 128 * mi},
				Intent: state.Intent{Region: "region0"},
			},
			{
				Ref: ref(ClaimCompanion, 0), Serial: SerialLocal, State: state.GrantRealised,
				Asked:  state.Asked{Source: state.SourceLocal, Kind: state.KindLocal, Mode: state.ModeNUMA, Size: 128 * mi},
				Intent: state.Intent{Region: "region0", Lend: numaLend},
				Got:    state.Got{Region: "region0", Dax: "dax0.3", NUMANode: node(2), Size: 128 * mi, Lend: numaLend},
			},
			{
				Ref: ref(ClaimShared, 0), Serial: SerialShared, State: state.GrantRealised,
				Asked:  state.Asked{Source: state.SourceLocal, Kind: state.KindLocalShared, Mode: state.ModeDax, Shared: true},
				Intent: state.Intent{Region: "region1"},
				Got:    state.Got{Region: "region1", Dax: "dax1.0", Size: 512 * mi},
			},
			{
				Ref: ref(ClaimMixed, 1), Serial: SerialShared, State: state.GrantRealised,
				Asked:  state.Asked{Source: state.SourceLocal, Kind: state.KindLocalShared, Mode: state.ModeDax, ReadOnly: true, Shared: true},
				Intent: state.Intent{Region: "region1"},
				Got:    state.Got{Region: "region1", Dax: "dax1.0", Size: 512 * mi},
			},
			{
				Ref: ref(ClaimFs, 0), Serial: SerialPersistent, State: state.GrantRealised,
				Asked:  state.Asked{Source: state.SourceLocal, Kind: state.KindLocalPersistent, Mode: state.ModeFS, ReadOnly: true, Path: "/run/cxl-fs/C1AE2001"},
				Intent: state.Intent{Region: "region2", NdRegion: "region7", MountPath: localFsMount},
				Got: state.Got{
					Region: "region2", Size: gi, Namespace: "namespace7.0", Mount: state.AccessDirect,
					BlockDevice: &state.BlockDevice{Name: "/dev/pmem7", Dev: state.DevNumbers{Major: 259, Minor: 0}},
				},
			},
			{
				Ref: ref(ClaimStale, 0), Serial: SerialStale, State: state.GrantRealised,
				Asked: state.Asked{Source: state.SourceLocal, Kind: state.KindLocal, Mode: state.ModeNUMA, Size: 128 * mi},
			},
			{
				Ref: ref(ClaimPoolNUMA, 0), Serial: SerialPoolNUMA, State: state.GrantRealised,
				Asked:  state.Asked{Source: state.SourcePool, Kind: state.KindPool, Mode: state.ModeNUMA, Size: gi},
				Intent: state.Intent{Region: "region4"},
				Got:    state.Got{Region: "region4", Dax: "dax4.0", NUMANode: node(4), Size: gi},
			},
			{
				Ref: ref(ClaimPoolFs, 0), Serial: SerialPoolFs, State: state.GrantRealised,
				Asked: state.Asked{
					Source: state.SourcePool, Kind: state.KindPoolPersistent, Mode: state.ModeFS, ReadOnly: true, Access: state.AccessCached,
					Path: "/run/cxl-fs/C1AE2002", Size: gi, Shared: true,
				},
				Intent: state.Intent{Region: "region3", NdRegion: "region8", MountPath: poolFsMount},
				Got: state.Got{
					Region: "region3", Size: gi, Namespace: "namespace8.0", Mount: state.AccessCached,
					BlockDevice: &state.BlockDevice{Name: "/dev/pmem8", Dev: state.DevNumbers{Major: 259, Minor: 1}},
				},
			},
			{
				Ref: ref(ClaimPoolDax, 0), Serial: SerialPoolDax, State: state.GrantRealised,
				Asked:  state.Asked{Source: state.SourcePool, Kind: state.KindPoolShared, Mode: state.ModeDax, Size: gi, Shared: true},
				Intent: state.Intent{Region: "region6"},
				Got:    state.Got{Region: "region6", Dax: "dax6.0", Size: gi},
			},
			{
				Ref: ref(ClaimPoolLeaseC, 0), Serial: SerialPoolFs, State: state.GrantRealised,
				Asked: state.Asked{
					Source: state.SourcePool, Kind: state.KindPoolPersistent, Mode: state.ModeFS, ReadOnly: true, Access: state.AccessCached,
					Path: "/run/cxl-fs/C1AE2002", Size: gi, Shared: true,
				},
				Intent: state.Intent{Region: "region3", NdRegion: "region8", MountPath: poolFsMount, BindTarget: bindTarget, Lend: fsLend},
				Got: state.Got{
					Region: "region3", Size: gi, Namespace: "namespace8.0", Mount: state.AccessCached,
					BlockDevice: &state.BlockDevice{Name: "/dev/pmem8", Dev: state.DevNumbers{Major: 259, Minor: 1}},
					Bind:        &state.Bind{Target: bindTarget, State: state.BindStale},
					Lend:        fsLend,
				},
			},
		},
		Preparations: []state.Preparation{
			{
				UID: ClaimNUMA, Driver: LocalDriver, ClaimName: "demo/c-numa", State: state.PreparationPrepared,
				Results: []state.Result{withSettings(grantResult(ClaimNUMA, 0, "mem", Node, "memdev-c100e2e1"), steer)},
			},
			{
				UID: ClaimDax, Driver: LocalDriver, ClaimName: "demo/c-dax", State: state.PreparationPrepared,
				Results: []state.Result{grantResult(ClaimDax, 0, "mem", Node, "memdev-c100e2e1")},
			},
			{
				UID: ClaimShared, Driver: LocalDriver, ClaimName: "demo/c-shared", State: state.PreparationUnpreparing,
				Results: []state.Result{grantResult(ClaimShared, 0, "mem", Node, "memdev-c100e2e2")},
			},
			{
				UID: ClaimMixed, Driver: LocalDriver, ClaimName: "demo/c-mixed", State: state.PreparationPrepared,
				Results: []state.Result{
					{
						Index: 0, Kind: state.ResultRegion, Request: "region", Pool: Node, Device: "region5-numa5",
						Region: &state.RegionResult{Name: "region5", NUMANode: 5, Size: 2 * gi, Consumed: int64(gi), NUMAAffinity: []int{1}},
					},
					grantResult(ClaimMixed, 1, "shared", Node, "memdev-c100e2e2"),
					{
						Index: 2, Kind: state.ResultDRAM, Request: "dram", Pool: Node, Device: "dram",
						DRAM: &state.DRAMResult{NUMANodes: []int{0, 1}, Size: 64 * gi},
					},
				},
			},
			{
				UID: ClaimKept, Driver: LocalDriver, ClaimName: "demo/c-kept", State: state.PreparationKept,
				Results: []state.Result{grantResult(ClaimKept, 0, "mem", Node, "memdev-c100e2e1")},
			},
			{
				UID: ClaimFs, Driver: LocalDriver, ClaimName: "demo/c-fs", State: state.PreparationUnrestored,
				Results: []state.Result{grantResult(ClaimFs, 0, "pmem", Node, "memdev-c1ae2001")},
			},
			{
				UID: ClaimLease, Driver: LocalDriver, ClaimName: "demo/c-lease", State: state.PreparationPrepared,
				Results: []state.Result{{
					Index: 0, Kind: state.ResultLeaseClaim, Request: "lease", Pool: Node, Device: "lease",
					LeaseClaim: &state.LeaseClaimResult{UID: ClaimLease},
				}},
			},
			{
				UID: ClaimStale, Driver: LocalDriver, ClaimName: "demo/c-stale", State: state.PreparationStale,
				Results: []state.Result{grantResult(ClaimStale, 0, "mem", Node, "memdev-c100e2e9")},
			},
			{
				UID: ClaimCompanion, Driver: LocalDriver, ClaimName: "demo/cxl-lease-" + LeaseNUMA,
				State: state.PreparationPrepared, Lease: LeaseNUMA,
				Results: []state.Result{grantResult(ClaimCompanion, 0, "memory", Node, "memdev-c100e2e1")},
			},
			{
				UID: ClaimPoolNUMA, Driver: PoolDriver, ClaimName: "demo/c-pnuma", State: state.PreparationPrepared,
				Results: []state.Result{grantResult(ClaimPoolNUMA, 0, "mem", "pool-a", "pooled3")},
			},
			{
				UID: ClaimPoolFs, Driver: PoolDriver, ClaimName: "demo/c-pfs", State: state.PreparationPrepared,
				Results: []state.Result{withShare(grantResult(ClaimPoolFs, 0, "mem", "pool-c", "pooled2"), "aaaaaaaa-0000-0000-0000-000000000001")},
			},
			{
				UID: ClaimPoolDax, Driver: PoolDriver, ClaimName: "demo/c-pdax", State: state.PreparationPrepared,
				Results: []state.Result{withShare(grantResult(ClaimPoolDax, 0, "mem", "pool-b", "pooled4"), "aaaaaaaa-0000-0000-0000-000000000002")},
			},
			{
				UID: ClaimPoolLeaseC, Driver: PoolDriver, ClaimName: "demo/cxl-lease-" + LeaseFs,
				State: state.PreparationUnpreparing, Lease: LeaseFs,
				Results: []state.Result{withShare(grantResult(ClaimPoolLeaseC, 0, "memory", "pool-c", "pooled2"), "aaaaaaaa-0000-0000-0000-000000000003")},
			},
		},
		LeaseClaims: []state.LeaseClaim{{
			UID: ClaimLease, Namespace: "demo", Name: "c-lease", Pods: []state.PodRef{requester},
			State: state.LeaseClaimOpen,
		}},
		Leases: []state.Lease{
			{
				ID: LeaseNUMA, Claim: ClaimLease, RequestID: "req-1", Pod: requester, Container: "main",
				Asked: state.LeaseAsked{Source: state.SourceLocal, Mode: state.ModeNUMA, Size: "128Mi"},
				Got:   &state.LeaseGot{Source: state.SourceLocal, Serial: SerialLocal, Size: 128 * mi},
				State: state.LeaseReady, Lend: &state.LeaseLend{NUMANode: node(2)}, Created: created,
			},
			{
				ID: LeaseFs, Claim: ClaimLease, Pod: requester, Container: "main",
				Asked: state.LeaseAsked{Mode: state.ModeFS, Serial: SerialPoolFs, Name: "mbt", Access: state.AccessCached, Shared: true},
				Got: &state.LeaseGot{
					Source: state.SourcePool, Serial: SerialPoolFs, Shared: true, Size: gi,
					Access: state.AccessCached, FSType: "erofs", Path: "/run/cxl/fs/mbt",
				},
				State: state.LeaseReleasingBusy, ReleaseRequested: true, DeletionRequested: true,
				Lend: &state.LeaseLend{BindTarget: bindTarget}, Created: created.Add(time.Minute),
			},
		},
		Provenance: []state.Provenance{
			{Serial: SerialLocal, State: state.ProvenanceLocal, LastSeen: created},
			{Serial: SerialShared, State: state.ProvenanceLocal, LastSeen: created},
			{Serial: SerialPersistent, State: state.ProvenanceLocal, LastSeen: created},
			{Serial: SerialPoolFs, State: state.ProvenancePool, Pool: PoolDriver + "/pool-c", LastSeen: created},
			{Serial: SerialPoolNUMA, State: state.ProvenancePool, Pool: PoolDriver + "/pool-a", LastSeen: created},
			{Serial: SerialPoolDax, State: state.ProvenancePool, Pool: PoolDriver + "/pool-b", LastSeen: created},
			{Serial: SerialHeld, State: state.ProvenanceHeld, HeldSince: created, HeldReason: state.HeldNeverListed},
		},
		Steered: []state.Steered{{Container: "c0ffee", CgroupDir: "/sys/fs/cgroup/kubepods.slice/c0ffee"}},
	}
	r.Sort()
	return r
}

// withSettings returns the result with the container settings.
func withSettings(r state.Result, s config.ResultSettings) state.Result {
	r.Settings = s
	return r
}

// withShare returns the result with the share id.
func withShare(r state.Result, id string) state.Result {
	r.ShareID = id
	return r
}
