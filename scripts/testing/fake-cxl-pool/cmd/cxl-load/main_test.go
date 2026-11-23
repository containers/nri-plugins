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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer that can be written and read at the same
// time.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// testCLI returns a cli with the nodes with memory nodes, the allowed CPUs
// cpus and the output buffers.
func testCLI(t *testing.T, nodes string, cpus []int) (*cli, *syncBuffer, *syncBuffer) {
	t.Helper()
	sysfs := t.TempDir()
	dir := filepath.Join(sysfs, "devices/system/node")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "has_memory"), []byte(nodes+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Every node has 16 GiB free.
	list, err := parseList(nodes)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range list {
		nodeDir := filepath.Join(dir, fmt.Sprintf("node%d", n))
		if err := os.MkdirAll(nodeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		meminfo := fmt.Sprintf("Node %d MemTotal:   33554432 kB\nNode %d MemFree:    16777216 kB\n", n, n)
		if err := os.WriteFile(filepath.Join(nodeDir, "meminfo"), []byte(meminfo), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	c := &cli{
		stdout:      stdout,
		stderr:      stderr,
		sysfs:       sysfs,
		allowedCPUs: func() ([]int, error) { return cpus, nil },
	}
	return c, stdout, stderr
}

func TestUsage(t *testing.T) {
	cpus := []int{0, 1, 2, 3, 4, 5, 6, 7}
	for _, tc := range []struct {
		args []string
		code int
		err  string
	}{
		{args: nil, code: exitUsage, err: "-numa-nodes and -cpus are required"},
		{args: []string{"-numa-nodes", "1"}, code: exitUsage, err: "-numa-nodes and -cpus are required"},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-3", "x"}, code: exitUsage, err: `unexpected argument "x"`},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-3", "-speed", "1"}, code: exitUsage, err: "flag provided but not defined: -speed"},
		{args: []string{"-numa-nodes", "b", "-cpus", "0-3"}, code: exitUsage, err: `-numa-nodes: list "b": "b" is not a number`},
		{args: []string{"-numa-nodes", "1", "-cpus", "3-2"}, code: exitUsage, err: "-cpus: list"},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-3", "-threads", "5"}, code: exitUsage, err: "-threads 5: from 0 to the 4 CPUs"},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-3", "-threads", "-1"}, code: exitUsage, err: "-threads -1"},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-3", "-buffer", "lots"}, code: exitUsage, err: `-buffer: "lots" is not a size`},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-3", "-block", "0"}, code: exitUsage, err: `-block: "0" is not a size`},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-3", "-writes", "101"}, code: exitUsage, err: "from 0 to 100"},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-3", "-pause", "fast"}, code: exitUsage, err: "invalid value"},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-3", "-block", "1M", "-buffer", "64K"}, code: exitUsage, err: "smaller than the block"},
		{args: []string{"-numa-nodes", "1", "-cpus", "0-9"}, code: exitUsage, err: "CPU 8 is not available"},
		{args: []string{"-numa-nodes", "3", "-cpus", "0-3"}, code: exitUsage, err: "node 3 is not available (the nodes with memory: 0-1)"},
		{args: []string{"-numa-nodes", "0", "-cpus", "0-7", "-buffer", "3G"}, code: exitError, err: "8 buffers of 3 GiB need 24.0 GiB of the 16.0 GiB that is free on nodes 0: at most 50% of it is taken"},
	} {
		c, stdout, stderr := testCLI(t, "0-1", cpus)
		code := c.run(context.Background(), tc.args)
		if code != tc.code || !strings.Contains(stderr.String(), tc.err) {
			t.Errorf("%v: exit %d, stderr %q, want exit %d with %q", tc.args, code, stderr.String(), tc.code, tc.err)
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: stdout %q, want none", tc.args, stdout.String())
		}
	}
}

func TestHelp(t *testing.T) {
	c, stdout, stderr := testCLI(t, "0", []int{0})
	if code := c.run(context.Background(), []string{"-h"}); code != exitOK {
		t.Errorf("-h: exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: cxl-load -numa-nodes LIST -cpus LIST") {
		t.Errorf("-h printed %q", stdout.String())
	}
}

func TestParseArgs(t *testing.T) {
	c, _, _ := testCLI(t, "0", []int{0})
	cfg, err := c.parseArgs([]string{"-numa-nodes", "2-3", "-cpus", "64-71", "-threads", "3", "-buffer", "1G",
		"-block", "128K", "-writes", "25", "-pause", "30us", "-duration", "1m", "-interval", "2s"})
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		nodes: []int{2, 3}, cpus: []int{64, 65, 66}, bufferSize: 1 << 30, blockSize: 128 << 10,
		writePercent: 25, pause: 30 * time.Microsecond, duration: time.Minute, interval: 2 * time.Second,
	}
	if formatList(cfg.nodes) != "2-3" || formatList(cfg.cpus) != "64-66" || cfg.bufferSize != want.bufferSize ||
		cfg.blockSize != want.blockSize || cfg.writePercent != want.writePercent || cfg.pause != want.pause ||
		cfg.duration != want.duration || cfg.interval != want.interval {
		t.Errorf("parseArgs = %+v, want %+v", cfg, want)
	}
	cfg, err = c.parseArgs([]string{"-numa-nodes", "0", "-cpus", "0-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.cpus) != 2 || cfg.writePercent != 0 || cfg.pause != 0 || cfg.duration != 0 ||
		cfg.interval != defaultInterval || cfg.bufferSize != 512<<20 || cfg.blockSize != 256<<10 {
		t.Errorf("parseArgs defaults = %+v", cfg)
	}
}

func TestRunForTheDuration(t *testing.T) {
	cfg := bindableConfig(t)
	c, stdout, stderr := testCLI(t, "0-1023", cfg.cpus)
	args := []string{"-numa-nodes", formatList(cfg.nodes), "-cpus", formatList(cfg.cpus), "-buffer", "4M", "-block", "64K",
		"-writes", "10", "-duration", "300ms", "-interval", "100ms"}
	if code := c.run(context.Background(), args); code != exitOK {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr.String(), stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"1 threads on CPUs " + formatList(cfg.cpus) + ", a buffer of 4 MiB each, bound to the memory of nodes " + formatList(cfg.nodes) + ": preparing",
		"running: block 64 KiB, writes 10%, pause 0s, a line every 100ms",
		" s: ", " GB/s (read ", "stopped after 0.", " on average, ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "running") > strings.Index(out, "stopped after") {
		t.Errorf("running comes after stopped:\n%s", out)
	}
}

func TestRunUntilCancelled(t *testing.T) {
	cfg := bindableConfig(t)
	c, stdout, stderr := testCLI(t, "0-1023", cfg.cpus)
	ctx, cancel := context.WithCancel(context.Background())
	args := []string{"-numa-nodes", formatList(cfg.nodes), "-cpus", formatList(cfg.cpus), "-buffer", "4M", "-block", "64K",
		"-pause", "1s", "-interval", "50ms"}
	go func() {
		for !strings.Contains(stdout.String(), "running") {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	started := time.Now()
	if code := c.run(ctx, args); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("a cancelled run with a pause of 1 s took %v", took)
	}
	if !strings.Contains(stdout.String(), "stopped after") {
		t.Errorf("no summary:\n%s", stdout.String())
	}
}
