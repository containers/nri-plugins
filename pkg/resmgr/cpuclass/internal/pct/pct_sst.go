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
	"os"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// pctClosConfig describes one CLOS configuration that the
// Allocator wants to program.
type pctClosConfig struct {
	ClosID  int
	MinFreq int // kHz
	MaxFreq int // kHz
}

// pctClosAssoc records the desired CLOS association for a CPU.
type pctClosAssoc struct {
	CPU    int
	ClosID int
}

// pctPunit describes one SST power domain (punit) exposed by the
// platform. PkgID and PunitID together uniquely identify it; CPUs
// is the set of logical CPUs in this punit; MaxHpCpus is the
// maximum number of CPUs this punit can sustain at the elevated
// PCT high-priority frequency (SST-TF bucket count, or SST-BF HP
// CPU count when TF is unsupported). MaxHpCpus == 0 means the
// platform does not expose HP capacity for this punit; the
// allocator excludes such punits from HP steering.
type pctPunit struct {
	PkgID     int
	PunitID   int
	CPUs      cpuset.CPUSet
	MaxHpCpus int
	// GuaranteedHpCpus is the count of HP CPUs on this punit that
	// can simultaneously sustain the highest turbo frequency the
	// platform exposes: the smallest non-zero SST-TF bucket's
	// HighPriorityCoreCount (smaller buckets unlock higher
	// frequencies), or len(SST-BF HighPriorityCPUs) when TF is
	// unsupported. 0 if neither feature exposes HP capacity.
	// Used to publish scheduler-visible HP capacity that reflects
	// "guaranteed top-turbo headroom" rather than the worst-case
	// MaxHpCpus.
	GuaranteedHpCpus int
}

// pctClosCfg carries the frequency bounds programmed for one CLOS,
// in kHz. Zero stands for "not specified / leave alone".
type pctClosCfg struct {
	MinFreq int
	MaxFreq int
}

// pctPunitID identifies one power domain by (package, punit) ID.
type pctPunitID struct {
	PkgID   int
	PunitID int
}

// sst is the subset of Intel SST functionality used by the
// cpuclass code. Implementations: sstGoresctrl for real
// hardware via goresctrl/pkg/sst, and sstMock for an
// in-memory fake seeded from OVERRIDE_SST.
type sst interface {
	// Supported reports whether SST is available.
	Supported() bool

	// ClosCount returns the number of CLOSes supported.
	ClosCount() int

	// PackageIDs returns the IDs of all packages.
	PackageIDs() []int

	// CPUsOfPackage returns the CPUs of the given package.
	CPUsOfPackage(pkgID int) []int

	// Punits returns the per-punit topology and HP capacity of
	// every package the platform exposes. Order is stable.
	Punits() []pctPunit

	// GetClosConfig returns the frequency bounds currently
	// programmed for closID. The second return value is false
	// when no information is available (e.g. closID not in
	// range, or the platform does not expose per-CLOS
	// configuration). Used in assoc-only mode to classify a CLOS
	// as HP or LP from its programmed MaxFreq.
	GetClosConfig(closID int) (pctClosCfg, bool, error)

	// PrepareManagedMode resets and enables SST-TF on every
	// package and selects ordered priority arbitration.
	PrepareManagedMode() error

	// ConfigureClos programs CLOS frequency bounds on every
	// package.
	ConfigureClos(cfg pctClosConfig) error

	// EnableCP enables SST-CP on every package.
	EnableCP() error

	// AssociateCPUs binds each CPU to the indicated CLOS.
	AssociateCPUs(assocs []pctClosAssoc) error

	// TFStatus returns the current SST-TF enabled state per
	// power domain. The map is empty when SST is unsupported.
	// The status is read at call time (SST-TF can be toggled
	// out-of-band by the operator). Used in assoc-only mode to
	// warn at configure time when SST-TF is disabled on a punit
	// hosting PCT-managed CPUs -- without SST-TF, HP cores on
	// that punit cannot exceed the standard turbo-ratio bucket
	// limit even if associated to a low-CLOS-ID (HP) CLOS.
	TFStatus() (map[pctPunitID]bool, error)

	// GetCPUClosID returns the current CLOS association of a CPU.
	GetCPUClosID(cpu int) (int, error)

	// Shutdown restores managed-mode platform state to defaults.
	Shutdown() error
}

// newSst returns an SST implementation: the in-memory mock when
// OVERRIDE_SST is set, otherwise the goresctrl-backed one.
func newSst() (sst, error) {
	if v := os.Getenv(sstOverrideEnvVar); v != "" {
		return newSstMock(v)
	}
	return newSstGoresctrl()
}
