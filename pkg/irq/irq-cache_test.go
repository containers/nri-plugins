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

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// fakeProc is a fake procfs which counts reads and writes.
type fakeProc struct {
	mu       sync.Mutex
	content  map[string]string
	reads    map[string]int
	writes   map[string]int
	writeErr error
}

func newFakeProc() *fakeProc {
	return &fakeProc{
		content: map[string]string{},
		reads:   map[string]int{},
		writes:  map[string]int{},
	}
}

func (f *fakeProc) set(path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content[path] = content
}

func (f *fakeProc) get(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.content[path]
}

func (f *fakeProc) failWrites(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeErr = err
}

func (f *fakeProc) readCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[path]
}

func (f *fakeProc) writeCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[path]
}

func (f *fakeProc) totalReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.reads {
		total += n
	}
	return total
}

func (f *fakeProc) totalWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.writes {
		total += n
	}
	return total
}

func (f *fakeProc) readFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[path]++
	content, ok := f.content[path]
	if !ok {
		return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
	}
	return []byte(content), nil
}

func (f *fakeProc) writeFile(path string, data []byte, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes[path]++
	if f.writeErr != nil {
		return f.writeErr
	}
	f.content[path] = string(data)
	return nil
}

const testInterrupts = `           CPU0       CPU1       CPU2       CPU3
  1:          0          0     601604          0  IR-IO-APIC    1-edge      i8042
  9:     236650          0          0          0  IR-IO-APIC    9-fasteoi   acpi
 16:          0          0         32          0  IR-IO-APIC   16-fasteoi   i801_smbus, processor_thermal_device_pci
NMI:          0          0          0          0  Non-maskable interrupts
LOC:     123456     123456     123456     123456  Local timer interrupts
`

// setupCache points the package to a fresh cache backed by a fake
// procfs mounted at /proc, and undoes this when the test finishes.
func setupCache(t *testing.T) *fakeProc {
	t.Helper()

	fake := newFakeProc()
	fake.set("/proc/interrupts", testInterrupts)

	oldCache, oldAllowed := cache, allowed
	allowed = nil

	SetProcRoot("")
	cache = newIrqCache()
	cache.ops = fileOps{readFile: fake.readFile, writeFile: fake.writeFile}

	t.Cleanup(func() {
		cache, allowed = oldCache, oldAllowed
		SetProcRoot("")
	})

	return fake
}

// withWritesBlocked calls the given function between BlockWrites and
// UnblockWrites.
func withWritesBlocked(fn func()) {
	BlockWrites()
	defer UnblockWrites()
	fn()
}

// writeBlock returns the write block counter of the cache.
func writeBlock() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.writeBlock
}

// affinityPath returns the path of the smp_affinity_list file of the
// given interrupt in the fake procfs.
func affinityPath(num int) string {
	return smpAffinityListPath("/proc", num)
}

// setAffinity sets the affinity of the given interrupt.
func setAffinity(t *testing.T, num int, cpus string) {
	t.Helper()
	if err := (&Irq{num: num}).SetAffinityCpus(cpuset.MustParse(cpus)); err != nil {
		t.Fatalf("SetAffinityCpus(%q) on irq %d failed: %v", cpus, num, err)
	}
}

// getAffinity returns the affinity of the given interrupt.
func getAffinity(t *testing.T, num int) string {
	t.Helper()
	cpus, err := (&Irq{num: num}).AffinityCpus()
	if err != nil {
		t.Fatalf("AffinityCpus() on irq %d failed: %v", num, err)
	}
	return cpus.String()
}

func TestInterruptsReadOnce(t *testing.T) {
	fake := setupCache(t)

	for i := range 5 {
		irqs, err := Interrupts()
		if err != nil {
			t.Fatalf("round %d: Interrupts() failed: %v", i, err)
		}
		if len(irqs) != 3 {
			t.Fatalf("round %d: expected 3 interrupts, got %d", i, len(irqs))
		}
		for n, num := range []int{1, 9, 16} {
			if irqs[n].Num() != num {
				t.Fatalf("round %d: interrupt %d is %d, expected %d", i, n, irqs[n].Num(), num)
			}
		}
	}

	if got := fake.readCount("/proc/interrupts"); got != 1 {
		t.Errorf("expected 1 read of /proc/interrupts, got %d", got)
	}
}

func TestAllowedPatternsCalculatedPerCall(t *testing.T) {
	fake := setupCache(t)

	first, err := Interrupts()
	if err != nil {
		t.Fatalf("Interrupts() failed: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("expected 3 interrupts, got %d", len(first))
	}

	if _, err := SetAllowedInterrupts([]string{"*i8042*"}); err != nil {
		t.Fatalf("SetAllowedInterrupts() failed: %v", err)
	}

	allowedIrqs, err := Interrupts()
	if err != nil {
		t.Fatalf("Interrupts() failed: %v", err)
	}
	if len(allowedIrqs) != 1 || allowedIrqs[0].Num() != 1 {
		t.Errorf("expected only irq 1 to be allowed, got %v", allowedIrqs)
	}

	denied := map[int]bool{}
	if err := ForEachInterrupt(func(irq *Irq) error {
		denied[irq.Num()] = !irq.isAllowed()
		return nil
	}); err != nil {
		t.Fatalf("ForEachInterrupt() failed: %v", err)
	}
	if denied[1] || !denied[9] || !denied[16] {
		t.Errorf("unexpected denied flags: %v", denied)
	}

	if _, err := SetAllowedInterrupts(nil); err != nil {
		t.Fatalf("SetAllowedInterrupts() failed: %v", err)
	}

	last, err := Interrupts()
	if err != nil {
		t.Fatalf("Interrupts() failed: %v", err)
	}
	if len(last) != 3 {
		t.Errorf("expected 3 interrupts, got %d", len(last))
	}

	// Interrupts must never be shared between calls: callers would see
	// each other's denied flags.
	if first[0] == last[0] {
		t.Errorf("Interrupts() returned the same *Irq on two calls")
	}

	if got := fake.readCount("/proc/interrupts"); got != 1 {
		t.Errorf("expected 1 read of /proc/interrupts, got %d", got)
	}
}

func TestAffinityReadOnceAndBlockedWrite(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(42)
	fake.set(path, "0-3\n")

	if got := getAffinity(t, 42); got != "0-3" {
		t.Errorf("AffinityCpus() = %q, want 0-3", got)
	}
	if got := getAffinity(t, 42); got != "0-3" {
		t.Errorf("AffinityCpus() = %q, want 0-3", got)
	}
	if got := fake.readCount(path); got != 1 {
		t.Errorf("expected 1 read of %s, got %d", path, got)
	}

	BlockWrites()
	setAffinity(t, 42, "1,3")

	// The affinity set is visible immediately, without reading procfs,
	// even though procfs still has the old value.
	if got := getAffinity(t, 42); got != "1,3" {
		t.Errorf("AffinityCpus() after set = %q, want 1,3", got)
	}
	if got := fake.readCount(path); got != 1 {
		t.Errorf("expected 1 read of %s, got %d", path, got)
	}
	if got := fake.get(path); got != "0-3\n" {
		t.Errorf("procfs content = %q, want it unwritten yet", got)
	}

	UnblockWrites()
	if got := fake.get(path); got != "1,3" {
		t.Errorf("procfs content after unblocking = %q, want 1,3", got)
	}
	if got := fake.writeCount(path); got != 1 {
		t.Errorf("expected 1 write of %s, got %d", path, got)
	}
}

func TestBlockedWritesCoalesced(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(42)
	fake.set(path, "0-3")

	BlockWrites()
	for _, cpus := range []string{"0", "1", "2", "0-1", "3"} {
		setAffinity(t, 42, cpus)
	}
	if got := fake.writeCount(path); got != 0 {
		t.Errorf("expected no writes while blocked, got %d", got)
	}

	UnblockWrites()
	if got := fake.writeCount(path); got != 1 {
		t.Errorf("expected 1 write of %s, got %d", path, got)
	}
	if got := fake.get(path); got != "3" {
		t.Errorf("procfs content = %q, want the last affinity 3", got)
	}
}

func TestNestedWriteBlocksWriteAtLastUnblock(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(42)
	fake.set(path, "0-3")

	BlockWrites()
	withWritesBlocked(func() {
		withWritesBlocked(func() {
			setAffinity(t, 42, "0")
		})
		if got := writeBlock(); got != 2 {
			t.Errorf("write block counter = %d, want 2", got)
		}
		if got := fake.writeCount(path); got != 0 {
			t.Errorf("expected no writes at write block 2, got %d", got)
		}
		setAffinity(t, 42, "1")
	})
	if got := fake.writeCount(path); got != 0 {
		t.Errorf("expected no writes at write block 1, got %d", got)
	}

	UnblockWrites()
	if got := writeBlock(); got != 0 {
		t.Errorf("write block counter = %d, want 0", got)
	}
	if got := fake.writeCount(path); got != 1 {
		t.Errorf("expected 1 write of %s, got %d", path, got)
	}
	if got := fake.get(path); got != "1" {
		t.Errorf("procfs content = %q, want the last affinity 1", got)
	}
}

func TestUnblockedWriteIsImmediate(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(42)
	fake.set(path, "0-3")

	setAffinity(t, 42, "1,3")
	if got := fake.get(path); got != "1,3" {
		t.Errorf("procfs content = %q, want 1,3 written immediately", got)
	}

	// Write errors are returned when writes are not blocked.
	fake.failWrites(errors.New("test write error"))
	if err := (&Irq{num: 42}).SetAffinityCpus(cpuset.MustParse("2")); err == nil {
		t.Errorf("SetAffinityCpus() succeeded despite a failing write")
	}
}

func TestUnbalancedUnblockWrites(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(42)
	fake.set(path, "0-3")

	UnblockWrites()
	if got := writeBlock(); got != 0 {
		t.Errorf("write block counter = %d, want 0", got)
	}

	// Blocking must still work after an unbalanced unblock.
	withWritesBlocked(func() {
		setAffinity(t, 42, "1")
		if got := fake.writeCount(path); got != 0 {
			t.Errorf("expected no writes while blocked, got %d", got)
		}
	})
	if got := fake.get(path); got != "1" {
		t.Errorf("procfs content = %q, want 1", got)
	}
}

func TestUnchangedAffinityNotWritten(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(42)
	fake.set(path, "0-3")

	if got := getAffinity(t, 42); got != "0-3" {
		t.Fatalf("AffinityCpus() = %q, want 0-3", got)
	}

	withWritesBlocked(func() {
		setAffinity(t, 42, "0-3")
	})
	if got := fake.writeCount(path); got != 0 {
		t.Errorf("expected no writes of %s, got %d", path, got)
	}
}

func TestFailedWriteForgottenAndRetried(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(42)
	fake.set(path, "0-3")
	fake.failWrites(errors.New("test write error"))

	if got := getAffinity(t, 42); got != "0-3" {
		t.Fatalf("AffinityCpus() = %q, want 0-3", got)
	}
	if err := (&Irq{num: 42}).SetAffinityCpus(cpuset.MustParse("1,3")); err == nil {
		t.Errorf("SetAffinityCpus() succeeded despite a failing write")
	}
	if got := fake.writeCount(path); got != 1 {
		t.Errorf("expected 1 write attempt of %s, got %d", path, got)
	}

	// The failed affinity must be forgotten: reads fall back to procfs
	// and the next update retries the write.
	if got := getAffinity(t, 42); got != "0-3" {
		t.Errorf("AffinityCpus() after a failed write = %q, want 0-3 from procfs", got)
	}
	if got := fake.readCount(path); got != 2 {
		t.Errorf("expected 2 reads of %s, got %d", path, got)
	}

	fake.failWrites(nil)
	setAffinity(t, 42, "1,3")
	if got := fake.get(path); got != "1,3" {
		t.Errorf("procfs content = %q, want 1,3", got)
	}
}

func TestReadOnlyAffinityWrittenOnce(t *testing.T) {
	fake := setupCache(t)
	unread, read := affinityPath(42), affinityPath(9)
	fake.set(unread, "0-3")
	fake.set(read, "0-1")
	fake.failWrites(syscall.EPERM)

	// One affinity is already known when its write fails, the other is
	// not. Both must end up reporting what procfs has.
	if got := getAffinity(t, 9); got != "0-1" {
		t.Fatalf("AffinityCpus() = %q, want 0-1", got)
	}
	withWritesBlocked(func() {
		setAffinity(t, 42, "1,3")
		setAffinity(t, 9, "2")
	})
	if got := getAffinity(t, 42); got != "0-3" {
		t.Errorf("AffinityCpus() = %q, want 0-3 from procfs", got)
	}
	if got := getAffinity(t, 9); got != "0-1" {
		t.Errorf("AffinityCpus() = %q, want 0-1 from procfs", got)
	}

	// Read-only affinities are not written again.
	withWritesBlocked(func() {
		for _, cpus := range []string{"1", "2", "0-3"} {
			setAffinity(t, 42, cpus)
			setAffinity(t, 9, cpus)
		}
	})
	for _, path := range []string{unread, read} {
		if got := fake.writeCount(path); got != 1 {
			t.Errorf("expected 1 write attempt of %s, got %d", path, got)
		}
	}
}

func TestSetProcRootInvalidates(t *testing.T) {
	fake := setupCache(t)
	fake.set(affinityPath(1), "0-3")

	if _, err := Interrupts(); err != nil {
		t.Fatalf("Interrupts() failed: %v", err)
	}
	if got := getAffinity(t, 1); got != "0-3" {
		t.Fatalf("AffinityCpus() = %q, want 0-3", got)
	}

	fake.set("/host/proc/interrupts", strings.ReplaceAll(testInterrupts, "acpi", "hostacpi"))
	fake.set(smpAffinityListPath("/host/proc", 1), "2,3")

	SetProcRoot("/host")

	irqs, err := Interrupts()
	if err != nil {
		t.Fatalf("Interrupts() failed: %v", err)
	}
	found := false
	for _, irq := range irqs {
		if strings.Contains(irq.Description(), "hostacpi") {
			found = true
		}
	}
	if !found {
		t.Errorf("interrupts not re-read after SetProcRoot: %v", irqs)
	}
	if got := getAffinity(t, 1); got != "2-3" {
		t.Errorf("AffinityCpus() after SetProcRoot = %q, want 2-3", got)
	}
}

func TestSetProcRootDropsBufferedWrites(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(42)
	fake.set(path, "0-3")

	BlockWrites()
	setAffinity(t, 42, "1,3")
	SetProcRoot("/host")
	UnblockWrites()

	if got := fake.totalWrites(); got != 0 {
		t.Errorf("expected buffered writes to be dropped, got %d writes", got)
	}
	if got := fake.get(path); got != "0-3" {
		t.Errorf("procfs content = %q, want the untouched 0-3", got)
	}
}

func TestDropCache(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(1)
	fake.set(path, "0-3")

	if _, err := Interrupts(); err != nil {
		t.Fatalf("Interrupts() failed: %v", err)
	}
	if got := getAffinity(t, 1); got != "0-3" {
		t.Fatalf("AffinityCpus() = %q, want 0-3", got)
	}

	BlockWrites()
	setAffinity(t, 1, "1")
	DropCache()
	UnblockWrites()

	if got := fake.totalWrites(); got != 0 {
		t.Errorf("expected buffered writes to be dropped, got %d writes", got)
	}
	if _, err := Interrupts(); err != nil {
		t.Fatalf("Interrupts() failed: %v", err)
	}
	if got := getAffinity(t, 1); got != "0-3" {
		t.Errorf("AffinityCpus() = %q, want 0-3 read again from procfs", got)
	}
	if got := fake.readCount("/proc/interrupts"); got != 2 {
		t.Errorf("expected 2 reads of /proc/interrupts, got %d", got)
	}
	if got := fake.readCount(path); got != 2 {
		t.Errorf("expected 2 reads of %s, got %d", path, got)
	}
}

func TestCallbackReentrancy(t *testing.T) {
	fake := setupCache(t)
	for _, num := range []int{1, 9, 16} {
		fake.set(affinityPath(num), "0-3")
	}

	// A callback which reads and writes affinities must not deadlock.
	withWritesBlocked(func() {
		err := ForEachInterrupt(func(irq *Irq) error {
			cpus, err := irq.AffinityCpus()
			if err != nil {
				return err
			}
			return irq.SetAffinityCpus(cpus.Union(cpuset.MustParse("4")))
		})
		if err != nil {
			t.Fatalf("ForEachInterrupt() failed: %v", err)
		}
	})

	for _, num := range []int{1, 9, 16} {
		if got := fake.get(affinityPath(num)); got != "0-4" {
			t.Errorf("irq %d procfs content = %q, want 0-4", num, got)
		}
	}
}

func TestAffinityErrorsNeverWrite(t *testing.T) {
	fake := setupCache(t)
	fake.set(affinityPath(42), "0-3")

	if err := (&Irq{num: 42}).SetAffinityCpus(cpuset.New()); err == nil {
		t.Errorf("SetAffinityCpus(empty) should fail")
	}

	err := (&Irq{num: 42, denied: true}).SetAffinityCpus(cpuset.MustParse("1"))
	if !errors.Is(err, ErrDeniedInterrupt) {
		t.Errorf("SetAffinityCpus() on a denied irq: %v, want %v", err, ErrDeniedInterrupt)
	}

	if got := fake.totalWrites(); got != 0 {
		t.Errorf("expected no writes, got %d", got)
	}
}

func TestFailedReadNotCached(t *testing.T) {
	fake := setupCache(t)

	fake.mu.Lock()
	delete(fake.content, "/proc/interrupts")
	fake.mu.Unlock()

	if _, err := Interrupts(); err == nil {
		t.Errorf("Interrupts() succeeded without /proc/interrupts")
	}
	if _, err := (&Irq{num: 42}).AffinityCpus(); err == nil {
		t.Errorf("AffinityCpus() succeeded without smp_affinity_list")
	}

	fake.set("/proc/interrupts", testInterrupts)
	fake.set(affinityPath(42), "0-3")

	if irqs, err := Interrupts(); err != nil || len(irqs) != 3 {
		t.Errorf("Interrupts() = %v, %v, want 3 interrupts", irqs, err)
	}
	if got := getAffinity(t, 42); got != "0-3" {
		t.Errorf("AffinityCpus() = %q, want 0-3", got)
	}
}

func TestConcurrentInterruptReaders(t *testing.T) {
	fake := setupCache(t)
	path := affinityPath(1)
	fake.set(path, "0-3")

	// Interrupts are read while validating a configuration, which
	// happens in the agent goroutine, in parallel with a policy which
	// sets affinities.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if _, err := ValidateAllowedReferencedInterrupts(
					[]string{"*acpi*"}, []string{"*i8042*"},
				); err == nil {
					t.Error("ValidateAllowedReferencedInterrupts() accepted a denied interrupt")
					return
				}
			}
		})
	}

	for pass := range 50 {
		withWritesBlocked(func() {
			setAffinity(t, 1, fmt.Sprintf("%d", pass%4))
		})
	}
	wg.Wait()

	if got := fake.readCount("/proc/interrupts"); got != 1 {
		t.Errorf("expected 1 read of /proc/interrupts, got %d", got)
	}
	if got := fake.get(path); got != "1" {
		t.Errorf("procfs content = %q, want the last affinity 1", got)
	}
}

func TestReadWriteAmplification(t *testing.T) {
	var (
		irqCount  = 300
		passCount = 101
		fake      = setupCache(t)
		sb        = strings.Builder{}
	)

	sb.WriteString("           CPU0       CPU1\n")
	for num := range irqCount {
		fmt.Fprintf(&sb, "%3d:          0          0  IR-IO-APIC  %d-edge  dev%d\n", num, num, num)
		fake.set(affinityPath(num), "0-3")
	}
	fake.set("/proc/interrupts", sb.String())

	// Mimic a policy synchronization: many nested passes over all
	// interrupts, reading and writing the affinity of each.
	BlockWrites()
	for pass := range passCount {
		withWritesBlocked(func() {
			irqs, err := Interrupts()
			if err != nil {
				t.Fatalf("pass %d: Interrupts() failed: %v", pass, err)
			}
			if len(irqs) != irqCount {
				t.Fatalf("pass %d: expected %d interrupts, got %d", pass, irqCount, len(irqs))
			}
			// Every pass sets a different affinity, just like a policy
			// which resizes its CPU pools while allocating containers.
			newCpus := cpuset.MustParse(fmt.Sprintf("%d", pass%4))
			for _, irq := range irqs {
				if _, err := irq.AffinityCpus(); err != nil {
					t.Fatalf("pass %d: AffinityCpus() failed: %v", pass, err)
				}
				if err := irq.SetAffinityCpus(newCpus); err != nil {
					t.Fatalf("pass %d: SetAffinityCpus() failed: %v", pass, err)
				}
			}
		})
	}

	if got := fake.readCount("/proc/interrupts"); got != 1 {
		t.Errorf("expected 1 read of /proc/interrupts, got %d", got)
	}
	if got, want := fake.totalReads(), 1+irqCount; got != want {
		t.Errorf("expected %d reads in total, got %d", want, got)
	}
	if got := fake.totalWrites(); got != 0 {
		t.Errorf("expected no writes while blocked, got %d", got)
	}

	UnblockWrites()
	if got := fake.totalWrites(); got != irqCount {
		t.Errorf("expected %d writes, got %d", irqCount, got)
	}
}
