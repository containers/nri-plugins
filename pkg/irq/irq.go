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
package irq

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

var (
	procRoot = "/"
	proc     = "/proc"

	log = logger.NewLogger("irq")
)

// SetProcRoot sets the procfs root directory and proc mountpoint.
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
}

// Irq represents a single numbered hardware interrupt.
type Irq struct {
	num         int
	description string
}

// Interrupts returns all numbered interrupts listed in
// /proc/interrupts.
func Interrupts() ([]*Irq, error) {
	data, err := os.ReadFile(filepath.Join(proc, "interrupts"))
	if err != nil {
		return nil, fmt.Errorf("failed to read interrupts: %w", err)
	}
	irqs := []*Irq{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if irq, ok := parseInterruptsLine(line); ok {
			irqs = append(irqs, irq)
		}
	}
	return irqs, nil
}

// parseInterruptsLine returns the interrupt parsed from a
// /proc/interrupts line and whether the line described a numbered
// interrupt.
func parseInterruptsLine(line string) (*Irq, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, false
	}
	head, rest := fields[0], fields[1:]
	if !strings.HasSuffix(head, ":") {
		return nil, false
	}
	num, err := strconv.Atoi(strings.TrimSuffix(head, ":"))
	if err != nil {
		return nil, false
	}
	// Skip the leading per-CPU interrupt counters and keep the
	// remaining chip and device description for matching.
	descStart := len(rest)
	for i, field := range rest {
		if _, err := strconv.Atoi(field); err != nil {
			descStart = i
			break
		}
	}
	return &Irq{
		num:         num,
		description: strings.Join(rest[descStart:], " "),
	}, true
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

// AffinityCpus returns the CPUs in the affinity of the interrupt.
func (irq *Irq) AffinityCpus() (cpuset.CPUSet, error) {
	data, err := os.ReadFile(irq.smpAffinityListPath())
	if err != nil {
		return cpuset.New(), fmt.Errorf("failed to read affinity of irq %d: %w", irq.num, err)
	}
	cpus, err := cpuset.Parse(strings.TrimSpace(string(data)))
	if err != nil {
		return cpuset.New(), fmt.Errorf("failed to parse affinity of irq %d: %w", irq.num, err)
	}
	return cpus, nil
}

// SetAffinityCpus sets the CPUs in the affinity of the interrupt.
func (irq *Irq) SetAffinityCpus(cpus cpuset.CPUSet) error {
	if cpus.IsEmpty() {
		return fmt.Errorf("refusing to set empty affinity on irq %d", irq.num)
	}
	if err := os.WriteFile(irq.smpAffinityListPath(), []byte(cpus.String()), 0644); err != nil {
		return fmt.Errorf("failed to set affinity of irq %d to %q: %w", irq.num, cpus, err)
	}
	log.Debugf("irq %s smp_affinity_list written: %s", irq, cpus)
	return nil
}

// smpAffinityListPath returns the path to the smp_affinity_list file
// of the interrupt.
func (irq *Irq) smpAffinityListPath() string {
	return filepath.Join(proc, "irq", strconv.Itoa(irq.num), "smp_affinity_list")
}
