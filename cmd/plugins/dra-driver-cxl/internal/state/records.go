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

package state

import (
	"cmp"
	"maps"
	"slices"

	"github.com/containers/nri-plugins/pkg/cxl/config"
)

// Records are the sections of the store's document: every record that the
// driver remembers, one section per record type.
type Records struct {
	Memdevs      []Memdev      `json:"memdevs,omitempty"`
	Regions      []Region      `json:"regions,omitempty"`
	Grants       []Grant       `json:"grants,omitempty"`
	Preparations []Preparation `json:"preparations,omitempty"`
	LeaseClaims  []LeaseClaim  `json:"leaseClaims,omitempty"`
	Leases       []Lease       `json:"leases,omitempty"`
	Provenance   []Provenance  `json:"provenance,omitempty"`
	Steered      []Steered     `json:"steered,omitempty"`
	ListedPools  []ListedPool  `json:"listedPools,omitempty"`
}

// Preparation returns the preparation of the claim uid by the driver, nil
// if there is none.
func (r *Records) Preparation(driver, uid string) *Preparation {
	for i := range r.Preparations {
		if p := &r.Preparations[i]; p.Driver == driver && p.UID == uid {
			return p
		}
	}
	return nil
}

// PreparationOf returns the preparation with a result of the grant ref,
// nil if there is none.
func (r *Records) PreparationOf(ref GrantRef) *Preparation {
	for i := range r.Preparations {
		p := &r.Preparations[i]
		if p.UID != ref.Claim {
			continue
		}
		if res := p.Result(ref.Result); res != nil && res.Grant != nil && *res.Grant == ref {
			return p
		}
	}
	return nil
}

// AllRealised tells whether every grant result of the preparation is
// REALISED.
func (r *Records) AllRealised(p *Preparation) bool {
	for _, res := range p.Results {
		if res.Grant == nil {
			continue
		}
		if g := r.Grant(*res.Grant); g == nil || g.State != GrantRealised {
			return false
		}
	}
	return true
}

// Grant returns the grant ref, nil if there is none.
func (r *Records) Grant(ref GrantRef) *Grant {
	for i := range r.Grants {
		if r.Grants[i].Ref == ref {
			return &r.Grants[i]
		}
	}
	return nil
}

// Memdev returns the memdev with the serial, nil if there is none.
func (r *Records) Memdev(serial Serial) *Memdev {
	for i := range r.Memdevs {
		if r.Memdevs[i].Serial == serial {
			return &r.Memdevs[i]
		}
	}
	return nil
}

// Region returns the region ref, nil if there is none.
func (r *Records) Region(ref RegionRef) *Region {
	for i := range r.Regions {
		if r.Regions[i].Ref() == ref {
			return &r.Regions[i]
		}
	}
	return nil
}

// DaxDevOf returns the device numbers of the dax device that the grant g
// got, those of its region's record; zero if it has none.
func (r *Records) DaxDevOf(g *Grant) DevNumbers {
	if reg := r.Region(g.Got.Region); reg != nil && g.Got.Dax != "" {
		if d := reg.DaxDevice(g.Got.Dax); d != nil {
			return d.Dev
		}
	}
	return DevNumbers{}
}

// MountOf returns the record of the node mount that the grant g uses, nil
// if none.
func (r *Records) MountOf(g *Grant) *Mount {
	if reg := r.Region(g.Got.Region); reg != nil && g.Got.Mount != "" {
		return reg.Mount(g.Got.Mount)
	}
	return nil
}

// Lease returns the lease with the id, nil if there is none.
func (r *Records) Lease(id string) *Lease {
	for i := range r.Leases {
		if r.Leases[i].ID == id {
			return &r.Leases[i]
		}
	}
	return nil
}

// LeaseClaim returns the lease claim with the uid, nil if there is none.
func (r *Records) LeaseClaim(uid string) *LeaseClaim {
	for i := range r.LeaseClaims {
		if r.LeaseClaims[i].UID == uid {
			return &r.LeaseClaims[i]
		}
	}
	return nil
}

// ProvenanceOf returns the provenance of the serial, nil if there is none.
func (r *Records) ProvenanceOf(serial Serial) *Provenance {
	for i := range r.Provenance {
		if r.Provenance[i].Serial == serial {
			return &r.Provenance[i]
		}
	}
	return nil
}

// Sort sorts every section by the identity of its records.
func (r *Records) Sort() {
	slices.SortStableFunc(r.Memdevs, func(a, b Memdev) int { return cmp.Compare(a.Serial, b.Serial) })
	slices.SortStableFunc(r.Regions, func(a, b Region) int { return cmp.Compare(a.Ref(), b.Ref()) })
	slices.SortStableFunc(r.Grants, func(a, b Grant) int { return a.Ref.Compare(b.Ref) })
	slices.SortStableFunc(r.Preparations, func(a, b Preparation) int {
		return cmp.Or(cmp.Compare(a.UID, b.UID), cmp.Compare(a.Driver, b.Driver))
	})
	slices.SortStableFunc(r.LeaseClaims, func(a, b LeaseClaim) int { return cmp.Compare(a.UID, b.UID) })
	slices.SortStableFunc(r.Leases, func(a, b Lease) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortStableFunc(r.Provenance, func(a, b Provenance) int { return cmp.Compare(a.Serial, b.Serial) })
	slices.SortStableFunc(r.Steered, func(a, b Steered) int { return cmp.Compare(a.Container, b.Container) })
	slices.SortStableFunc(r.ListedPools, func(a, b ListedPool) int { return cmp.Compare(a.Pool, b.Pool) })
}

// Clone returns a deep copy of the records that shares no memory with
// them.
func (r *Records) Clone() Records {
	return Records{
		Memdevs:      cloneEach(r.Memdevs, (*Memdev).Clone),
		Regions:      cloneEach(r.Regions, (*Region).Clone),
		Grants:       cloneEach(r.Grants, (*Grant).Clone),
		Preparations: cloneEach(r.Preparations, (*Preparation).Clone),
		LeaseClaims:  cloneEach(r.LeaseClaims, (*LeaseClaim).Clone),
		Leases:       cloneEach(r.Leases, (*Lease).Clone),
		Provenance:   slices.Clone(r.Provenance),
		Steered:      slices.Clone(r.Steered),
		ListedPools:  slices.Clone(r.ListedPools),
	}
}

// Clone returns a deep copy of the memdev.
func (m *Memdev) Clone() Memdev {
	c := *m
	c.Facts.NUMAAffinity = clonePtr(m.Facts.NUMAAffinity)
	c.Regions = slices.Clone(m.Regions)
	c.Hollow = slices.Clone(m.Hollow)
	c.StaleBinds = slices.Clone(m.StaleBinds)
	c.Grants = slices.Clone(m.Grants)
	return c
}

// Clone returns a deep copy of the region.
func (r *Region) Clone() Region {
	c := *r
	c.Targets = slices.Clone(r.Targets)
	c.NUMANode = clonePtr(r.NUMANode)
	c.DaxDevices = cloneEach(r.DaxDevices, func(d *DaxDevice) DaxDevice {
		dc := *d
		dc.Users = slices.Clone(d.Users)
		return dc
	})
	if r.Pmem != nil {
		p := *r.Pmem
		p.BlockDevice = clonePtr(r.Pmem.BlockDevice)
		c.Pmem = &p
	}
	c.Mounts = cloneEach(r.Mounts, func(m *Mount) Mount {
		mc := *m
		mc.Users = slices.Clone(m.Users)
		return mc
	})
	return c
}

// Clone returns a deep copy of the grant.
func (g *Grant) Clone() Grant {
	c := *g
	c.Intent.Noted = slices.Clone(g.Intent.Noted)
	c.Intent.Lend = cloneLend(g.Intent.Lend)
	c.Got.NUMANode = clonePtr(g.Got.NUMANode)
	c.Got.BlockDevice = clonePtr(g.Got.BlockDevice)
	c.Got.Bind = clonePtr(g.Got.Bind)
	c.Got.Lend = cloneLend(g.Got.Lend)
	return c
}

// Clone returns a deep copy of the preparation.
func (p *Preparation) Clone() Preparation {
	c := *p
	c.Results = cloneEach(p.Results, func(r *Result) Result {
		rc := *r
		rc.Settings = cloneSettings(r.Settings)
		rc.Grant = clonePtr(r.Grant)
		if r.Region != nil {
			reg := *r.Region
			reg.NUMAAffinity = slices.Clone(r.Region.NUMAAffinity)
			rc.Region = &reg
		}
		if r.DRAM != nil {
			d := *r.DRAM
			d.NUMANodes = slices.Clone(r.DRAM.NUMANodes)
			rc.DRAM = &d
		}
		rc.LeaseClaim = clonePtr(r.LeaseClaim)
		return rc
	})
	return c
}

// Clone returns a deep copy of the lease claim.
func (l *LeaseClaim) Clone() LeaseClaim {
	c := *l
	c.Pods = slices.Clone(l.Pods)
	return c
}

// Clone returns a deep copy of the lease.
func (l *Lease) Clone() Lease {
	c := *l
	c.Got = clonePtr(l.Got)
	if l.Lend != nil {
		lend := *l.Lend
		lend.NUMANode = clonePtr(l.Lend.NUMANode)
		c.Lend = &lend
	}
	return c
}

// cloneLend returns a deep copy of the lend, nil for nil.
func cloneLend(l *GrantLend) *GrantLend {
	if l == nil {
		return nil
	}
	c := *l
	c.NUMANode = clonePtr(l.NUMANode)
	return &c
}

// cloneSettings returns a deep copy of the container settings.
func cloneSettings(s config.ResultSettings) config.ResultSettings {
	c := config.ResultSettings{Origins: maps.Clone(s.Origins)}
	if s.Container != nil {
		ctr := *s.Container
		if s.Container.Steering != nil {
			st := *s.Container.Steering
			st.Waypoints = cloneEach(s.Container.Steering.Waypoints, func(w *config.Waypoint) config.Waypoint {
				return config.Waypoint{TargetUsages: slices.Clone(w.TargetUsages)}
			})
			ctr.Steering = &st
		}
		c.Container = &ctr
	}
	return c
}

// cloneEach returns the deep copies of the values made by clone, nil for
// nil.
func cloneEach[T any](values []T, clone func(*T) T) []T {
	if values == nil {
		return nil
	}
	out := make([]T, len(values))
	for i := range values {
		out[i] = clone(&values[i])
	}
	return out
}

// clonePtr returns a pointer to a copy of *p, nil for nil.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}
