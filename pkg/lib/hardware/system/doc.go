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

// Package system is pkg/sysfs reimplemented on top of
// [github.com/containers/nri-plugins/pkg/lib/hardware].
//
// Everything pkg/sysfs exports is exported here with the same name and the same
// signature, so a consumer moves over by changing one import line. Most already
// import pkg/sysfs aliased to "system", which is why this package is called
// that: for those files even the alias stays as it was.
//
// # Why it exists
//
// It is a migration step, and a proof. pkg/sysfs stays in the tree beside it, so
// both implementations are in one build and equivalence_test.go can run every
// method of both against the same recorded sysfs trees and compare the answers.
// That is a much stronger statement than a rewritten pkg/sysfs could make, where
// the only reference left would be in git history.
//
// It also splits the work into reviewable pieces. This package plus the hardware
// package underneath it change no behaviour and no caller, so they can land on
// their own. Moving consumers off pkg/sysfs, and deleting it, comes after.
//
// Nothing new should be built on this package. New code should use
// [github.com/containers/nri-plugins/pkg/lib/hardware] directly; this is
// here to be deleted.
//
// # Where it is faithful, and where it cannot be
//
// The intent is bug-for-bug compatibility, including the parts of pkg/sysfs
// which are odd. Some of it is worth calling out.
//
// Cache is an exported struct with unexported fields, and at least one caller
// uses a *Cache as a map key, relying on pkg/sysfs handing out one pointer per
// cache. This package interns its *Cache values the same way, so pointer
// identity keeps working.
//
// MemoryInfo reads the machine on every call in pkg/sysfs. It does the same
// here, rather than answering from hardware's cached capacity, because callers
// use it to read current usage.
//
// A NUMA node with no CPUs of its own reports package 0 and die 0, which is what
// pkg/sysfs reports -- not because such a node is in package 0 but because that
// is the zero value of a field it never assigns. hardware.MemoryNode says -1,
// which is honest; this says 0, which is compatible. It is the last thing here
// which copies a pkg/sysfs quirk rather than an intent.
//
// Sst, SstInfo and SstClos expose Intel Speed Select through goresctrl types.
// The hardware package deliberately has nothing to do with SST, so the
// discovery for those three lives here. It is the last thing to move, and it
// moves to whoever ends up wanting it -- today only pkg/cpuallocator does.
//
// SetCpusOnline, SetCPUFrequencyLimits and CPU.SetFrequencyLimits write to
// sysfs. The hardware package only reads, so these are implemented here over
// [hardware.WriterFS], obtained from [hardware.Machine.FS] so that a write goes
// to the tree the topology was read from. They have no callers left in the tree;
// they are here because the interface has them.
//
// Discover re-runs discovery on an existing System and mutates it in place.
// Nothing calls it with anything the constructors did not already ask for.
//
// The DiscoveryFlag values are accepted and ignored, exactly as pkg/sysfs
// ignores them: DiscoverSystem discards its arguments before passing them on,
// and the flags inside Discover all take the same path anyway.
//
// SetSysRoot keeps its package-global behaviour, so that the one caller in
// pkg/resmgr does not have to change. The global is read when a System is
// constructed and not consulted afterwards.
//
// # What it costs
//
// Every set crosses a representation boundary: hardware works in
// [libcpu.CpuMask], these interfaces in cpuset.CPUSet and idset.IDSet, so each
// call which returns a set converts one. That is fine for a layer whose purpose
// is to be removed, and it is a reason not to leave it in place longer than
// necessary.
package system
