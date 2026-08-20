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

package irq

// This file implements the caching layer of the irq package. All procfs
// reads, writes and parsing of this package happen here.
//
// Caching assumes that interrupts are neither added nor removed at
// runtime, and that no entity outside this package alters interrupt
// affinities. Therefore the interrupts file is read and parsed only
// once, and the affinity of an interrupt is read only until its first
// known value.

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// irqInfo is a numbered interrupt as parsed from the interrupts file,
// without the allow pattern dependent denied flag.
type irqInfo struct {
	num         int
	description string
}

// fileOps contains the replaceable file operations of the cache.
type fileOps struct {
	readFile  func(string) ([]byte, error)
	writeFile func(string, []byte, os.FileMode) error
}

// irqCache caches interrupt data read from procfs and buffers
// interrupt affinities to be written to procfs.
type irqCache struct {
	mu   sync.Mutex
	proc string  // procfs mountpoint, all paths are built from this
	ops  fileOps // file operations to use

	info     map[int]irqInfo       // cached numbered interrupts, nil if not read
	cpus     map[int]cpuset.CPUSet // affinities read from or set through the cache
	pending  map[int]cpuset.CPUSet // affinities not yet written to procfs, empty if unblocked
	readOnly map[int]bool          // interrupts whose affinity cannot be written

	writeBlock int // writes to affinities are only buffered while positive
}

// cache is the interrupt cache of this package.
var cache = newIrqCache()

// newIrqCache returns a new interrupt cache with real procfs file
// operations.
func newIrqCache() *irqCache {
	return &irqCache{
		proc: proc,
		ops: fileOps{
			readFile:  os.ReadFile,
			writeFile: os.WriteFile,
		},
		cpus:     map[int]cpuset.CPUSet{},
		pending:  map[int]cpuset.CPUSet{},
		readOnly: map[int]bool{},
	}
}

// interruptsPath returns the path to the interrupts file in procfs.
func interruptsPath(proc string) string {
	return filepath.Join(proc, "interrupts")
}

// smpAffinityListPath returns the path to the smp_affinity_list file
// of the given interrupt in procfs.
func smpAffinityListPath(proc string, num int) string {
	return filepath.Join(proc, "irq", strconv.Itoa(num), "smp_affinity_list")
}

// isReadOnlyAffinity returns whether the error of a failed affinity
// write means that the affinity cannot be written at all.
func isReadOnlyAffinity(err error) bool {
	// Writing a read-only smp_affinity_list fails with EPERM, writing
	// an affinity which the kernel manages fails with EIO.
	return errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EIO) ||
		errors.Is(err, syscall.EROFS)
}

// parseInterruptsLine returns the interrupt parsed from an interrupts
// file line and whether the line described a numbered interrupt.
func parseInterruptsLine(line string) (irqInfo, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return irqInfo{}, false
	}
	head, rest := fields[0], fields[1:]
	if !strings.HasSuffix(head, ":") {
		return irqInfo{}, false
	}
	num, err := strconv.Atoi(strings.TrimSuffix(head, ":"))
	if err != nil {
		return irqInfo{}, false
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
	return irqInfo{
		num:         num,
		description: strings.Join(rest[descStart:], " "),
	}, true
}

// parseInterrupts returns the numbered interrupts parsed from the
// contents of the interrupts file, keyed by interrupt number.
func parseInterrupts(data []byte) map[int]irqInfo {
	infos := map[int]irqInfo{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if info, ok := parseInterruptsLine(line); ok {
			infos[info.num] = info
		}
	}
	return infos
}

// interruptInfo returns the numbered interrupts of the system, keyed by
// interrupt number. The interrupts file is read and parsed only once.
// The returned map is shared and never modified.
func (c *irqCache) interruptInfo() (map[int]irqInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.info != nil {
		return c.info, nil
	}

	data, err := c.ops.readFile(interruptsPath(c.proc))
	if err != nil {
		// Failed reads are never cached.
		return nil, err
	}

	c.info = parseInterrupts(data)
	log.Debugf("cached %d numbered interrupts from %s", len(c.info), interruptsPath(c.proc))

	return c.info, nil
}

// forEachInterrupt calls the given function on every numbered interrupt
// of the system in interrupt number order, stopping early and returning
// the error of the first failing call. Whether an interrupt is denied is
// calculated from the given allow patterns:
//   - fn: function to call on each interrupt.
//   - allow: patterns which define the allowed interrupts.
func (c *irqCache) forEachInterrupt(fn func(*Irq) error, allow []string) error {
	infos, err := c.interruptInfo()
	if err != nil {
		return fmt.Errorf("failed to read interrupts: %w", err)
	}
	for _, num := range slices.Sorted(maps.Keys(infos)) {
		// Mint a fresh Irq on every call: allow patterns, and hence
		// denied flags, may have changed.
		info := infos[num]
		irq := &Irq{
			num:         info.num,
			description: info.description,
			denied:      !isAllowedInterruptBy(info.description, allow),
		}
		if err := fn(irq); err != nil {
			return err
		}
	}
	return nil
}

// affinityOf returns the CPUs in the affinity of the given interrupt.
// The affinity is read from procfs only until it is known, and
// affinities set through the cache are visible before they have been
// written to procfs.
func (c *irqCache) affinityOf(num int) (cpuset.CPUSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if cpus, ok := c.cpus[num]; ok {
		return cpus, nil
	}

	data, err := c.ops.readFile(smpAffinityListPath(c.proc, num))
	if err != nil {
		return cpuset.New(), fmt.Errorf("failed to read affinity of irq %d: %w", num, err)
	}
	cpus, err := cpuset.Parse(strings.TrimSpace(string(data)))
	if err != nil {
		return cpuset.New(), fmt.Errorf("failed to parse affinity of irq %d: %w", num, err)
	}
	c.cpus[num] = cpus

	return cpus, nil
}

// setAffinity sets the CPUs in the affinity of the given interrupt.
// While writes are blocked, the affinity is only buffered and no error
// is returned. Otherwise it is written to procfs immediately.
func (c *irqCache) setAffinity(num int, cpus cpuset.CPUSet) error {
	c.mu.Lock()
	if c.readOnly[num] {
		// Unwritable, do not even try again.
		c.mu.Unlock()
		return nil
	}
	_, buffered := c.pending[num]
	if known, ok := c.cpus[num]; ok && !buffered && known.Equals(cpus) {
		// Already in procfs.
		c.mu.Unlock()
		return nil
	}
	// An unknown affinity, or buffered but not written yet.
	c.cpus[num] = cpus
	c.pending[num] = cpus

	// Write now or later?
	blocked := c.writeBlock > 0
	c.mu.Unlock()

	if blocked {
		return nil
	}
	return c.writePending()
}

// blockWrites starts buffering interrupt affinities instead of writing
// them to procfs.
func (c *irqCache) blockWrites() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeBlock++
}

// unblockWrites removes one interrupt affinity write block, writing all
// buffered affinities to procfs once the last block is removed.
func (c *irqCache) unblockWrites() {
	c.mu.Lock()
	if c.writeBlock == 0 {
		c.mu.Unlock()
		log.Errorf("interrupt affinity write block counter went negative, keeping it at 0")
		return
	}
	c.writeBlock--
	blocked := c.writeBlock > 0
	c.mu.Unlock()

	if blocked {
		return
	}
	if err := c.writePending(); err != nil {
		log.Errorf("failed to write buffered interrupt affinities: %v", err)
	}
}

// writePending writes all buffered interrupt affinities to procfs in
// interrupt number order, returning the first error, if any.
func (c *irqCache) writePending() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var firstErr error

	for _, num := range slices.Sorted(maps.Keys(c.pending)) {
		cpus := c.pending[num]
		delete(c.pending, num)

		err := c.ops.writeFile(smpAffinityListPath(c.proc, num), []byte(cpus.String()), 0644)
		if err == nil {
			log.Debugf("irq %d (%s) smp_affinity_list written: %s",
				num, c.info[num].description, cpus)
			continue
		}

		// Write failed. Forget the affinity, so that the next
		// read falls back to procfs and the next update
		// recalculates and retries.
		delete(c.cpus, num)
		if isReadOnlyAffinity(err) {
			c.readOnly[num] = true
			log.Warnf("irq %d (%s) affinity is read-only, not setting it again: %v",
				num, c.info[num].description, err)
		} else {
			log.Warnf("failed to set affinity of irq %d (%s) to %q: %v",
				num, c.info[num].description, cpus, err)
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("failed to set affinity of irq %d to %q: %w", num, cpus, err)
		}
	}

	return firstErr
}

// reset drops all cached interrupt data, buffered affinities included,
// and points the cache to the given procfs mountpoint. The write block
// counter is not affected.
func (c *irqCache) reset(procDir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proc = procDir
	c.info = nil
	c.cpus = map[int]cpuset.CPUSet{}
	c.pending = map[int]cpuset.CPUSet{}
	c.readOnly = map[int]bool{}
}
