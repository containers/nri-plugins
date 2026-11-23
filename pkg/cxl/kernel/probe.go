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

package kernel

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"sync/atomic"
	"unsafe"
)

// ErrProbeUnsupported is returned by ProbeAliasing on architectures where
// cache lines cannot be flushed.
var ErrProbeUnsupported = errors.New("address aliasing probe is not supported on this architecture")

// Aliasing is the result of an address line probe of memory.
type Aliasing struct {
	// Offset is the smallest power of two P for which offsets X and X+P
	// reach the same memory, 0 if no aliasing was found. The memory
	// holds Offset distinct bytes at most: only [0, Offset) can be used.
	Offset uint64
	// Base is the first offset X where it was seen.
	Base uint64
	// Garbage is the offset of a marker that read back neither as written
	// nor as the marker of an alias, -1 if none.
	Garbage int64
}

const (
	probeMarker = 0x5a17c0ffee000000
	probePage   = 4096
)

// probeBases are the offsets below every power of two that the probe
// writes markers at: the start, an odd page, and a few far from the start.
var probeBases = []uint64{0, 3 * probePage, 5 << 20, 7<<30 + 5*probePage, 33<<30 + 11*probePage}

// ProbeAliasing runs an address line test (memtest86 "walking ones") on
// the memory of a dax device: for every power of two P from 4 KiB up to the
// size of the device and a few base offsets X below it, it writes a marker
// at X, another one at X+P, and reads X back. If X holds the marker of X+P,
// both reach the same memory: the device or the platform ignores the
// address bit log2(P). Every store and load bypasses the CPU caches (cache
// lines are flushed), so that a dirty line of an aliased address cannot
// hide the aliasing. The probe overwrites 16 bytes at a few hundred
// offsets: run it before the memory is in use. A device of size 0 has no
// aliasing. The context ends the wait for the character device.
func (k *Kernel) ProbeAliasing(ctx context.Context, dax string) (*Aliasing, error) {
	size, err := k.DaxDeviceSize(dax)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return &Aliasing{Garbage: -1}, nil
	}
	major, minor, err := k.DaxDevNumbers(dax)
	if err != nil {
		return nil, err
	}
	a, err := k.sys().ProbeDax(ctx, major, minor, size)
	if err != nil && !errors.Is(err, ErrProbeUnsupported) {
		return nil, fmt.Errorf("probing %s: %w", dax, err)
	}
	return a, err
}

// ZeroError is the error of the writes of the zeros to the memory of a dax
// device.
type ZeroError struct {
	Dax string
	Err error
}

// Error returns the error in words.
func (e *ZeroError) Error() string {
	return fmt.Sprintf("zeroing %s: %v", e.Dax, e.Err)
}

// Unwrap returns the error of the writes.
func (e *ZeroError) Unwrap() error {
	return e.Err
}

// Zero writes zeros to all of the memory of a dax device bound to
// device_dax; the error of the writes is a *ZeroError.
func (k *Kernel) Zero(ctx context.Context, dax string) error {
	size, err := k.DaxDeviceSize(dax)
	if err != nil {
		return err
	}
	major, minor, err := k.DaxDevNumbers(dax)
	if err != nil {
		return err
	}
	align := uint64(0)
	if real, err := filepath.EvalSymlinks(k.daxDev(dax)); err == nil {
		// .../regionN/dax_regionN/daxN.M
		align, _ = readUint(filepath.Join(filepath.Dir(real), "dax_region", "align"))
	}
	if err := k.sys().ZeroDax(ctx, major, minor, size, align); err != nil {
		return &ZeroError{Dax: dax, Err: err}
	}
	k.logf("%s: zeroed %d bytes", dax, size)
	return nil
}

// daxNodeDirPrefix is the prefix of the temporary directories of private dax
// device nodes under DevRoot.
const daxNodeDirPrefix = ".cxl-dax-"

// RemoveDaxNodeDirs removes the temporary directories of private dax device
// nodes under DevRoot that a process stopped in a zeroing or a probe left,
// and returns those that it removed.
func (k *Kernel) RemoveDaxNodeDirs() ([]string, error) {
	dirs, err := filepath.Glob(filepath.Join(k.devRoot(), daxNodeDirPrefix+"*"))
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, dir)
	}
	return removed, errors.Join(errs...)
}

// ProbeMemory runs the address line test of ProbeAliasing on mapped memory.
func ProbeMemory(mem []byte) *Aliasing {
	size := uint64(len(mem))
	res := &Aliasing{Garbage: -1}
	word := func(off uint64) *uint64 { return (*uint64)(unsafe.Pointer(&mem[off])) }
	store := func(off, v uint64) {
		atomic.StoreUint64(word(off), v)
		flushLine(uintptr(unsafe.Pointer(&mem[off])))
		memoryFence()
	}
	load := func(off uint64) uint64 {
		flushLine(uintptr(unsafe.Pointer(&mem[off])))
		memoryFence()
		return atomic.LoadUint64(word(off))
	}
	for p := uint64(probePage); p < size; p <<= 1 {
		for _, b := range probeBases {
			if b+p+8 > size || b >= p {
				continue
			}
			store(b, probeMarker|1)
			store(b+p, probeMarker|2)
			switch v := load(b); v {
			case probeMarker | 1:
			case probeMarker | 2:
				if res.Offset == 0 {
					res.Offset, res.Base = p, b
				}
			default:
				if res.Garbage < 0 {
					res.Garbage = int64(b)
				}
			}
		}
		if res.Offset != 0 {
			break
		}
	}
	return res
}

func (a *Aliasing) String() string {
	switch {
	case a == nil:
		return "not probed"
	case a.Offset != 0:
		return fmt.Sprintf("offsets %#x and %#x reach the same memory (address bit %d is ignored): only the first %d bytes are distinct",
			a.Base, a.Base+a.Offset, bits.TrailingZeros64(a.Offset), a.Offset)
	case a.Garbage >= 0:
		return fmt.Sprintf("no aliasing, but offset %#x did not read back what was written", a.Garbage)
	}
	return "no aliasing"
}
