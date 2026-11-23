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

package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// scenario makes the transition of a row on a fake and returns the states
// observed before and after it.
type scenario[S comparable] func(t *testing.T) (from, to S)

// rowTo returns the state that row i of a table goes to.
func rowTo[S comparable](table fsm.Table[S], i int) S {
	if table.Rows[i].Stays {
		return table.Rows[i].From[0]
	}
	return table.Rows[i].To
}

// rowKey returns the key of the scenario of a row: from->to, and #N for the
// N-th row (from 0) with the same states. A row of a kernel table has one
// from state.
func rowKey[S comparable](table fsm.Table[S], i int) string {
	n := 0
	for j := range table.Rows[:i] {
		if table.Rows[j].From[0] == table.Rows[i].From[0] && rowTo(table, j) == rowTo(table, i) {
			n++
		}
	}
	return fmt.Sprintf("%v->%v#%d", table.Rows[i].From[0], rowTo(table, i), n)
}

// checkTable runs the scenario of every row of a table: each row has one,
// and the scenario observes the transition of its row.
func checkTable[S comparable](t *testing.T, name string, table fsm.Table[S], scenarios map[string]scenario[S]) {
	t.Helper()
	if !table.Observed || table.Field != "State" || len(table.States) == 0 {
		t.Errorf("%s: table %s.%s, observed %v, states %v", name, table.Object, table.Field, table.Observed, table.States)
	}
	used := map[string]bool{}
	for i, tr := range table.Rows {
		if len(tr.From) != 1 || len(tr.Triggers) != 0 || tr.When == "" {
			t.Errorf("%s: row %d: from %v, triggers %v, when %q", name, i, tr.From, tr.Triggers, tr.When)
			continue
		}
		key := rowKey(table, i)
		used[key] = true
		run := scenarios[key]
		if run == nil {
			t.Errorf("%s: the row %s (%s) has no table test", name, key, tr.When)
			continue
		}
		t.Run(key, func(t *testing.T) {
			from, to := run(t)
			if from != tr.From[0] || to != rowTo(table, i) {
				t.Fatalf("%s (%s): observed %v -> %v", key, tr.When, from, to)
			}
			noErr(t, table.Check(from, to, fsm.AnyTrigger))
		})
	}
	for key := range scenarios {
		if !used[key] {
			t.Errorf("%s: the scenario %s has no row", name, key)
		}
	}
}

func TestTable(t *testing.T) {
	table := fsm.Table[kernel.RegionState]{
		Object: "RegionInfo", Field: "State", Observed: true,
		Rows: []fsm.Transition[kernel.RegionState]{{From: []kernel.RegionState{kernel.RegionDisabled}, To: kernel.RegionEnabled}},
	}
	if !table.Allows(kernel.RegionDisabled, kernel.RegionEnabled, fsm.AnyTrigger) ||
		table.Allows(kernel.RegionEnabled, kernel.RegionDisabled, fsm.AnyTrigger) {
		t.Fatal("Allows")
	}
	noErr(t, table.Check(kernel.RegionHollow, kernel.RegionHollow, fsm.AnyTrigger))
	err := table.Check(kernel.RegionEnabled, kernel.RegionHollow, fsm.AnyTrigger)
	if !errors.Is(err, fsm.ErrTransition) || err.Error() != "RegionInfo.State: ENABLED -> HOLLOW: transition not in the table" {
		t.Fatalf("error %v", err)
	}
	for _, tt := range []fsm.TableText{
		kernel.MemdevTransitions.Text(), kernel.RegionTransitions.Text(), kernel.DaxTransitions.Text(),
		kernel.BlockTransitions.Text(), kernel.NdRegionTransitions.Text(), kernel.NamespaceTransitions.Text(),
	} {
		if !tt.Observed || len(tt.Rows) == 0 {
			t.Errorf("table %s.%s: observed %v, %d rows", tt.Object, tt.Field, tt.Observed, len(tt.Rows))
		}
	}
	// the names of the states, in JSON too (cxl-dump)
	if b, err := json.Marshal(kernel.DaxKmem); err != nil || string(b) != `"KMEM"` {
		t.Fatalf("%s %v", b, err)
	}
	if kernel.MemdevState(9).String() != "state(9)" {
		t.Fatal(kernel.MemdevState(9).String())
	}
}

func TestMemdevTransitions(t *testing.T) {
	state := func(f *fake.Fake) kernel.MemdevState { return f.Kernel().Memdev(serial0).State }
	plug := func(t *testing.T, delay time.Duration, disabled bool) *fake.Fake {
		f := fake.New(t)
		f.EndpointDelay = delay
		f.Plug(fake.MemdevSpec{Serial: serial0, Disabled: disabled})
		return f
	}
	lag := 200 * time.Millisecond
	ctx := context.Background()
	checkTable(t, "memdev", kernel.MemdevTransitions, map[string]scenario[kernel.MemdevState]{
		"ABSENT->UNBOUND#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := fake.New(t)
			from := state(f)
			f.Plug(fake.MemdevSpec{Serial: serial0, Disabled: true})
			return from, state(f)
		},
		"ABSENT->BOUND#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := fake.New(t)
			f.EndpointDelay = lag
			from := state(f)
			f.Plug(fake.MemdevSpec{Serial: serial0})
			return from, state(f)
		},
		"ABSENT->READY#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := fake.New(t)
			from := state(f)
			f.Plug(fake.MemdevSpec{Serial: serial0})
			return from, state(f)
		},
		"UNBOUND->BOUND#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := plug(t, lag, true)
			from := state(f)
			noErr(t, f.Kernel().EnableMemdev(ctx, "mem0"))
			return from, state(f)
		},
		"UNBOUND->READY#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := plug(t, 0, true)
			from := state(f)
			noErr(t, f.Kernel().EnableMemdev(ctx, "mem0"))
			return from, state(f)
		},
		"BOUND->READY#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := plug(t, 30*time.Millisecond, false)
			from := state(f)
			_, err := f.Kernel().WaitMemdev(ctxTimeout(t, 5*time.Second), serial0, kernel.MemdevReady)
			noErr(t, err)
			return from, state(f)
		},
		"BOUND->UNBOUND#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := plug(t, lag, false)
			from := state(f)
			noErr(t, f.Kernel().DisableMemdev(ctx, "mem0"))
			return from, state(f)
		},
		"READY->UNBOUND#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := plug(t, 0, false)
			from := state(f)
			noErr(t, f.Kernel().DisableMemdev(ctx, "mem0"))
			return from, state(f)
		},
		"UNBOUND->ABSENT#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := plug(t, 0, true)
			from := state(f)
			f.Unplug(serial0)
			return from, state(f)
		},
		"BOUND->ABSENT#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := plug(t, lag, false)
			from := state(f)
			f.Unplug(serial0)
			return from, state(f)
		},
		"READY->ABSENT#0": func(t *testing.T) (kernel.MemdevState, kernel.MemdevState) {
			f := plug(t, 0, false)
			r := createRegion(t, f.Kernel(), "mem0", kernel.RegionRAM, 0)
			from := state(f)
			f.Unplug(serial0)
			if !f.Kernel().HollowRegion(r) {
				t.Errorf("%s not hollow", r)
			}
			return from, state(f)
		},
	})
}

func TestRegionTransitions(t *testing.T) {
	state := func(k *kernel.Kernel, r string) kernel.RegionState { s, _ := k.Region(r); return s.State }
	ctx := context.Background()
	enabled := func(t *testing.T) (*fake.Fake, *kernel.Kernel, string) {
		f, k := newNode(t)
		return f, k, createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	}
	disabled := func(t *testing.T) (*fake.Fake, *kernel.Kernel, string) {
		f, k, r := enabled(t)
		noErr(t, k.DisableRegion(ctx, r))
		return f, k, r
	}
	checkTable(t, "region", kernel.RegionTransitions, map[string]scenario[kernel.RegionState]{
		"ABSENT->ENABLED#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			_, k := newNode(t)
			from := state(k, "region0")
			return from, state(k, createRegion(t, k, "mem0", kernel.RegionRAM, 0))
		},
		"ABSENT->HOLLOW#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			f, k := newNode(t)
			from := state(k, "region0")
			return from, state(k, f.HollowRegion("decoder0.0", 256*mib))
		},
		"ABSENT->DISABLED#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			f, k := newNode(t)
			from := state(k, "region0")
			r := f.HollowRegion("decoder0.0", 256*mib)
			f.Write("bus/cxl/devices/"+r+"/target0", "decoder4.0")
			return from, state(k, r)
		},
		"HOLLOW->ENABLED#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			f, k := newNode(t)
			r := f.HollowRegion("decoder0.0", 256*mib)
			from := state(k, r)
			f.Write("bus/cxl/devices/"+r+"/target0", "decoder4.0")
			f.Write("bus/cxl/devices/"+r+"/commit", "1")
			f.LinkTo("bus/cxl/devices/"+r+"/driver", "bus/cxl/drivers/cxl_region")
			return from, state(k, r)
		},
		"DISABLED->ENABLED#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			_, k, r := disabled(t)
			from := state(k, r)
			noErr(t, k.EnableRegion(ctx, r))
			return from, state(k, r)
		},
		"ENABLED->DISABLED#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			_, k, r := enabled(t)
			from := state(k, r)
			noErr(t, k.DisableRegion(ctx, r))
			return from, state(k, r)
		},
		"DISABLED->ABSENT#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			_, k, r := disabled(t)
			from := state(k, r)
			noErr(t, k.DestroyRegion(ctx, r))
			return from, state(k, r)
		},
		"ENABLED->HOLLOW#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			f, k, r := enabled(t)
			from := state(k, r)
			f.Unplug(serial0)
			return from, state(k, r)
		},
		"DISABLED->HOLLOW#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			f, k, r := disabled(t)
			from := state(k, r)
			f.Unplug(serial0)
			return from, state(k, r)
		},
		"HOLLOW->DISABLED#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			f, k := newNode(t)
			r := f.HollowRegion("decoder0.0", 256*mib)
			from := state(k, r)
			f.Write("bus/cxl/devices/"+r+"/target0", "decoder4.0")
			return from, state(k, r)
		},
		"ENABLED->ABSENT#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			_, k, r := enabled(t)
			from := state(k, r)
			noErr(t, k.DestroyRegion(ctx, r))
			return from, state(k, r)
		},
		"HOLLOW->ABSENT#0": func(t *testing.T) (kernel.RegionState, kernel.RegionState) {
			f, k, r := enabled(t)
			f.Unplug(serial0)
			from := state(k, r)
			noErr(t, k.DeleteHollowRegion(r))
			return from, state(k, r)
		},
	})
}

func TestDaxTransitions(t *testing.T) {
	state := func(k *kernel.Kernel, dax string) kernel.DaxState { d, _ := k.DaxDevice(dax); return d.State }
	ctx := context.Background()
	// seed returns a slice node with dax0.0 of half the region, unbound,
	// and a new dax0.1 of size 0
	seed := func(t *testing.T) (*fake.Fake, *kernel.Kernel) {
		f, k := daxNode(t)
		noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 512*mib))
		noErr(t, f.WriteAttr(f.P("bus/cxl/devices/region0/dax_region0/dax_region/create"), "1"))
		return f, k
	}
	unbound := func(t *testing.T) (*fake.Fake, *kernel.Kernel) {
		f, k := seed(t)
		noErr(t, k.ResizeDaxDevice(ctx, "dax0.1", bs))
		return f, k
	}
	bound := func(t *testing.T, driver string) (*fake.Fake, *kernel.Kernel) {
		f, k := unbound(t)
		noErr(t, k.BindDax(ctx, "dax0.1", driver))
		return f, k
	}
	// other returns an unbound node whose dax0.1 another driver has
	other := func(t *testing.T) (*fake.Fake, *kernel.Kernel) {
		f, k := unbound(t)
		f.Write("bus/dax/drivers/fsdev_dax/uevent", "")
		f.LinkTo("bus/dax/devices/dax0.1/driver", "bus/dax/drivers/fsdev_dax")
		return f, k
	}
	withDriver := func(t *testing.T, driver string) (*fake.Fake, *kernel.Kernel) {
		if driver == "fsdev_dax" {
			return other(t)
		}
		return bound(t, driver)
	}
	resize0 := func(driver string) scenario[kernel.DaxState] {
		return func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := withDriver(t, driver)
			from := state(k, "dax0.1")
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.1", 0))
			return from, state(k, "dax0.1")
		}
	}
	rebind := func(driver, to string) scenario[kernel.DaxState] {
		return func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := withDriver(t, driver)
			from := state(k, "dax0.1")
			noErr(t, k.BindDax(ctx, "dax0.1", to))
			return from, state(k, "dax0.1")
		}
	}
	disable := func(t *testing.T, k *kernel.Kernel) kernel.DaxState {
		noErr(t, k.DisableRegion(ctx, "region0"))
		return state(k, "dax0.1")
	}
	checkTable(t, "dax", kernel.DaxTransitions, map[string]scenario[kernel.DaxState]{
		"ABSENT->KMEM#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := newNode(t)
			from := state(k, "dax0.0")
			createRegion(t, k, "mem0", kernel.RegionRAM, 0)
			return from, state(k, "dax0.0")
		},
		"ABSENT->SEED#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			f, k := daxNode(t)
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 512*mib))
			from := state(k, "dax0.1")
			noErr(t, f.WriteAttr(f.P("bus/cxl/devices/region0/dax_region0/dax_region/create"), "1"))
			return from, state(k, "dax0.1")
		},
		// a part on the seed daxN.0: sized and bound by one Prepare
		"SEED->KMEM#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := daxNode(t)
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 0))
			from := state(k, "dax0.0")
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", bs))
			noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverKmem))
			return from, state(k, "dax0.0")
		},
		"SEED->DEVICE_DAX#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := daxNode(t)
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 0))
			from := state(k, "dax0.0")
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", bs))
			noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverDeviceDax))
			return from, state(k, "dax0.0")
		},
		"SEED->UNBOUND#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := seed(t)
			from := state(k, "dax0.1")
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.1", bs))
			return from, state(k, "dax0.1")
		},
		"UNBOUND->SEED#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := unbound(t)
			from := state(k, "dax0.1")
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.1", 0))
			return from, state(k, "dax0.1")
		},
		"UNBOUND->UNBOUND#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := unbound(t)
			from := state(k, "dax0.1")
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.1", 2*bs))
			return from, state(k, "dax0.1")
		},
		"UNBOUND->DEVICE_DAX#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := unbound(t)
			from := state(k, "dax0.1")
			noErr(t, k.BindDax(ctx, "dax0.1", kernel.DriverDeviceDax))
			return from, state(k, "dax0.1")
		},
		"UNBOUND->KMEM#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := unbound(t)
			from := state(k, "dax0.1")
			noErr(t, k.BindDax(ctx, "dax0.1", kernel.DriverKmem))
			return from, state(k, "dax0.1")
		},
		"UNBOUND->DEVICE_DAX#1": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			f, k := unbound(t)
			f.SetDaxID(kernel.DriverDeviceDax, "dax0.1")
			from := state(k, "dax0.1")
			// another tool binds dax0.0 with new_id
			noErr(t, f.WriteAttr(f.P("bus/dax/drivers/device_dax/new_id"), "dax0.0"))
			return from, state(k, "dax0.1")
		},
		"UNBOUND->OTHER#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			f, k := unbound(t)
			from := state(k, "dax0.1")
			f.Write("bus/dax/drivers/fsdev_dax/uevent", "")
			f.LinkTo("bus/dax/devices/dax0.1/driver", "bus/dax/drivers/fsdev_dax")
			return from, state(k, "dax0.1")
		},
		"DEVICE_DAX->UNBOUND#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := bound(t, kernel.DriverDeviceDax)
			from := state(k, "dax0.1")
			noErr(t, k.UnbindDax(ctx, "dax0.1"))
			return from, state(k, "dax0.1")
		},
		"KMEM->UNBOUND#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := bound(t, kernel.DriverKmem)
			from := state(k, "dax0.1")
			noErr(t, k.UnbindDax(ctx, "dax0.1"))
			return from, state(k, "dax0.1")
		},
		"OTHER->UNBOUND#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			f, k := unbound(t)
			f.Write("bus/dax/drivers/fsdev_dax/uevent", "")
			f.LinkTo("bus/dax/devices/dax0.1/driver", "bus/dax/drivers/fsdev_dax")
			from := state(k, "dax0.1")
			noErr(t, k.UnbindDax(ctx, "dax0.1"))
			return from, state(k, "dax0.1")
		},
		"SEED->ABSENT#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := seed(t)
			from := state(k, "dax0.1")
			noErr(t, k.DeleteDaxDevice(ctx, "region0", "dax0.1"))
			return from, state(k, "dax0.1")
		},
		"SEED->ABSENT#1": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := seed(t)
			return state(k, "dax0.1"), disable(t, k)
		},
		"UNBOUND->ABSENT#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := unbound(t)
			return state(k, "dax0.1"), disable(t, k)
		},
		"DEVICE_DAX->ABSENT#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := bound(t, kernel.DriverDeviceDax)
			return state(k, "dax0.1"), disable(t, k)
		},
		"KMEM->ABSENT#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := bound(t, kernel.DriverKmem)
			return state(k, "dax0.1"), disable(t, k)
		},
		"OTHER->ABSENT#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := other(t)
			from := state(k, "dax0.1")
			noErr(t, k.DeleteDaxDevice(ctx, "region0", "dax0.1"))
			return from, state(k, "dax0.1")
		},
		"ABSENT->UNBOUND#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			_, k := daxNode(t)
			noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 512*mib))
			from := state(k, "dax0.1")
			d, err := k.CreateDaxDevice(ctx, "region0", bs)
			noErr(t, err)
			return from, state(k, d)
		},
		"ABSENT->DEVICE_DAX#0": func(t *testing.T) (kernel.DaxState, kernel.DaxState) {
			f, k := newNode(t)
			f.SetDaxID(kernel.DriverDeviceDax, "dax0.0")
			from := state(k, "dax0.0")
			createRegion(t, k, "mem0", kernel.RegionRAM, 0)
			return from, state(k, "dax0.0")
		},
		"KMEM->SEED#0":        resize0(kernel.DriverKmem),
		"DEVICE_DAX->SEED#0":  resize0(kernel.DriverDeviceDax),
		"OTHER->SEED#0":       resize0("fsdev_dax"),
		"KMEM->DEVICE_DAX#0":  rebind(kernel.DriverKmem, kernel.DriverDeviceDax),
		"DEVICE_DAX->KMEM#0":  rebind(kernel.DriverDeviceDax, kernel.DriverKmem),
		"OTHER->DEVICE_DAX#0": rebind("fsdev_dax", kernel.DriverDeviceDax),
		"OTHER->KMEM#0":       rebind("fsdev_dax", kernel.DriverKmem),
	})
}

func TestBlockTransitions(t *testing.T) {
	ctx := context.Background()
	// block is the first block of dax0.0 of a slice node, unbound
	const block = 74
	state := func(k *kernel.Kernel) kernel.BlockState { return k.MemoryBlock(block).State }
	kmem := func(t *testing.T) (*fake.Fake, *kernel.Kernel) {
		f, k := daxNode(t)
		noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverKmem))
		return f, k
	}
	checkTable(t, "block", kernel.BlockTransitions, map[string]scenario[kernel.BlockState]{
		"ABSENT->OFFLINE#0": func(t *testing.T) (kernel.BlockState, kernel.BlockState) {
			_, k := daxNode(t)
			from := state(k)
			noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverKmem))
			return from, state(k)
		},
		"ABSENT->ONLINE#0": func(t *testing.T) (kernel.BlockState, kernel.BlockState) {
			f, k := daxNode(t)
			f.AutoOnline = 1
			from := state(k)
			noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverKmem))
			deadline := time.Now().Add(5 * time.Second)
			for state(k) != kernel.BlockOnline && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			return from, state(k)
		},
		"OFFLINE->ONLINE#0": func(t *testing.T) (kernel.BlockState, kernel.BlockState) {
			_, k := kmem(t)
			from := state(k)
			noErr(t, k.OnlineBlocks([]int{block}, true))
			return from, state(k)
		},
		"ONLINE->OFFLINE#0": func(t *testing.T) (kernel.BlockState, kernel.BlockState) {
			_, k := kmem(t)
			noErr(t, k.OnlineBlocks([]int{block}, true))
			from := state(k)
			noErr(t, k.OfflineBlocks([]int{block}))
			return from, state(k)
		},
		"ONLINE->ABSENT#0": func(t *testing.T) (kernel.BlockState, kernel.BlockState) {
			_, k := newNode(t)
			r := createRegion(t, k, "mem0", kernel.RegionRAM, 0)
			noErr(t, k.OnlineBlocks([]int{block}, true))
			from := state(k)
			noErr(t, k.DestroyRegion(ctx, r))
			return from, state(k)
		},
		"OFFLINE->ABSENT#0": func(t *testing.T) (kernel.BlockState, kernel.BlockState) {
			_, k := kmem(t)
			from := state(k)
			noErr(t, k.UnbindDax(ctx, "dax0.0"))
			return from, state(k)
		},
	})
}

func TestNdRegionTransitions(t *testing.T) {
	ctx := ctxTimeout(t, 10*time.Second)
	state := func(k *kernel.Kernel) kernel.NdRegionState { nd, _ := k.NdRegion("region0"); return nd.State }
	probing := func(t *testing.T) (*fake.Fake, *kernel.Kernel) {
		f, k := pmemNode(t)
		f.PmemDelay = 50 * time.Millisecond
		createRegion(t, k, "mem0", kernel.RegionPmem, 0)
		return f, k
	}
	checkTable(t, "nd region", kernel.NdRegionTransitions, map[string]scenario[kernel.NdRegionState]{
		"ABSENT->PROBING#0": func(t *testing.T) (kernel.NdRegionState, kernel.NdRegionState) {
			f, k := pmemNode(t)
			f.PmemDelay = 50 * time.Millisecond
			from := state(k)
			createRegion(t, k, "mem0", kernel.RegionPmem, 0)
			return from, state(k)
		},
		"PROBING->SETTLED#0": func(t *testing.T) (kernel.NdRegionState, kernel.NdRegionState) {
			_, k := probing(t)
			from := state(k)
			_, err := k.Namespaces(ctx, "region0")
			noErr(t, err)
			return from, state(k)
		},
		"ABSENT->SETTLED#0": func(t *testing.T) (kernel.NdRegionState, kernel.NdRegionState) {
			_, k := pmemNode(t)
			from := state(k)
			createRegion(t, k, "mem0", kernel.RegionPmem, 0)
			return from, state(k)
		},
		"SETTLED->PROBING#0": func(t *testing.T) (kernel.NdRegionState, kernel.NdRegionState) {
			f, k := pmemNode(t)
			createRegion(t, k, "mem0", kernel.RegionPmem, 0)
			from := state(k)
			var to kernel.NdRegionState
			// the pfn device of the new namespace without its mode yet
			f.After("ndctl create-namespace", func() {
				f.Remove(ndRegion0 + "/pfn0.0/mode")
				to = state(k)
				go func() {
					time.Sleep(20 * time.Millisecond)
					f.Write(ndRegion0+"/pfn0.0/mode", "ram")
				}()
			})
			_, err := k.CreateFsdaxNamespace(ctx, "region0", "")
			noErr(t, err)
			return from, to
		},
		"PROBING->ABSENT#0": func(t *testing.T) (kernel.NdRegionState, kernel.NdRegionState) {
			_, k := probing(t)
			from := state(k)
			noErr(t, k.DisableRegion(ctx, "region0"))
			return from, state(k)
		},
		"SETTLED->ABSENT#0": func(t *testing.T) (kernel.NdRegionState, kernel.NdRegionState) {
			_, k := pmemNode(t)
			createRegion(t, k, "mem0", kernel.RegionPmem, 0)
			from := state(k)
			noErr(t, k.DisableRegion(ctx, "region0"))
			return from, state(k)
		},
	})
}

func TestNamespaceTransitions(t *testing.T) {
	ctx := ctxTimeout(t, 10*time.Second)
	ns := ndRegion0 + "/namespace0.0"
	state := func(k *kernel.Kernel) kernel.NamespaceState {
		nd, _ := k.NdRegion("region0")
		if len(nd.Namespaces) == 0 {
			return kernel.NsAbsent
		}
		return nd.Namespaces[0].State
	}
	// seed returns a pmem node with region0 whose namespace0.0 is the seed
	seed := func(t *testing.T) (*fake.Fake, *kernel.Kernel) {
		f, k := pmemNode(t)
		createRegion(t, k, "mem0", kernel.RegionPmem, 0)
		return f, k
	}
	// the states of namespace0.0 that a probe finishes in
	toProbing := func(f *fake.Fake) { f.Write(ns+"/size", itoa(pmemNsBytes)); f.Remove(ns + "/holder") }
	toRaw := func(f *fake.Fake) {
		f.Write(ns+"/size", itoa(pmemNsBytes))
		f.Write(ns+"/holder", "")
		f.Write(ns+"/block/pmem0/dev", "259:7")
		f.Write(ns+"/block/pmem0/size", itoa(pmemNsBytes/512))
		f.LinkTo("class/block/pmem0", ns+"/block/pmem0")
	}
	toSector := func(f *fake.Fake) {
		btt := ndRegion0 + "/btt0.1"
		f.Write(ns+"/size", itoa(pmemNsBytes))
		f.Write(ns+"/holder", "btt0.1")
		f.LinkTo("bus/nd/devices/btt0.1", btt)
		f.LinkTo(btt+"/driver", "bus/nd/drivers/nd_pmem")
		f.Write(btt+"/block/pmem0s/dev", "259:8")
		f.Write(btt+"/block/pmem0s/size", itoa(pmemBlockBytes/512))
		f.LinkTo("class/block/pmem0s", btt+"/block/pmem0s")
	}
	toDevdax := func(f *fake.Fake) {
		f.Write(ns+"/size", itoa(pmemNsBytes))
		f.Write(ns+"/holder", "dax0.1")
		f.LinkTo("bus/nd/devices/dax0.1", ndRegion0+"/dax0.1")
		f.LinkTo(ndRegion0+"/dax0.1/driver", "bus/nd/drivers/nd_pmem")
	}
	toFsdax := func(f *fake.Fake) { f.Fsdax("region0", "pfn0.1") }
	probe := func(to func(*fake.Fake)) scenario[kernel.NamespaceState] {
		return func(t *testing.T) (kernel.NamespaceState, kernel.NamespaceState) {
			f, k := seed(t)
			toProbing(f)
			from := state(k)
			to(f)
			return from, state(k)
		}
	}
	// probed makes namespace0.0 of region0 go, and come back probed in
	// the state of to
	probed := func(to func(*fake.Fake)) scenario[kernel.NamespaceState] {
		return func(t *testing.T) (kernel.NamespaceState, kernel.NamespaceState) {
			f, k := seed(t)
			f.Remove(ns)
			f.Remove("bus/nd/devices/namespace0.0")
			from := state(k)
			to(f)
			f.LinkTo("bus/nd/devices/namespace0.0", ns)
			return from, state(k)
		}
	}
	gone := func(to func(*fake.Fake)) scenario[kernel.NamespaceState] {
		return func(t *testing.T) (kernel.NamespaceState, kernel.NamespaceState) {
			f, k := seed(t)
			to(f)
			from := state(k)
			noErr(t, k.DisableRegion(ctx, "region0"))
			return from, state(k)
		}
	}
	create := func(to func(*fake.Fake), existing string) scenario[kernel.NamespaceState] {
		return func(t *testing.T) (kernel.NamespaceState, kernel.NamespaceState) {
			f, k := seed(t)
			to(f)
			from := state(k)
			var after kernel.NamespaceState
			f.After("ndctl create-namespace", func() {
				f.Remove(ndRegion0 + "/pfn0.0/mode")
				after = state(k)
				go func() {
					time.Sleep(20 * time.Millisecond)
					f.Write(ndRegion0+"/pfn0.0/mode", "ram")
				}()
			})
			_, err := k.CreateFsdaxNamespace(ctx, "region0", existing)
			noErr(t, err)
			return from, after
		}
	}
	checkTable(t, "namespace", kernel.NamespaceTransitions, map[string]scenario[kernel.NamespaceState]{
		"ABSENT->PROBING#0": func(t *testing.T) (kernel.NamespaceState, kernel.NamespaceState) {
			f, k := seed(t)
			f.Remove(ns)
			f.Remove("bus/nd/devices/namespace0.0")
			f.Write(ndRegion0+"/init_namespaces", "0/0")
			from := state(k)
			f.Write(ns+"/size", itoa(pmemNsBytes))
			f.LinkTo("bus/nd/devices/namespace0.0", ns)
			return from, state(k)
		},
		"PROBING->SEED#0":   probe(func(f *fake.Fake) { f.Write(ns+"/size", "0") }),
		"PROBING->RAW#0":    probe(toRaw),
		"PROBING->FSDAX#0":  probe(toFsdax),
		"PROBING->SECTOR#0": probe(toSector),
		"PROBING->DEVDAX#0": probe(toDevdax),
		"SEED->PROBING#0":   create(func(*fake.Fake) {}, ""),
		"RAW->PROBING#0":    create(toRaw, "namespace0.0"),
		"SEED->FSDAX#0": func(t *testing.T) (kernel.NamespaceState, kernel.NamespaceState) {
			_, k := seed(t)
			from := state(k)
			_, err := k.CreateFsdaxNamespace(ctx, "region0", "")
			noErr(t, err)
			return from, state(k)
		},
		"RAW->FSDAX#0": func(t *testing.T) (kernel.NamespaceState, kernel.NamespaceState) {
			f, k := seed(t)
			toRaw(f)
			from := state(k)
			_, err := k.CreateFsdaxNamespace(ctx, "region0", "namespace0.0")
			noErr(t, err)
			return from, state(k)
		},
		"ABSENT->SEED#0": func(t *testing.T) (kernel.NamespaceState, kernel.NamespaceState) {
			_, k := pmemNode(t)
			from := state(k)
			createRegion(t, k, "mem0", kernel.RegionPmem, 0)
			return from, state(k)
		},
		"ABSENT->FSDAX#0": func(t *testing.T) (kernel.NamespaceState, kernel.NamespaceState) {
			f, k := pmemNode(t)
			f.SetLabels(serial0, true)
			from := state(k)
			createRegion(t, k, "mem0", kernel.RegionPmem, 0)
			return from, state(k)
		},
		"ABSENT->RAW#0":     probed(toRaw),
		"ABSENT->SECTOR#0":  probed(toSector),
		"ABSENT->DEVDAX#0":  probed(toDevdax),
		"PROBING->ABSENT#0": gone(toProbing),
		"SEED->ABSENT#0":    gone(func(*fake.Fake) {}),
		"RAW->ABSENT#0":     gone(toRaw),
		"FSDAX->ABSENT#0":   gone(toFsdax),
		"SECTOR->ABSENT#0":  gone(toSector),
		"DEVDAX->ABSENT#0":  gone(toDevdax),
	})
}
