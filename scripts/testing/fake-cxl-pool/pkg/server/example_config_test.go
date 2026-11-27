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

package server

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The example config and the config of its second server parse.
func TestExampleConfig(t *testing.T) {
	const path = "../../config.example.yaml"
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// one value of each section
	if cfg.Name != "fake-cxl-pool" || cfg.Listen != "127.0.0.1:9909" || uint64(*cfg.SerialBase) != 0xc1ee0000 ||
		cfg.Dir != "/tmp/fake-cxl-pool" || len(cfg.Devices) != 3 || !cfg.Devices[2].Persistent ||
		time.Duration(cfg.Discovery.Interval) != 10*time.Second || time.Duration(cfg.DetachTimeout) != 15*time.Second {
		t.Fatalf("unexpected config %+v", cfg)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(b), "# Several pool servers\n")
	if !ok {
		t.Fatal("no \"Several pool servers\" block")
	}
	var lines []string
	for _, l := range strings.Split(block, "\n") {
		if y, ok := strings.CutPrefix(l, "#   "); ok {
			lines = append(lines, y)
		}
	}
	cfg, err = ParseConfig([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(lines, "\n"))
	}
	if cfg.Name != "pool-b" || cfg.StateFile != "/tmp/fake-cxl-pool-b.state.json" {
		t.Fatalf("unexpected config of the second server %+v", cfg)
	}
}
