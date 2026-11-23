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

// Package klogtest captures the klog output of a test.
package klogtest

import (
	"flag"
	"strconv"
	"strings"
	"sync"
	"testing"

	"k8s.io/klog/v2"
)

// Capture makes klog write to a buffer at the verbosity v for the test,
// each line once, and returns a function that returns the output so far.
func Capture(t testing.TB, v int) func() string {
	t.Helper()
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	for name, value := range map[string]string{"logtostderr": "false", "alsologtostderr": "false",
		"one_output": "true", "v": strconv.Itoa(v)} {
		if err := fs.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var buf strings.Builder
	klog.SetOutput(writer(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() {
		klog.Flush()
		_ = fs.Set("logtostderr", "true")
		_ = fs.Set("one_output", "false")
		_ = fs.Set("v", "0")
	})
	return func() string {
		klog.Flush()
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// writer is an io.Writer of a function.
type writer func([]byte) (int, error)

func (f writer) Write(p []byte) (int, error) { return f(p) }
