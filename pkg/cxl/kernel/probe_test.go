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

package kernel_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// The private dax device nodes that a stopped zeroing or probe left under
// DevRoot are removed; other directories stay.
func TestRemoveDaxNodeDirs(t *testing.T) {
	dev := t.TempDir()
	k := &kernel.Kernel{DevRoot: dev}
	left := []string{".cxl-dax-1", ".cxl-dax-2"}
	for _, d := range append(slices.Clone(left), "dax0.0-dir", ".cxl-other", ".cxl-probe-3") {
		if err := os.MkdirAll(filepath.Join(dev, d, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := k.RemoveDaxNodeDirs()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, d := range left {
		want = append(want, filepath.Join(dev, d))
	}
	slices.Sort(removed)
	if !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	for _, d := range []string{"dax0.0-dir", ".cxl-other"} {
		if _, err := os.Stat(filepath.Join(dev, d)); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	if removed, err := k.RemoveDaxNodeDirs(); err != nil || len(removed) != 0 {
		t.Fatalf("removed again %v, %v", removed, err)
	}
}
