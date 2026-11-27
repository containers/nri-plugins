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
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// updateObserved makes TestObserverReadsStateJSON write the observations
// of the fixtures into their observed.json.
var updateObserved = flag.Bool("update-observed", false, "write testdata/observe/*/observed.json")

// observedParts are the parts of a VM observation that mbt-observe.py
// takes from the driver's state.
type observedParts struct {
	State      *ObsDriverState          `json:"state"`
	PoolClaims map[string]*ObsPoolClaim `json:"poolClaims"`
}

// decodeStrictly decodes data into v, refusing unknown fields.
func decodeStrictly(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// canonical returns data re-encoded with sorted keys.
func canonical(t *testing.T, data []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestObserverReadsStateJSON runs mbt-observe.py over the fixtures of
// testdata/observe (a state.json of the driver each): the parts that it
// derives from state.json are those of the fixture's observed.json, and
// they carry exactly the fields of the model's observation types.
func TestObserverReadsStateJSON(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dirs, err := filepath.Glob(filepath.Join("testdata", "observe", "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures in testdata/observe: %v", err)
	}
	// testdata/mbt-observe.py links the observer into the module: go
	// test caches a result only over the files of the module that the
	// test opens, and these are the files that python reads
	script, observer := filepath.Join("testdata", "observe-state.py"), filepath.Join("testdata", "mbt-observe.py")
	inputs, err := filepath.Glob(filepath.Join("testdata", "observe", "*", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range append(inputs, script, observer) {
		if _, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}
	args := append([]string{"-I", "-B", script, observer}, dirs...)
	cmd := exec.Command(python, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("observe-state.py: %v\n%s", err, stderr.String())
	}
	seen := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var obs struct {
			Dir      string          `json:"dir"`
			Observed json.RawMessage `json:"observed"`
			Errors   []string        `json:"errors"`
		}
		if err := json.Unmarshal(sc.Bytes(), &obs); err != nil {
			t.Fatal(err)
		}
		seen++
		if len(obs.Errors) > 0 {
			t.Errorf("%s: errors %v", obs.Dir, obs.Errors)
		}
		path := filepath.Join(obs.Dir, "observed.json")
		if *updateObserved {
			var indented bytes.Buffer
			if err := json.Indent(&indented, obs.Observed, "", "  "); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(indented.Bytes(), '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonical(t, obs.Observed); got != canonical(t, want) {
			t.Errorf("%s: from state.json\n%s\nexpected (%s)\n%s", obs.Dir, got, path, canonical(t, want))
		}
		var parts observedParts
		if err := decodeStrictly(obs.Observed, &parts); err != nil {
			t.Errorf("%s: the parts do not decode into the observation types: %v", obs.Dir, err)
			continue
		}
		if parts.State == nil || parts.State.Leases == nil {
			t.Errorf("%s: no state, or no leases", obs.Dir)
			continue
		}
		if filepath.Base(obs.Dir) == "records" {
			checkRichParts(t, &parts)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(dirs) {
		t.Errorf("%d observations of %d fixtures", seen, len(dirs))
	}
}

// checkRichParts checks that the parts of the fixture of every kind of
// record have something of each part.
func checkRichParts(t *testing.T, parts *observedParts) {
	t.Helper()
	st := parts.State
	lend, block := false, false
	for _, pc := range parts.PoolClaims {
		lend = lend || pc.Lend != nil && string(*pc.Lend) != "null"
		for _, d := range pc.Devices {
			block = block || d.BlockDevice != nil
		}
	}
	uses := 0
	for _, m := range st.LocalMemdevs {
		for _, n := range m.Claims {
			uses += n
		}
	}
	local, pool := st.Prepared[localDriver], st.Prepared[poolDriver]
	if len(local) == 0 || len(pool) == 0 || uses == 0 || len(st.LeaseClaims) == 0 || len(st.Leases) == 0 || !lend || !block {
		t.Errorf("records: prepared %v and %v, %d local slices, %d lease claims, %d leases, a lend %v, a block device %v",
			local, pool, uses, len(st.LeaseClaims), len(st.Leases), lend, block)
	}
}
