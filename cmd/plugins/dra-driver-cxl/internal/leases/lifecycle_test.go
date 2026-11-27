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

package leases

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// A companion that cannot be created fails the lease and releases it: the
// client gets Failed with the error; a claim created before the pod failed
// is deleted by the release.
func TestCompanionCreateFails(t *testing.T) {
	e := brokerEnv(t)
	var podFails atomic.Bool
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if podFails.Load() {
			return true, nil, errors.New("quota exceeded")
		}
		return false, nil, nil
	})
	e.openClaim()
	e.start()
	// a companion claim of another node and a user pod have the name
	claim := companionClaim("000000a1")
	claim.Labels[device.NodeLabel] = "node2"
	_, err := e.client.ResourceV1().ResourceClaims(envNS).Create(context.Background(), claim, metav1.CreateOptions{})
	mustOK(t, err)
	_, err = e.client.CoreV1().Pods(envNS).Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: envNS}}, metav1.CreateOptions{})
	mustOK(t, err)
	r, _ := e.call(api.Request{Op: api.OpRequest, Size: "64Mi"})
	if r.State != api.StateFailed || !strings.HasPrefix(r.Error, "creating companion claim demo/cxl-lease-000000a1: ") {
		t.Fatalf("response %+v", r)
	}
	e.waitState(r.Lease, state.LeaseNone)
	_, err = e.client.ResourceV1().ResourceClaims(envNS).Get(context.Background(), claim.Name, metav1.GetOptions{})
	mustOK(t, err)
	_, err = e.client.CoreV1().Pods(envNS).Get(context.Background(), claim.Name, metav1.GetOptions{})
	mustOK(t, err)

	podFails.Store(true)
	r, _ = e.call(api.Request{Op: api.OpRequest, Size: "64Mi"})
	if r.State != api.StateFailed || r.Error != "creating companion pod demo/cxl-lease-000000a2: quota exceeded" || r.Lease != "000000a2" {
		t.Fatalf("response %+v", r)
	}
	e.waitState(r.Lease, state.LeaseNone)
	e.wantGone(CompanionName("000000a2"))
}

// waitStored waits until the stored lease satisfies cond.
func (e *leaseEnv) waitStored(id string, cond func(l *state.Lease) bool) {
	e.t.Helper()
	for i := 0; i < 1000; i++ {
		if l := e.stored(id); l != nil && cond(l) {
			return
		}
		e.sleep()
	}
	e.t.Fatalf("lease %s: %+v", id, e.stored(id))
}

// A lease claim whose socket does not start at a start is OPEN_NO_LISTENER;
// its Prepare again or the reconcile starts it.
func TestRelistenFails(t *testing.T) {
	e := brokerEnv(t)
	e.openClaim()
	e.b.Stop()
	_ = e.st.Close()
	dir := filepath.Dir(e.sock())
	mustOK(t, os.RemoveAll(dir))
	mustOK(t, os.WriteFile(dir, []byte("x"), 0o600))
	e.boot()
	if c := e.st.Snapshot().LeaseClaim(envLeaseClaim); c == nil || c.State != state.LeaseClaimOpenNoListener {
		t.Fatalf("lease claim %+v", c)
	}
	e.start()
	mustOK(t, os.Remove(dir))
	e.openClaim()
	if c := e.st.Snapshot().LeaseClaim(envLeaseClaim); c == nil || c.State != state.LeaseClaimOpen {
		t.Fatalf("lease claim %+v", c)
	}
	if resp, _ := e.call(api.Request{Op: api.OpList}); resp.Error != "" {
		t.Fatalf("list %+v", resp)
	}
	// a start whose listen fails again
	e.b.Stop()
	_ = e.st.Close()
	mustOK(t, os.RemoveAll(dir))
	mustOK(t, os.WriteFile(dir, []byte("x"), 0o600))
	e.boot()
	e.b.Stop()
	_ = e.st.Close()
	e.boot()
	if c := e.st.Snapshot().LeaseClaim(envLeaseClaim); c == nil || c.State != state.LeaseClaimOpenNoListener {
		t.Fatalf("lease claim %+v", c)
	}
	// the reconcile listens again once it can
	e.b.reconcile()
	if c := e.st.Snapshot().LeaseClaim(envLeaseClaim); c == nil || c.State != state.LeaseClaimOpenNoListener {
		t.Fatalf("lease claim after a failed listen of the reconcile %+v", c)
	}
	mustOK(t, os.Remove(dir))
	e.b.reconcile()
	if c := e.st.Snapshot().LeaseClaim(envLeaseClaim); c == nil || c.State != state.LeaseClaimOpen {
		t.Fatalf("lease claim after the reconcile %+v", c)
	}
	e.start()
	if resp, _ := e.call(api.Request{Op: api.OpList}); resp.Error != "" {
		t.Fatalf("list %+v", resp)
	}
}
