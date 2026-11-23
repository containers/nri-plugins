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

// Command statedoc renders the transition tables and the record headers of
// the CXL driver's internal/state and internal/containers packages, and
// those of the kernel objects of pkg/cxl/kernel, into
// docs/memory/cxl/states.md: one section per object with its purpose,
// identity, states, transition table, invariants, persistence, and locks
// and waits. It runs by go generate in cmd/plugins/dra-driver-cxl.
package main
