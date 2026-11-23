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

package engine

import (
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
)

// The values of the release policy: every cell of the matrix, with and without
// a region of another memdev above. The local cells: the region of an unshared
// memdev released in order for a claim whose Prepare did not finish, else
// destroyed; a shared region kept idle once it is established (set up and
// zeroed, or kept by an earlier release), a new one released in order for its
// claim; an unshared one destroyed; a persistent one released in order, kept
// for the claim of a rollback, idle otherwise.
func TestPolicy(t *testing.T) {
	above := []string{"region9"}
	for _, c := range []struct {
		kind                    Kind
		unprepare, rollback     Decision
		recovery, recoveryAbove Decision
		idle                    Decision
	}{
		{KindLocal, Destroy, InOrder, InOrder, InOrder, Destroy},
		{KindLocalShared, KeepIdle, KeepIdle, KeepIdle, KeepIdle, KeepIdle},
		{KindLocalSharedNew, KeepIdle, InOrder, InOrder, InOrder, KeepIdle},
		{KindLocalSharedUnlisted, Destroy, InOrder, Destroy, Destroy, Destroy},
		{KindLocalPersistent, InOrderIdle, InOrder, InOrderIdle, InOrderIdle, InOrderIdle},
		{KindLocalPersistentNew, InOrderIdle, InOrder, InOrder, InOrder, InOrderIdle},
		{KindPool, Destroy, Destroy, Destroy, Keep, Destroy},
		{KindPoolShared, Destroy, Destroy, Destroy, Keep, Destroy},
		{KindPoolPersistent, Destroy, Destroy, Destroy, Keep, Destroy},
	} {
		for _, cell := range []struct {
			release Release
			above   []string
			want    Decision
		}{
			{ReleaseUnprepare, nil, c.unprepare},
			{ReleaseUnprepare, above, c.unprepare},
			{ReleaseRollback, nil, c.rollback},
			{ReleaseRollback, above, c.rollback},
			{ReleaseRecovery, nil, c.recovery},
			{ReleaseRecovery, above, c.recoveryAbove},
			{ReleaseIdle, nil, c.idle},
			{ReleaseIdle, above, c.idle},
		} {
			if got := Policy(cell.release, c.kind, cell.above); got != cell.want {
				t.Errorf("Policy(%s, %s, %v) = %s, want %s", cell.release, c.kind, cell.above, got, cell.want)
			}
		}
	}
	if got := Policy(ReleaseUnprepare, Kind("dcd"), nil); got != Keep {
		t.Errorf("an unknown kind: %s", got)
	}
	for _, c := range []struct {
		d        Decision
		release  Release
		forClaim bool
	}{
		{InOrder, ReleaseRollback, true}, {InOrder, ReleaseRecovery, true}, {InOrder, ReleaseUnprepare, false},
		{InOrder, ReleaseIdle, false}, {Keep, ReleaseRecovery, true}, {Keep, ReleaseUnprepare, false},
		{KeepIdle, ReleaseRollback, false}, {InOrderIdle, ReleaseRecovery, false}, {Destroy, ReleaseRollback, false},
	} {
		if got := c.d.forClaim(c.release); got != c.forClaim {
			t.Errorf("%s at %s: for the claim %v, want %v", c.d, c.release, got, c.forClaim)
		}
	}
}

// The row of the policy of the grants of a memdev.
func TestKindOf(t *testing.T) {
	for _, c := range []struct {
		asked       state.Asked
		shared      bool
		established bool
		want        Kind
	}{
		{state.Asked{Source: state.SourcePool, Kind: state.KindPool}, false, true, KindPool},
		{state.Asked{Source: state.SourcePool, Kind: state.KindPoolShared, Shared: true}, false, false, KindPoolShared},
		{state.Asked{Source: state.SourcePool, Kind: state.KindPoolPersistent}, false, true, KindPoolPersistent},
		{state.Asked{Source: state.SourceLocal, Kind: state.KindLocal}, false, false, KindLocal},
		{state.Asked{Source: state.SourceLocal, Kind: state.KindLocal}, true, true, KindLocal},
		{state.Asked{Source: state.SourceLocal, Kind: state.KindLocalShared}, true, true, KindLocalShared},
		{state.Asked{Source: state.SourceLocal, Kind: state.KindLocalShared}, true, false, KindLocalSharedNew},
		{state.Asked{Source: state.SourceLocal, Kind: state.KindLocalShared}, false, true, KindLocalSharedUnlisted},
		{state.Asked{Source: state.SourceLocal, Kind: state.KindLocalShared}, false, false, KindLocal},
		{state.Asked{Source: state.SourceLocal, Kind: state.KindLocalPersistent}, true, true, KindLocalPersistent},
		{state.Asked{Source: state.SourceLocal, Kind: state.KindLocalPersistent}, false, false, KindLocalPersistentNew},
	} {
		if got := kindOf(c.asked, c.shared, c.established); got != c.want {
			t.Errorf("kindOf(%+v, %v, %v) = %s, want %s", c.asked, c.shared, c.established, got, c.want)
		}
	}
}
