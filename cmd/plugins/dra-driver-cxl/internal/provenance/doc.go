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

// Package provenance knows which memdevs of the node are devices of a
// memory pool: the Provenance record of each serial (UNKNOWN, HELD, POOL,
// LOCAL) and the Watcher that lists the pool driver's ResourceSlices and
// the pool claims of the node. Its consumers are the publisher of the
// node's ResourceSlices, which never publishes a pool device or a held
// memdev as a local one, and the device engine's check before a local
// grant (CheckLocal, with a fresh list). The transition table of
// Provenance.State is in internal/state and docs/memory/cxl/states.md.
package provenance
