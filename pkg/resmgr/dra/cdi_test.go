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
	"os"
	"reflect"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"tags.cncf.io/container-device-interface/pkg/cdi"
	specs "tags.cncf.io/container-device-interface/specs-go"
)

const testClaimUID = types.UID("8b9c5a6e-1f2d-4c3b-9a8e-7d6c5b4a3f21")

// newTestCDIStore creates a store writing to a directory of the test's own.
func newTestCDIStore(t *testing.T) *cdiStore {
	t.Helper()

	s, err := newCDIStore(testDriverName, t.TempDir())
	if err != nil {
		t.Fatalf("newCDIStore() failed: %v", err)
	}

	return s
}

// specDir is where the store under test writes its specs.
func (s *cdiStore) specDir(t *testing.T) string {
	t.Helper()

	dirs := s.cache.GetSpecDirectories()
	if len(dirs) != 1 {
		t.Fatalf("expected a single CDI spec directory, got %v", dirs)
	}

	return dirs[0]
}

// readSpec reads back the spec the store wrote for a claim.
func readSpec(t *testing.T, s *cdiStore, uid types.UID) *specs.Spec {
	t.Helper()

	path := s.specPath(uid)
	spec, err := cdi.ReadSpec(path, 0)
	if err != nil {
		t.Fatalf("failed to read back CDI spec %q: %v", path, err)
	}

	return spec.Spec
}

func TestNewCDIStore(t *testing.T) {
	for _, tc := range []struct {
		name       string
		driverName string
		fail       bool
	}{
		{name: "valid driver name", driverName: testDriverName},
		{name: "empty driver name", driverName: "", fail: true},
		{name: "driver name starting with a digit", driverName: "1.nri.io", fail: true},
		{name: "driver name with an invalid character", driverName: "test/nri.io", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newCDIStore(tc.driverName, t.TempDir())
			switch {
			case tc.fail && err == nil:
				t.Errorf("newCDIStore(%q) succeeded, expected an error", tc.driverName)
			case !tc.fail && err != nil:
				t.Errorf("newCDIStore(%q) failed: %v", tc.driverName, err)
			case !tc.fail && s == nil:
				t.Errorf("newCDIStore(%q) returned no store", tc.driverName)
			}
		})
	}
}

func TestCDIStoreDefaultDir(t *testing.T) {
	s, err := newCDIStore(testDriverName, "")
	if err != nil {
		t.Fatalf("newCDIStore() failed: %v", err)
	}

	if dirs := s.cache.GetSpecDirectories(); !slices.Equal(dirs, []string{cdi.DefaultDynamicDir}) {
		t.Errorf("expected spec directory %q, got %v", cdi.DefaultDynamicDir, dirs)
	}
}

func TestHasClaim(t *testing.T) {
	s := newTestCDIStore(t)

	if has, err := s.HasClaim(testClaimUID); err != nil {
		t.Errorf("HasClaim() failed: %v", err)
	} else if has {
		t.Errorf("claim %s has a spec before one was written", testClaimUID)
	}

	if _, err := s.WriteClaim(testClaimUID, []specs.ContainerEdits{{Env: []string{"NRI=1"}}}); err != nil {
		t.Fatalf("WriteClaim() failed: %v", err)
	}

	// This is what tells a prepared claim from an unprepared one, so it has to
	// agree with what WriteClaim writes and RemoveClaim removes, not merely with
	// itself.
	if has, err := s.HasClaim(testClaimUID); err != nil {
		t.Errorf("HasClaim() failed: %v", err)
	} else if !has {
		t.Errorf("claim %s has no spec after one was written", testClaimUID)
	}

	if err := s.RemoveClaim(testClaimUID); err != nil {
		t.Fatalf("RemoveClaim() failed: %v", err)
	}

	if has, err := s.HasClaim(testClaimUID); err != nil {
		t.Errorf("HasClaim() failed: %v", err)
	} else if has {
		t.Errorf("claim %s still has a spec after it was removed", testClaimUID)
	}
}

func TestWriteClaim(t *testing.T) {
	s := newTestCDIStore(t)

	// The second device carries an edit the driver has no idea about, which is
	// the point: what a device grants is the policy's business.
	edits := []specs.ContainerEdits{
		{
			Env: []string{"NRI_CPUS=2-3"},
		},
		{
			Env: []string{"NRI_CPUS=4-5"},
			Mounts: []*specs.Mount{
				{
					HostPath:      "/host/path",
					ContainerPath: "/container/path",
					Type:          "bind",
					Options:       []string{"bind", "ro"},
				},
			},
		},
	}

	names, err := s.WriteClaim(testClaimUID, edits)
	if err != nil {
		t.Fatalf("WriteClaim() failed: %v", err)
	}

	expected := []string{
		testDriverName + "/device=claim-" + string(testClaimUID) + "-0",
		testDriverName + "/device=claim-" + string(testClaimUID) + "-1",
	}
	if !slices.Equal(names, expected) {
		t.Errorf("WriteClaim() returned devices %v, expected %v", names, expected)
	}

	spec := readSpec(t, s, testClaimUID)

	if kind := testDriverName + "/device"; spec.Kind != kind {
		t.Errorf("spec kind is %q, expected %q", spec.Kind, kind)
	}
	if spec.Version == "" {
		t.Errorf("spec has no version")
	}

	devices := []specs.Device{
		{Name: "claim-" + string(testClaimUID) + "-0", ContainerEdits: edits[0]},
		{Name: "claim-" + string(testClaimUID) + "-1", ContainerEdits: edits[1]},
	}
	if !reflect.DeepEqual(spec.Devices, devices) {
		t.Errorf("spec devices are\n%+v\nexpected\n%+v", spec.Devices, devices)
	}
}

// TestWriteClaimVersion checks that the version of a spec follows the edits it
// contains: a policy using a kind of edit which needs a newer CDI version gets
// that version without the driver knowing about the edit.
func TestWriteClaimVersion(t *testing.T) {
	s := newTestCDIStore(t)

	plain := types.UID("plain")
	if _, err := s.WriteClaim(plain, []specs.ContainerEdits{{Env: []string{"FOO=bar"}}}); err != nil {
		t.Fatalf("WriteClaim() failed: %v", err)
	}

	// Mount.Type was added in CDI v0.4.0.
	typed := types.UID("typed")
	edits := []specs.ContainerEdits{
		{
			Mounts: []*specs.Mount{
				{HostPath: "/host/path", ContainerPath: "/container/path", Type: "bind"},
			},
		},
	}
	if _, err := s.WriteClaim(typed, edits); err != nil {
		t.Fatalf("WriteClaim() failed: %v", err)
	}

	plainVersion := readSpec(t, s, plain).Version
	typedVersion := readSpec(t, s, typed).Version
	if plainVersion == typedVersion {
		t.Errorf("both specs got version %q, expected the mount to need a newer one",
			typedVersion)
	}
}

func TestWriteClaimRejected(t *testing.T) {
	edits := []specs.ContainerEdits{{Env: []string{"FOO=bar"}}}

	for _, tc := range []struct {
		name  string
		uid   types.UID
		edits []specs.ContainerEdits
	}{
		{name: "no devices", uid: testClaimUID},
		{name: "no UID", edits: edits},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestCDIStore(t)

			if _, err := s.WriteClaim(tc.uid, tc.edits); err == nil {
				t.Errorf("WriteClaim() succeeded, expected an error")
			}

			if entries, err := os.ReadDir(s.specDir(t)); err != nil {
				t.Errorf("failed to read the spec directory: %v", err)
			} else if len(entries) != 0 {
				t.Errorf("a rejected WriteClaim() wrote %d file(s)", len(entries))
			}
		})
	}
}

// TestWriteClaimAgain checks that re-preparing a claim replaces its spec
// instead of failing or leaving a second one behind.
func TestWriteClaimAgain(t *testing.T) {
	s := newTestCDIStore(t)

	for _, env := range []string{"NRI_CPUS=2-3", "NRI_CPUS=4-5"} {
		if _, err := s.WriteClaim(testClaimUID, []specs.ContainerEdits{{Env: []string{env}}}); err != nil {
			t.Fatalf("WriteClaim() failed: %v", err)
		}
	}

	if entries, err := os.ReadDir(s.specDir(t)); err != nil {
		t.Errorf("failed to read the spec directory: %v", err)
	} else if len(entries) != 1 {
		t.Errorf("expected a single spec file, got %d", len(entries))
	}

	spec := readSpec(t, s, testClaimUID)
	if len(spec.Devices) != 1 || !slices.Equal(spec.Devices[0].ContainerEdits.Env, []string{"NRI_CPUS=4-5"}) {
		t.Errorf("spec was not replaced, its devices are %+v", spec.Devices)
	}
}

func TestRemoveClaim(t *testing.T) {
	s := newTestCDIStore(t)

	if _, err := s.WriteClaim(testClaimUID, []specs.ContainerEdits{{Env: []string{"FOO=bar"}}}); err != nil {
		t.Fatalf("WriteClaim() failed: %v", err)
	}

	// Removing twice, and removing a claim we never wrote, must all succeed:
	// the kubelet may ask to unprepare the same claim more than once.
	for _, uid := range []types.UID{testClaimUID, testClaimUID, "unknown"} {
		if err := s.RemoveClaim(uid); err != nil {
			t.Errorf("RemoveClaim(%s) failed: %v", uid, err)
		}
	}

	if entries, err := os.ReadDir(s.specDir(t)); err != nil {
		t.Errorf("failed to read the spec directory: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("the spec directory still has %d file(s)", len(entries))
	}
}
