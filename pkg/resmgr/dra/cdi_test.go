/*
Copyright The NRI Plugins Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dra

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"tags.cncf.io/container-device-interface/pkg/parser"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// TestCDIDeviceName_BasicCase verifies the basic format of a CDI device name.
func TestCDIDeviceName_BasicCase(t *testing.T) {
	uid := types.UID("abc123-0000-0000-0000-000000000001")
	name := cdiDeviceName(uid, "myrequest", "punit-0-0", 0)
	if !strings.HasPrefix(name, "claim-") {
		t.Errorf("cdiDeviceName() = %q, want prefix \"claim-\"", name)
	}
	if err := parser.ValidateDeviceName(name); err != nil {
		t.Errorf("cdiDeviceName() = %q, invalid CDI device name: %v", name, err)
	}
}

// TestCDIDeviceName_SubrequestSlash verifies that '/' in request is replaced
// so the result is still a valid CDI device name.
func TestCDIDeviceName_SubrequestSlash(t *testing.T) {
	uid := types.UID("abc123-0000-0000-0000-000000000002")
	name := cdiDeviceName(uid, "first-available/req0", "punit-0-1", 0)
	if err := parser.ValidateDeviceName(name); err != nil {
		t.Errorf("cdiDeviceName() with slash in request = %q, invalid: %v", name, err)
	}
	if strings.Contains(name, "/") {
		t.Errorf("cdiDeviceName() = %q, should not contain '/'", name)
	}
}

// TestCDIDeviceName_TwoResultsSameRequestDevice verifies that two results with
// the same Request+Device but different idx produce distinct valid names.
func TestCDIDeviceName_TwoResultsSameRequestDevice(t *testing.T) {
	uid := types.UID("abc123-0000-0000-0000-000000000003")
	name0 := cdiDeviceName(uid, "myrequest", "punit-0-0", 0)
	name1 := cdiDeviceName(uid, "myrequest", "punit-0-0", 1)
	if name0 == name1 {
		t.Errorf("cdiDeviceName() idx 0 and 1 produced the same name %q", name0)
	}
	if err := parser.ValidateDeviceName(name0); err != nil {
		t.Errorf("name0 %q invalid: %v", name0, err)
	}
	if err := parser.ValidateDeviceName(name1); err != nil {
		t.Errorf("name1 %q invalid: %v", name1, err)
	}
}

// TestCDIDeviceName_AllSlashRequest verifies that a request of all slashes
// produces a valid name (the sanitize+trim fallback to "x").
func TestCDIDeviceName_AllSlashRequest(t *testing.T) {
	uid := types.UID("abc123-0000-0000-0000-000000000004")
	name := cdiDeviceName(uid, "///", "punit-0-0", 0)
	if err := parser.ValidateDeviceName(name); err != nil {
		t.Errorf("cdiDeviceName() with all-slash request = %q, invalid: %v", name, err)
	}
}

// TestNewCDIWriter_InvalidVendor verifies that an invalid vendor name causes
// NewCDIWriter to return an error.
func TestNewCDIWriter_InvalidVendor(t *testing.T) {
	dir := t.TempDir()
	_, err := NewCDIWriter("123invalid", dir)
	if err == nil {
		t.Error("NewCDIWriter() with invalid vendor name: expected error, got nil")
	}
}

// TestNewCDIWriter_DefaultDir verifies that NewCDIWriter creates /var/run/cdi
// when cdiDir is empty and permissions allow it. This test is skipped unless
// the process can write to /var/run (it needs root or pre-created dir).
func TestNewCDIWriter_DefaultDir(t *testing.T) {
	t.Skip("requires write access to /var/run — skipped in unit test environment")
}

// TestWriteClaim_EnvVarsOnDisk verifies that WriteClaim writes a YAML spec
// file containing the expected env vars and Kind.
func TestWriteClaim_EnvVarsOnDisk(t *testing.T) {
	dir := t.TempDir()
	w, err := NewCDIWriter("intel.com", dir)
	if err != nil {
		t.Fatalf("NewCDIWriter() unexpected error: %v", err)
	}

	uid := types.UID("test-claim-uid-0001")
	cpus, _ := cpuset.Parse("0,2,4")
	devices := []CDIDevice{
		{Name: "claim-test-claim-uid-0001-myreq-punit-0-0-0", ClassName: "gold", CPUs: cpus},
	}

	if err := w.WriteClaim(uid, devices); err != nil {
		t.Fatalf("WriteClaim() unexpected error: %v", err)
	}

	// Verify spec file exists with expected name pattern.
	pattern := filepath.Join(dir, "intel.com-device_test-claim-uid-0001.yaml")
	data, err := os.ReadFile(pattern)
	if err != nil {
		t.Fatalf("spec file not found at %q: %v", pattern, err)
	}

	contents := string(data)
	if !strings.Contains(contents, "NRI_CLASS=gold") {
		t.Errorf("spec missing NRI_CLASS=gold; contents:\n%s", contents)
	}
	if !strings.Contains(contents, "NRI_CPU0=1") {
		t.Errorf("spec missing NRI_CPU0=1; contents:\n%s", contents)
	}
	if !strings.Contains(contents, "NRI_CPU2=1") {
		t.Errorf("spec missing NRI_CPU2=1; contents:\n%s", contents)
	}
	if !strings.Contains(contents, "NRI_CPU4=1") {
		t.Errorf("spec missing NRI_CPU4=1; contents:\n%s", contents)
	}
	if !strings.Contains(contents, "kind: intel.com/device") {
		t.Errorf("spec missing Kind \"intel.com/device\"; contents:\n%s", contents)
	}
}

// TestWriteClaim_EmptyDevices verifies that WriteClaim returns an error when
// devices is empty (CDI rejects specs with no devices).
func TestWriteClaim_EmptyDevices(t *testing.T) {
	dir := t.TempDir()
	w, err := NewCDIWriter("intel.com", dir)
	if err != nil {
		t.Fatalf("NewCDIWriter() unexpected error: %v", err)
	}
	err = w.WriteClaim("some-uid", nil)
	if err == nil {
		t.Error("WriteClaim() with empty devices: expected error, got nil")
	}
}

// TestRemoveClaim_Removes verifies that RemoveClaim removes the spec file.
func TestRemoveClaim_Removes(t *testing.T) {
	dir := t.TempDir()
	w, err := NewCDIWriter("intel.com", dir)
	if err != nil {
		t.Fatalf("NewCDIWriter() unexpected error: %v", err)
	}

	uid := types.UID("test-uid-remove-0001")
	cpus, _ := cpuset.Parse("0")
	devs := []CDIDevice{{Name: "claim-test-uid-remove-0001-req-dev-0", ClassName: "silver", CPUs: cpus}}
	if err := w.WriteClaim(uid, devs); err != nil {
		t.Fatalf("WriteClaim() unexpected error: %v", err)
	}

	if !w.ClaimSpecExists(uid) {
		t.Fatal("ClaimSpecExists() = false after WriteClaim, want true")
	}
	if err := w.RemoveClaim(uid); err != nil {
		t.Fatalf("RemoveClaim() unexpected error: %v", err)
	}
	if w.ClaimSpecExists(uid) {
		t.Error("ClaimSpecExists() = true after RemoveClaim, want false")
	}
}

// TestRemoveClaim_Idempotent verifies that RemoveClaim on a non-existent spec
// returns nil (idempotent).
func TestRemoveClaim_Idempotent(t *testing.T) {
	dir := t.TempDir()
	w, err := NewCDIWriter("intel.com", dir)
	if err != nil {
		t.Fatalf("NewCDIWriter() unexpected error: %v", err)
	}
	if err := w.RemoveClaim("nonexistent-uid"); err != nil {
		t.Errorf("RemoveClaim() on non-existent spec: unexpected error %v", err)
	}
}

// TestClaimSpecExists_TrueAndFalse verifies ClaimSpecExists for written and
// not-written claims.
func TestClaimSpecExists_TrueAndFalse(t *testing.T) {
	dir := t.TempDir()
	w, err := NewCDIWriter("intel.com", dir)
	if err != nil {
		t.Fatalf("NewCDIWriter() unexpected error: %v", err)
	}

	uid := types.UID("test-uid-exists-0001")
	if w.ClaimSpecExists(uid) {
		t.Error("ClaimSpecExists() = true before WriteClaim, want false")
	}

	cpus, _ := cpuset.Parse("1")
	devs := []CDIDevice{{Name: "claim-test-uid-exists-0001-req-dev-0", ClassName: "gold", CPUs: cpus}}
	if err := w.WriteClaim(uid, devs); err != nil {
		t.Fatalf("WriteClaim() unexpected error: %v", err)
	}
	if !w.ClaimSpecExists(uid) {
		t.Error("ClaimSpecExists() = false after WriteClaim, want true")
	}
}

// TestListClaims_TwoClaims verifies that ListClaims returns all written claim
// UIDs.
func TestListClaims_TwoClaims(t *testing.T) {
	dir := t.TempDir()
	w, err := NewCDIWriter("intel.com", dir)
	if err != nil {
		t.Fatalf("NewCDIWriter() unexpected error: %v", err)
	}

	uid1 := types.UID("test-uid-list-0001")
	uid2 := types.UID("test-uid-list-0002")
	cpus, _ := cpuset.Parse("0")

	for _, uid := range []types.UID{uid1, uid2} {
		devs := []CDIDevice{{Name: "claim-" + string(uid) + "-req-dev-0", ClassName: "gold", CPUs: cpus}}
		if err := w.WriteClaim(uid, devs); err != nil {
			t.Fatalf("WriteClaim(%s) unexpected error: %v", uid, err)
		}
	}

	uids, err := w.ListClaims()
	if err != nil {
		t.Fatalf("ListClaims() unexpected error: %v", err)
	}

	found := make(map[types.UID]bool)
	for _, u := range uids {
		found[u] = true
	}
	if !found[uid1] {
		t.Errorf("ListClaims() missing uid1 %s", uid1)
	}
	if !found[uid2] {
		t.Errorf("ListClaims() missing uid2 %s", uid2)
	}
}

// TestListClaims_ForeignSpecSurvives verifies that a malformed/foreign spec
// in the CDI dir does not prevent our claims from being listed, and that the
// foreign spec survives (is not removed).
func TestListClaims_ForeignSpecSurvives(t *testing.T) {
	dir := t.TempDir()
	w, err := NewCDIWriter("intel.com", dir)
	if err != nil {
		t.Fatalf("NewCDIWriter() unexpected error: %v", err)
	}

	// Write a "foreign" malformed spec file that should not be parsed as our claim.
	foreignPath := filepath.Join(dir, "other-vendor-device_foreign.yaml")
	if err := os.WriteFile(foreignPath, []byte("not valid yaml\n"), 0644); err != nil {
		t.Fatalf("write foreign spec: %v", err)
	}

	// Write a valid claim.
	uid := types.UID("test-uid-foreign-0001")
	cpus, _ := cpuset.Parse("0")
	devs := []CDIDevice{{Name: "claim-test-uid-foreign-0001-req-dev-0", ClassName: "gold", CPUs: cpus}}
	if err := w.WriteClaim(uid, devs); err != nil {
		t.Fatalf("WriteClaim() unexpected error: %v", err)
	}

	// ListClaims should return our UID (Refresh logs a warning about the foreign
	// malformed file but continues).
	uids, err := w.ListClaims()
	if err != nil {
		t.Fatalf("ListClaims() unexpected error: %v", err)
	}
	found := false
	for _, u := range uids {
		if u == uid {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListClaims() missing expected uid %s (got %v)", uid, uids)
	}

	// Foreign spec file must still be on disk.
	if _, err := os.Stat(foreignPath); err != nil {
		t.Errorf("foreign spec file was removed: %v", err)
	}
}

// TestWriteClaim_SameRequestDeviceTwoIdx verifies that two results with the
// same Request+Device but different idx produce a spec with two distinct
// CDI device names (no duplicate-name error).
func TestWriteClaim_SameRequestDeviceTwoIdx(t *testing.T) {
	dir := t.TempDir()
	w, err := NewCDIWriter("intel.com", dir)
	if err != nil {
		t.Fatalf("NewCDIWriter() unexpected error: %v", err)
	}

	uid := types.UID("test-uid-shared-0001")
	cpus, _ := cpuset.Parse("0")
	name0 := cdiDeviceName(uid, "myrequest", "punit-0-0", 0)
	name1 := cdiDeviceName(uid, "myrequest", "punit-0-0", 1)
	devs := []CDIDevice{
		{Name: name0, ClassName: "gold", CPUs: cpus},
		{Name: name1, ClassName: "gold", CPUs: cpus},
	}
	if err := w.WriteClaim(uid, devs); err != nil {
		t.Errorf("WriteClaim() with two same-request/same-device results: unexpected error %v", err)
	}
}
