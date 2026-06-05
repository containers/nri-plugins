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

package pct

import (
	"fmt"
	"sort"

	gosst "github.com/intel/goresctrl/pkg/sst"
	"github.com/intel/goresctrl/pkg/utils"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// sstGoresctrl is the real-hardware sst backed by
// goresctrl/pkg/sst. Per-(pkg, punit) topology and HP capacity
// are snapshotted at Init() time -- the goresctrl Platform itself
// snapshots CPU topology at Init(), so refreshing here would not
// pick up CPU hotplug either.
type sstGoresctrl struct {
	plat *gosst.Platform
	// punits is the cached per-punit topology + HP capacity in
	// stable order (sorted by PkgID, then PunitID).
	punits []pctPunit
}

func newSstGoresctrl() (sst, error) {
	b := &sstGoresctrl{}
	if !gosst.SstSupported() {
		return b, nil
	}
	plat, err := gosst.Init()
	if err != nil {
		return nil, fmt.Errorf("SST init failed: %w", err)
	}
	b.plat = plat
	b.punits = discoverPunits(plat)
	return b, nil
}

// discoverPunits snapshots per-punit topology and HP capacity for
// every package the platform exposes. The PP level is the current
// level of the first punit of each package, mirroring the
// approach of goresctrl's "sst info" CLI. Logged at INFO so
// operators can correlate placement decisions with the platform
// state observed at startup. A failure on one package does not
// abort discovery for the others.
func discoverPunits(plat *gosst.Platform) []pctPunit {
	out := []pctPunit{}
	if plat == nil {
		return out
	}
	for _, pkg := range plat.Packages() {
		pkgID := pkg.ID()
		st, err := pkg.GetStatus()
		if err != nil {
			log.Warnf("pct: SST status unavailable for package %d: %v", pkgID, err)
			continue
		}
		// Pick the current PP level from any punit (they share
		// a level on every platform we have seen); warn on
		// divergence and stick with the first one.
		level := -1
		for _, pu := range st.Punits {
			if level < 0 {
				level = pu.PP.CurrentLevel
				continue
			}
			if pu.PP.CurrentLevel != level {
				log.Warnf("pct: package %d punits report differing PP levels; using level %d", pkgID, level)
				break
			}
		}
		if level < 0 {
			log.Warnf("pct: package %d has no punits, skipping discovery", pkgID)
			continue
		}
		info, err := pkg.GetPerfLevelInfo(level)
		if err != nil {
			log.Warnf("pct: SST PerfLevelInfo unavailable for package %d level %d: %v", pkgID, level, err)
			continue
		}
		// Stable per-punit iteration.
		punitIDs := make([]int, 0, len(st.Punits))
		for id := range st.Punits {
			punitIDs = append(punitIDs, int(id))
		}
		sort.Ints(punitIDs)
		for _, pid := range punitIDs {
			pu := st.Punits[utils.ID(pid)]
			cpus := cpuset.New(pu.CPUs.Members()...)
			max := 0
			gtd := 0
			if pi, ok := info[utils.ID(pid)]; ok {
				max = punitMaxHpCpus(pi)
				gtd = punitGuaranteedHpCpus(pi)
				log.Infof("pct: SST discovered: pkg=%d punit=%d level=%d cpus=%s maxHpCpus=%d guaranteedHpCpus=%d (tf=%v bf=%v)",
					pkgID, pid, level, cpus, max, gtd, pi.TF.Supported, pi.BF.Supported)
			} else {
				log.Infof("pct: SST discovered: pkg=%d punit=%d level=%d cpus=%s maxHpCpus=0 (no PerfLevelInfo)",
					pkgID, pid, level, cpus)
			}
			out = append(out, pctPunit{
				PkgID:            pkgID,
				PunitID:          pid,
				CPUs:             cpus,
				MaxHpCpus:        max,
				GuaranteedHpCpus: gtd,
			})
		}
	}
	return out
}

// punitMaxHpCpus returns the maximum number of CPUs that can be
// promoted to high priority on this punit at the queried PP
// level. SST-TF takes precedence: the largest bucket's
// HighPriorityCoreCount sets the upper bound (smaller buckets
// allow higher turbo but admit fewer HP cores -- the allocator
// only needs to know the cap). When TF is unsupported or all
// buckets are empty, fall back to len(BF.HighPriorityCPUs); BF
// guarantees those CPUs run at an elevated *base* frequency, so
// the count is exact. Returns 0 only when neither feature
// exposes any HP CPUs.
func punitMaxHpCpus(pi *gosst.PerfLevelInfo) int {
	if pi == nil {
		return 0
	}
	max := 0
	if pi.TF.Supported {
		for _, b := range pi.TF.Buckets {
			if b.HighPriorityCoreCount > max {
				max = b.HighPriorityCoreCount
			}
		}
	}
	if max == 0 && pi.BF.Supported {
		max = len(pi.BF.HighPriorityCPUs)
	}
	return max
}

// punitGuaranteedHpCpus returns the count of HP CPUs that can
// simultaneously reach the platform's highest exposed turbo
// frequency on this punit. With SST-TF, smaller buckets unlock
// higher turbo frequencies, so the smallest non-zero
// HighPriorityCoreCount across buckets is the figure of merit:
// staying at or below it lets every HP CPU sustain the top-bucket
// frequency. When TF is unsupported, fall back to
// len(BF.HighPriorityCPUs) -- BF guarantees those CPUs run at the
// elevated base frequency, and there is no further headroom to
// reserve. Returns 0 when neither feature exposes HP capacity.
func punitGuaranteedHpCpus(pi *gosst.PerfLevelInfo) int {
	if pi == nil {
		return 0
	}
	if pi.TF.Supported {
		min := 0
		for _, b := range pi.TF.Buckets {
			if b.HighPriorityCoreCount <= 0 {
				continue
			}
			if min == 0 || b.HighPriorityCoreCount < min {
				min = b.HighPriorityCoreCount
			}
		}
		if min > 0 {
			return min
		}
	}
	if pi.BF.Supported {
		return len(pi.BF.HighPriorityCPUs)
	}
	return 0
}

func (b *sstGoresctrl) Supported() bool { return b.plat != nil }

func (b *sstGoresctrl) ClosCount() int {
	if b.plat == nil {
		return 0
	}
	return b.plat.ClosCount()
}

func (b *sstGoresctrl) PackageIDs() []int {
	if b.plat == nil {
		return nil
	}
	seen := map[int]bool{}
	ids := []int{}
	for _, pu := range b.punits {
		if seen[pu.PkgID] {
			continue
		}
		seen[pu.PkgID] = true
		ids = append(ids, pu.PkgID)
	}
	sort.Ints(ids)
	return ids
}

func (b *sstGoresctrl) CPUsOfPackage(pkgID int) []int {
	if b.plat == nil {
		return nil
	}
	out := []int{}
	for _, pu := range b.punits {
		if pu.PkgID != pkgID {
			continue
		}
		out = append(out, pu.CPUs.UnsortedList()...)
	}
	sort.Ints(out)
	return out
}

// Punits returns the cached per-punit topology and HP capacity.
func (b *sstGoresctrl) Punits() []pctPunit {
	if b.plat == nil {
		return nil
	}
	// Return a defensive copy so callers cannot mutate cached state.
	out := make([]pctPunit, len(b.punits))
	copy(out, b.punits)
	return out
}

func (b *sstGoresctrl) PrepareManagedMode() error {
	if b.plat == nil {
		return fmt.Errorf("SST not supported on this host")
	}
	for _, pkg := range b.plat.Packages() {
		if err := pkg.CPReset(); err != nil {
			return fmt.Errorf("CPReset on package %d: %w", pkg.ID(), err)
		}
		if err := pkg.TFEnable(); err != nil {
			return fmt.Errorf("TFEnable on package %d: %w", pkg.ID(), err)
		}
		if err := pkg.CPSetPriorityType(gosst.Ordered); err != nil {
			return fmt.Errorf("CPSetPriorityType on package %d: %w", pkg.ID(), err)
		}
	}
	return nil
}

func (b *sstGoresctrl) ConfigureClos(cfg pctClosConfig) error {
	if b.plat == nil {
		return fmt.Errorf("SST not supported on this host")
	}
	// pctClosConfig stores frequencies in kHz; goresctrl ClosConfig
	// uses MHz (max ratio-encoded 25500 MHz on mbox platforms).
	cc := gosst.ClosConfig{MinFreq: cfg.MinFreq / 1000, MaxFreq: cfg.MaxFreq / 1000}
	for _, pkg := range b.plat.Packages() {
		if err := pkg.ClosConfigure(cfg.ClosID, cc); err != nil {
			return fmt.Errorf("ClosConfigure(%d) on package %d: %w", cfg.ClosID, pkg.ID(), err)
		}
	}
	return nil
}

func (b *sstGoresctrl) EnableCP() error {
	if b.plat == nil {
		return fmt.Errorf("SST not supported on this host")
	}
	for _, pkg := range b.plat.Packages() {
		if err := pkg.CPEnable(); err != nil {
			return fmt.Errorf("CPEnable on package %d: %w", pkg.ID(), err)
		}
	}
	return nil
}

func (b *sstGoresctrl) AssociateCPUs(assocs []pctClosAssoc) error {
	if b.plat == nil {
		return fmt.Errorf("SST not supported on this host")
	}
	byClos := map[int]utils.IDSet{}
	for _, a := range assocs {
		if _, ok := byClos[a.ClosID]; !ok {
			byClos[a.ClosID] = utils.NewIDSet()
		}
		byClos[a.ClosID].Add(utils.ID(a.CPU))
	}
	for clos, cpus := range byClos {
		if err := b.plat.ClosAssociate(clos, cpus); err != nil {
			return fmt.Errorf("ClosAssociate(%d) for cpus %s: %w", clos, cpus, err)
		}
	}
	return nil
}

func (b *sstGoresctrl) GetCPUClosID(cpu int) (int, error) {
	if b.plat == nil {
		return 0, fmt.Errorf("SST not supported on this host")
	}
	return b.plat.GetCPUClosID(utils.ID(cpu))
}

func (b *sstGoresctrl) TFStatus() (map[pctPunitID]bool, error) {
	out := map[pctPunitID]bool{}
	if b.plat == nil {
		return out, nil
	}
	for _, pkg := range b.plat.Packages() {
		st, err := pkg.GetStatus()
		if err != nil {
			return nil, fmt.Errorf("TFStatus: package %d status: %w", pkg.ID(), err)
		}
		for pid, pu := range st.Punits {
			out[pctPunitID{PkgID: pkg.ID(), PunitID: int(pid)}] = pu.TF.Enabled
		}
	}
	return out, nil
}

// GetClosConfig returns the frequency bounds programmed on CLOS
// closID, queried from the first package (CLOS programming is
// applied identically to every package by ConfigureClos). The
// second return value is false when SST is unsupported, the
// package status cannot be read, or closID is out of range.
func (b *sstGoresctrl) GetClosConfig(closID int) (pctClosCfg, bool, error) {
	if b.plat == nil {
		return pctClosCfg{}, false, nil
	}
	pkgs := b.plat.Packages()
	if len(pkgs) == 0 {
		return pctClosCfg{}, false, nil
	}
	st, err := pkgs[0].GetStatus()
	if err != nil {
		return pctClosCfg{}, false, fmt.Errorf("GetClosConfig: package %d status: %w", pkgs[0].ID(), err)
	}
	// Pick any punit -- per-package ConfigureClos programs all
	// punits identically. goresctrl reports CLOS Config.Min/MaxFreq
	// in MHz; convert to kHz so callers always see the same unit as
	// they passed to ConfigureClos.
	for _, pu := range st.Punits {
		if closID < 0 || closID >= len(pu.Clos) {
			return pctClosCfg{}, false, nil
		}
		return pctClosCfg{
			MinFreq: pu.Clos[closID].Config.MinFreq * 1000,
			MaxFreq: pu.Clos[closID].Config.MaxFreq * 1000,
		}, true, nil
	}
	return pctClosCfg{}, false, nil
}

// MaxHpCpus method removed in favor of Punits().

func (b *sstGoresctrl) Shutdown() error {
	if b.plat == nil {
		return nil
	}
	for _, pkg := range b.plat.Packages() {
		if err := pkg.CPReset(); err != nil {
			return fmt.Errorf("CPReset on package %d: %w", pkg.ID(), err)
		}
		if err := pkg.TFDisable(); err != nil {
			return fmt.Errorf("TFDisable on package %d: %w", pkg.ID(), err)
		}
		if err := pkg.CPDisable(); err != nil {
			return fmt.Errorf("CPDisable on package %d: %w", pkg.ID(), err)
		}
	}
	return nil
}
