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

// Package dra is the DRA side of the driver, its kubelet plugins: one Plugin
// type with two instances, cxl.nri.io (the local memdevs, regions, DRAM and
// the lease device of the node; it publishes the node's ResourceSlices) and
// the pool driver, that differ only in their SourceRules. A claim is one
// Preparation record per driver (internal/state) with a result per allocation
// result: a grant on a memdev that the device engine realises, a region, the
// DRAM, or the lease claim. The package has the companion gate of lease
// claims, the CDI spec of each driver built from the records, and the
// publisher of the node's ResourceSlices. The transition table of
// Preparation.State is in docs/memory/cxl/states.md.
package dra
