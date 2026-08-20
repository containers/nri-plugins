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

// irq package provides low-level functions to read interrupts from
// /proc/interrupts and to read and write CPU affinities of interrupts
// through /proc/irq/NUMBER/smp_affinity_list.
//
// Interrupt data read from procfs is cached, which assumes that
// interrupts are neither added nor removed at runtime and that no
// entity outside this package alters interrupt affinities. Affinities
// set between BlockWrites and UnblockWrites are buffered, and only
// the affinity set last for an interrupt reaches procfs.
package irq

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

var (
	procRoot = "/"
	proc     = "/proc"

	log = logger.NewLogger("irq")

	allowed []string

	// ErrDeniedInterrupt is the error returned for attempts to reference or control
	// globally disallowed interrupts.
	ErrDeniedInterrupt = errors.New("denied interrupt")
)

// SetProcRoot sets the procfs root directory and proc mountpoint. All
// data cached from the previous mountpoint is dropped, including
// buffered interrupt affinities.
func SetProcRoot(root string) {
	if root != "" {
		procRoot = filepath.Clean(root)
		if procRoot != "" && !filepath.IsAbs(procRoot) {
			a, err := filepath.Abs(procRoot)
			if err != nil {
				panic(fmt.Errorf("failed to resolve procfs root %q to absolute path: %v", procRoot, err))
			}
			procRoot = a
		}
		if procRoot == "/" {
			procRoot = ""
		}
	} else {
		procRoot = ""
	}
	proc = filepath.Join(procRoot, "/proc")
	cache.reset(proc)
}

// DropCache drops all cached interrupt data: the interrupts read from
// the interrupts file, the affinities read from or written to procfs,
// and the affinities which are buffered but not written yet.
func DropCache() {
	cache.reset(proc)
}

// BlockWrites starts buffering interrupt affinities instead of writing
// them to procfs. Every call must be paired with an UnblockWrites call,
// and the calls may be nested.
func BlockWrites() {
	cache.blockWrites()
}

// UnblockWrites removes one write block set by BlockWrites. Removing
// the last block writes all buffered interrupt affinities to procfs,
// logging write errors instead of returning them.
func UnblockWrites() {
	cache.unblockWrites()
}

// ValidateAllowedPatterns validates zero or more globbing patterns
// for interrupt description matching.
func ValidateAllowedPatterns(patterns []string) error {
	for _, p := range patterns {
		if _, err := path.Match(p, "0"); err != nil {
			return fmt.Errorf("invalid globbing pattern %q: %v", p, err)
		}
	}
	return nil
}

// SetAllowedInterrupts configures which interrupts can be controller
// using this package. With allow patterns unset any interrupt can be
// controlled. With patterns set, only interrupts with a matching
// description in /proc/interrupts can be controlled. If any pattern
// fails validity checking an error is returned. On success, the old
// patterns previously in effect are returned.
func SetAllowedInterrupts(patterns []string) ([]string, error) {
	if err := ValidateAllowedPatterns(patterns); err != nil {
		return nil, err
	}

	old := allowed
	allowed = patterns
	log.Debugf("allowed interrupts to %v", patterns)

	return old, nil
}

// IsAllowedInterrupt returns true if the IRQ corresponding to the given
// description is allowed to be controlled via this package, as defined
// by the currently set allowed patterns.
func IsAllowedInterrupt(description string) bool {
	return isAllowedInterruptBy(description, allowed)
}

// isAllowedInterruptBy returns true if the IRQ corresponding to the given
// description is allowed to be controlled via this package, as defined by
// the given allow patterns.
func isAllowedInterruptBy(description string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, p := range allow {
		if ok, _ := path.Match(p, description); ok {
			return true
		}
	}
	return false
}

// Irq represents a single numbered hardware interrupt.
type Irq struct {
	num         int
	description string
	denied      bool
}

// ForEachInterrupt collects all numbered interrupts listed in
// /proc/interrupts, calling the given function on each. If the
// function returns an error, iteration is stopped early and
// the received error is returned. It uses the currently set
// allowed patterns to determine if an interrupt is allowed to
// be controller by this package.
func ForEachInterrupt(fn func(*Irq) error) error {
	return forEachInterrupt(fn, allowed)
}

// forEachInterrupt collects all numbered interrupts listed in
// /proc/interrupts, calling the given function on each. If the
// function returns an error, iteration is stopped early and the
// received error is returned. It uses the given allow patterns
// to determine if an interrupt is allowed to be controlled
// by this package.
func forEachInterrupt(fn func(*Irq) error, allow []string) error {
	return cache.forEachInterrupt(fn, allow)
}

// Interrupts collects and returns the numbered interrupts listed in
// /proc/interrupts which can be controlled by this package according
// to the currently set allowed patterns.
func Interrupts() ([]*Irq, error) {
	return allowedInterrupts(allowed)
}

// allowedInterrupts collects and returns the numbered interrupts
// listed in /proc/interrupts which can be controlled by this package
// according to the given allow patterns.
func allowedInterrupts(allow []string) ([]*Irq, error) {
	var (
		irqs           = []*Irq{}
		collectAllowed = func(irq *Irq) error {
			if !irq.denied {
				irqs = append(irqs, irq)
			}
			return nil
		}
	)

	if err := forEachInterrupt(collectAllowed, allow); err != nil {
		return nil, err
	}

	return irqs, nil
}

// ValidateAllowedReferencedInterrupts takes a set of user interrupt glob
// patterns and validates them against the denied set of HW IRQs, as defined
// by the given allow patterns. Returns denied IRQs referenced by any pattern,
// with denied reference details in errors.
func ValidateAllowedReferencedInterrupts(references []string, allow []string) ([]*Irq, error) {
	var (
		errs          []error
		denied        = []*Irq{}
		collectDenied = func(irq *Irq) error {
			if !irq.denied {
				return nil
			}

			for _, p := range references {
				if ok, _ := path.Match(p, irq.description); ok {
					denied = append(denied, irq)
					errs = append(errs,
						fmt.Errorf("%w: irq %d (%s) denied but matched by user pattern %q",
							ErrDeniedInterrupt, irq.num, irq.description, p))
				}
			}

			return nil
		}
	)

	if err := forEachInterrupt(collectDenied, allow); err != nil {
		return nil, err
	}

	return denied, errors.Join(errs...)
}

// ValidateReferencedInterrupts takes a set of user interrupt glob
// patterns and validates them against the denied set of HW IRQs,
// as defined by the currently set allowed patterns. Returns denied IRQs
// referenced by any pattern, with denied reference details in errors.
func ValidateReferencedInterrupts(references []string) ([]*Irq, error) {
	denied, err := ValidateAllowedReferencedInterrupts(references, allowed)
	return denied, err
}

// Num returns the number of the interrupt.
func (irq *Irq) Num() int {
	return irq.num
}

// Description returns the chip and device description of the interrupt.
func (irq *Irq) Description() string {
	return irq.description
}

// String returns the interrupt as a human readable string.
func (irq *Irq) String() string {
	return fmt.Sprintf("irq %d (%s)", irq.num, irq.description)
}

// Match returns whether the interrupt matches the pattern string,
// that is either the exact interrupt number or a shell-like wildcard
// pattern match of its description.
func (irq *Irq) Match(pattern string) bool {
	if pattern == "" {
		return false
	}
	if pattern == strconv.Itoa(irq.num) {
		return true
	}
	match, err := path.Match(pattern, irq.description)
	return err == nil && match
}

func (irq *Irq) isAllowed() bool {
	return !irq.denied
}

// AffinityCpus returns the CPUs in the affinity of the interrupt. The
// returned CPUs are the ones set last through this package, even if
// they have not reached procfs yet.
func (irq *Irq) AffinityCpus() (cpuset.CPUSet, error) {
	return cache.affinityOf(irq.num)
}

// SetAffinityCpus sets the CPUs in the affinity of the interrupt.
// While writes are blocked, the affinity is only buffered and write
// errors are logged instead of being returned.
func (irq *Irq) SetAffinityCpus(cpus cpuset.CPUSet) error {
	if !irq.isAllowed() {
		return fmt.Errorf("%w: refusing to set affinity of irq %d", ErrDeniedInterrupt, irq.num)
	}
	if cpus.IsEmpty() {
		return fmt.Errorf("refusing to set empty affinity on irq %d", irq.num)
	}
	return cache.setAffinity(irq.num, cpus)
}
