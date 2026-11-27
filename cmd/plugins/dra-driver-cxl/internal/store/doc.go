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

// Package store keeps the one persistent document of the CXL DRA driver,
// state.json in the plugin data directory of cxl.nri.io: the records of
// package state (Preparation, Grant, Memdev, Region, Provenance,
// ListedPool, Lease, LeaseClaim, Steered), their states documented in
// docs/memory/cxl/states.md.
//
// A commit is a pure function of the document applied to a copy; one
// writer goroutine applies the pending commits in groups and writes each
// group once (a synced temporary file, the current file linked to
// state.json.prev, a rename, a synced directory) before readers see it.
// Durable commits wait for that write, lazy ones only for their
// application. A commit whose context ends before the writer takes it is
// withdrawn. A taken commit waits for its outcome. So a failed commit
// means that the change did not happen. A failed write discards the
// group: the snapshot is never ahead of the disk. A failed sync of the
// directory is the one case where the disk may be ahead: the discarded
// group may be visible in state.json, and the next write links it to
// state.json.prev with its generation; this is harmless because a record
// is committed before the kernel objects it names are made.
//
// The document is decoded strictly: an unknown field, another schema, data
// after the document or a state that a stored record may not have make it
// undecodable, so a record field added by a later build makes an older
// build start LOST: drain the node's claims before a rollback. When neither
// state.json nor state.json.prev decodes the store is LOST and refuses
// every commit. At a load of another boot the records are changed as a
// reboot requires.
package store
