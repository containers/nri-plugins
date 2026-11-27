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
	"path/filepath"
	"testing"
)

func TestBackingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "d0.raw")
	if err := ensureBackingFile(path, 256<<20); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() != 256<<20 {
		t.Fatalf("unexpected file %v %v", st, err)
	}
	// a larger existing file is kept
	if err := ensureBackingFile(path, 1<<20); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() != 256<<20 {
		t.Fatalf("file was shrunk")
	}
	if err := removeBackingFile(path); err != nil {
		t.Fatal(err)
	}
	if err := removeBackingFile(path); err != nil {
		t.Fatal(err)
	}
	// the label storage area file of a data file
	for in, want := range map[string]string{
		"/pool/llm0.raw":     "/pool/llm0.lsa",
		"/pool/llm0":         "/pool/llm0.lsa",
		"/pool/a.b/llm0.img": "/pool/a.b/llm0.lsa",
		"/pool/llm0.x.raw":   "/pool/llm0.x.lsa",
	} {
		if got := lsaPath(in); got != want {
			t.Errorf("lsaPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"dev0", "shared0", "n4-cxl-fedora-43-containerd.memdev3", "a"} {
		if err := validName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", "Dev0", "a_b", "-a", "a@b", "a/b"} {
		if err := validName(n); err == nil {
			t.Errorf("%q: expected error", n)
		}
	}
}
