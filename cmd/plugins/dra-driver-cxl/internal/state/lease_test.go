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

package state

import (
	"reflect"
	"testing"
)

func TestLeaseLends(t *testing.T) {
	pod := PodRef{Namespace: "demo", Name: "requester", UID: "pod-uid-1"}
	numa := func(id string, st LeaseState, node int) Lease {
		return Lease{ID: id, Pod: pod, Container: "app", Asked: LeaseAsked{Mode: ModeNUMA}, State: st, Lend: &LeaseLend{NUMANode: NUMANodePtr(node)}}
	}
	other := numa("l5", LeaseReady, 2)
	other.Container = "sidecar"
	dax := numa("l6", LeaseReady, 3)
	dax.Asked.Mode = ModeDax
	r := &Records{Leases: []Lease{
		numa("l3", LeaseReady, 2), numa("l1", LeaseReady, 2), numa("l2", LeaseReady, 4), numa("l4", LeaseReleasing, 5),
		other, dax, {ID: "l7", Pod: pod, Container: "app", Asked: LeaseAsked{Mode: ModeNUMA}, State: LeaseReady},
	}}
	got := LeaseLends(r, pod.UID, "app")
	want := map[int][]string{2: {"l1", "l3"}, 4: {"l2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lends %v, expected %v", got, want)
	}
	if got := LeaseLends(r, "pod-uid-2", "app"); got != nil {
		t.Fatalf("lends of another pod %v", got)
	}
}

func TestLeaseAsksSerial(t *testing.T) {
	for _, c := range []struct {
		a    LeaseAsked
		want bool
	}{{LeaseAsked{}, false}, {LeaseAsked{Serial: 0xc1ee0001}, true}, {LeaseAsked{SerialZero: true}, true}} {
		if got := c.a.AsksSerial(); got != c.want {
			t.Errorf("%+v: %v", c.a, got)
		}
	}
}
