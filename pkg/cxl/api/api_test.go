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

package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRequestResponseRoundTrip(t *testing.T) {
	req := Request{Op: OpRequest, Source: SourcePool, Mode: ModeFS, Share: true, ReadOnly: ptr(false), Access: AccessCached,
		FSType: "ext4", Name: "models", Size: "256Mi", Serial: "0xc1ae0001", Wait: "10s", RequestID: "r1", Lease: "1a2b3c4d", All: true}
	data, err := json.Marshal(&req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"op":"request","source":"pool","mode":"fs","share":true,"readOnly":false,"access":"cached","fsType":"ext4","name":"models","size":"256Mi","serial":"0xc1ae0001","wait":"10s","requestID":"r1","lease":"1a2b3c4d","all":true}`
	if string(data) != want {
		t.Fatalf("request JSON\n%s\nwant\n%s", data, want)
	}
	var back Request
	if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back, req) {
		t.Fatalf("request round trip %+v %v", back, err)
	}
	if data, _ := json.Marshal(&Request{Op: OpList}); string(data) != `{"op":"list"}` {
		t.Fatalf("minimal request %s", data)
	}

	node := 2
	resp := Response{Lease: "1a2b3c4d", State: StateReady, Source: SourceLocal, Mode: ModeNUMA, Size: 536870912,
		Serial: "0xc100e2e1", NUMANode: &node, Env: map[string]string{"CXL_C100E2E1_MODE": "numa"},
		Leases: &[]Response{{Lease: "00000001", State: StatePending}}}
	data, err = json.Marshal(&resp)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"lease":"1a2b3c4d","state":"Ready","source":"local","mode":"numa","share":false,"readOnly":false,"size":536870912,"serial":"0xc100e2e1","numaNode":2,"env":{"CXL_C100E2E1_MODE":"numa"},"leases":[{"lease":"00000001","state":"Pending","share":false,"readOnly":false}]}`
	if string(data) != want {
		t.Fatalf("response JSON\n%s\nwant\n%s", data, want)
	}
	var rback Response
	if err := json.Unmarshal(data, &rback); err != nil || !reflect.DeepEqual(rback, resp) {
		t.Fatalf("response round trip %+v %v", rback, err)
	}

	fs := Response{Lease: "1a2b3c4d", State: StateReady, Source: SourcePool, Mode: ModeFS, Share: true, ReadOnly: true,
		Access: AccessDirect, FSType: "erofs", Path: FSDir + "/models", Size: 534773760, Serial: "0xc1ae0001"}
	data, err = json.Marshal(&fs)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"lease":"1a2b3c4d","state":"Ready","source":"pool","mode":"fs","share":true,"readOnly":true,"access":"direct","fsType":"erofs","path":"/run/cxl/fs/models","size":534773760,"serial":"0xc1ae0001"}`
	if string(data) != want {
		t.Fatalf("fs response JSON\n%s\nwant\n%s", data, want)
	}

	block := Response{Lease: "1a2b3c4d", State: StateReady, Source: SourcePool, Mode: ModeBlock, Share: false, ReadOnly: false,
		Block: "/dev/pmem0", Size: 534773760, Serial: "0xc1ae0001"}
	data, err = json.Marshal(&block)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"lease":"1a2b3c4d","state":"Ready","source":"pool","mode":"block","share":false,"readOnly":false,"size":534773760,"serial":"0xc1ae0001","block":"/dev/pmem0"}`
	if string(data) != want {
		t.Fatalf("block response JSON\n%s\nwant\n%s", data, want)
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		req Request
		err string
	}{
		{Request{Op: OpRequest, Size: "512Mi"}, ""},
		{Request{Op: OpRequest, Serial: "0xc1ae0001"}, ""},
		{Request{Op: OpRequest, Share: true}, ""},
		{Request{Op: OpRequest, Share: true, ReadOnly: ptr(true), Serial: "3249405953"}, ""},
		{Request{Op: OpRequest, Source: SourceLocal, Size: "64Mi", Mode: ModeDax, ReadOnly: ptr(true)}, ""},
		{Request{Op: OpRequest, Size: "1Gi", Wait: "0s", RequestID: "r1"}, ""},
		{Request{Op: OpRelease, Lease: "1a2b3c4d"}, ""},
		{Request{Op: OpRelease, All: true, Wait: "1m"}, ""},
		{Request{Op: OpList}, ""},
		{Request{Op: OpFd, Lease: "1a2b3c4d"}, ""},
		{Request{Op: OpEnv, Lease: "1a2b3c4d", Wait: "60s"}, ""},
		{Request{Op: OpRequest, Size: "1Gi", Source: "remote"}, `invalid source "remote" (local or pool)`},
		{Request{Op: OpRequest, Size: "1Gi", Source: "any"}, `invalid source "any" (local or pool)`},
		{Request{Op: OpRequest, Size: "1Gi", Mode: "devdax"}, `invalid mode "devdax" (numa, dax, fs or block)`},
		// mode block: the whole device may be written, a share is read-only
		{Request{Op: OpRequest, Serial: "0xc1ae0001", Mode: ModeBlock}, ""},
		{Request{Op: OpRequest, Serial: "0xc1ae0001", Mode: ModeBlock, Source: SourcePool, ReadOnly: ptr(true)}, ""},
		{Request{Op: OpRequest, Serial: "0xc1ae0001", Mode: ModeBlock, Share: true, ReadOnly: ptr(true)}, ""},
		{Request{Op: OpRequest, Size: "1Gi", Mode: ModeBlock}, ""},
		{Request{Op: OpRequest, Serial: "0xc1ae0001", Mode: ModeBlock, Share: true}, "mode block with share needs readOnly"},
		{Request{Op: OpRequest, Mode: ModeBlock, Share: true, Source: SourceLocal}, "mode block with share needs readOnly"},
		{Request{Op: OpRequest, Mode: ModeBlock, Access: AccessDirect, Size: "64Mi"}, "access needs mode fs"},
		{Request{Op: OpRequest, Mode: ModeBlock, Name: "x", Size: "64Mi"}, "name needs mode fs"},
		// local persistent memdevs have mode block
		{Request{Op: OpRequest, Source: SourceLocal, Mode: ModeBlock, Size: "64Mi"}, ""},
		{Request{Op: OpRequest, Source: SourceLocal, Mode: ModeBlock, Share: true, ReadOnly: ptr(true)}, ""},
		{Request{Op: OpRequest, Mode: ModeBlock}, "size or serial is required"},
		// mode fs
		{Request{Op: OpRequest, Share: true, ReadOnly: ptr(true), Mode: ModeFS, Serial: "0xc1ae0001"}, ""},
		{Request{Op: OpRequest, Share: true, ReadOnly: ptr(true), Mode: ModeFS, Source: SourcePool, Access: AccessCached,
			Name: "models", Serial: "0xc1ae0001"}, ""},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: ModeFS, Access: AccessDirect, Serial: "0xc1ae0001"}, ""},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: ModeFS, Name: "Model_1.0-x", Size: "64Mi"}, ""},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: ModeFS, Name: strings.Repeat("n", 255), Size: "64Mi"}, ""},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: ModeFS, Source: "remote", Access: "slow"}, `invalid source "remote" (local or pool)`},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: "memory", Access: "slow"}, `invalid mode "memory" (numa, dax, fs or block)`},
		{Request{Op: OpRequest, Share: true, ReadOnly: ptr(true), Mode: ModeFS, Access: "slow", Size: "64Mi"}, `invalid access "slow" (direct or cached)`},
		{Request{Op: OpRequest, Access: "slow", Size: "64Mi"}, `invalid access "slow" (direct or cached)`},
		{Request{Op: OpRequest, Share: true, Mode: ModeNUMA, Access: AccessCached}, "shared memory is never onlined as RAM: use mode dax"},
		{Request{Op: OpRequest, Access: AccessCached, Size: "64Mi"}, "access needs mode fs"},
		{Request{Op: OpRequest, Access: AccessDirect, Share: true, Name: "x"}, "access needs mode fs"},
		{Request{Op: OpRequest, Name: "x", Size: "64Mi"}, "name needs mode fs"},
		{Request{Op: OpRequest, Name: "a/b", Mode: ModeDax, Size: "64Mi"}, "name needs mode fs"},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: ModeFS, Name: "a/b", Size: "64Mi"},
			`invalid name "a/b" (letters, digits, ".", "_" and "-", not "." or "..")`},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: ModeFS, Name: ".", Size: "64Mi"}, `invalid name "."`},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: ModeFS, Name: "..", Size: "64Mi"}, `invalid name ".."`},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: ModeFS, Name: "a b", Size: "64Mi"}, `invalid name "a b"`},
		{Request{Op: OpRequest, ReadOnly: ptr(true), Mode: ModeFS, Name: strings.Repeat("n", 256), Size: "64Mi"}, `invalid name "nnn`},
		{Request{Op: OpRequest, Mode: ModeFS, Name: "a/b", Size: "64Mi"}, `invalid name "a/b"`},
		// mode fs is read-only unless readOnly false, of any source
		{Request{Op: OpRequest, Serial: "0xc1ae0001", Share: true, Mode: ModeFS}, ""},
		{Request{Op: OpRequest, Source: SourceLocal, Mode: ModeFS, Size: "64Mi"}, ""},
		{Request{Op: OpRequest, Source: SourceLocal, Mode: ModeFS, ReadOnly: ptr(true), Share: true, Size: "64Mi"}, ""},
		{Request{Op: OpRequest, Source: SourceLocal, Mode: ModeFS, ReadOnly: ptr(false), Serial: "0xc100f5e1"}, ""},
		{Request{Op: OpRequest, Mode: ModeFS, ReadOnly: ptr(false), FSType: "ext4", Share: true}, ""},
		{Request{Op: OpRequest, Mode: ModeFS, FSType: "my_fs-2", Size: "64Mi"}, ""},
		{Request{Op: OpRequest, Mode: ModeFS, FSType: "0" + strings.Repeat("f", 31), Size: "64Mi"}, ""},
		// name, then fsType
		{Request{Op: OpRequest, Mode: ModeFS, Name: "a/b", FSType: "Ext4", ReadOnly: ptr(false)}, `invalid name "a/b"`},
		{Request{Op: OpRequest, Mode: ModeDax, FSType: "Ext4", ReadOnly: ptr(false), Size: "64Mi"}, "fsType needs mode fs"},
		{Request{Op: OpRequest, FSType: "ext4", Size: "64Mi"}, "fsType needs mode fs"},
		{Request{Op: OpRequest, Share: true, FSType: "ext4"}, "fsType needs mode fs"},
		{Request{Op: OpRequest, Mode: ModeFS, FSType: "Ext4", ReadOnly: ptr(false)}, `invalid fsType "Ext4" (lower-case letters, digits, "_" and "-")`},
		{Request{Op: OpRequest, Mode: ModeFS, FSType: "-x", Size: "64Mi"}, `invalid fsType "-x" (lower-case letters, digits, "_" and "-")`},
		{Request{Op: OpRequest, Mode: ModeFS, FSType: "fuse.sshfs", Size: "64Mi"}, `invalid fsType "fuse.sshfs"`},
		{Request{Op: OpRequest, Mode: ModeFS, FSType: strings.Repeat("f", 33), Size: "64Mi"}, `invalid fsType "fff`},
		// readOnly false is the default of dax and block, of no effect
		{Request{Op: OpRequest, Mode: ModeBlock, ReadOnly: ptr(false), Size: "64Mi"}, ""},
		{Request{Op: OpRequest, Size: "64Mi", ReadOnly: ptr(false)}, ""},
		{Request{Op: OpRequest, Mode: ModeFS, ReadOnly: ptr(true)}, "size or serial is required"},
		{Request{Op: OpRequest, Mode: ModeFS, ReadOnly: ptr(false)}, "size or serial is required"},
		{Request{Op: OpRequest, Mode: ModeFS, ReadOnly: ptr(true), Serial: "c1ae0001"}, `invalid serial "c1ae0001"`},
		{Request{Op: OpRequest, Share: true, Mode: ModeNUMA}, "shared memory is never onlined as RAM: use mode dax"},
		// a share of a shared local device
		{Request{Op: OpRequest, Share: true, Source: SourceLocal}, ""},
		{Request{Op: OpRequest, Share: true, Source: SourceLocal, ReadOnly: ptr(true), Serial: "0xc100e2e1"}, ""},
		{Request{Op: OpRequest, Size: "1Gi", ReadOnly: ptr(true)}, "readOnly needs mode dax, fs or block"},
		{Request{Op: OpRequest, Size: "1Gi", ReadOnly: ptr(true), Mode: ModeNUMA}, "readOnly needs mode dax, fs or block"},
		{Request{Op: OpRequest}, "size or serial is required"},
		{Request{Op: OpRequest, Serial: "c1ae0001"}, `invalid serial "c1ae0001"`},
		{Request{Op: OpRelease}, "release takes lease or all"},
		{Request{Op: OpRelease, Lease: "1a2b3c4d", All: true}, "release takes lease or all"},
		{Request{Op: OpRequest, Size: "1Gi", Wait: "soon"}, `invalid wait "soon"`},
		{Request{Op: OpRelease, All: true, Wait: "-1s"}, `invalid wait "-1s"`},
		{Request{Op: OpRequest, Size: "1Gi", Wait: "1h"}, ""},
		{Request{Op: OpRequest, Size: "1Gi", Wait: "61m"}, `invalid wait "61m" (at most 1h0m0s)`},
		{Request{Op: OpRequest, Size: "1Gi", RequestID: "Pod-1_a." + strings.Repeat("r", 56)}, ""},
		{Request{Op: OpRequest, Size: "1Gi", RequestID: strings.Repeat("r", 65)}, `invalid requestID "rrr`},
		{Request{Op: OpList, RequestID: "a/b"}, `invalid requestID "a/b" (1 to 64 letters, digits, ".", "_" and "-")`},
		{Request{Op: OpRelease, Lease: "1A2B3C4D"}, `invalid lease "1A2B3C4D" (8 lower-case hex digits)`},
		{Request{Op: OpFd, Lease: "1a2b3c4"}, `invalid lease "1a2b3c4"`},
		{Request{Op: OpEnv, Lease: "1a2b3c4d5"}, `invalid lease "1a2b3c4d5"`},
		{Request{Op: OpEnv, Lease: "1a2b3c4g"}, `invalid lease "1a2b3c4g"`},
		{Request{Op: OpFd}, "fd needs lease"},
		{Request{Op: OpEnv}, "env needs lease"},
		{Request{Op: "lease"}, `unknown op "lease"`},
	} {
		err := c.req.Validate()
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%+v: %v", c.req, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%+v: expected error %q, got %v", c.req, c.err, err)
		}
	}
}

func TestEffectiveMode(t *testing.T) {
	for _, c := range []struct {
		req  Request
		mode string
	}{
		{Request{}, ModeNUMA},
		{Request{Share: true}, ModeDax},
		{Request{Mode: ModeDax}, ModeDax},
		{Request{Mode: ModeNUMA}, ModeNUMA},
		{Request{Mode: ModeFS, Share: true}, ModeFS},
		{Request{Mode: ModeBlock, Share: true}, ModeBlock},
	} {
		if got := c.req.EffectiveMode(); got != c.mode {
			t.Errorf("%+v: mode %s, want %s", c.req, got, c.mode)
		}
	}
}

func TestKeys(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{Key(0xc100e2e1, 0), "C100E2E1"},
		{Key(0xc100e2e1, 1), "C100E2E1"},
		{Key(0xc100e2e1, 2), "C100E2E1_2"},
		{Key(0x1, 3), "1_3"},
		{RegionKey("region0"), "REGION0"},
		{RegionKey("fake-region12"), "FAKE_REGION12"},
		{Nth(DRAMKey, 2), "DRAM_2"},
		{Nth(RegionKey("region1"), 1), "REGION1"},
	} {
		if c.got != c.want {
			t.Errorf("key %q, want %q", c.got, c.want)
		}
	}
}

func TestEnv(t *testing.T) {
	for _, c := range []struct {
		name string
		mem  Memory
		want map[string]string
	}{
		{"memdev numa", Memory{Key: Key(0xc100e2e1, 1), Serial: "0xc100e2e1", Mode: ModeNUMA, Size: 134217728, NUMANode: 2},
			map[string]string{"CXL_C100E2E1_SERIAL": "0xc100e2e1", "CXL_C100E2E1_SHARED": "0", "CXL_C100E2E1_MODE": "numa",
				"CXL_C100E2E1_SIZE": "134217728", "CXL_C100E2E1_NUMA_NODE": "2"}},
		{"memdev dax read-only", Memory{Key: Key(0xc100e2e1, 1), Serial: "0xc100e2e1", Mode: ModeDax, Size: 134217728, Dax: "/dev/dax0.1", ReadOnly: true},
			map[string]string{"CXL_C100E2E1_SERIAL": "0xc100e2e1", "CXL_C100E2E1_SHARED": "0", "CXL_C100E2E1_MODE": "dax",
				"CXL_C100E2E1_SIZE": "134217728", "CXL_C100E2E1_DAX": "/dev/dax0.1", "CXL_C100E2E1_READ_ONLY": "1"}},
		{"shared pool device dax", Memory{Key: Key(0xc1ae0001, 1), Serial: "0xc1ae0001", Shared: true, Mode: ModeDax, Size: 268435456, Dax: "/dev/dax0.0"},
			map[string]string{"CXL_C1AE0001_SERIAL": "0xc1ae0001", "CXL_C1AE0001_SHARED": "1", "CXL_C1AE0001_MODE": "dax",
				"CXL_C1AE0001_SIZE": "268435456", "CXL_C1AE0001_DAX": "/dev/dax0.0", "CXL_C1AE0001_READ_ONLY": "0"}},
		{"second slice", Memory{Key: Key(0xc100e2e1, 2), Serial: "0xc100e2e1", Mode: ModeNUMA, Size: 134217728, NUMANode: 0},
			map[string]string{"CXL_C100E2E1_2_SERIAL": "0xc100e2e1", "CXL_C100E2E1_2_SHARED": "0", "CXL_C100E2E1_2_MODE": "numa",
				"CXL_C100E2E1_2_SIZE": "134217728", "CXL_C100E2E1_2_NUMA_NODE": "0"}},
		{"region", Memory{Key: RegionKey("region0"), Mode: ModeNUMA, Size: 67108864, NUMANode: 2},
			map[string]string{"CXL_REGION0_MODE": "numa", "CXL_REGION0_SIZE": "67108864", "CXL_REGION0_NUMA_NODE": "2"}},
		{"dram", Memory{Key: DRAMKey, Mode: ModeNUMA, Size: 67108864, NUMANodes: "0-1"},
			map[string]string{"CXL_DRAM_MODE": "numa", "CXL_DRAM_SIZE": "67108864", "CXL_DRAM_NUMA_NODES": "0-1"}},
		{"pool device block", Memory{Key: Key(0xc1ae0001, 1), Serial: "0xc1ae0001", Shared: true, Mode: ModeBlock, Size: 534773760, Path: "/dev/pmem1"},
			map[string]string{"CXL_C1AE0001_SERIAL": "0xc1ae0001", "CXL_C1AE0001_SHARED": "1", "CXL_C1AE0001_MODE": "block",
				"CXL_C1AE0001_SIZE": "534773760", "CXL_C1AE0001_PATH": "/dev/pmem1", "CXL_C1AE0001_READ_ONLY": "0"}},
		{"pool device fs", Memory{Key: Key(0xc1ae0001, 2), Serial: "0xc1ae0001", Shared: true, Mode: ModeFS, Size: 534773760,
			Path: "/run/cxl-fs/C1AE0001_2", Access: AccessCached, ReadOnly: true},
			map[string]string{"CXL_C1AE0001_2_SERIAL": "0xc1ae0001", "CXL_C1AE0001_2_SHARED": "1", "CXL_C1AE0001_2_MODE": "fs",
				"CXL_C1AE0001_2_SIZE": "534773760", "CXL_C1AE0001_2_PATH": "/run/cxl-fs/C1AE0001_2", "CXL_C1AE0001_2_ACCESS": "cached",
				"CXL_C1AE0001_2_READ_ONLY": "1"}},
		{"pool device fs read-write", Memory{Key: Key(0xc1af0001, 1), Serial: "0xc1af0001", Shared: true, Mode: ModeFS, Size: 534773760,
			Path: "/data", Access: AccessDirect, FSType: "ext4"},
			map[string]string{"CXL_C1AF0001_SERIAL": "0xc1af0001", "CXL_C1AF0001_SHARED": "1", "CXL_C1AF0001_MODE": "fs",
				"CXL_C1AF0001_SIZE": "534773760", "CXL_C1AF0001_PATH": "/data", "CXL_C1AF0001_ACCESS": "direct",
				"CXL_C1AF0001_READ_ONLY": "0", "CXL_C1AF0001_FS_TYPE": "ext4"}},
		{"block has no FS_TYPE", Memory{Key: Key(0xc1af0001, 1), Serial: "0xc1af0001", Mode: ModeBlock, Size: 1, Path: "/dev/pmem0", FSType: "ext4"},
			map[string]string{"CXL_C1AF0001_SERIAL": "0xc1af0001", "CXL_C1AF0001_SHARED": "0", "CXL_C1AF0001_MODE": "block",
				"CXL_C1AF0001_SIZE": "1", "CXL_C1AF0001_PATH": "/dev/pmem0", "CXL_C1AF0001_READ_ONLY": "0"}},
		{"dax lease without a device path", Memory{Key: Key(0xc1ee0001, 1), Serial: "0xc1ee0001", Mode: ModeDax, Size: 1},
			map[string]string{"CXL_C1EE0001_SERIAL": "0xc1ee0001", "CXL_C1EE0001_SHARED": "0", "CXL_C1EE0001_MODE": "dax",
				"CXL_C1EE0001_SIZE": "1", "CXL_C1EE0001_READ_ONLY": "0"}},
	} {
		if got := Env(c.mem); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: env\n%v\nwant\n%v", c.name, got, c.want)
		}
	}
}

func TestExecEnv(t *testing.T) {
	dax := Env(Memory{Key: Key(0xc1ee0001, 1), Serial: "0xc1ee0001", Mode: ModeDax, Size: 536870912, Dax: "/dev/dax0.0"})
	want := []string{"CXL_C1EE0001_FD=3", "CXL_C1EE0001_MODE=dax", "CXL_C1EE0001_READ_ONLY=0", "CXL_C1EE0001_SERIAL=0xc1ee0001",
		"CXL_C1EE0001_SHARED=0", "CXL_C1EE0001_SIZE=536870912", "CXL_LEASE=1a2b3c4d"}
	if got := ExecEnv(dax, "1a2b3c4d", 3); !reflect.DeepEqual(got, want) {
		t.Fatalf("dax exec env\n%v\nwant\n%v", got, want)
	}
	// without an fd the device path stays
	want = []string{"CXL_C1EE0001_DAX=/dev/dax0.0", "CXL_C1EE0001_MODE=dax", "CXL_C1EE0001_READ_ONLY=0", "CXL_C1EE0001_SERIAL=0xc1ee0001",
		"CXL_C1EE0001_SHARED=0", "CXL_C1EE0001_SIZE=536870912", "CXL_LEASE=1a2b3c4d"}
	if got := ExecEnv(dax, "1a2b3c4d", -1); !reflect.DeepEqual(got, want) {
		t.Fatalf("dax exec env without fd\n%v\nwant\n%v", got, want)
	}
	numa := Env(Memory{Key: Key(0xc100e2e1, 1), Serial: "0xc100e2e1", Mode: ModeNUMA, Size: 134217728, NUMANode: 2})
	want = []string{"CXL_C100E2E1_MODE=numa", "CXL_C100E2E1_NUMA_NODE=2", "CXL_C100E2E1_SERIAL=0xc100e2e1", "CXL_C100E2E1_SHARED=0",
		"CXL_C100E2E1_SIZE=134217728", "CXL_LEASE=00000001"}
	if got := ExecEnv(numa, "00000001", -1); !reflect.DeepEqual(got, want) {
		t.Fatalf("numa exec env\n%v\nwant\n%v", got, want)
	}
	// a block lease: the fd takes the place of the device path
	block := Env(Memory{Key: Key(0xc1ae0001, 1), Serial: "0xc1ae0001", Shared: true, Mode: ModeBlock, Size: 534773760, Path: "/dev/pmem0"})
	want = []string{"CXL_C1AE0001_FD=3", "CXL_C1AE0001_MODE=block", "CXL_C1AE0001_READ_ONLY=0", "CXL_C1AE0001_SERIAL=0xc1ae0001",
		"CXL_C1AE0001_SHARED=1", "CXL_C1AE0001_SIZE=534773760", "CXL_LEASE=000000a3"}
	if got := ExecEnv(block, "000000a3", 3); !reflect.DeepEqual(got, want) {
		t.Fatalf("block exec env\n%v\nwant\n%v", got, want)
	}
	want = []string{"CXL_C1AE0001_MODE=block", "CXL_C1AE0001_PATH=/dev/pmem0", "CXL_C1AE0001_READ_ONLY=0", "CXL_C1AE0001_SERIAL=0xc1ae0001",
		"CXL_C1AE0001_SHARED=1", "CXL_C1AE0001_SIZE=534773760", "CXL_LEASE=000000a3"}
	if got := ExecEnv(block, "000000a3", -1); !reflect.DeepEqual(got, want) {
		t.Fatalf("block exec env without fd\n%v\nwant\n%v", got, want)
	}
	// an fs lease keeps its path, also with an fd
	fs := Env(Memory{Key: Key(0xc1af0001, 1), Serial: "0xc1af0001", Shared: true, Mode: ModeFS, Size: 534773760, Path: "/run/cxl/fs/models",
		Access: AccessDirect, ReadOnly: true})
	want = []string{"CXL_C1AF0001_ACCESS=direct", "CXL_C1AF0001_MODE=fs", "CXL_C1AF0001_PATH=/run/cxl/fs/models", "CXL_C1AF0001_READ_ONLY=1",
		"CXL_C1AF0001_SERIAL=0xc1af0001", "CXL_C1AF0001_SHARED=1", "CXL_C1AF0001_SIZE=534773760", "CXL_LEASE=000000a4"}
	for _, fd := range []int{-1, 3} {
		if got := ExecEnv(fs, "000000a4", fd); !reflect.DeepEqual(got, want) {
			t.Fatalf("fs exec env with fd %d\n%v\nwant\n%v", fd, got, want)
		}
	}
	if got := EnvList(map[string]string{"B": "2", "A": "1"}); !reflect.DeepEqual(got, []string{"A=1", "B=2"}) {
		t.Fatalf("env list %v", got)
	}
}

func TestSerials(t *testing.T) {
	for _, c := range []struct {
		in  string
		v   uint64
		err bool
	}{
		{"0xc100e2e1", 0xc100e2e1, false},
		{"0XC100E2E1", 0xc100e2e1, false},
		{"3238060769", 0xc100e2e1, false},
		{"010", 10, false},
		{"0x", 0, true},
		{"c100e2e1", 0, true},
		{"", 0, true},
		{"-1", 0, true},
	} {
		v, err := ParseSerial(c.in)
		if (err != nil) != c.err || v != c.v {
			t.Errorf("ParseSerial(%q) = %v, %v", c.in, v, err)
		}
		if err != nil && err.Error() != `invalid serial "`+c.in+`"` {
			t.Errorf("ParseSerial(%q) error %v", c.in, err)
		}
	}
	if s := FormatSerial(0xc100e2e1); s != "0xc100e2e1" {
		t.Fatalf("FormatSerial %s", s)
	}
}

func TestValidFSType(t *testing.T) {
	valid := []string{"erofs", "ext4", "gfs2", "9p", "my_fs", "a-b", "x", strings.Repeat("a", 32)}
	invalid := []string{"", "Ext4", "_x", "-x", "fuse.sshfs", "a b", "a/b", strings.Repeat("a", 33)}
	for _, name := range valid {
		if !ValidFSType(name) {
			t.Errorf("ValidFSType(%q) = false", name)
		}
	}
	for _, name := range invalid {
		if ValidFSType(name) {
			t.Errorf("ValidFSType(%q) = true", name)
		}
	}
}

func TestEffectiveReadOnly(t *testing.T) {
	for _, c := range []struct {
		req  Request
		want bool
	}{
		{Request{Mode: ModeFS}, true},
		{Request{Mode: ModeFS, ReadOnly: ptr(false)}, false},
		{Request{Mode: ModeDax}, false},
		{Request{Share: true, ReadOnly: ptr(true)}, true},
		{Request{Mode: ModeBlock}, false},
	} {
		if got := c.req.EffectiveReadOnly(); got != c.want {
			t.Errorf("%+v: read-only %v, want %v", c.req, got, c.want)
		}
	}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T {
	return &v
}
