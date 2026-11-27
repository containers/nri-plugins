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

package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
)

// A pool Prepare and a local Prepare of two memdevs run at once: the engine
// lock serializes their kernel commands and sysfs writes, no two overlap.
func TestEngineLockSerializesKernelCalls(t *testing.T) {
	for round := 0; round < 3; round++ {
		v := newEnv(t, stackSpecs...)
		var inside, overlaps, calls atomic.Int32
		var check func()
		check = func() {
			v.f.Before("", check)
			calls.Add(1)
			if inside.Add(1) > 1 {
				overlaps.Add(1)
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
		}
		v.f.Before("", check)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = v.prepare(poolDriver, "p", poolReq("p", 0, serialB, state.ModeNUMA, state.KindPool))
		}()
		go func() {
			defer wg.Done()
			errs[1] = v.prepare(localDriver, "l", sliceReq("l", 0, state.ModeNUMA, 256*mib))
		}()
		wg.Wait()
		v.f.Before("", func() {})
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := overlaps.Load(); n > 0 {
			t.Fatalf("%d of %d kernel calls overlapped another", n, calls.Load())
		}
		v.mustUnprepare(poolDriver, "p")
		v.mustUnprepare(localDriver, "l")
	}
}
