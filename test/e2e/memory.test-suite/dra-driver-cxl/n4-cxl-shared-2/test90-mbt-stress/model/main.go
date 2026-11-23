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

// mbt-cxl is the model-based test generator and oracle of the CXL DRA
// driver's e2e test test90-mbt-stress (../contract.txt).
//
//	mbt-cxl online [flags]              steps on stdin/stdout (the coproc)
//	mbt-cxl online --replay FILE ...    the steps of an mbt-steps.txt
//	mbt-cxl generate [flags] > code.gen.sh
//	mbt-cxl generate --from FILE ...    a code.gen.sh of an mbt-steps.txt
//	mbt-cxl check --step N --expect FILE --observed FILE [--since T]
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

func main() {
	tuneRuntime()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// tuneRuntime sets the defaults that the P4 measurements chose, unless
// GOMAXPROCS or GOGC are set: the search is one goroutine that allocates
// a lot of short-lived states, and the garbage collector of a host with
// many CPUs spends most of the time on its own synchronization (256 CPUs:
// depth 3 on the full profile took 2.4x longer with all of them than with
// GOMAXPROCS=2 and GOGC=400). With --plan-states a plan keeps its distinct
// states until it ends (canon.go, ~300 MB at 150000): unless GOMEMLIMIT is
// set, a soft limit of 768 MiB keeps GOGC=400 from growing the heap to
// five times that.
func tuneRuntime() {
	if os.Getenv("GOMAXPROCS") == "" && runtime.NumCPU() > 2 {
		runtime.GOMAXPROCS(2)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(400)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(768 << 20)
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage: mbt-cxl COMMAND [flags]
commands:
  online      run the line protocol with the bash executor (stdin/stdout)
  generate    print a code.gen.sh (off-line steps and expectations)
  check       check one observation against one expectation
run "mbt-cxl COMMAND -h" for the flags of a command
`)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "online":
		return runOnline(args[1:], stdin, stdout, stderr)
	case "generate":
		return runGenerate(args[1:], stdout, stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "mbt-cxl: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

// commonFlags are the flags of online and generate.
type commonFlags struct {
	profile       string
	seed          int64
	depth         int
	steps         int
	duration      time.Duration
	unsettled     int
	settleTimeout time.Duration
	avoid         string
	localBlocks   string
	nodeHB0       int
	nodeHB1       int
	coverage      string
	randomness    int
	maxRestarts   int
	planAhead     bool
	planTime      time.Duration
	maxPaths      int
	out           string
	replay        string
	from          string
	avoidHoles    bool
	unblock       time.Duration
	planStates    int
	stallWindow   time.Duration
	fencing       string
	reboot        string
}

// defaultCoverage is the default mix: actions weighed 3, faults in
// context 2, pairs of actions 1.
const defaultCoverage = "C1x3,C2x2,C3"

func (c *commonFlags) register(fs *flag.FlagSet, online bool) {
	fs.StringVar(&c.profile, "profile", "fullp", "slot profile: "+strings.Join(profileNames(), ", "))
	fs.Int64Var(&c.seed, "seed", 0, "random seed (0: time-based, printed)")
	fs.IntVar(&c.depth, "depth", 3, "search depth of the planner")
	fs.IntVar(&c.steps, "steps", 0, "steps after the initial settle, teardown not counted (0: no limit)")
	fs.IntVar(&c.unsettled, "unsettled", 3, "operations in flight before a forced settle")
	fs.StringVar(&c.avoid, "avoid", "", "comma list of VERB or VERB:ARG to leave out (ARG: slot, VM, pool, kind, mode)")
	fs.StringVar(&c.localBlocks, "local-blocks", "vm2=8,vm1=2,vm0=2", "128Mi blocks of the local memdev of each VM")
	fs.IntVar(&c.nodeHB0, "numa-node-hb0", 2, "NUMA node of the regions under host bridge 0 (local, pool-a, pool-b)")
	fs.IntVar(&c.nodeHB1, "numa-node-hb1", 3, "NUMA node of the regions under host bridge 1 (pool-c)")
	fs.StringVar(&c.coverage, "coverage", defaultCoverage, "coverage criteria with optional weights xN: C1 actions, C2 faults in context, C3 action pairs, C4 abstract states, or all")
	fs.IntVar(&c.randomness, "randomness", 1, "gofmbt randomness level (0 none, 1 among equally good, ... 4 any increasing path)")
	fs.IntVar(&c.maxRestarts, "max-restarts", 2, "crash and driver-kill only below this many restarts (CrashLoopBackOff)")
	fs.IntVar(&c.maxPaths, "max-paths", 0, "paths evaluated per plan at most (0: no limit; reproducible)")
	fs.IntVar(&c.planStates, "plan-states", defaultPlanStates, "states the planner caches per plan at most (0: no cache; 150000 caches a whole depth-3 plan: ~30% less plan time for ~0.7 GB; GOMEMLIMIT bounds the heap, default 768 MiB)")
	fs.BoolVar(&c.avoidHoles, "avoid-holes", false, "never generate an action that destroys a CXL region below the top of its decoder stack (A9)")
	fs.StringVar(&c.fencing, "fencing", "allowed-mems", "the fencing mode of the drivers (MBT_FENCING, A36): allowed-mems, none, or switch (starts in allowed-mems, generates fencing-switch)")
	fs.StringVar(&c.reboot, "reboot", "", "the node-reboot variant (MBT_REBOOT, A41): guest (systemctl reboot) or qemu (the qemu process restarted); empty: no node-reboot")
	fs.DurationVar(&c.stallWindow, "stall-window", defaultStallWindow, "a stalled contender (A19) is promoted within this long of the settle that first saw it; if not longer than the settle timeout, settles wait for its promotion, else it is settled stalled or promoted")
	if online {
		fs.DurationVar(&c.duration, "duration", 20*time.Minute, "time budget of the run before the teardown (0: no limit)")
		fs.DurationVar(&c.settleTimeout, "settle-timeout", 180*time.Second, "a settle that waits longer fails")
		fs.DurationVar(&c.unblock, "unblock-timeout", 330*time.Second, "a pod blocked by a decoder hole must run this long after the hole cleared (A9)")
		fs.BoolVar(&c.planAhead, "plan-ahead", true, "plan the next step while bash executes the current one")
		fs.DurationVar(&c.planTime, "plan-time", 10*time.Second, "time budget of one plan (0: complete every depth, reproducible)")
		fs.StringVar(&c.out, "out", "", "directory of mbt-steps.txt and mbt-fail.txt")
		fs.StringVar(&c.replay, "replay", "", "the steps of this file (mbt-steps.txt) instead of planned ones")
	} else {
		fs.StringVar(&c.from, "from", "", "the steps of this file (mbt-steps.txt) instead of planned ones")
	}
}

// options turns the flags into run options.
func (c *commonFlags) options(online bool, stderr io.Writer) (runOpts, error) {
	cfg := defaultConfig()
	cfg.Online = online
	if c.from != "" {
		// the steps of an on-line run may name "any" slots
		cfg.Online = true
	}
	if err := parseLocalBlocks(c.localBlocks, &cfg.LocalBlocks); err != nil {
		return runOpts{}, err
	}
	cfg.NUMANodeHB = [nBridges]int{HB0: c.nodeHB0, HB1: c.nodeHB1}
	if c.nodeHB0 == c.nodeHB1 {
		return runOpts{}, errors.New("--numa-node-hb0 and --numa-node-hb1 must differ")
	}
	cfg.Unsettled = c.unsettled
	if cfg.Unsettled < 1 || cfg.Unsettled > 255 {
		return runOpts{}, errors.New("--unsettled must be 1 to 255")
	}
	cfg.MaxRestarts = c.maxRestarts
	cfg.AvoidHoles = c.avoidHoles
	if c.stallWindow <= 0 {
		return runOpts{}, errors.New("--stall-window must be positive")
	}
	cfg.StallWindow = c.stallWindow
	fencing, err := parseFencingRun(c.fencing)
	if err != nil {
		return runOpts{}, fmt.Errorf("--fencing: %w", err)
	}
	cfg.Fencing = fencing
	reboot, err := parseRebootRun(c.reboot)
	if err != nil {
		return runOpts{}, fmt.Errorf("--reboot: %w", err)
	}
	cfg.Reboot = reboot
	settleTimeout := c.settleTimeout
	if !online {
		settleTimeout = 180 * time.Second // MBT_SETTLE_TIMEOUT's default (check --settle-timeout)
	}
	cfg.StallStrict = c.stallWindow <= settleTimeout
	cfg.PlanStates = c.planStates
	if cfg.PlanStates < 0 {
		return runOpts{}, errors.New("--plan-states must not be negative")
	}
	if c.avoid != "" {
		for _, r := range strings.Split(c.avoid, ",") {
			if r = strings.TrimSpace(r); r != "" {
				cfg.Avoid = append(cfg.Avoid, r)
			}
		}
		if err := checkAvoid(cfg.Avoid); err != nil {
			return runOpts{}, err
		}
	}
	if err := cfg.loadProfile(c.profile); err != nil {
		return runOpts{}, err
	}
	cov, err := parseCoverage(c.coverage)
	if err != nil {
		return runOpts{}, err
	}
	if c.depth < 1 {
		return runOpts{}, errors.New("--depth must be at least 1")
	}
	seed := c.seed
	if seed == 0 {
		seed = time.Now().UnixNano() % 1000000007
		_, _ = fmt.Fprintf(stderr, "mbt-cxl: seed %d\n", seed)
	}
	o := runOpts{
		cfg:            cfg,
		cov:            cov,
		depth:          c.depth,
		seed:           seed,
		randomness:     c.randomness,
		maxSteps:       c.steps,
		duration:       c.duration,
		settleTimeout:  c.settleTimeout,
		planAhead:      c.planAhead,
		planTime:       c.planTime,
		maxPaths:       c.maxPaths,
		outDir:         c.out,
		unblockTimeout: c.unblock,
	}
	o.header = fmt.Sprintf("--profile %s --seed %d --depth %d --coverage %s --randomness %d --unsettled %d --max-restarts %d --local-blocks %s --numa-node-hb0 %d --numa-node-hb1 %d --avoid %q --avoid-holes=%v --stall-window %s --fencing %s --reboot=%s --max-paths %d --plan-states %d",
		cfg.Profile, seed, c.depth, cov, c.randomness, c.unsettled, c.maxRestarts, formatLocalBlocks(cfg.LocalBlocks), c.nodeHB0, c.nodeHB1, c.avoid, c.avoidHoles, c.stallWindow, cfg.Fencing, cfg.Reboot, c.maxPaths, c.planStates)
	if online {
		o.header += fmt.Sprintf(" --steps %d --duration %s --settle-timeout %s --unblock-timeout %s --plan-ahead=%v --plan-time %s", c.steps, c.duration, c.settleTimeout, c.unblock, c.planAhead, c.planTime)
	} else {
		o.header += fmt.Sprintf(" --steps %d", c.steps)
	}
	return o, nil
}

func runOnline(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("online", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c commonFlags
	c.register(fs, true)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var acts []string
	if c.replay != "" {
		// A16: the model of a replay is the one of the steps file
		data, err := os.ReadFile(c.replay)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "mbt-cxl: %v\n", err)
			return 2
		}
		lines, header := readSteps(string(data))
		if err := applyHeader(fs, header, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "mbt-cxl: %s: %v\n", c.replay, err)
			return 2
		}
		acts = []string{}
		for _, l := range lines {
			acts = append(acts, l.Action)
		}
	}
	o, err := c.options(true, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mbt-cxl: %v\n", err)
		return 2
	}
	if c.replay != "" {
		o.replay = acts
		o.header += " --replay " + c.replay
	}
	r, err := newOnlineRun(o, stdin, stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mbt-cxl: %v\n", err)
		return 2
	}
	if err := r.run(); err != nil {
		if !errors.Is(err, errFailed) {
			_, _ = fmt.Fprintf(stderr, "mbt-cxl: %v\n", err)
		}
		return 1
	}
	return 0
}

// modelFlags are the flags of a steps file's header that define the
// model of a replay or --from (A16): they win over the command line
// (--fencing and --reboot must agree with it: clusterFlags). The
// measured facts of the cluster (--local-blocks, --numa-node-hb0/1) stay the
// command line's (EXEC warns when they differ from the header), and so
// do the budget, the timeouts and the planner.
var modelFlags = map[string]bool{
	"profile": true, "unsettled": true, "max-restarts": true, "avoid": true, "avoid-holes": true, "stall-window": true, "fencing": true, "reboot": true,
}

// clusterFlags are the model flags of a steps file's header that also
// configure the cluster (code.var.sh: --fencing is MBT_FENCING, the
// driver configuration of every node; --reboot is MBT_REBOOT, the
// node-reboot verb of the executor): a command line value that
// contradicts the header is an error, never replaced, or the model and
// the cluster would run in different modes. A header without one is
// not of this mbt-cxl: an error; a file without a header has no model of
// its own: the command line's.
var clusterFlags = map[string]bool{"fencing": true, "reboot": true}

// applyHeader sets the model flags of a steps file's header over the
// command line's, and says where they differ; an error for a cluster
// flag that differs.
func applyHeader(fs *flag.FlagSet, header []string, stderr io.Writer) error {
	explicit := map[string]string{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = f.Value.String() })
	inHeader := map[string]string{}
	for i := 0; i < len(header); i++ {
		name, val, hasVal := strings.Cut(strings.TrimLeft(header[i], "-"), "=")
		if !hasVal && i+1 < len(header) && !strings.HasPrefix(header[i+1], "--") {
			val = header[i+1]
			i++
		}
		f := fs.Lookup(name)
		if f == nil || !modelFlags[name] {
			continue
		}
		inHeader[name] = val
	}
	if len(header) > 0 {
		for name := range clusterFlags {
			if _, ok := inHeader[name]; !ok {
				return fmt.Errorf("the header of the steps file has no --%s: not a file of this mbt-cxl", name)
			}
		}
	}
	names := make([]string, 0, len(inHeader))
	for name := range inHeader {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		val := inHeader[name]
		if old, ok := explicit[name]; ok && old != val {
			if clusterFlags[name] {
				return fmt.Errorf("--%s=%s of the command line, but the steps file is of --%s=%s (it configures the cluster too: MBT_%s; edit the header to run the file in another mode)",
					name, old, name, val, strings.ToUpper(name))
			}
			_, _ = fmt.Fprintf(stderr, "mbt-cxl: --%s %q of the command line replaced by %q of the steps file (A16)\n", name, old, val)
		}
		if err := fs.Set(name, val); err != nil {
			return fmt.Errorf("header --%s %q: %w", name, val, err)
		}
	}
	return nil
}

func runGenerate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c commonFlags
	c.register(fs, false)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var from []stepLine
	if c.from != "" {
		data, err := os.ReadFile(c.from)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "mbt-cxl: %v\n", err)
			return 2
		}
		lines, header := readSteps(string(data))
		if err := applyHeader(fs, header, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "mbt-cxl: %s: %v\n", c.from, err)
			return 2
		}
		from = append([]stepLine{}, lines...)
	}
	o, err := c.options(false, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mbt-cxl: %v\n", err)
		return 2
	}
	if c.from != "" {
		o.header += " --from " + c.from
	} else if c.steps <= 0 {
		_, _ = fmt.Fprintln(stderr, "mbt-cxl: generate needs --steps (or --from)")
		return 2
	}
	if err := generate(o, from, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "mbt-cxl: %v\n", err)
		return 1
	}
	return 0
}
