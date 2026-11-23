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
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// CoreFront is a front-end of the node of the core model: it runs kubelet's
// requests of the claims of the slots, as the DRA drivers do, over the
// store and the engine of each start of the process.
type CoreFront interface {
	// NewEngine returns the engine of a start with the options o. The
	// front-end of the start is over this engine.
	NewEngine(o Options) (*Engine, error)
	// Recover recovers the records of a start and starts the front-end,
	// as the process does.
	Recover(ctx context.Context) error
	// Prepare and Unprepare run kubelet's request of the claim uid of a
	// slot and return its error.
	Prepare(sl CoreSlot, uid string) error
	Unprepare(sl CoreSlot, uid string) error
	// Check returns the violations of the invariants of the front-end now.
	Check() []string
}

// CoreSlot is a slot of claims of the core model: the memdev, how its
// claims use it, and whether they are companions of a lease.
type CoreSlot struct {
	ID       string
	Serial   uint64
	Source   state.Source
	Kind     state.GrantKind
	Mode     state.Mode
	ReadOnly bool
	Lease    bool
}

// CoreMemdev is a memdev of the node of the core model and the row of the
// release policy of its claims.
type CoreMemdev struct {
	Spec   fake.MemdevSpec
	Source state.Source
	Kind   state.GrantKind
}

// CoreMemdevs returns the memdevs of the node of the core model.
func CoreMemdevs() []CoreMemdev {
	out := make([]CoreMemdev, 0, len(coreMemdevs))
	for _, d := range coreMemdevs {
		out = append(out, CoreMemdev{Spec: d.spec, Source: d.source, Kind: d.kind})
	}
	return out
}

// coreSlotOf returns the slot i as a front-end sees it.
func coreSlotOf(i int) CoreSlot {
	sl := &coreSlots[i]
	d := &coreMemdevs[sl.md]
	return CoreSlot{ID: sl.id, Serial: d.serial, Source: d.source, Kind: d.kind, Mode: sl.mode, ReadOnly: sl.readOnly, Lease: sl.lease}
}

// RunCoreModel runs the core model with the flags of TestCoreModel, the
// requests through the front-end that newFront returns for the fake kernel
// and the directory of the node: the cuts count every commit of the store,
// the front-end's checks run at every step.
func RunCoreModel(t *testing.T, newFront func(f *fake.Fake, dir string) CoreFront) {
	coreFront = true
	t.Cleanup(func() { coreFront = false })
	r := newCoreRun(t, newFront)
	if *coreReplay != "" {
		r.replay(*coreReplay)
	} else {
		r.generate()
	}
	r.summary()
}
