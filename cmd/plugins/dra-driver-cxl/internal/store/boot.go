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

package store

import (
	"slices"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// applyBoot changes the records of doc as a load in the boot running
// requires, and returns whether it changed the document. Nothing of a
// record of another boot exists in the kernel any more: its preparations
// become PREPARED_STALE (kubelet prepares them again or unprepares them);
// the grants of a stale preparation keep Serial, Source and Asked, and of
// Got and Intent only the lease and requester of a lend and the target of a
// lease bind; the memdev and region records, the grants that no preparation names and
// the leases are dropped (the first reconcile deletes the leases'
// companions); lease claims, provenance and steering records are kept.
func applyBoot(doc *Document, running string) (bool, error) {
	if doc.BootID == running {
		return false, nil
	}
	if err := stalePreparations(doc, running); err != nil {
		return false, err
	}
	if err := dropOtherBoot(doc, running); err != nil {
		return false, err
	}
	doc.BootID = running
	return true, nil
}

// stalePreparations makes the preparations of a document of another boot
// than running PREPARED_STALE; a stale one stays as it is.
func stalePreparations(doc *Document, running string) error {
	for i := range doc.Preparations {
		p := &doc.Preparations[i]
		if p.State == state.PreparationStale {
			continue
		}
		klog.Warningf("store: claim %s (%s) of %s was prepared in boot %s, this is %s: not prepared any more",
			p.ClaimName, p.UID, p.Driver, doc.BootID, running)
		if err := fsm.Apply(&state.PreparationTable, &p.State, state.PreparationStale, state.TriggerLoad); err != nil {
			return err
		}
		if err := clearGrants(doc, p); err != nil {
			return err
		}
	}
	return nil
}

// clearGrants clears Got and Intent of the grants of a stale preparation
// but the lease and requester of a lend and the target of a lease bind.
func clearGrants(doc *Document, p *state.Preparation) error {
	for _, res := range p.Results {
		if res.Grant == nil {
			continue
		}
		g := doc.Grant(*res.Grant)
		if g == nil {
			continue
		}
		if err := state.GrantTable.Check(g.State, g.State, state.TriggerLoad); err != nil {
			return err
		}
		kept := state.Intent{BindTarget: g.Intent.BindTarget, Lend: g.Intent.Lend}
		if kept.Lend == nil {
			kept.Lend = g.Got.Lend
		}
		if g.Got.Bind != nil && kept.BindTarget == "" {
			kept.BindTarget = g.Got.Bind.Target
		}
		if kept.Lend != nil {
			l := *kept.Lend
			l.NUMANode = nil
			kept.Lend = &l
		}
		g.Got, g.Intent = state.Got{}, kept
	}
	return nil
}

// checkDrops returns the error of the first lease, memdev or region record
// whose table refuses its drop on load.
func checkDrops(doc *Document) error {
	for i := range doc.Leases {
		if err := state.LeaseTable.Check(doc.Leases[i].State, state.LeaseNone, state.TriggerLoad); err != nil {
			return err
		}
	}
	for i := range doc.Memdevs {
		if err := state.MemdevTable.Check(doc.Memdevs[i].State, state.MemdevNone, state.TriggerLoad); err != nil {
			return err
		}
	}
	for i := range doc.Regions {
		if err := state.RegionTable.Check(doc.Regions[i].State, state.RegionNone, state.TriggerLoad); err != nil {
			return err
		}
	}
	return nil
}

// namedGrants returns the grants that a result of a preparation names.
func namedGrants(doc *Document) map[state.GrantRef]bool {
	named := map[state.GrantRef]bool{}
	for i := range doc.Preparations {
		for _, r := range doc.Preparations[i].Results {
			if r.Grant != nil {
				named[*r.Grant] = true
			}
		}
	}
	return named
}

// dropOtherBoot drops the leases, the memdev and region records and the
// grants of no claim of a document of another boot than running.
func dropOtherBoot(doc *Document, running string) error {
	if err := checkDrops(doc); err != nil {
		return err
	}
	named := namedGrants(doc)
	grants := slices.DeleteFunc(slices.Clone(doc.Grants), func(g state.Grant) bool { return !named[g.Ref] })
	for i := range doc.Grants {
		if g := &doc.Grants[i]; !named[g.Ref] {
			if err := state.GrantTable.Check(g.State, state.GrantNone, state.TriggerLoad); err != nil {
				return err
			}
		}
	}
	unnamed := len(doc.Grants) - len(grants)
	if len(doc.Leases) == 0 && len(doc.Memdevs) == 0 && len(doc.Regions) == 0 && unnamed == 0 {
		return nil
	}
	klog.Warningf("store: the document is of boot %s, this is %s: dropping %d leases, %d memdevs, %d regions and %d grants of no claim",
		doc.BootID, running, len(doc.Leases), len(doc.Memdevs), len(doc.Regions), unnamed)
	doc.Leases, doc.Memdevs, doc.Regions, doc.Grants = nil, nil, nil, grants
	return nil
}
