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

// Package leases is the lease broker of the CXL memory driver: the lease
// claims of the pods (claims on the lease device of cxl.nri.io), their
// sockets and the protocol of the cxl-request CLI on them, the leases that
// running containers ask for there, the companion claims and pods that
// hold the memory of the leases, and the reconcile that releases what is
// left over.
//
// Objects: Lease (lease.go) and LeaseClaim (leaseclaim.go), whose records
// and transition tables are state.Lease and state.LeaseClaim; the Broker
// (broker.go) that owns them; the companion (companion.go) with the hooks
// that the drivers call while they prepare it; the identity of the
// processes that connect to the sockets (identity.go). The states and their transitions are
// rendered in docs/memory/cxl/states.md.
package leases
