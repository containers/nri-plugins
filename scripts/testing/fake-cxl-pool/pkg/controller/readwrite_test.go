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

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
)

var (
	condAttached = device.AttachedCondition(device.PoolDriverName)
	condFailed   = device.AttachFailedCondition(device.PoolDriverName)
)

// The device sections of read-only and read-write fs claims.
var (
	roFs   = []resourceapi.DeviceAllocationConfiguration{claimConfig(`"device": {"mode": "fs"}`)}
	rwFs   = []resourceapi.DeviceAllocationConfiguration{claimConfig(`"device": {"mode": "fs", "readOnly": false}`)}
	rwExt4 = []resourceapi.DeviceAllocationConfiguration{claimConfig(`"device": {"mode": "fs", "readOnly": false, "fsType": "ext4"}`)}
	rwGfs2 = []resourceapi.DeviceAllocationConfiguration{claimConfig(`"device": {"mode": "fs", "readOnly": false, "fsType": "gfs2"}`)}
)

// newFsEnv returns a test environment whose pool has the shared
// persistent device fs0, published.
func newFsEnv(t *testing.T) *env {
	t.Helper()
	e := newEnvWith(t, func(p *fakePool, _ *Config) {
		p.devices["fs0"] = api.Device{Name: "fs0", Serial: "0xc1af0001", Size: 512 << 20, Shared: true, Persistent: true,
			Scope: api.ScopePool, State: api.DeviceFree}
	})
	e.sync()
	return e
}

// share creates a claim of one share of fs0 on the node with the config.
func (e *env) share(name, nodeName string, config []resourceapi.DeviceAllocationConfiguration) {
	e.t.Helper()
	e.createClaimConfig(name, "fs0", nodeName, "share-"+name, 1, config)
}

// attachedData returns the status data of the Attached condition of a
// claim, nil if it is not Attached.
func (e *env) attachedData(name string) map[string]any {
	e.t.Helper()
	cond, st := e.condition(name, condAttached)
	if cond == nil {
		return nil
	}
	data := map[string]any{}
	if err := json.Unmarshal(st.Data.Raw, &data); err != nil {
		e.t.Fatal(err)
	}
	return data
}

// wantAttached fails unless the claim is Attached without AttachFailed and
// its status data has singleHost as given and coherent true.
func (e *env) wantAttached(name string, singleHost bool) map[string]any {
	e.t.Helper()
	data := e.attachedData(name)
	if data == nil {
		c, _ := e.condition(name, condFailed)
		e.t.Fatalf("claim %s: not Attached (AttachFailed %+v)", name, c)
	}
	if c, _ := e.condition(name, condFailed); c != nil {
		e.t.Fatalf("claim %s: AttachFailed next to Attached: %+v", name, c)
	}
	var want any
	if singleHost {
		want = true
	}
	if data["singleHost"] != want || data["coherent"] != true || data["persistent"] != true {
		e.t.Fatalf("claim %s: data %v, expected singleHost %v, coherent and persistent true", name, data, singleHost)
	}
	return data
}

// wantConflict fails unless the claim has AttachFailed reason Conflict and
// no Attached condition.
func (e *env) wantConflict(name string) {
	e.t.Helper()
	c, _ := e.condition(name, condFailed)
	if c == nil || c.Reason != ReasonConflict || !strings.Contains(c.Message, "single-host") {
		e.t.Fatalf("claim %s: AttachFailed %+v, expected reason Conflict naming the single-host attachment", name, c)
	}
	if c, _ := e.condition(name, condAttached); c != nil {
		e.t.Fatalf("claim %s: Attached next to AttachFailed: %+v", name, c)
	}
}

// exclusiveAttachment returns whether the pool has the attachment of fs0
// to the host, and whether it is exclusive.
func (e *env) exclusiveAttachment(host string) (bool, bool) {
	e.pool.mu.Lock()
	defer e.pool.mu.Unlock()
	a, ok := e.pool.atts[api.AttachmentID("fs0", host)]
	return ok, a.SingleHost
}

// ReadWriteOnce: a read-write share of a single-host filesystem makes the
// attachment of its node exclusive; readers and writers of that node
// share it; claims of other nodes Conflict until the writers are gone.
func TestReadWriteFsExclusive(t *testing.T) {
	t.Parallel()
	e := newFsEnv(t)
	e.share("rw-a", "node1", rwFs)
	e.sync()
	if att, _ := e.pool.calls(); len(att) != 1 || att[0] != "fs0@vm1 k8s:resourceclaim/uid-rw-a exclusive" {
		t.Fatalf("attach requests %v", att)
	}
	if data := e.wantAttached("rw-a", true); data["exclusive"] != nil || data["shared"] != true {
		t.Fatalf("claim rw-a: data %v, expected shared and no exclusive (one share)", data)
	}
	if line := "attach fs0 to vm1 for claim default/rw-a device fs0 node node1: single host for a read-write filesystem claim"; !strings.Contains(e.logs.String(), line) {
		t.Fatalf("log lacks %q", line)
	}

	// a reader and a second writer of the same node (the attachment is
	// exclusive already: no attach request, no log)
	e.share("ro-c", "node1", roFs)
	e.share("rw-b", "node1", rwExt4)
	e.sync()
	e.wantAttached("ro-c", false)
	e.wantAttached("rw-b", true)
	if ok, excl := e.exclusiveAttachment("vm1"); !ok || !excl {
		t.Fatalf("attachment fs0@vm1: exists %v exclusive %v", ok, excl)
	}

	// another node, reader or writer, while the writers run
	e.share("ro-d", "node2", roFs)
	e.share("rw-e", "node2", rwFs)
	e.sync()
	e.wantConflict("ro-d")
	e.wantConflict("rw-e")
	if ok, _ := e.exclusiveAttachment("node2"); ok {
		t.Fatal("fs0 attached to node2 next to a writer of node1")
	}
	e.wantAttached("rw-a", true)
	e.wantAttached("ro-c", false)

	// the writers are gone: the attachment of node1 is shared again, and
	// the claims of node2 attach
	e.deleteClaim("rw-e")
	e.deallocate("rw-a")
	e.deallocate("rw-b")
	e.sync()
	if ok, excl := e.exclusiveAttachment("vm1"); !ok || excl {
		t.Fatalf("attachment fs0@vm1 after the writers: exists %v exclusive %v", ok, excl)
	}
	e.waitStatus("ro-d", 1)
	e.sync()
	e.wantAttached("ro-d", false)
	e.wantAttached("ro-c", false)
	if ok, excl := e.exclusiveAttachment("node2"); !ok || excl {
		t.Fatalf("attachment fs0@node2: exists %v exclusive %v", ok, excl)
	}
}

// A writer that joins readers of two nodes fails alone: making the
// existing attachment exclusive conflicts, the readers keep theirs.
func TestReadWriteFsJoinsReadersOfTwoNodes(t *testing.T) {
	t.Parallel()
	e := newFsEnv(t)
	e.share("ro-1", "node1", roFs)
	e.share("ro-2", "node2", roFs)
	e.sync()
	e.wantAttached("ro-1", false)
	e.wantAttached("ro-2", false)

	e.waitStatus("ro-1", 1)
	e.waitStatus("ro-2", 1)
	e.share("rw-1", "node1", rwExt4)
	e.sync()
	if line := "attach fs0 to vm1 for claim default/rw-1 device fs0 node node1: single host for a read-write ext4 claim"; !strings.Contains(e.logs.String(), line) {
		t.Fatalf("log lacks %q", line)
	}
	att, _ := e.pool.calls()
	if !slices.Contains(att, "fs0@vm1 k8s:resourceclaim/uid-ro-1 exclusive") {
		t.Fatalf("no exclusive attach request for the writer: %v", att)
	}
	e.wantConflict("rw-1")
	e.wantAttached("ro-1", false)
	e.wantAttached("ro-2", false)
	if ok, excl := e.exclusiveAttachment("vm1"); !ok || excl {
		t.Fatalf("attachment fs0@vm1: exists %v exclusive %v", ok, excl)
	}
	// the status of the readers stays as it is while the writer waits
	e.waitStatus("rw-1", 1)
	rv1, rv2 := e.claim("ro-1").ResourceVersion, e.claim("ro-2").ResourceVersion
	e.sync()
	e.wantConflict("rw-1")
	if e.claim("ro-1").ResourceVersion != rv1 || e.claim("ro-2").ResourceVersion != rv2 {
		t.Fatal("status of a reader rewritten while the writer waits")
	}

	// the reader of the other node goes: its attachment is detached, then
	// the writer gets the attachment exclusively
	e.deallocate("ro-2")
	e.sync()
	e.sync()
	e.wantAttached("rw-1", true)
	e.wantAttached("ro-1", false)
	if ok, excl := e.exclusiveAttachment("vm1"); !ok || !excl {
		t.Fatalf("attachment fs0@vm1: exists %v exclusive %v", ok, excl)
	}
}

// A new exclusive attachment that conflicts is made again without the
// flag for the wants that do not need it: the reader of the node is
// Attached, the writer fails alone, and gets the attachment exclusively
// when the other node's claim is gone.
func TestReadWriteFsNewAttachmentReaderAndWriter(t *testing.T) {
	t.Parallel()
	e := newFsEnv(t)
	e.share("ro-1", "node1", roFs)
	e.sync()
	e.wantAttached("ro-1", false)

	e.waitStatus("ro-1", 1)
	e.share("ro-d", "node2", roFs)
	e.share("rw-e", "node2", rwFs)
	e.sync()
	att, _ := e.pool.calls()
	if n := len(att); n < 3 || att[n-2] != "fs0@node2 k8s:resourceclaim/uid-ro-d exclusive" || att[n-1] != "fs0@node2 k8s:resourceclaim/uid-ro-d" {
		t.Fatalf("attach requests %v, expected an exclusive one, then one without the flag", att)
	}
	e.wantAttached("ro-d", false)
	e.wantConflict("rw-e")
	e.wantAttached("ro-1", false)
	if ok, excl := e.exclusiveAttachment("node2"); !ok || excl {
		t.Fatalf("attachment fs0@node2: exists %v exclusive %v", ok, excl)
	}

	// steady while node1 has the device: the writer keeps failing, the
	// reader stays Attached
	e.waitStatus("rw-e", 1)
	e.waitStatus("ro-d", 1)
	rv := e.claim("ro-d").ResourceVersion
	e.sync()
	e.wantConflict("rw-e")
	if e.claim("ro-d").ResourceVersion != rv {
		t.Fatal("status of the reader rewritten while the writer waits")
	}

	// node1's claim goes: detached, then node2's attachment is exclusive
	// for the writer
	e.deallocate("ro-1")
	e.sync()
	e.sync()
	e.wantAttached("rw-e", true)
	e.wantAttached("ro-d", false)
	if ok, excl := e.exclusiveAttachment("node2"); !ok || !excl {
		t.Fatalf("attachment fs0@node2: exists %v exclusive %v", ok, excl)
	}
	if ok, _ := e.exclusiveAttachment("vm1"); ok {
		t.Fatal("fs0 still attached to vm1")
	}
}

// A new attachment that conflicts also without the exclusive flag fails
// every want of its key.
func TestReadWriteFsNewAttachmentConflict(t *testing.T) {
	t.Parallel()
	e := newFsEnv(t)
	e.pool.addAttachment(api.Attachment{Device: "fs0", Host: "foreign", Owner: "k8s:resourceclaim/other-cluster", SingleHost: true})
	e.share("rw-1", "node1", rwFs)
	e.share("ro-1", "node1", roFs)
	e.sync()
	e.wantConflict("rw-1")
	if c, _ := e.condition("ro-1", condFailed); c == nil || c.Reason != ReasonConflict {
		t.Fatalf("claim ro-1: AttachFailed %+v, expected Conflict", c)
	}
	if c, _ := e.condition("ro-1", condAttached); c != nil {
		t.Fatalf("claim ro-1: Attached %+v", c)
	}
}

// A cluster filesystem on coherent memory is read-write on several hosts;
// all shares stay exclusive with singleHost; a device that is not shared
// never gets the exclusive flag.
func TestReadWriteFsClusterAndExclusive(t *testing.T) {
	t.Parallel()
	e := newFsEnv(t)
	e.share("gfs-1", "node1", rwGfs2)
	e.share("gfs-2", "node2", rwGfs2)
	e.sync()
	e.wantAttached("gfs-1", false)
	e.wantAttached("gfs-2", false)
	for _, call := range func() []string { a, _ := e.pool.calls(); return a }() {
		if strings.HasSuffix(call, " exclusive") {
			t.Fatalf("exclusive attach for gfs2 on a coherent device: %s", call)
		}
	}
	if strings.Contains(e.logs.String(), "exclusive for a read-write") {
		t.Fatal("gfs2 on a coherent device logged as an exclusive writer")
	}
	e.deallocate("gfs-1")
	e.deallocate("gfs-2")

	// all shares: exclusive and singleHost, not logged as a writer
	e.createClaimConfig("all", "fs0", "node1", "share-all", device.SharesUnlimited, rwFs)
	e.sync()
	e.sync()
	if data := e.wantAttached("all", true); data["exclusive"] != true {
		t.Fatalf("claim all: data %v, expected exclusive", data)
	}
	if strings.Contains(e.logs.String(), "exclusive for a read-write") {
		t.Fatal("a claim of all shares logged as an exclusive writer")
	}

	// a device that is not shared
	e.createClaimConfig("p", "pooled0", "node1", "", 0, rwFs)
	e.sync()
	if att, _ := e.pool.calls(); !slices.Contains(att, "pooled0@vm1 k8s:resourceclaim/uid-p") {
		t.Fatalf("attach requests %v, expected pooled0 without the exclusive flag", att)
	}
	if data := e.attachedData("p"); data == nil || data["singleHost"] != nil || data["exclusive"] != nil {
		t.Fatalf("claim p: data %v", data)
	}
}

// An attached writer whose MemoryConfig the controller cannot resolve
// keeps the exclusive flag of its attachment.
func TestReadWriteFsUnresolvedWriterKeepsExclusive(t *testing.T) {
	t.Parallel()
	e := newFsEnv(t)
	e.share("rw-a", "node1", []resourceapi.DeviceAllocationConfiguration{claimConfig(`"device": {"mode": "fs", "readOnly": false, "bogus": 1}`)})
	e.pool.addAttachment(api.Attachment{Device: "fs0", Host: "vm1", Owner: e.c.Owner("uid-rw-a"), SingleHost: true, Slot: api.Slot{Bus: "ds9"}})
	e.markAttached("rw-a")
	e.share("ro-c", "node1", roFs)
	e.sync()
	if !strings.Contains(e.logs.String(), `request mem device fs0: invalid configuration: `) || !strings.Contains(e.logs.String(), "(attached already, kept)") {
		t.Fatal("no log of the kept invalid configuration")
	}
	if ok, excl := e.exclusiveAttachment("vm1"); !ok || !excl {
		t.Fatalf("attachment fs0@vm1: exists %v exclusive %v", ok, excl)
	}
	for _, call := range func() []string { a, _ := e.pool.calls(); return a }() {
		if strings.HasPrefix(call, "fs0@vm1 ") && !strings.HasSuffix(call, " exclusive") {
			t.Fatalf("attach request without the exclusive flag: %s", call)
		}
	}
	e.wantAttached("ro-c", false)
	e.wantAttached("rw-a", true)
	e.share("ro-d", "node2", roFs)
	e.sync()
	e.wantConflict("ro-d")

	// the writer goes: the reader's attachment is shared again
	e.deallocate("rw-a")
	e.sync()
	if ok, excl := e.exclusiveAttachment("vm1"); !ok || excl {
		t.Fatalf("attachment fs0@vm1 after the writer: exists %v exclusive %v", ok, excl)
	}
}

// The resolved MemoryConfig comes with an error of the controller's own
// object, so that the device section still decides the exclusive flag.
func TestPoolSettingsErrorKeepsResolved(t *testing.T) {
	t.Parallel()
	e := newFsEnv(t)
	claim := e.createClaimConfig("rw-n", "fs0", "node1", "share-rw-n", 1, []resourceapi.DeviceAllocationConfiguration{
		claimConfig(`"device": {"mode": "fs", "readOnly": false}, "pool": {"opaque": {"fake-cxl-pool": {"numaNode": -1}}}`)})
	r := claim.Status.Allocation.Devices.Results[1]
	_, res, err := e.c.poolSettings(claim, &r)
	if err == nil || res == nil || !e.c.writerResult(&r, res) {
		t.Fatalf("poolSettings: resolved %v, error %v; expected both, and a writer", res, err)
	}
	claim = e.createClaimConfig("bad", "fs0", "node1", "share-bad", 1, []resourceapi.DeviceAllocationConfiguration{
		claimConfig(`"device": {"mode": "fs", "bogus": 1}`)})
	r = claim.Status.Allocation.Devices.Results[1]
	if _, res, err := e.c.poolSettings(claim, &r); err == nil || res != nil {
		t.Fatalf("poolSettings of an unknown field: resolved %v, error %v", res, err)
	}
}

// When the attach without the exclusive flag gets no answer, the claims
// that do not need the flag wait: they are not refused.
func TestReadWriteFsFallbackNoAnswer(t *testing.T) {
	t.Parallel()
	e := newFsEnv(t)
	e.share("ro-1", "node1", roFs)
	e.sync()
	e.wantAttached("ro-1", false)
	e.waitStatus("ro-1", 1)
	e.share("ro-d", "node2", roFs)
	e.share("rw-e", "node2", rwFs)

	// the reconcile ends (its context is cancelled) during the attach
	// without the flag
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.pool.mu.Lock()
	e.pool.attachHook = func(name string, req api.AttachRequest) string {
		if name == "fs0" && req.Host == "node2" && !req.SingleHost {
			cancel()
			return api.CodeConflict
		}
		return ""
	}
	e.pool.mu.Unlock()
	hm, err := e.c.mapNodes(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.reconcile(ctx, hm); err != nil {
		t.Fatal(err)
	}
	e.wantConflict("rw-e")
	if c, _ := e.condition("ro-d", condFailed); c != nil {
		t.Fatalf("claim ro-d refused without an answer: %+v", c)
	}
	if c, _ := e.condition("ro-d", condAttached); c != nil {
		t.Fatalf("claim ro-d: Attached %+v", c)
	}

	// the next reconcile gets the answer
	e.pool.mu.Lock()
	e.pool.attachHook = nil
	e.pool.mu.Unlock()
	e.waitStatus("rw-e", 1)
	e.sync()
	e.wantAttached("ro-d", false)
	e.wantConflict("rw-e")
}
