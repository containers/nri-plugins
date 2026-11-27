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

package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// Each invariant finds the violation of one rule on a node whose records
// and kernel otherwise agree.
func TestInvariantViolations(t *testing.T) {
	v := pmemEnv(t, "ext4")
	v.f.Plug(fake.MemdevSpec{Serial: serialA})
	v.mustPrepare(poolDriver, "n", poolReq("n", 0, serialA, state.ModeNUMA, state.KindPool))
	v.mustPrepare(poolDriver, "w", fsGrant("w", false, ""))
	k := v.f.Kernel()
	snap, err := k.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	opts := checkOptions{
		MountsDirs:  []string{filepath.Join(v.dir, poolDriver, "mounts")},
		LeaseFsRoot: filepath.Join(v.dir, localDriver, "lease-fs"),
		Fenced:      []int{2},
	}
	if out := check(v.records(), snap, opts); len(out) > 0 {
		t.Fatalf("violations of a consistent node: %v", out)
	}
	numa := v.grant("n", 0)
	for _, c := range []struct {
		inv    string
		change func(r *state.Records, s *kernel.Snapshot, o *checkOptions)
		want   string
	}{
		{"I1", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			s.Regions = append(s.Regions, kernel.RegionInfo{Name: "region9", State: kernel.RegionEnabled, Serials: []uint64{serialA}})
		}, "region9 on it that no record names"},
		{"I1", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			for i := range s.Regions {
				if s.Regions[i].Name == string(numa.Got.Region) {
					s.Regions[i].State = kernel.RegionDisabled
				}
			}
		}, "OWNED, the kernel has it DISABLED"},
		{"I2", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			s.DaxDevices = append(s.DaxDevices, kernel.DaxDeviceInfo{Name: "dax9.1", Region: string(numa.Got.Region), State: kernel.DaxUnbound, Size: mib})
		}, "dax9.1 (UNBOUND) that no record names"},
		{"I3", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			g := r.Grant(state.GrantRef{Claim: "w"})
			g.Got.BlockDevice.Dev.Minor = 99
		}, "is not in the kernel"},
		{"I5", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) { o.Fenced = []int{} }, "which is not fenced"},
		{"I6", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			r.Grant(state.GrantRef{Claim: "w"}).Asked.ReadOnly = true
		}, "superblock read-only false with writer false"},
		{"I7", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			for i := range s.Devices {
				s.Devices[i].ReadOnly = true
			}
		}, "read-only true with writer true"},
		{"I8", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			s.Mounts = append(s.Mounts, kernel.MountInfo{Target: filepath.Join(o.MountsDirs[0], "a1-direct"), Source: "/dev/pmem7"})
		}, "no realised grant of a prepared claim lists it"},
		{"I8", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			// a rollback that a stop cut: not a release that kubelet retries
			r.Grant(state.GrantRef{Claim: "w"}).State = state.GrantReleasing
			r.Preparation(poolDriver, "w").State = state.PreparationPreparing
		}, "no realised grant of a prepared claim lists it"},
		{"I10", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			r.Provenance = append(r.Provenance, state.Provenance{Serial: serialL, State: state.ProvenanceHeld})
			r.Grants = append(r.Grants, state.Grant{Ref: state.GrantRef{Claim: "l"}, Serial: serialL,
				Asked: state.Asked{Source: state.SourceLocal}, State: state.GrantJournaled})
		}, "a new local grant on serial 0x11 of provenance HELD"},
		{"I12", func(r *state.Records, s *kernel.Snapshot, o *checkOptions) {
			r.Leases = append(r.Leases, state.Lease{ID: "0123abcd", State: state.LeaseReady, Lend: &state.LeaseLend{NUMANode: state.NUMANodePtr(7)}})
		}, "lends NUMA node 7, which has no memory online"},
	} {
		r := v.records().Clone()
		s := *snap
		s.Regions = append([]kernel.RegionInfo(nil), snap.Regions...)
		s.DaxDevices = append([]kernel.DaxDeviceInfo(nil), snap.DaxDevices...)
		s.Devices = append([]kernel.BlockDevice(nil), snap.Devices...)
		s.Mounts = append([]kernel.MountInfo(nil), snap.Mounts...)
		o := opts
		c.change(&r, &s, &o)
		found := false
		for _, viol := range check(&r, &s, o) {
			found = found || (viol.Invariant == c.inv && strings.Contains(viol.Text, c.want))
		}
		if !found {
			t.Errorf("%s: no violation %q in %v", c.inv, c.want, check(&r, &s, o))
		}
	}
	// a lease that is released keeps its lend record while the memory of
	// its companion goes offline
	r := v.records().Clone()
	r.Leases = append(r.Leases, state.Lease{ID: "0123abcd", State: state.LeaseReleasing, Lend: &state.LeaseLend{NUMANode: state.NUMANodePtr(7)}})
	if out := check(&r, snap, opts); len(out) > 0 {
		t.Errorf("violations of a lease that is released: %v", out)
	}
	// the lease claim of a prepared claim is the lease broker's record: its
	// absence is a violation of the document, not one of the engine's
	r = v.records().Clone()
	r.Preparations = append(r.Preparations, state.Preparation{UID: "lc", Driver: localDriver, ClaimName: "demo/lc",
		State: state.PreparationPrepared, Results: []state.Result{{Index: 0, Kind: state.ResultLeaseClaim,
			LeaseClaim: &state.LeaseClaimResult{UID: "lc"}}}})
	if !slices.ContainsFunc(state.Check(&r), func(v state.Violation) bool { return v.Invariant == state.InvariantLeaseClaims }) {
		t.Errorf("no violation of the missing lease claim in %v", state.Check(&r))
	}
	if out := check(&r, snap, opts); len(out) > 0 {
		t.Errorf("the engine reports the missing lease claim: %v", out)
	}
	// the records of a source that the engine does not own are not checked
	r = v.records().Clone()
	r.Memdev(serialA).Regions = append(r.Memdev(serialA).Regions, "region9")
	s := *snap
	s.Regions = append(append([]kernel.RegionInfo(nil), snap.Regions...),
		kernel.RegionInfo{Name: "region9", State: kernel.RegionEnabled, Serials: []uint64{serialA}})
	if out := check(&r, &s, checkOptions{}); len(out) == 0 {
		t.Errorf("no violations of the records of the pool")
	}
	if out := check(&r, &s, checkOptions{Sources: []state.Source{state.SourceLocal}}); len(out) > 0 {
		t.Errorf("records of the pool checked: %v", out)
	}
	_ = os.Remove(filepath.Join(opts.MountsDirs[0], "a1-direct"))
	v.mustUnprepare(poolDriver, "w")
	v.mustUnprepare(poolDriver, "n")
}

// A part whose user is the grant of a Prepare that did not finish is a
// violation of I2, but for the grant of a numa companion and of a KEPT
// preparation, which the start keeps with its part for their Unprepare.
func TestI2UnfinishedUser(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.mustPrepare(localDriver, "s", sliceReq("s", 0, state.ModeNUMA, 256*mib))
	snap, err := v.f.Kernel().Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	part := v.grant("s", 0).Got.Dax
	for _, c := range []struct {
		name   string
		change func(g *state.Grant, p *state.Preparation)
		want   bool
	}{
		{"journaled", func(g *state.Grant, p *state.Preparation) {}, true},
		{"a numa companion", func(g *state.Grant, p *state.Preparation) {
			g.Intent.Lend = &state.GrantLend{Lease: "l1"}
		}, false},
		{"of a KEPT preparation", func(g *state.Grant, p *state.Preparation) { p.State = state.PreparationKept }, false},
	} {
		r := v.records().Clone()
		g, p := r.Grant(state.GrantRef{Claim: "s"}), r.Preparation(localDriver, "s")
		g.State, p.State = state.GrantJournaled, state.PreparationPreparing
		c.change(g, p)
		found := false
		for _, viol := range check(&r, snap, checkOptions{Fenced: []int{2}}) {
			found = found || (viol.Invariant == "I2" && strings.Contains(viol.Text, part+" has no REALISED or RELEASING user"))
		}
		if found != c.want {
			t.Errorf("%s: violation of I2 %v, want %v: %v", c.name, found, c.want, check(&r, snap, checkOptions{Fenced: []int{2}}))
		}
	}
}
