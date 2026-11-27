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

// Package containers is the NRI plugin of the CXL driver: the record of
// each container of the node (Container), how the plugin manages its
// memory (steered by the steering package, fenced, or left alone), the fenced NUMA
// nodes and the lends of NUMA nodes of CXL memory to running containers.
// It is the one writer of the cpuset.mems of the containers that it does
// not steer: one computation (wantMems) and one writer (applyMems) under the
// update token. The Plugin implements the NRI events, the fence of the
// device engine (Fence) and the lender of the lease broker and of the
// companion claims (LendNUMANode, RevokeNUMANode).
//
// The table of the containers is in memory; the Steered records and the
// leases that seed the lends are in the store. The transitions of
// Container.Lifecycle and Container.Management are the tables of
// container.go, rendered into docs/memory/cxl/states.md.
package containers
