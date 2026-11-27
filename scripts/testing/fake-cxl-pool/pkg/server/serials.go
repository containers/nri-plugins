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

package server

import (
	"fmt"
	"sync"
)

// serials allocates serial numbers: shared devices from sharedBase, the
// others from base. Uniqueness is checked over all serials.
type serials struct {
	mu         sync.Mutex
	sharedBase uint64
	base       uint64
	used       map[uint64]string // serial -> device
}

// newSerials returns a serial allocator with the bases of shared devices
// and of the others.
func newSerials(sharedBase, base uint64) *serials {
	return &serials{sharedBase: sharedBase, base: base, used: map[uint64]string{}}
}

// Use marks a serial used by a device. It fails if another device has it.
func (s *serials) Use(serial uint64, device string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.used[serial]; ok && d != device {
		return fmt.Errorf("serial 0x%x is already used by device %q", serial, d)
	}
	s.used[serial] = device
	return nil
}

// Free frees a serial.
func (s *serials) Free(serial uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.used, serial)
}

// Next allocates the lowest free serial above the base of the device kind
// (shared or unshared) for a device.
func (s *serials) Next(device string, shared bool) uint64 {
	return s.NextExcept(device, shared, nil)
}

// NextExcept is Next that also skips the serials for which skip is true.
func (s *serials) NextExcept(device string, shared bool, skip func(uint64) bool) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.base
	if shared {
		base = s.sharedBase
	}
	for sn := base + 1; ; sn++ {
		if _, ok := s.used[sn]; !ok && (skip == nil || !skip(sn)) {
			s.used[sn] = device
			return sn
		}
	}
}

// Owner returns the device that uses the serial.
func (s *serials) Owner(serial uint64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.used[serial]
	return d, ok
}
