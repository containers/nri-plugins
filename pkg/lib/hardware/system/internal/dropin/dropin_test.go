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

package dropin

import (
	"os"
	"strings"
	"testing"
)

// TestSourcesAreIdentical checks that the two api.go files differ only in their
// package clause and their import of the package under test.
func TestSourcesAreIdentical(t *testing.T) {
	viaSysfs := mustRead(t, "viasysfs/api.go")
	viaSystem := mustRead(t, "viasystem/api.go")

	if len(viaSysfs) != len(viaSystem) {
		t.Fatalf("the two files have %d and %d lines; they must differ only in "+
			"the package clause, the package doc and the import",
			len(viaSysfs), len(viaSystem))
	}

	for i := range viaSysfs {
		a, b := viaSysfs[i], viaSystem[i]
		if a == b || allowedDifference(a, b) {
			continue
		}
		t.Errorf("line %d differs beyond the import:\n  viasysfs:  %s\n  viasystem: %s",
			i+1, a, b)
	}
}

// allowedDifference reports whether two corresponding lines are allowed to
// differ: the package clause, the package doc naming the other file, and the
// import of the package under test.
func allowedDifference(a, b string) bool {
	switch {
	case strings.HasPrefix(a, "package ") && strings.HasPrefix(b, "package "):
		return true
	case strings.HasPrefix(strings.TrimSpace(a), "//"):
		return strings.HasPrefix(strings.TrimSpace(b), "//")
	case strings.Contains(a, "nri-plugins/pkg/sysfs"):
		return strings.Contains(b, "nri-plugins/pkg/lib/hardware/system")
	}
	return false
}

func mustRead(t *testing.T, path string) []string {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(blob), "\n"), "\n")
}
