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

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	nriapi "github.com/containerd/nri/pkg/api"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/containers"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/leases"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// An NRI CreateContainer never waits for the lease broker in the driver's
// wiring (the broker and the NRI plugin over one store, wireLenders, the
// broker started with the plugin): while a commit of the broker is held, a
// requester container is created with the node of its READY numa lease,
// read from the store.
func TestCreateContainerDoesNotWaitForTheBroker(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, device.DriverName)
	for _, d := range []string{"sys/bus/cxl/devices", "sys/devices/system/node", "cdi", device.DriverName} {
		noErr(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	ctx := context.Background()
	var armed atomic.Bool
	entered, unblock := make(chan struct{}), make(chan struct{})
	var st *store.Store
	st, err := store.Open(ctx, store.Options{Dir: local, BootID: "boot-1", AfterCommit: func() {
		// the commit of the lease claim, held until the test ends
		if st.Snapshot().LeaseClaim("lc1") != nil && armed.CompareAndSwap(true, false) {
			close(entered)
			<-unblock
		}
	}})
	noErr(t, err)
	defer func() { noErr(t, st.Close()) }()
	noErr(t, st.Commit(ctx, store.Durable, func(doc *store.Document) error {
		doc.Leases = append(doc.Leases, state.Lease{ID: "0000aaaa", Claim: "lc0", Pod: state.PodRef{Namespace: "ns", Name: "pod", UID: "u1"},
			Container: "app", State: state.LeaseReady, Asked: state.LeaseAsked{Mode: state.ModeNUMA, Size: "256Mi"},
			Lend: &state.LeaseLend{NUMANode: state.NUMANodePtr(2)}, Created: time.Now().UTC()})
		return nil
	}))

	flags := &flags{Pool: true, PoolPrepareTimeout: time.Second, CDIRoot: filepath.Join(root, "cdi"),
		NodeName: testNode, Leases: true}
	cfg := &pluginConfig{Flags: flags, KubeletPluginDir: local}
	nri, err := containers.New(containers.Options{Defaults: config.Container{Fencing: config.FencingNone}, Store: st, Leases: true,
		SysRoot: root})
	noErr(t, err)
	k := kernelAt(root)
	opts := nodeOptions(cfg, &DriverConfig{}, k, st)
	opts.Fence = fenceOf(nri)
	node, err := dra.NewNode(opts)
	noErr(t, err)
	_, err = node.Recover(ctx)
	noErr(t, err)
	// newBroker's broker, with a client of the test
	broker, err := leases.New(leases.Options{Store: st, Client: k8sfake.NewClientset(), NodeName: testNode, DataDir: local,
		CLI: filepath.Join(root, "cxl-request"), WaitDefault: time.Second, PoolDriver: device.PoolDriverName,
		MountPoints: func() ([]string, error) { return mountPoints(k) }, OpenDevice: k.OpenDevice, PublishedDevice: publishedDevice(node)})
	noErr(t, err)
	node.Local.SetLeases(broker)
	broker.Start(wireLenders(nri, broker, node))
	defer broker.Stop()

	armed.Store(true)
	go func() {
		_ = broker.OpenClaim(&resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: "lc1", Name: "lease", Namespace: "ns"}})
	}()
	<-entered
	created := make(chan error, 1)
	go func() {
		pod := &nriapi.PodSandbox{Id: "sb", Uid: "u1", Namespace: "ns", Name: "pod"}
		_, _, err := nri.CreateContainer(ctx, pod, &nriapi.Container{Id: "c1", PodSandboxId: "sb", Name: "app"})
		created <- err
	}()
	select {
	case err := <-created:
		close(unblock)
		noErr(t, err)
	case <-time.After(5 * time.Second):
		close(unblock)
		t.Fatal("CreateContainer waits for a commit of the broker")
	}
	if got := nri.LentNodes("u1", "app"); !slices.Equal(got, []int{2}) {
		t.Fatalf("lent nodes %v, expected the READY lease's [2]", got)
	}
}
