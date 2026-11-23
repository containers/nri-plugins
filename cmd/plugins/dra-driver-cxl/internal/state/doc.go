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

// Package state has the records that the CXL DRA driver remembers, one
// file per record type: Preparation, Grant (with its Bind), Memdev, Region
// (with its DaxDevices and Mounts), Provenance, Lease, LeaseClaim, Steered,
// ListedPool and Probe (the address line probe of a local memdev, in a file
// of its own). Every State field is an enum with its transition table as
// data next to the type; fsm.Apply refuses a transition that is not in
// the table. The tables and the record headers are rendered into
// docs/memory/cxl/states.md by go generate.
package state
