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

// cxl-load streams memory reads and writes over buffers bound to the
// memory of some NUMA nodes, from threads pinned to some CPUs, at a
// bandwidth that a pause after each block sets. Run on the CPUs of one
// socket against the memory of that socket, it queues up the memory
// controllers: the latency that the CPUs of the other socket see to that
// memory rises, like it does to a busy CXL link or device.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Defaults of the flags.
const (
	defaultBuffer   = "512M"
	defaultBlock    = "256K"
	defaultInterval = 5 * time.Second
)

const usage = `Usage: cxl-load -numa-nodes LIST -cpus LIST [OPTIONS]

Streams reads and writes over a buffer of each thread, bound to the memory of
the nodes and touched before the load starts, from threads pinned to the CPUs,
one thread per CPU. After each block a thread pauses, which sets the bandwidth.
Prints the achieved bandwidth every interval, and a summary when it stops: after
-duration, or at SIGINT or SIGTERM.

Options:
  -numa-nodes LIST       NUMA nodes whose memory the buffers are bound to, like 2,3 or 2-3
  -cpus LIST        CPUs of the threads, like 64-79: the first CPUs of the nodes' CPUs
                    are the cores; the buffers of all threads should be larger than the
                    caches of the CPUs, or the load stays in the cache
  -threads N        run on the first N of the CPUs (default: all of them)
  -buffer SIZE      buffer of each thread, like 512M or 1G (default ` + defaultBuffer + `)
  -block SIZE       bytes that a thread streams between two pauses (default ` + defaultBlock + `)
  -writes PERCENT   share of the blocks that are written, the others are read, 0 to 100
                    (default 0); the written bytes count once, the memory moves about twice
  -pause DURATION   a thread waits this long after each block, like 20us or 1ms
                    (default 0: as fast as the thread can); it spins on the clock,
                    so the CPU stays busy
  -duration TIME    stop after this long (default 0: run until SIGINT or SIGTERM)
  -interval TIME    time between two lines of achieved bandwidth (default 5s)

Sizes: K, M and G are powers of 1024. Bandwidth: GB/s are 10^9 bytes per second.
Exit codes: 0 stopped, 1 error, 2 usage.
`

// usageError is an error in the use of the command line.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// cli is the command line of cxl-load.
type cli struct {
	stdout, stderr io.Writer
	// sysfs is the sysfs directory of the NUMA nodes.
	sysfs string
	// allowedCPUs returns the CPUs that this process may run on.
	allowedCPUs func() ([]int, error)
}

func main() {
	c := &cli{
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		sysfs:       "/sys",
		allowedCPUs: allowedCPUs,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := c.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// run runs the command line args until ctx is done and returns the exit
// code.
func (c *cli) run(ctx context.Context, args []string) int {
	err := c.execute(ctx, args)
	var ue *usageError
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		_, _ = fmt.Fprint(c.stdout, usage)
		return exitOK
	case errors.As(err, &ue):
		_, _ = fmt.Fprintf(c.stderr, "cxl-load: %s\n\n%s", ue.msg, usage)
		return exitUsage
	default:
		_, _ = fmt.Fprintf(c.stderr, "cxl-load: %v\n", err)
		return exitError
	}
}

// logf writes a line to the standard output, prefixed with the name of the
// command.
func (c *cli) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.stdout, "cxl-load: "+format+"\n", args...)
}

// parseArgs returns the configuration of the command line args.
func (c *cli) parseArgs(args []string) (*config, error) {
	fs := flag.NewFlagSet("cxl-load", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	nodes := fs.String("numa-nodes", "", "")
	cpus := fs.String("cpus", "", "")
	threads := fs.Int("threads", 0, "")
	buffer := fs.String("buffer", defaultBuffer, "")
	block := fs.String("block", defaultBlock, "")
	writes := fs.Int("writes", 0, "")
	pause := fs.Duration("pause", 0, "")
	duration := fs.Duration("duration", 0, "")
	interval := fs.Duration("interval", defaultInterval, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, usagef("%v", err)
	}
	if fs.NArg() > 0 {
		return nil, usagef("unexpected argument %q", fs.Arg(0))
	}
	if *nodes == "" || *cpus == "" {
		return nil, usagef("-numa-nodes and -cpus are required")
	}
	cfg := &config{writePercent: *writes, pause: *pause, duration: *duration, interval: *interval}
	var err error
	if cfg.nodes, err = parseList(*nodes); err != nil {
		return nil, usagef("-numa-nodes: %v", err)
	}
	if cfg.cpus, err = parseList(*cpus); err != nil {
		return nil, usagef("-cpus: %v", err)
	}
	if *threads < 0 || *threads > len(cfg.cpus) {
		return nil, usagef("-threads %d: from 0 to the %d CPUs of -cpus (one thread per CPU)", *threads, len(cfg.cpus))
	}
	if *threads > 0 {
		cfg.cpus = cfg.cpus[:*threads]
	}
	if cfg.bufferSize, err = parseSize(*buffer); err != nil {
		return nil, usagef("-buffer: %v", err)
	}
	if cfg.blockSize, err = parseSize(*block); err != nil {
		return nil, usagef("-block: %v", err)
	}
	return cfg, nil
}

// execute runs the load of the command line args until ctx is done or the
// duration has passed.
func (c *cli) execute(ctx context.Context, args []string) error {
	cfg, err := c.parseArgs(args)
	if err != nil {
		return err
	}
	allowed, err := c.allowedCPUs()
	if err != nil {
		return err
	}
	withMemory, err := memoryNodes(c.sysfs)
	if err != nil {
		return err
	}
	if err := cfg.validate(allowed, withMemory); err != nil {
		return usagef("%v", err)
	}
	if err := c.checkFreeMemory(cfg); err != nil {
		return err
	}
	// Every thread keeps a processor of the Go runtime while it streams,
	// and the main goroutine needs one besides.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(len(cfg.cpus) + 2))
	l := newLoad(cfg)
	c.logf("%d threads on CPUs %s, a buffer of %s each, bound to the memory of nodes %s: preparing",
		len(cfg.cpus), formatList(cfg.cpus), formatSize(cfg.bufferSize/cfg.blockSize*cfg.blockSize), formatList(cfg.nodes))
	if err := l.prepare(); err != nil {
		return err
	}
	if err := c.checkPlacement(l); err != nil {
		l.halt()
		return err
	}
	if cfg.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.duration)
		defer cancel()
	}
	c.logf("running: block %s, writes %d%%, pause %s, a line every %s",
		formatSize(cfg.blockSize), cfg.writePercent, formatDuration(cfg.pause), formatDuration(cfg.interval))
	started := time.Now()
	l.start()
	l.report(ctx, c.logf, started)
	l.halt()
	elapsed := time.Since(started)
	t := l.totals()
	c.logf("stopped after %.1f s: %s on average, %.1f GB moved",
		elapsed.Seconds(), formatRates(t, elapsed), float64(t.read+t.written)/1e9)
	return nil
}

// maxFreeShare is the share of the free memory of the nodes, in percent,
// that the buffers may take: a bound allocation that does not fit
// reclaims memory and then kills processes.
const maxFreeShare = 50

// checkFreeMemory returns an error if the buffers of cfg are more than
// maxFreeShare percent of the free memory of its nodes.
func (c *cli) checkFreeMemory(cfg *config) error {
	free, err := freeMemory(c.sysfs, cfg.nodes)
	if err != nil {
		return err
	}
	need := int64(len(cfg.cpus)) * int64(cfg.bufferSize/cfg.blockSize*cfg.blockSize)
	if need > free/100*maxFreeShare {
		return fmt.Errorf("%d buffers of %s need %.1f GiB of the %.1f GiB that is free on nodes %s: at most %d%% of it is taken (use fewer threads or a smaller -buffer)",
			len(cfg.cpus), formatSize(cfg.bufferSize), float64(need)/(1<<30), float64(free)/(1<<30), formatList(cfg.nodes), maxFreeShare)
	}
	return nil
}

// checkPlacement reports the nodes the buffers of l are on and returns an
// error if any is on a node that is not asked for. A host that does not
// permit the query is told and the load goes on.
func (c *cli) checkPlacement(l *load) error {
	perNode, samples, err := l.placement()
	if errors.Is(err, errNotPermitted) {
		c.logf("WARNING: the placement of the buffers is not checked: %v", err)
		return nil
	}
	if err != nil {
		return err
	}
	c.logf("placement of the buffers (%d pages sampled): %s", samples, formatPlacement(perNode, samples))
	if off := offNodes(perNode, l.cfg.nodes); len(off) > 0 {
		return fmt.Errorf("pages of the buffers are on nodes %s, not on nodes %s", formatList(off), formatList(l.cfg.nodes))
	}
	return nil
}
