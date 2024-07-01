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

// Package libmem implements a simple memory accounting and allocation
// scheme for resource management policy plugins. The primary interface
// to libmem is the Allocator type.
//
// # Allocator, Nodes
//
// Allocator is set up with one or more nodes. A node usually corresponds
// to an actual NUMA node in the system. It has some amount of associated
// memory. That memory has some known memory type, ordinary DRAM memory,
// high-capacity non-volatile/persistent PMEM memory, or high-bandwidth
// HBM memory. A node comes with a vector of distances which describe the
// cost of moving memory from the node to other nodes. A node also might
// have an associated set of CPU cores which are considered to be close
// topologically to the node's memory.
//
// # Memory Zones, Allocation Requests
//
// Allocator divides memory into zones. A memory zone is simply a set of
// one or more memory nodes. Memory is allocated using requests. Each
// request has an affinity and a type preference. Affinity states which
// zone the allocated memory should be close to and in the simplest case
// it is the set of nodes from which memory is allocated from. The type
// preference indicates which type or types of memory should be allocated.
// A type preference can be strict, in which case failing to satisfy the
// preference results in a failed allocation. The request also has an
// inertia. This tells the allocator how eagerly it can / easily it
// should move the allocation to other memory zones later, if some zone
// runs out of memory due to subsequent allocations.
//
// # Allocation Algorithm, Initial Zone Selection
//
// Allocation starts by finding an initial zone for the request. This
// zone is the closest zone that satisfies the type preferences of the
// request and has at least one node with normal memory (as opposed to
// movable memory). Note that for non-strict requests this zone might not
// have all the preferred types.
//
// # Allocation Algorithm, Overflow Handling
//
// Once the initial zone is found, Allocator checks if any memory zone
// is oversubscribed or IOW is in the danger of running out of memory.
// A zone is considered oversubscribed if the total size of allocations
// that fit into the zone exceeds the total amount of memory available
// in the nodes of the zone. An allocation is said to fit into a zone if
// it is assigned to (IOW satisfied using) the zone, or another zone with
// a subset of the nodes of the first zone. For instance, allocations with
// assigned zones {0}, {2}, and {0, 2}, fully fit into {0, 1, 2, 3}, while
// zones {0, 4}, {2, 5}, or {0, 2, 4, 5} do not.
//
// If any zone is oversubscribed, an overflow handling algorithm kicks in
// to reduce memory usage in overflowing nodes. Memory usage is reduced by
// moving allocations from the zone to a new one with a superset of nodes
// of the original zone. An expansion algorithm using node affinity, types
// and distance vectors is used to determine the superset zone. Overflow
// handling prefers moving allocations with lower inertia first. Allocation
// fails if the overflow handler cannot resolve all overflows.
//
// # Customizing an Allocator
//
// Allocator can be customized in multiple ways. The simplest but most
// limited is to set up the Allocator with a curated set of node distance
// vectors. Since node expansion looks at the distance vectors to decide
// how to expand a zone, by altering the distance vector one can change the
// the order and set of new nodes considered during zone expansion.
//
// Another more involved but direct and more flexible way to customize an
// Allocator is to explicitly set it up with custom functions for node
// expansion and/or overflow resolution. These custom functions are called
// whenever node expansion is necessary (initial zone selection, overflow
// resolution), or when oversubscription has been detected. These custom
// functions get access to the built-in default implementations. Often
// this allows implementing exception style extra handling just for cases
// of special interest but otherwise rely on the default implementations.
//
// # Allocation Offers
//
// It is sometimes necessary or desirable to compare multiple allocation
// alternatives, especially ones with different memory affinity, before
// making a final allocation decision. Often this decision is based on how
// well a possible allocation aligns with resources from other domains,
// for instance with CPU cores, GPUs, or other devices.
//
// libmem provides memory offers for this. An offer captures all details
// necessary to make a memory allocation without actually updating the
// Allocators state with those details. Multiple parallel offers can be
// queried at any time. An offer, but only a single offer, can then be
// turned into an allocation by committing it, once the best allocation
// alternative has been determined.
package libmem
