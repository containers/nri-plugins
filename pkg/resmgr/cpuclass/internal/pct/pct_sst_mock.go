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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// sstOverrideEnvVar holds JSON seeding the in-memory SST mock.
// Follows the existing OVERRIDE_SYS_CACHES / OVERRIDE_SYS_CPUFREQ
// convention in pkg/sysfs/system.go.
const (
	sstOverrideEnvVar      = "OVERRIDE_SST"
	sstOverrideStateDirVar = "OVERRIDE_SST_STATE_DIR"
	sstOverrideStateFile   = "state.json"
)

// sstMockClos seeds the per-CLOS state of one package.
type sstMockClos struct {
	ID      int    `json:"id"`
	MinFreq int    `json:"min_freq"`
	MaxFreq int    `json:"max_freq"`
	CPUs    string `json:"cpus,omitempty"` // listset like "0-15"
}

// sstMockPunit seeds one punit's CPUs and HP capacity.
type sstMockPunit struct {
	ID               int    `json:"id"`
	CPUs             string `json:"cpus"` // listset
	MaxHpCpus        int    `json:"max_hp_cpus,omitempty"`
	GuaranteedHpCpus int    `json:"guaranteed_hp_cpus,omitempty"`
}

// sstMockPackage seeds one package's worth of SST state.
type sstMockPackage struct {
	ID          int    `json:"id"`
	CPUs        string `json:"cpus"` // listset of all CPUs in the package
	TFSupported bool   `json:"tf_supported"`
	TFEnabled   bool   `json:"tf_enabled"`
	CPSupported bool   `json:"cp_supported"`
	CPEnabled   bool   `json:"cp_enabled"`
	CPPriority  string `json:"cp_priority,omitempty"` // "ordered" or "proportional"
	// MaxHpCpus seeds a per-package HP CPU count for the
	// back-compat case where Punits is not specified -- one
	// synthetic punit is created containing every package CPU
	// and this MaxHpCpus value.
	MaxHpCpus int             `json:"max_hp_cpus,omitempty"`
	Punits    []*sstMockPunit `json:"punits,omitempty"`
	Clos      []*sstMockClos  `json:"clos,omitempty"`
}

// sstMockDoc is the full JSON document accepted in OVERRIDE_SST.
type sstMockDoc struct {
	Supported bool              `json:"supported"`
	ClosCount int               `json:"clos_count"`
	Packages  []*sstMockPackage `json:"packages"`
}

// sstMock is an in-memory sst implementation. Seed
// state comes from OVERRIDE_SST; mutations from policy calls are
// recorded into the in-memory doc and persisted to a state file
// after every operation so e2e tests can inspect the result.
type sstMock struct {
	doc      *sstMockDoc
	cpuPkg   map[int]*sstMockPackage // cpu -> package
	cpuClos  map[int]int             // cpu -> currently-associated CLOS id
	stateDir string
}

func newSstMock(jsonData string) (sst, error) {
	doc := &sstMockDoc{}
	if err := json.Unmarshal([]byte(jsonData), doc); err != nil {
		return nil, fmt.Errorf("failed to parse %s JSON: %w", sstOverrideEnvVar, err)
	}
	if doc.ClosCount == 0 {
		doc.ClosCount = 4
	}
	b := &sstMock{
		doc:      doc,
		cpuPkg:   map[int]*sstMockPackage{},
		cpuClos:  map[int]int{},
		stateDir: os.Getenv(sstOverrideStateDirVar),
	}
	if b.stateDir == "" {
		b.stateDir = "/tmp/nri-pct-mock"
	}
	for _, pkg := range doc.Packages {
		cpus, err := parseCPUList(pkg.CPUs)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid cpus %q in package %d: %w", sstOverrideEnvVar, pkg.CPUs, pkg.ID, err)
		}
		for _, c := range cpus {
			b.cpuPkg[c] = pkg
			b.cpuClos[c] = 0
		}
		// If seed pre-associates CPUs to non-zero CLOSes, honor that.
		for _, cl := range pkg.Clos {
			if cl.CPUs == "" {
				continue
			}
			clCpus, err := parseCPUList(cl.CPUs)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid clos.cpus %q: %w", sstOverrideEnvVar, cl.CPUs, err)
			}
			for _, c := range clCpus {
				b.cpuClos[c] = cl.ID
			}
		}
	}
	if err := b.persist(); err != nil {
		log.Warnf("pct mock: failed to write initial state file: %v", err)
	}
	log.Infof("pct mock: seeded with %d package(s), supported=%v, closCount=%d, stateDir=%q",
		len(doc.Packages), doc.Supported, doc.ClosCount, b.stateDir)
	return b, nil
}

func (b *sstMock) Supported() bool { return b.doc.Supported }

func (b *sstMock) ClosCount() int { return b.doc.ClosCount }

func (b *sstMock) PackageIDs() []int {
	ids := make([]int, 0, len(b.doc.Packages))
	for _, p := range b.doc.Packages {
		ids = append(ids, p.ID)
	}
	sort.Ints(ids)
	return ids
}

func (b *sstMock) CPUsOfPackage(pkgID int) []int {
	for _, p := range b.doc.Packages {
		if p.ID == pkgID {
			cpus, _ := parseCPUList(p.CPUs)
			return cpus
		}
	}
	return nil
}

func (b *sstMock) pkgEnsureClos(pkg *sstMockPackage, clos int) *sstMockClos {
	for _, c := range pkg.Clos {
		if c.ID == clos {
			return c
		}
	}
	c := &sstMockClos{ID: clos}
	pkg.Clos = append(pkg.Clos, c)
	sort.Slice(pkg.Clos, func(i, j int) bool { return pkg.Clos[i].ID < pkg.Clos[j].ID })
	return c
}

func (b *sstMock) PrepareManagedMode() error {
	for _, pkg := range b.doc.Packages {
		// CPReset: clear CLOS configs, associate all CPUs to CLOS 0.
		pkg.Clos = nil
		cpus, _ := parseCPUList(pkg.CPUs)
		for _, c := range cpus {
			b.cpuClos[c] = 0
		}
		pkg.TFEnabled = true
		pkg.CPPriority = "ordered"
	}
	log.Debugf("pct mock: PrepareManagedMode done (CPReset+TFEnable+CPSetPriorityType=ordered)")
	return b.persist()
}

func (b *sstMock) ConfigureClos(cfg pctClosConfig) error {
	for _, pkg := range b.doc.Packages {
		c := b.pkgEnsureClos(pkg, cfg.ClosID)
		c.MinFreq = cfg.MinFreq
		c.MaxFreq = cfg.MaxFreq
	}
	log.Debugf("pct mock: ConfigureClos %+v", cfg)
	return b.persist()
}

func (b *sstMock) EnableCP() error {
	for _, pkg := range b.doc.Packages {
		pkg.CPEnabled = true
	}
	log.Debugf("pct mock: EnableCP done")
	return b.persist()
}

func (b *sstMock) AssociateCPUs(assocs []pctClosAssoc) error {
	for _, a := range assocs {
		if _, ok := b.cpuPkg[a.CPU]; !ok {
			return fmt.Errorf("pct mock: CPU %d not present in any seeded package", a.CPU)
		}
		b.cpuClos[a.CPU] = a.ClosID
	}
	// Refresh per-CLOS CPU lists on each package for readable state.
	for _, pkg := range b.doc.Packages {
		clos2cpus := map[int][]int{}
		cpus, _ := parseCPUList(pkg.CPUs)
		for _, c := range cpus {
			cl := b.cpuClos[c]
			clos2cpus[cl] = append(clos2cpus[cl], c)
		}
		for _, cl := range pkg.Clos {
			cl.CPUs = formatCPUList(clos2cpus[cl.ID])
			delete(clos2cpus, cl.ID)
		}
		for clID, list := range clos2cpus {
			c := b.pkgEnsureClos(pkg, clID)
			c.CPUs = formatCPUList(list)
		}
	}
	log.Debugf("pct mock: AssociateCPUs %+v", assocs)
	return b.persist()
}

func (b *sstMock) GetCPUClosID(cpu int) (int, error) {
	cl, ok := b.cpuClos[cpu]
	if !ok {
		return 0, fmt.Errorf("pct mock: CPU %d not present in any seeded package", cpu)
	}
	return cl, nil
}

// Punits returns the per-punit topology of every seeded package.
// If a package's seed omits the Punits list, a single synthetic
// punit (ID 0) is returned spanning every CPU of the package,
// carrying the package-level MaxHpCpus for back-compat with the
// pre-punit OVERRIDE_SST schema.
func (b *sstMock) Punits() []pctPunit {
	out := []pctPunit{}
	// Stable order: sort packages by ID, punits by ID.
	pkgIDs := make([]int, 0, len(b.doc.Packages))
	pkgByID := map[int]*sstMockPackage{}
	for _, p := range b.doc.Packages {
		pkgIDs = append(pkgIDs, p.ID)
		pkgByID[p.ID] = p
	}
	sort.Ints(pkgIDs)
	for _, pid := range pkgIDs {
		pkg := pkgByID[pid]
		if len(pkg.Punits) == 0 {
			cpus, _ := parseCPUList(pkg.CPUs)
			out = append(out, pctPunit{
				PkgID:            pkg.ID,
				PunitID:          0,
				CPUs:             cpuset.New(cpus...),
				MaxHpCpus:        pkg.MaxHpCpus,
				GuaranteedHpCpus: pkg.MaxHpCpus,
			})
			continue
		}
		punits := append([]*sstMockPunit(nil), pkg.Punits...)
		sort.Slice(punits, func(i, j int) bool { return punits[i].ID < punits[j].ID })
		for _, pu := range punits {
			cpus, _ := parseCPUList(pu.CPUs)
			gtd := pu.GuaranteedHpCpus
			if gtd == 0 {
				gtd = pu.MaxHpCpus
			}
			out = append(out, pctPunit{
				PkgID:            pkg.ID,
				PunitID:          pu.ID,
				CPUs:             cpuset.New(cpus...),
				MaxHpCpus:        pu.MaxHpCpus,
				GuaranteedHpCpus: gtd,
			})
		}
	}
	return out
}

// GetClosConfig returns the frequency bounds currently programmed
// for closID. The mock's CLOS state is shared across packages by
// construction (ConfigureClos writes it to all packages); we
// return the first package's entry.
func (b *sstMock) GetClosConfig(closID int) (pctClosCfg, bool, error) {
	for _, pkg := range b.doc.Packages {
		for _, cl := range pkg.Clos {
			if cl.ID != closID {
				continue
			}
			return pctClosCfg{MinFreq: cl.MinFreq, MaxFreq: cl.MaxFreq}, true, nil
		}
		// First package checked, no entry for closID.
		return pctClosCfg{}, false, nil
	}
	return pctClosCfg{}, false, nil
}

// TFStatus mirrors the per-package TFEnabled flag onto each of
// the package's punits (the mock's TF state is per-package).
func (b *sstMock) TFStatus() (map[pctPunitID]bool, error) {
	out := map[pctPunitID]bool{}
	for _, pkg := range b.doc.Packages {
		if len(pkg.Punits) == 0 {
			out[pctPunitID{PkgID: pkg.ID, PunitID: 0}] = pkg.TFEnabled
			continue
		}
		for _, pu := range pkg.Punits {
			out[pctPunitID{PkgID: pkg.ID, PunitID: pu.ID}] = pkg.TFEnabled
		}
	}
	return out, nil
}

func (b *sstMock) Shutdown() error {
	for cpu := range b.cpuClos {
		b.cpuClos[cpu] = 0
	}
	for _, pkg := range b.doc.Packages {
		pkg.Clos = nil
		pkg.TFEnabled = false
		pkg.CPEnabled = false
	}
	log.Debugf("pct mock: Shutdown done")
	return b.persist()
}

func (b *sstMock) persist() error {
	if err := os.MkdirAll(b.stateDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b.doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.stateDir, sstOverrideStateFile), data, 0o644)
}

// parseCPUList parses a listset string like "0-3,8,10-12".
func parseCPUList(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	out := []int{}
	for _, part := range splitComma(s) {
		if part == "" {
			continue
		}
		lo, hi, err := parseRange(part)
		if err != nil {
			return nil, err
		}
		for i := lo; i <= hi; i++ {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out, nil
}

// formatCPUList formats an int slice as a listset like "0-3,8,10-12".
func formatCPUList(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := append([]int(nil), ids...)
	sort.Ints(sorted)
	var parts []string
	lo := sorted[0]
	prev := lo
	flush := func() {
		if lo == prev {
			parts = append(parts, fmt.Sprintf("%d", lo))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", lo, prev))
		}
	}
	for _, v := range sorted[1:] {
		if v == prev+1 {
			prev = v
			continue
		}
		flush()
		lo, prev = v, v
	}
	flush()
	return joinComma(parts)
}

func splitComma(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func parseRange(s string) (int, int, error) {
	for i, r := range s {
		if r == '-' {
			lo, err := atoi(s[:i])
			if err != nil {
				return 0, 0, err
			}
			hi, err := atoi(s[i+1:])
			if err != nil {
				return 0, 0, err
			}
			return lo, hi, nil
		}
	}
	v, err := atoi(s)
	if err != nil {
		return 0, 0, err
	}
	return v, v, nil
}

func atoi(s string) (int, error) {
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, fmt.Errorf("invalid integer %q: %w", s, err)
	}
	return v, nil
}
