// Copyright 2019 Intel Corporation. All Rights Reserved.
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
	"testing"

	idset "github.com/intel/goresctrl/pkg/utils"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// adapterTestSys is a minimal sysfs.System implementation sufficient for
// cpuclass.New()/Configure(). Only CPUIDs is overridden; every other method
// is delegated to the embedded nil interface, which panics if called (and is
// never called by cpuclass.New/Configure in practice).
type adapterTestSys struct {
	sysfs.System
}

func (s *adapterTestSys) CPUIDs() []idset.ID { return nil }

// newActiveClassHandler builds a *cpuclass.Handler with an active managed
// PCT allocator (via the goresctrl SST in-memory mock), one HP class named
// "hp". Mirrors cpuclass.newConfiguredHandler (internal, unexported), kept
// in sync intentionally — this package cannot import that internal test
// helper.
func newActiveClassHandler(t *testing.T) *cpuclass.Handler {
	t.Helper()
	t.Setenv("OVERRIDE_SST", `{"supported":true,"clos_count":4,"packages":[{"id":0,"cpus":"0-7","tf_supported":true,"tf_enabled":true,"cp_supported":true,"cp_enabled":false,"punits":[{"id":0,"cpus":"0-7","max_hp_cpus":4,"guaranteed_hp_cpus":4}]}]}`)
	t.Setenv("OVERRIDE_SST_STATE_DIR", t.TempDir())
	h, err := cpuclass.New(&adapterTestSys{})
	if err != nil {
		t.Fatalf("cpuclass.New() failed: %v", err)
	}
	if err := h.Configure(cpuclass.ConfigSpec{
		Classes: []*cfgapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		Allowed: cpuset.MustParse("0-7"),
	}); err != nil {
		t.Fatalf("Configure() failed: %v", err)
	}
	return h
}

// newInactiveClassHandler builds a *cpuclass.Handler without SST support, so
// PCT stays disabled ("inactive"): all DRA-facing methods report their
// nil/zero-value defaults.
func newInactiveClassHandler(t *testing.T) *cpuclass.Handler {
	t.Helper()
	t.Setenv("OVERRIDE_SST", "")
	h, err := cpuclass.New(&adapterTestSys{})
	if err != nil {
		t.Fatalf("cpuclass.New() failed: %v", err)
	}
	_ = h.Configure(cpuclass.ConfigSpec{
		Classes: []*cfgapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		Allowed: cpuset.MustParse("0-7"),
	})
	return h
}

// TestPolicyDRAAdapterRoutesToCurrentHandler verifies the adapter forwards
// to whatever *cpuclass.Handler is installed in p.cpuClasses *at call time*,
// not to a handler captured once at construction. This matters because
// initialize() sets p.cpuClasses = nil and installs a brand new Handler on
// every Reconfigure — a cached pointer would go stale.
func TestPolicyDRAAdapterRoutesToCurrentHandler(t *testing.T) {
	p := &policy{}
	a := &policyDRAAdapter{p: p}

	// 1. cpuClasses nil ("no cpuClass config yet"): all methods must return
	// safe zero-values, no panic.
	p.cpuClasses = nil
	if a.IsHPClass("hp") {
		t.Error("IsHPClass with nil cpuClasses: got true, want false")
	}
	if _, err := a.PickHpCpus(0, 0, 1, cpuset.New()); err == nil {
		t.Error("PickHpCpus with nil cpuClasses: got nil error, want error")
	}
	if devs, err := a.DRADevices(DRADriverName); err != nil || len(devs) != 0 {
		t.Errorf("DRADevices with nil cpuClasses: got (%v, %v), want (empty, nil)", devs, err)
	}
	a.ReleaseHpCpus(0, 0, cpuset.New()) // must not panic
	if err := a.AccountHpCpus(0, 0, cpuset.New()); err == nil {
		t.Error("AccountHpCpus with nil cpuClasses: got nil error, want error")
	}

	// 2. Swap in an inactive handler (PCT unsupported): still all
	// zero-value defaults, but now backed by a real (non-nil) Handler.
	p.cpuClasses = newInactiveClassHandler(t)
	if a.IsHPClass("hp") {
		t.Error("IsHPClass with inactive handler: got true, want false")
	}
	if _, err := a.PickHpCpus(0, 0, 1, cpuset.New()); err == nil {
		t.Error("PickHpCpus with inactive handler: got nil error, want error")
	}

	// 3. Swap in an active handler with class "hp": the adapter must
	// immediately observe the new handler's behavior — proving it reads
	// p.cpuClasses fresh on every call rather than a cached pointer.
	p.cpuClasses = newActiveClassHandler(t)
	if !a.IsHPClass("hp") {
		t.Error("IsHPClass with active handler: got false, want true")
	}
	cpus, err := a.PickHpCpus(0, 0, 2, cpuset.New())
	if err != nil {
		t.Fatalf("PickHpCpus with active handler: %v", err)
	}
	if cpus.Size() != 2 {
		t.Errorf("PickHpCpus with active handler: got %d CPUs, want 2", cpus.Size())
	}
	devs, err := a.DRADevices(DRADriverName)
	if err != nil {
		t.Fatalf("DRADevices with active handler: %v", err)
	}
	if len(devs) == 0 {
		t.Error("DRADevices with active handler: got 0 devices, want > 0")
	}
	a.ReleaseHpCpus(0, 0, cpus)
	if err := a.AccountHpCpus(0, 0, cpus); err != nil {
		t.Errorf("AccountHpCpus with active handler: %v", err)
	}

	// 4. Swap back to nil: must return to safe zero-values, proving the
	// forwarding is not sticky/cached from step 3.
	p.cpuClasses = nil
	if a.IsHPClass("hp") {
		t.Error("IsHPClass after swap back to nil: got true, want false")
	}
}

func TestPolicyDRAAdapterNilCpuClassesNoPanic(t *testing.T) {
	p := &policy{cpuClasses: nil}
	a := &policyDRAAdapter{p: p}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("adapter method panicked with nil cpuClasses: %v", r)
		}
	}()

	_, _ = a.PickHpCpus(0, 0, 1, cpuset.New())
	a.ReleaseHpCpus(0, 0, cpuset.New())
	_ = a.AccountHpCpus(0, 0, cpuset.New())
	_ = a.IsHPClass("hp")
	_, _ = a.DRADevices(DRADriverName)
}
