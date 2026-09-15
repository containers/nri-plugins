// Copyright 2020 Intel Corporation. All Rights Reserved.
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

// Package sysfs discovers the CPU and memory topology of a machine, and the
// details of its caches, CPU frequencies and Intel Speed Select state.
//
// Deprecated: use [github.com/containers/nri-plugins/pkg/lib/hardware] instead.
// Nothing in this repository uses this package any more, and it will be removed
// in a later release. It is still here so that anything outside the repository
// has a release in which to move over.
//
// # Moving over
//
// [github.com/containers/nri-plugins/pkg/lib/hardware] is not a renaming of this
// package. It describes the same hardware with a smaller interface, and the
// differences are deliberate:
//
//   - A machine is discovered once and does not change. There is no Discover on
//     an existing one, and no discovery flags: everything is read up front.
//   - Lookups return concrete handles which are never nil. A CPU or a node the
//     machine does not have reports Valid() == false rather than being a nil
//     behind a non-nil interface.
//   - Packages, dies, clusters, cores and caches are all zones, addressed by
//     their full coordinates, since the kernel numbers dies and cores within
//     their package. hardware.TopologyIndex is the lookup table for those.
//   - CPU sets are [github.com/containers/nri-plugins/pkg/lib/cpu] masks rather
//     than k8s.io/utils/cpuset sets, and the ones a machine hands out are sealed.
//   - Discovery reads through an io/fs.FS rooted at the host root, which is what
//     a test passes a recorded or synthetic tree through, instead of a package
//     global sys root.
//   - Intel Speed Select is not there. It is a property of the running platform
//     rather than of its shape, and the code which wanted it now probes for
//     itself.
//
// [github.com/containers/nri-plugins/pkg/lib/hardware/system] reimplements this
// package's interface on top of that one, and is what the tree used while its
// consumers were moved over one at a time. A caller which wants the migration in
// two steps rather than one can do the same, but it is going away with this
// package rather than outliving it.
//
// ParseFileEntries and GetMemoryCapacity have moved and are only forwarded from
// here: they are [github.com/containers/nri-plugins/pkg/utils/parse].FileEntries
// and [github.com/containers/nri-plugins/pkg/utils].GetMemoryCapacity now, and
// neither has anything to do with topology.
package sysfs
