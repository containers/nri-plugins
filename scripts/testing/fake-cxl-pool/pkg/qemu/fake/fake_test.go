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

package fake

import (
	"context"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
)

var (
	pmemData = qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: "fcp_llm0.hp1", Size: 512 << 20, MemPath: "/pool/llm0.raw", Share: true}
	pmemLSA  = qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: "fcp_llm0.hp1-lsa", Size: 256 << 10, MemPath: "/pool/llm0.lsa", Share: true}
	pmemDev  = qemu.CXLType3{ID: "fcp_llm0.hp1", Bus: "ds0", PersistentMemdev: "fcp_llm0.hp1", LSA: "fcp_llm0.hp1-lsa", Serial: 0xc1ae0001}
)

// device returns the cxl-type3 device of the tree by id, nil if not found.
func device(t *qemu.Tree, id string) *qemu.Type3Device {
	for i := range t.Type3 {
		if t.Type3[i].ID == id {
			return &t.Type3[i]
		}
	}
	return nil
}

func TestFakePersistentChecks(t *testing.T) {
	ctx := context.Background()
	f := NewQemu(map[string]int{"hb0": 0}, map[string]string{"ds0": "hb0", "ds1": "hb0"})
	for _, b := range []qemu.MemoryBackend{pmemData, pmemLSA, {QomType: qemu.MemoryBackendFile, ID: "other", Size: 1 << 28, MemPath: "/x", Share: true}} {
		if err := f.ObjectAdd(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		d    qemu.CXLType3
		want string
	}{
		{qemu.CXLType3{ID: "a", Bus: "ds0", Serial: 1}, "at least one memdev property must be set"},
		{qemu.CXLType3{ID: "b", Bus: "ds0", PersistentMemdev: "fcp_llm0.hp1", Serial: 1}, "lsa property must be set for persistent devices"},
		{qemu.CXLType3{ID: "c", Bus: "ds0", PersistentMemdev: "nosuch", LSA: "fcp_llm0.hp1-lsa", Serial: 1}, "Property 'cxl-type3.persistent-memdev' can't find value 'nosuch'"},
		{qemu.CXLType3{ID: "d", Bus: "ds0", PersistentMemdev: "fcp_llm0.hp1", LSA: "nosuch", Serial: 1}, "Property 'cxl-type3.lsa' can't find value 'nosuch'"},
		{qemu.CXLType3{ID: "e", Bus: "ds0", PersistentMemdev: "fcp_llm0.hp1", LSA: "fcp_llm0.hp1", Serial: 1}, "memory backend fcp_llm0.hp1 can't be used multiple times."},
	} {
		if err := f.DeviceAdd(ctx, tc.d); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("device_add %+v: expected %q, got %v", tc.d, tc.want, err)
		}
	}
	if err := f.DeviceAdd(ctx, pmemDev); err != nil {
		t.Fatal(err)
	}
	// both backends are in use now
	for _, d := range []qemu.CXLType3{
		{ID: "f", Bus: "ds1", PersistentMemdev: "other", LSA: "fcp_llm0.hp1-lsa", Serial: 2},
		{ID: "g", Bus: "ds1", VolatileMemdev: "fcp_llm0.hp1", Serial: 2},
	} {
		if err := f.DeviceAdd(ctx, d); err == nil || !strings.Contains(err.Error(), "can't be used multiple times") {
			t.Fatalf("device_add %+v of a busy backend: %v", d, err)
		}
	}
	for _, id := range []string{pmemData.ID, pmemLSA.ID} {
		if err := f.ObjectDel(ctx, id); err == nil || !strings.Contains(err.Error(), "is in use") {
			t.Fatalf("object_del %s of a busy backend: %v", id, err)
		}
	}
	tree, err := f.QueryTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := device(tree, "fcp_llm0.hp1"); d == nil || d.PersistentMemdev != "fcp_llm0.hp1" || d.LSA != "fcp_llm0.hp1-lsa" ||
		d.VolatileMemdev != "" || d.Memdev() != "fcp_llm0.hp1" {
		t.Fatalf("unexpected tree device %+v", d)
	}
	if err := f.DeviceDel(ctx, pmemDev.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{pmemData.ID, pmemLSA.ID} {
		if err := f.ObjectDel(ctx, id); err != nil {
			t.Fatalf("object_del %s after device_del: %v", id, err)
		}
	}
}
