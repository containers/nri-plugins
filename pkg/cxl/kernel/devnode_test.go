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
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// OpenDevice opens a block device read-write and read-only, waits for a
// node that appears, and fails at once with another error.
func TestOpenBlockDevice(t *testing.T) {
	k := &kernel.Kernel{}
	ctx := context.Background()
	dir := t.TempDir()
	dev := filepath.Join(dir, "pmem0")
	noErr(t, os.WriteFile(dev, []byte("BLK"), 0o600))
	for _, readOnly := range []bool{false, true} {
		f, err := k.OpenDevice(ctx, dev, readOnly)
		noErr(t, err)
		_, werr := f.Write([]byte("x"))
		_ = f.Close()
		if (werr == nil) == readOnly {
			t.Fatalf("read-only %v: write error %v", readOnly, werr)
		}
	}
	late := filepath.Join(dir, "pmem1")
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(late, []byte("BLK"), 0o600)
	}()
	f, err := k.OpenDevice(ctx, late, true)
	noErr(t, err)
	_ = f.Close()
	start := time.Now()
	if _, err := k.OpenDevice(ctx, filepath.Join(dev, "x"), true); err == nil || time.Since(start) > time.Second {
		t.Fatalf("open below a file: %v after %v", err, time.Since(start))
	}
}
