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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestObserverDaxReads runs testdata/observe-dax.py. mbt-observe.py counts
// the online memory of a dax device from the ranges in /proc/iomem. It
// opens no mappingN file of a dax device. Its stacks of a slow observation
// have the marker that the executor looks for, the kernel stacks and the
// Python stacks.
func TestObserverDaxReads(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	script, observer := filepath.Join("testdata", "observe-dax.py"), filepath.Join("testdata", "mbt-observe.py")
	executor := filepath.Join("testdata", "mbt.source.sh")
	// go test caches a result only over the files of the module that the
	// test opens: the observer and the executor are links in testdata
	for _, path := range []string{script, observer, executor} {
		if _, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(python, "-I", "-B", script, observer, executor).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("observe-dax.py: %v\n%s", err, out)
	}
}
