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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// On a store in LOST mode the driver starts (no crash loop): the device
// engine recovers nothing, the pool driver, the node state and the lease
// broker start empty, the first Prepare fails with the text of the lost
// state, and nothing is written: the document and the CDI specs stay as
// they are and no other JSON file appears.
func TestStartOnALostStore(t *testing.T) {
	e := newDriverTestEnv(t)
	e.config.Flags.Leases = true
	local := e.config.KubeletPluginDir
	pool := filepath.Join(filepath.Dir(local), device.PoolDriverName)
	noErr(t, os.WriteFile(filepath.Join(local, store.FileName), []byte("{"), 0o600))
	// the CDI specs of claims prepared before the store was lost
	specs := map[string]string{}
	for name, kind := range map[string]string{device.ClaimsCDISpecName: device.CDIKind(device.DriverName),
		device.PoolCDISpecName: device.CDIKind(device.PoolDriverName)} {
		specs[name] = `{"cdiVersion":"0.6.0","kind":"` + kind + `","devices":[{"name":"claim-u0-0","containerEdits":{"env":["A=1"]}}]}`
		noErr(t, os.WriteFile(filepath.Join(e.config.Flags.CDIRoot, name), []byte(specs[name]), 0o600))
	}
	e.start()
	if r := e.d.pool.Records(); len(r.Preparations) != 0 || len(e.d.pool.Serials()) != 0 {
		t.Errorf("pool driver: preparations %v, serials %v", r.Preparations, e.d.pool.Serials())
	}
	if uses := e.d.publisher.LocalRecords().Memdevs; len(uses) != 0 {
		t.Errorf("local uses %v", uses)
	}
	e.synchronize()
	// the memdev is held: the serials of pool devices are lost
	injectDevices(e, &dra.DeviceInfo{Name: "dram", Dev: &dra.DRAM{NUMANodes: []int{0}, Size: 1 << 30}})
	claim := localClaim("u1", 0)
	claim.Status.Allocation.Devices.Results[0].Device = "dram"
	claim.Status.Allocation.Devices.Results[0].ConsumedCapacity = nil
	if err := e.prepare(e.d.local, claim); err == nil || !strings.Contains(err.Error(), "the driver state is lost") ||
		!strings.Contains(err.Error(), "--new-state") {
		t.Errorf("Prepare in LOST mode: %v", err)
	}
	if e.d.local.ClaimInfo("u1") != nil {
		t.Error("the claim is prepared")
	}
	noErr(t, e.unprepare(e.d.local, "u1"))
	e.stop()
	if data, err := os.ReadFile(filepath.Join(local, store.FileName)); err != nil || string(data) != "{" {
		t.Errorf("the lost document was rewritten: %q %v", data, err)
	}
	for name, want := range specs {
		if data, err := os.ReadFile(filepath.Join(e.config.Flags.CDIRoot, name)); err != nil || string(data) != want {
			t.Errorf("CDI spec %s changed in LOST mode: %q %v", name, data, err)
		}
	}
	for _, dir := range []string{local, pool} {
		files, err := filepath.Glob(filepath.Join(dir, "*.json"))
		noErr(t, err)
		for _, f := range files {
			if f != filepath.Join(local, store.FileName) {
				t.Errorf("%s written in LOST mode", f)
			}
		}
	}
}
