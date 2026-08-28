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

// Tests for the DRA ClaimAllocator pass-through methods on Handler.

package cpuclass_test

import (
	"testing"

	idset "github.com/intel/goresctrl/pkg/utils"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// Compile-time assertion: *Handler must satisfy dra.ClaimAllocator.
var _ dra.ClaimAllocator = (*cpuclass.Handler)(nil)

// draTestSys is a minimal sysfs.System implementation for DRA pass-through
// tests. Only CPUIDs is overridden; all other methods are delegated to the
// embedded nil interface, which panics if called. In practice, only CPUIDs
// is invoked during Handler.New() (by cpufreq platform discovery).
type draTestSys struct {
	sysfs.System
}

func (s *draTestSys) CPUIDs() []idset.ID { return nil }

// newConfiguredHandler creates a Handler with an active managed PCT
// allocator using the SST in-memory mock (OVERRIDE_SST). The mock is
// seeded with one package (ID 0), one punit (ID 0), CPUs 0-7, and
// GuaranteedHpCpus=4. t.Setenv restores the env after the test.
func newConfiguredHandler(t *testing.T) *cpuclass.Handler {
	t.Helper()
	t.Setenv("OVERRIDE_SST", `{"supported":true,"clos_count":4,"packages":[{"id":0,"cpus":"0-7","tf_supported":true,"tf_enabled":true,"cp_supported":true,"cp_enabled":false,"punits":[{"id":0,"cpus":"0-7","max_hp_cpus":4,"guaranteed_hp_cpus":4}]}]}`)
	t.Setenv("OVERRIDE_SST_STATE_DIR", t.TempDir())
	h, err := cpuclass.New(&draTestSys{})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if err := h.Configure(cpuclass.ConfigSpec{
		Classes: []*policyapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		Allowed: cpuset.MustParse("0-7"),
	}); err != nil {
		t.Fatalf("Configure() failed: %v", err)
	}
	return h
}

// newInactiveHandler creates a Handler without OVERRIDE_SST so that
// SST is reported as unsupported and PCT stays in disabled mode.
// Configure is called with an HP class to exercise the "SST not
// supported → PCT disabled" path.
func newInactiveHandler(t *testing.T) *cpuclass.Handler {
	t.Helper()
	// Ensure OVERRIDE_SST is unset (t.Setenv restores original value).
	t.Setenv("OVERRIDE_SST", "")
	h, err := cpuclass.New(&draTestSys{})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	// Configure with a PCT class; SST unsupported → pct stays disabled.
	_ = h.Configure(cpuclass.ConfigSpec{
		Classes: []*policyapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		Allowed: cpuset.MustParse("0-7"),
	})
	return h
}

// ---------------------------------------------------------------------------
// PickHpCpus
// ---------------------------------------------------------------------------

func TestHandlerPickHpCpus_NilHandler(t *testing.T) {
	var h *cpuclass.Handler
	_, err := h.PickHpCpus(0, 0, 1, cpuset.New())
	if err == nil {
		t.Fatal("expected error from nil handler, got nil")
	}
}

func TestHandlerPickHpCpus_NilPct(t *testing.T) {
	h := &cpuclass.Handler{} // pct is nil
	_, err := h.PickHpCpus(0, 0, 1, cpuset.New())
	if err == nil {
		t.Fatal("expected error from nil pct, got nil")
	}
}

func TestHandlerPickHpCpus_InactivePct(t *testing.T) {
	h := newInactiveHandler(t)
	_, err := h.PickHpCpus(0, 0, 1, cpuset.New())
	if err == nil {
		t.Fatal("expected error from inactive PCT, got nil")
	}
}

func TestHandlerPickHpCpus_ActiveDelegates(t *testing.T) {
	h := newConfiguredHandler(t)
	got, err := h.PickHpCpus(0, 0, 2, cpuset.New())
	if err != nil {
		t.Fatalf("PickHpCpus(0,0,2) = %v, want nil error", err)
	}
	if got.Size() != 2 {
		t.Errorf("PickHpCpus(0,0,2) returned %d CPUs, want 2", got.Size())
	}
	// Requesting more than GuaranteedHpCpus (4) must error.
	if _, err := h.PickHpCpus(0, 0, 5, cpuset.New()); err == nil {
		t.Error("PickHpCpus(0,0,5) with capacity=4: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// ReleaseHpCpus
// ---------------------------------------------------------------------------

func TestHandlerReleaseHpCpus_NilHandler(t *testing.T) {
	var h *cpuclass.Handler
	// must not panic
	h.ReleaseHpCpus(0, 0, cpuset.New())
}

func TestHandlerReleaseHpCpus_NilPct(t *testing.T) {
	h := &cpuclass.Handler{}
	// must not panic
	h.ReleaseHpCpus(0, 0, cpuset.New())
}

func TestHandlerReleaseHpCpus_ActiveDelegates(t *testing.T) {
	h := newConfiguredHandler(t)
	// Pick 2 CPUs, then release them.
	cpus, err := h.PickHpCpus(0, 0, 2, cpuset.New())
	if err != nil {
		t.Fatalf("PickHpCpus setup: %v", err)
	}
	h.ReleaseHpCpus(0, 0, cpus)
	// After release, the full 4-CPU capacity must be available again.
	got, err := h.PickHpCpus(0, 0, 4, cpuset.New())
	if err != nil {
		t.Fatalf("PickHpCpus after release: %v", err)
	}
	if got.Size() != 4 {
		t.Errorf("post-release PickHpCpus returned %d CPUs, want 4", got.Size())
	}
}

// ---------------------------------------------------------------------------
// AccountHpCpus
// ---------------------------------------------------------------------------

func TestHandlerAccountHpCpus_NilHandler(t *testing.T) {
	var h *cpuclass.Handler
	if err := h.AccountHpCpus(0, 0, cpuset.New()); err == nil {
		t.Fatal("expected error from nil handler, got nil")
	}
}

func TestHandlerAccountHpCpus_NilPct(t *testing.T) {
	h := &cpuclass.Handler{}
	if err := h.AccountHpCpus(0, 0, cpuset.New()); err == nil {
		t.Fatal("expected error from nil pct, got nil")
	}
}

func TestHandlerAccountHpCpus_InactivePct(t *testing.T) {
	h := newInactiveHandler(t)
	if err := h.AccountHpCpus(0, 0, cpuset.MustParse("0")); err == nil {
		t.Fatal("expected error from inactive PCT, got nil")
	}
}

func TestHandlerAccountHpCpus_ActiveDelegates(t *testing.T) {
	h := newConfiguredHandler(t)
	// AccountHpCpus simulates restart reconciliation (union semantics,
	// no allocation): pick 2, release, then re-account.
	cpus, err := h.PickHpCpus(0, 0, 2, cpuset.New())
	if err != nil {
		t.Fatalf("PickHpCpus setup: %v", err)
	}
	h.ReleaseHpCpus(0, 0, cpus)
	if err := h.AccountHpCpus(0, 0, cpus); err != nil {
		t.Fatalf("AccountHpCpus(%s) = %v, want nil", cpus, err)
	}
	// Idempotency: calling again with the same CPUs must not error.
	if err := h.AccountHpCpus(0, 0, cpus); err != nil {
		t.Fatalf("AccountHpCpus idempotent call = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// IsHPClass
// ---------------------------------------------------------------------------

func TestHandlerIsHPClass_NilHandler(t *testing.T) {
	var h *cpuclass.Handler
	if h.IsHPClass("hp") {
		t.Error("IsHPClass on nil handler: got true, want false")
	}
}

func TestHandlerIsHPClass_NilPct(t *testing.T) {
	h := &cpuclass.Handler{}
	if h.IsHPClass("hp") {
		t.Error("IsHPClass with nil pct: got true, want false")
	}
}

func TestHandlerIsHPClass_InactivePct(t *testing.T) {
	h := newInactiveHandler(t)
	// PCT disabled → all classes report non-HP.
	if h.IsHPClass("hp") {
		t.Error("IsHPClass with inactive PCT: got true, want false")
	}
}

func TestHandlerIsHPClass_ActiveDelegates(t *testing.T) {
	h := newConfiguredHandler(t)
	// "hp" class has pctPriority=high → must report HP.
	if !h.IsHPClass("hp") {
		t.Error("IsHPClass(\"hp\") = false, want true")
	}
	// Unknown / non-HP names must return false.
	if h.IsHPClass("lp") {
		t.Error("IsHPClass(\"lp\") = true, want false")
	}
	if h.IsHPClass("") {
		t.Error("IsHPClass(\"\") = true, want false")
	}
}
