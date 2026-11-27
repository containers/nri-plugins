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
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// maxPlacementSamples bounds the number of pages of a buffer whose node
// the placement report queries.
const maxPlacementSamples = 512

// config is what the command line asks of the load.
type config struct {
	// nodes are the NUMA nodes whose memory the buffers are bound to.
	nodes []int
	// cpus are the CPUs of the threads, one thread per CPU in order.
	cpus []int
	// bufferSize is the size in bytes of the buffer of each thread.
	bufferSize int
	// blockSize is the number of bytes that a thread streams between two
	// pauses.
	blockSize int
	// writePercent is the share of the blocks that are written, the
	// others are read.
	writePercent int
	// pause is the time that a thread waits after each block.
	pause time.Duration
	// duration is how long the load runs, 0 until it is stopped.
	duration time.Duration
	// interval is the time between two lines of achieved bandwidth.
	interval time.Duration
}

// validate returns an error if cfg cannot run: the sizes, the percent and
// the times, and the CPUs and nodes against the CPUs the process may use
// and the nodes with memory.
func (cfg *config) validate(allowed, withMemory []int) error {
	switch {
	case len(cfg.cpus) == 0:
		return errors.New("no CPUs")
	case len(cfg.nodes) == 0:
		return errors.New("no nodes")
	case cfg.blockSize < 4096 || cfg.blockSize%64 != 0:
		return fmt.Errorf("the block is %d bytes: it must be a multiple of 64 and at least 4096", cfg.blockSize)
	case cfg.bufferSize < cfg.blockSize:
		return fmt.Errorf("the buffer (%d bytes) is smaller than the block (%d bytes)", cfg.bufferSize, cfg.blockSize)
	case cfg.writePercent < 0 || cfg.writePercent > 100:
		return fmt.Errorf("the write share is %d%%: it is from 0 to 100", cfg.writePercent)
	case cfg.pause < 0 || cfg.duration < 0:
		return errors.New("the pause and the duration cannot be negative")
	case cfg.interval <= 0:
		return errors.New("the report interval must be positive")
	}
	if err := checkSubset("CPU", cfg.cpus, allowed, "the CPUs this process may use"); err != nil {
		return err
	}
	return checkSubset("node", cfg.nodes, withMemory, "the nodes with memory")
}

// totals is an amount of streamed bytes.
type totals struct {
	read, written uint64
}

// minus returns t less u.
func (t totals) minus(u totals) totals {
	return totals{read: t.read - u.read, written: t.written - u.written}
}

// worker streams over its own buffer from its own thread.
type worker struct {
	id  int
	cpu int
	cfg *config
	// stop is closed to stop the worker; begin when the workers start
	// streaming.
	stop  <-chan struct{}
	begin <-chan struct{}
	// mapping is the buffer, and buffer the same as words.
	mapping []byte
	buffer  []uint64
	// read and written count the bytes streamed so far.
	read, written atomic.Uint64
	// sink keeps the result of the reads, so that they are not left out.
	sink uint64
}

// prepare pins the calling thread to the CPU of the worker and makes its
// buffer: mapped, bound to the nodes, and every page touched.
func (w *worker) prepare() error {
	if err := pinThread(w.cpu); err != nil {
		return err
	}
	size := w.cfg.bufferSize / w.cfg.blockSize * w.cfg.blockSize
	mapping, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return fmt.Errorf("cannot map a buffer of %s: %w", formatSize(size), err)
	}
	w.mapping = mapping
	w.buffer = words(mapping)
	// Huge pages make the stream cheaper for the CPU's TLB; the host may
	// not have them.
	_ = unix.Madvise(mapping, unix.MADV_HUGEPAGE)
	if err := bindMemory(mapping, w.cfg.nodes); err != nil {
		return err
	}
	for off := 0; off < len(mapping); off += unix.Getpagesize() {
		mapping[off] = 1
	}
	return nil
}

// release unmaps the buffer.
func (w *worker) release() {
	if w.mapping != nil {
		_ = unix.Munmap(w.mapping)
		w.mapping, w.buffer = nil, nil
	}
}

// run prepares the worker on its own thread, sends the result on ready,
// and streams from the start until it is stopped.
func (w *worker) run(wg *sync.WaitGroup, ready chan<- error) {
	defer wg.Done()
	// The thread is never unlocked: it ends with this goroutine and takes
	// its CPU pin with it.
	runtime.LockOSThread()
	defer w.release()
	if err := w.prepare(); err != nil {
		ready <- fmt.Errorf("thread %d on CPU %d: %w", w.id, w.cpu, err)
		return
	}
	ready <- nil
	select {
	case <-w.begin:
	case <-w.stop:
		return
	}
	w.stream()
}

// stream reads and writes the blocks of the buffer in turn, pausing after
// each, until the worker is stopped.
func (w *worker) stream() {
	blockWords := w.cfg.blockSize / wordSize
	blocks := len(w.buffer) / blockWords
	var sum, value uint64
	for n := 0; ; n++ {
		select {
		case <-w.stop:
			w.sink = sum + value
			return
		default:
		}
		first := n % blocks * blockWords
		block := w.buffer[first : first+blockWords]
		if isWriteBlock(n, w.cfg.writePercent) {
			value = writeWords(block, value)
			w.written.Add(uint64(w.cfg.blockSize))
		} else {
			sum += readWords(block)
			w.read.Add(uint64(w.cfg.blockSize))
		}
		w.wait(w.cfg.pause)
	}
}

// wait returns after d, or when the worker is stopped.
func (w *worker) wait(d time.Duration) {
	if d <= 0 {
		return
	}
	// Spin on the clock: a thread that sleeps and wakes thousands of times
	// a second makes the Go scheduler hand its processor over and back each
	// time, which costs more than the pause.
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case <-w.stop:
			return
		default:
		}
	}
}

// load is the threads of one run.
type load struct {
	cfg     *config
	workers []*worker
	stop    chan struct{}
	begin   chan struct{}
	wg      sync.WaitGroup
}

// newLoad returns a load of one worker for each CPU of cfg.cpus.
func newLoad(cfg *config) *load {
	l := &load{cfg: cfg, stop: make(chan struct{}), begin: make(chan struct{})}
	for i, cpu := range cfg.cpus {
		l.workers = append(l.workers, &worker{id: i, cpu: cpu, cfg: cfg, stop: l.stop, begin: l.begin})
	}
	return l
}

// prepare starts the workers and returns when each has its buffer, or the
// first error: then the workers are stopped.
func (l *load) prepare() error {
	ready := make(chan error, len(l.workers))
	for _, w := range l.workers {
		l.wg.Add(1)
		go w.run(&l.wg, ready)
	}
	var first error
	for range l.workers {
		if err := <-ready; err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		l.halt()
	}
	return first
}

// start lets the prepared workers begin streaming.
func (l *load) start() {
	close(l.begin)
}

// halt stops the workers and returns when they have ended.
func (l *load) halt() {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	l.wg.Wait()
}

// totals returns the bytes that all workers have streamed so far.
func (l *load) totals() totals {
	var t totals
	for _, w := range l.workers {
		t.read += w.read.Load()
		t.written += w.written.Load()
	}
	return t
}

// placement returns the number of sampled pages of the buffers on each
// node and the number of samples, from a query of about
// maxPlacementSamples pages of each buffer.
func (l *load) placement() (map[int]int, int, error) {
	pageSize := unix.Getpagesize()
	var addrs []uintptr
	for _, w := range l.workers {
		pages := len(w.mapping) / pageSize
		step := max(1, pages/maxPlacementSamples)
		for p := 0; p < pages; p += step {
			addrs = append(addrs, uintptr(unsafe.Pointer(&w.mapping[p*pageSize])))
		}
	}
	status, err := pageNodes(addrs)
	if err != nil {
		return nil, 0, err
	}
	perNode := map[int]int{}
	for _, node := range status {
		if node < 0 {
			return nil, 0, fmt.Errorf("a page of the buffers has no node (error %d): %w", -node, unix.Errno(-node))
		}
		perNode[int(node)]++
	}
	return perNode, len(status), nil
}

// formatPlacement returns the share of samples on each node, like
// "node 2 51%, node 3 49%".
func formatPlacement(perNode map[int]int, samples int) string {
	nodes := make([]int, 0, len(perNode))
	for n := range perNode {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	out := ""
	for i, n := range nodes {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("node %d %.0f%%", n, 100*float64(perNode[n])/float64(samples))
	}
	return out
}

// offNodes returns the nodes of perNode that are not in nodes.
func offNodes(perNode map[int]int, nodes []int) []int {
	wanted := map[int]bool{}
	for _, n := range nodes {
		wanted[n] = true
	}
	var off []int
	for n := range perNode {
		if !wanted[n] {
			off = append(off, n)
		}
	}
	sort.Ints(off)
	return off
}

// formatRates returns the bandwidth of the bytes streamed in the time
// elapsed, like "N.NN GB/s (read N.NN, write N.NN)": GB are 10^9 bytes,
// the bytes written are counted once.
func formatRates(t totals, elapsed time.Duration) string {
	sec := elapsed.Seconds()
	if sec <= 0 {
		return "0.00 GB/s (read 0.00, write 0.00)"
	}
	read := float64(t.read) / sec / 1e9
	write := float64(t.written) / sec / 1e9
	return fmt.Sprintf("%.2f GB/s (read %.2f, write %.2f)", read+write, read, write)
}

// report writes the achieved bandwidth to out every interval, from the
// time started until ctx is done.
func (l *load) report(ctx context.Context, out func(format string, args ...any), started time.Time) {
	tick := time.NewTicker(l.cfg.interval)
	defer tick.Stop()
	last, lastTime := l.totals(), started
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			cur := l.totals()
			out("%.1f s: %s", now.Sub(started).Seconds(), formatRates(cur.minus(last), now.Sub(lastTime)))
			last, lastTime = cur, now
		}
	}
}
