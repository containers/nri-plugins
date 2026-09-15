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

// Package hardware discovers the CPU and memory topology of a machine.
//
// [Discover] reads the topology once and returns an immutable [Machine].
// Nothing in a Machine changes afterwards, so a single instance can be
// discovered at startup and shared by everything that needs it.
//
// # Zones
//
// A [Zone] is a set of CPUs which share a piece of hardware: a package, a die,
// a cluster, a NUMA node, a cache, a core. Each is tagged with the [Level] it
// represents, and [Machine.Zones] returns all of them at one level: all the
// packages, or all the L3 caches. [Machine.Levels] says which levels a machine
// has at all.
//
// The zones are flat, not a tree. Which one contains which is a property of the
// hardware rather than of the [Level] constants, so containment is a query:
// [ZoneOf] for the zone at a level holding a set of CPUs, [ZonesWithin] and
// [ZonesOverlapping] for the zones inside or straddling one. A zone which needs
// naming rather than searching for is addressed by its coordinates, since the
// kernel numbers dies and cores within their package; [TopologyIndex] is the
// lookup table for those.
//
// This is deliberately one abstraction rather than an accessor family per
// level. Adding a level costs a constant, not a new set of methods, and a
// consumer which wants the topology as a hierarchy builds one over the levels
// it cares about, instead of walking one this package guessed at.
//
// # Handles
//
// [CPU], [MemoryNode], [Cache] and [Zone] are handles into the Machine which
// returned them. They are never nil, and every method on them is safe to call
// on a handle for something which does not exist; such a handle reports
// Valid() == false and otherwise answers with zero values. Looking up a CPU
// which is not present is therefore not an error to be checked at every call
// site, only where it matters.
//
// There is one handle per piece of hardware, so asking a Machine twice for the
// same CPU, node, cache or zone gives back the same pointer. Handles are
// therefore comparable, and usable as map keys: code which builds a structure
// of its own over the zones can key that structure by *Zone.
//
// # Sets
//
// CPU sets are [libcpu.CpuMask]. Every set a Machine hands out is sealed, so it
// is safe to read from several goroutines and will panic if modified: Clone it
// first if you need to change one. Sets of ids which are not CPUs -- packages,
// NUMA nodes, caches -- are plain sorted []ID.
//
// Functions here take sets as the [libcpu.CPUSet] interface, so that either
// implementation can be passed in, and return the concrete [libcpu.CpuMask]
// they built.
//
// # Filesystem
//
// Discovery reads through an [fs.FS] rooted at the host root, so
// "sys/devices/system/cpu/online" and "proc/meminfo" are both reachable. By
// default that is the real filesystem at "/"; [WithRoot] points it elsewhere,
// for a host filesystem mounted inside a container, and [WithFS] substitutes
// any fs.FS, which is how the tests run against recorded topologies and
// synthetic ones.
//
// # Scope
//
// This package reads topology. Discovery never writes anything, and it does not
// cover Intel Speed Select, cpufreq control, or uncore frequency: those are
// separate concerns with their own state and their own failure modes.
//
// The one concession to writing is [WriterFS]: a caller which reads a topology
// through an fs.FS and then wants to write to the same tree can supply one, and
// nothing here requires it.
package hardware
