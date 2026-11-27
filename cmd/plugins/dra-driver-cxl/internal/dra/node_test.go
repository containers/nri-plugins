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

package dra

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// newTestNode returns a node on a fake kernel without memdevs. Its store
// and its CDI directory are in a temporary directory. set changes the
// options before NewNode.
func newTestNode(t *testing.T, set func(*NodeOptions)) *Node {
	t.Helper()
	k := fake.New(t)
	dir := t.TempDir()
	opts := NodeOptions{
		Kernel:   k.Kernel(),
		Dir:      filepath.Join(dir, device.DriverName),
		CDIDir:   filepath.Join(dir, "cdi"),
		NodeName: envNode,
	}
	if set != nil {
		set(&opts)
	}
	st, err := store.Open(context.Background(), store.Options{Dir: opts.Dir, BootID: k.BootID()})
	mustOK(t, err)
	t.Cleanup(func() { _ = st.Close() })
	opts.Store = st
	n, err := NewNode(opts)
	mustOK(t, err)
	return n
}

// The plugin data directory of the pool driver is next to that of
// cxl.nri.io. Each driver has its mounts directory in its own.
func TestNodeDirs(t *testing.T) {
	n := newTestNode(t, nil)
	if n.Pool != nil {
		t.Fatal("a pool plugin without the pool driver")
	}
	if got, want := n.Local.Rules().MountsDir, filepath.Join(n.opts.Dir, mountsDirName); got != want {
		t.Fatalf("local mounts directory %s, want %s", got, want)
	}
	n = newTestNode(t, func(o *NodeOptions) { o.PoolDriver = device.PoolDriverName })
	poolDir := filepath.Join(filepath.Dir(n.opts.Dir), device.PoolDriverName)
	if got, want := n.Pool.Rules().MountsDir, filepath.Join(poolDir, mountsDirName); got != want {
		t.Fatalf("pool mounts directory %s, want %s", got, want)
	}
	if _, err := os.Stat(poolDir); err != nil {
		t.Fatal(err)
	}
}

// The node publishes local memdevs only when it manages them.
func TestNodeLocalSnapshot(t *testing.T) {
	for _, managed := range []bool{false, true} {
		n := newTestNode(t, func(o *NodeOptions) {
			o.Local.Managed = managed
			o.Publish.ManageMemdevs = managed
		})
		_, err := n.Recover(context.Background())
		mustOK(t, err)
		snap := n.Publisher.LocalRecords()
		if managed != (snap != nil) || snap != nil && len(snap.Memdevs) != 0 {
			t.Fatalf("managed %v: local snapshot %+v", managed, snap)
		}
	}
}

// WriteCDI removes the claims spec without claims and the pool spec
// without the pool driver. It keeps the specs of other drivers.
func TestNodeWriteCDIRemovesUnownedSpecs(t *testing.T) {
	cdiDir := t.TempDir()
	for name, kind := range map[string]string{device.ClaimsCDISpecName: device.CDIKind(device.DriverName),
		device.PoolCDISpecName: device.CDIKind(device.PoolDriverName), "gpu.yaml": "example.com/gpu"} {
		data := `{"cdiVersion":"0.6.0","kind":"` + kind + `","devices":[{"name":"d","containerEdits":{"env":["A=1"]}}]}`
		mustOK(t, os.WriteFile(filepath.Join(cdiDir, name), []byte(data), 0o600))
	}
	n := newTestNode(t, func(o *NodeOptions) { o.CDIDir = cdiDir })
	mustOK(t, n.WriteCDI())
	var left []string
	m, _ := filepath.Glob(filepath.Join(cdiDir, "*"))
	for _, path := range m {
		left = append(left, filepath.Base(path))
	}
	if !slices.Equal(left, []string{"gpu.yaml"}) {
		t.Errorf("left %v, want [gpu.yaml]", left)
	}
}

// The watcher lists the pool ResourceSlices only after the recovery has
// guarded the local memdevs.
func TestNodeFirstListAfterRecovery(t *testing.T) {
	var n *Node
	lists, guarded := 0, 0
	client := k8sfake.NewClientset()
	client.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		if n != nil && n.guarded.Load() {
			guarded++
		}
		return false, nil, nil
	})
	n = newTestNode(t, func(o *NodeOptions) { o.Client = client })
	_, err := n.Recover(context.Background())
	mustOK(t, err)
	if lists == 0 || guarded != lists {
		t.Fatalf("%d lists, %d after the recovery", lists, guarded)
	}
}

// The local guard refuses a memdev that the pool driver prepares, before
// the watcher is asked.
func TestNodeLocalGuardPoolSerial(t *testing.T) {
	ctx := context.Background()
	e := newDriverEnv(t, nil, poolSpec(serialPoolX))
	e.mustPrepare(e.pool, poolClaim("u1", "pooled0", poolExclusiveData))
	wantErrText(t, e.node.localGuard(ctx, serialPoolX),
		"memdev 0xc1ee0001 is prepared by the pool driver "+device.PoolDriverName+": it is not used as a local memdev")
	mustOK(t, e.node.localGuard(ctx, serialLocal))
	e.mustUnprepare(e.pool, "u1")
	mustOK(t, e.node.localGuard(ctx, serialPoolX))
}

// A local claim on a memdev that a pool slice lists is refused before the
// memdev is touched, sliced and shared.
func TestPrepareLocalPoolDevice(t *testing.T) {
	ctx := context.Background()
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec(), sharedSpec()})
	client := k8sfake.NewClientset()
	e.client = client
	e.setShared(serialShared)
	_, err := client.ResourceV1().ResourceSlices().Create(ctx, pubResourceSlice("pool-c", device.PoolDriverName,
		pubSerialAttr(string(device.AttrSerial), api.FormatSerial(serialLocal)),
		pubSerialAttr(string(device.AttrSerial), api.FormatSerial(serialShared))), metav1.CreateOptions{})
	mustOK(t, err)
	e.mark()
	const refusal = " is a pool device of " + device.PoolDriverName + "/pool-c: it is not used as a local memdev"
	wantErrText(t, e.prepare(e.local, localClaim("u1", localDev, envBlockSize)).Err, "memdev 0xc100e2e1"+refusal)
	wantErrText(t, e.prepare(e.local, localClaim("u2", MemdevDeviceName(serialShared), -1, cfgDax)).Err, "is a pool device")
	if cmds := e.commands(); strings.Contains(cmds, "create-region") {
		t.Fatalf("the pool devices were used:\n%s", cmds)
	}
	e.wantClean()
}
