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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

const (
	serial0 = 0xc1f00001
	serial1 = 0xc1f00002
	serial2 = 0xc1f00003
	mib     = 1 << 20
	gib     = 1 << 30
	// bs is the memory block size of the fake.
	bs = 128 * mib
	// window0 is the start of the ram window of host bridge 12.
	window0 = 0x250000000
)

// newNode returns a fake with mem0 (256 MiB of RAM, serial0) plugged under
// host bridge 12, and a kernel on it.
func newNode(t *testing.T) (*fake.Fake, *kernel.Kernel) {
	t.Helper()
	f := fake.New(t)
	if name := f.Plug(fake.MemdevSpec{Serial: serial0}); name != "mem0" {
		t.Fatalf("plugged %s, expected mem0", name)
	}
	return f, f.Kernel()
}

// ctxTimeout returns a context that ends after d.
func ctxTimeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// wantErr checks that err has the kind (nil: any) and its text has want.
func wantErr(t *testing.T, err, kind error, want string) {
	t.Helper()
	switch {
	case err == nil:
		t.Fatalf("expected an error with %q, got none", want)
	case kind != nil && !errors.Is(err, kind):
		t.Fatalf("error %q is not of kind %v", err, kind)
	case !strings.Contains(err.Error(), want):
		t.Fatalf("error %q does not have %q", err, want)
	}
}

// createRegion creates a region of the type on the memdev under the root
// decoder of its host bridge and returns its name.
func createRegion(t *testing.T, k *kernel.Kernel, memdev, typ string, size uint64) string {
	t.Helper()
	d, err := k.RootDecoder(memdev, typ)
	noErr(t, err)
	r, err := k.CreateRegion(context.Background(), kernel.CreateRegionRequest{Memdev: memdev, Decoder: d, Type: typ, Size: size})
	noErr(t, err)
	return r
}

// observe returns a snapshot of the fake.
func observe(t *testing.T, k *kernel.Kernel) *kernel.Snapshot {
	t.Helper()
	s, err := k.Observe(context.Background())
	noErr(t, err)
	return s
}
