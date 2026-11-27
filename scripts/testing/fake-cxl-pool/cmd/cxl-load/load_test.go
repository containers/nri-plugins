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
	"context"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// goodConfig returns a valid configuration on CPUs 0-3 and nodes 0-1.
func goodConfig() *config {
	return &config{
		nodes:        []int{0, 1},
		cpus:         []int{0, 1, 2, 3},
		bufferSize:   8 << 20,
		blockSize:    64 << 10,
		writePercent: 20,
		interval:     time.Second,
	}
}

func TestValidate(t *testing.T) {
	allowed := []int{0, 1, 2, 3, 4}
	withMemory := []int{0, 1, 3}
	for _, tc := range []struct {
		name   string
		change func(*config)
		err    string
	}{
		{name: "ok", change: func(*config) {}},
		{name: "no CPUs", change: func(c *config) { c.cpus = nil }, err: "no CPUs"},
		{name: "no nodes", change: func(c *config) { c.nodes = nil }, err: "no nodes"},
		{name: "small block", change: func(c *config) { c.blockSize = 1024 }, err: "multiple of 64 and at least 4096"},
		{name: "odd block", change: func(c *config) { c.blockSize = 4100 }, err: "multiple of 64 and at least 4096"},
		{name: "small buffer", change: func(c *config) { c.bufferSize = 4096 }, err: "buffer (4096 bytes) is smaller than the block"},
		{name: "writes", change: func(c *config) { c.writePercent = 101 }, err: "from 0 to 100"},
		{name: "negative pause", change: func(c *config) { c.pause = -1 }, err: "cannot be negative"},
		{name: "no interval", change: func(c *config) { c.interval = 0 }, err: "interval must be positive"},
		{name: "CPU not allowed", change: func(c *config) { c.cpus = []int{0, 7} }, err: "CPU 7 is not available"},
		{name: "node without memory", change: func(c *config) { c.nodes = []int{2} }, err: "node 2 is not available (the nodes with memory: 0-1,3)"},
	} {
		cfg := goodConfig()
		tc.change(cfg)
		err := cfg.validate(allowed, withMemory)
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: error %v, want one with %q", tc.name, err, tc.err)
		}
	}
}

func TestFormatRates(t *testing.T) {
	for _, tc := range []struct {
		t       totals
		elapsed time.Duration
		want    string
	}{
		{t: totals{read: 10e9}, elapsed: 2 * time.Second, want: "5.00 GB/s (read 5.00, write 0.00)"},
		{t: totals{read: 3e9, written: 1e9}, elapsed: time.Second, want: "4.00 GB/s (read 3.00, write 1.00)"},
		{t: totals{read: 1e9}, elapsed: 0, want: "0.00 GB/s (read 0.00, write 0.00)"},
	} {
		if got := formatRates(tc.t, tc.elapsed); got != tc.want {
			t.Errorf("formatRates(%+v, %v) = %q, want %q", tc.t, tc.elapsed, got, tc.want)
		}
	}
}

func TestFormatPlacement(t *testing.T) {
	if got := formatPlacement(map[int]int{3: 100, 2: 300}, 400); got != "node 2 75%, node 3 25%" {
		t.Errorf("formatPlacement = %q", got)
	}
	if got := formatPlacement(map[int]int{5: 7}, 7); got != "node 5 100%" {
		t.Errorf("formatPlacement = %q", got)
	}
}

func TestOffNodes(t *testing.T) {
	got := offNodes(map[int]int{2: 1, 3: 1, 0: 5, 7: 1}, []int{2, 3})
	if !reflect.DeepEqual(got, []int{0, 7}) {
		t.Errorf("offNodes = %v, want [0 7]", got)
	}
	if got := offNodes(map[int]int{2: 1}, []int{2, 3}); len(got) != 0 {
		t.Errorf("offNodes = %v, want none", got)
	}
}

func TestWaitStopsAtOnce(t *testing.T) {
	stop := make(chan struct{})
	w := &worker{stop: stop}
	close(stop)
	started := time.Now()
	w.wait(10 * time.Second)
	if took := time.Since(started); took > time.Second {
		t.Errorf("a stopped worker waited %v", took)
	}
}

func TestWaitTakesAtLeastThePause(t *testing.T) {
	w := &worker{stop: make(chan struct{})}
	for _, d := range []time.Duration{50 * time.Microsecond, 3 * time.Millisecond} {
		started := time.Now()
		w.wait(d)
		if took := time.Since(started); took < d {
			t.Errorf("wait(%v) took %v", d, took)
		}
	}
	w.wait(0)
}

// pinnable returns an error unless a thread can be pinned to the CPU cpu.
// The thread that is tried ends with its goroutine.
func pinnable(cpu int) error {
	done := make(chan error)
	go func() {
		runtime.LockOSThread()
		done <- pinThread(cpu)
	}()
	return <-done
}

// bindableConfig returns a configuration of one thread on an allowed CPU
// and a node on which memory can be bound here, or skips the test.
func bindableConfig(t *testing.T) *config {
	t.Helper()
	node := numaNode(t)
	cpus, err := allowedCPUs()
	if err != nil || len(cpus) == 0 {
		t.Skipf("no CPUs to pin to: %v", err)
	}
	if err := pinnable(cpus[0]); err != nil {
		t.Skipf("threads cannot be pinned here: %v", err)
	}
	cfg := goodConfig()
	cfg.nodes = []int{node}
	cfg.cpus = cpus[:1]
	return cfg
}

func TestLoadStreams(t *testing.T) {
	cfg := bindableConfig(t)
	l := newLoad(cfg)
	if err := l.prepare(); err != nil {
		t.Fatal(err)
	}
	perNode, samples, err := l.placement()
	if err != nil {
		t.Logf("placement not checked: %v", err)
	} else if off := offNodes(perNode, cfg.nodes); len(off) > 0 || samples == 0 {
		t.Errorf("%d samples on %v, off the bound nodes: %v", samples, perNode, off)
	}
	if got := l.totals(); got != (totals{}) {
		t.Errorf("bytes streamed before the start: %+v", got)
	}
	l.start()
	deadline := time.Now().Add(10 * time.Second)
	for l.totals().read == 0 || l.totals().written == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("nothing streamed in 10 s: %+v", l.totals())
		}
		time.Sleep(time.Millisecond)
	}
	l.halt()
	got := l.totals()
	if got.read%uint64(cfg.blockSize) != 0 || got.written%uint64(cfg.blockSize) != 0 {
		t.Errorf("streamed %+v: not whole blocks of %d", got, cfg.blockSize)
	}
	if again := l.totals(); again != got {
		t.Errorf("bytes streamed after halt: %+v then %+v", got, again)
	}
}

func TestLoadPauseSetsBandwidth(t *testing.T) {
	cfg := bindableConfig(t)
	cfg.writePercent = 0
	cfg.pause = 20 * time.Millisecond
	l := newLoad(cfg)
	if err := l.prepare(); err != nil {
		t.Fatal(err)
	}
	l.start()
	time.Sleep(400 * time.Millisecond)
	l.halt()
	// At most one block per pause: 64 KiB in 20 ms is 3.3 MB/s; with the
	// block that is in progress, 25 blocks in 400 ms is far above it.
	blocks := l.totals().read / uint64(cfg.blockSize)
	if blocks == 0 || blocks > 25 {
		t.Errorf("%d blocks in 400 ms with a pause of %v, want 1 to 25", blocks, cfg.pause)
	}
}

func TestPrepareFailureStopsAll(t *testing.T) {
	cfg := bindableConfig(t)
	cfg.cpus = append(cfg.cpus, cpuSetBits)
	l := newLoad(cfg)
	err := l.prepare()
	if err == nil || !strings.Contains(err.Error(), "thread 1 on CPU") || !strings.Contains(err.Error(), "above the") {
		t.Fatalf("prepare with a CPU that cannot be pinned: error %v", err)
	}
	for _, w := range l.workers {
		if w.mapping != nil {
			t.Errorf("worker %d still holds its buffer", w.id)
		}
	}
}

func TestReportLinesUntilDone(t *testing.T) {
	cfg := goodConfig()
	cfg.interval = 20 * time.Millisecond
	l := newLoad(cfg)
	l.workers[0].read.Store(1e9)
	var lines []string
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Millisecond)
	defer cancel()
	l.report(ctx, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}, time.Now())
	if len(lines) < 2 {
		t.Fatalf("%d report lines in 110 ms at 20 ms intervals, want at least 2: %q", len(lines), lines)
	}
	// 10^9 bytes in the first interval, none after.
	if !strings.Contains(lines[0], " GB/s (read ") || !strings.HasSuffix(lines[1], "0.00 GB/s (read 0.00, write 0.00)") {
		t.Errorf("report lines %q", lines[:2])
	}
}
