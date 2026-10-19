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

package e2eish

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// Replay replays the E2EISH records of the e2e test logs in
// testdata/e2eish-replay and $E2EISH_REPLAY against the policy of the plugin,
// each as a subtest. See "Replaying test logs as unit tests" in
// test/e2e/README.md.
func Replay(t *testing.T, plugin *Plugin) {
	logs, err := filepath.Glob("testdata/e2eish-replay/*.log")
	require.NoError(t, err)
	logs = append(logs, filepath.SplitList(os.Getenv("E2EISH_REPLAY"))...)

	for _, path := range logs {
		// A run.sh.output is named after its test, by its directory.
		name := filepath.Base(path)
		if name == "run.sh.output" {
			name = filepath.Base(filepath.Dir(path))
		}
		t.Run(name, func(t *testing.T) {
			records, err := readRecords(path)
			require.NoError(t, err)
			if other := pluginOf(path, records); other != "" && other != plugin.Name {
				t.Skipf("a log of the %s plugin", other)
			}
			replayRecords(t, plugin, records)
		})
	}
}

// pluginOf gives the plugin a log launches, by its first launch record, or
// by the innermost directory of its path named after a plugin, for logs whose
// records do not tell.
func pluginOf(path string, records []*record) string {
	for _, r := range records {
		if r.Op == "launch" && r.Plugin != "" {
			return r.Plugin
		}
	}
	dirs := strings.Split(filepath.ToSlash(path), "/")
	for _, dir := range slices.Backward(dirs) {
		switch dir {
		case "topology-aware", "balloons", "template":
			return dir
		}
	}
	return ""
}

func (p *Plugin) unsupportedPod(pod *corev1.Pod) string {
	if p.UnsupportedPod == nil {
		return ""
	}
	return p.UnsupportedPod(pod)
}

func (p *Plugin) unsupportedConfig(cfg any) string {
	if p.UnsupportedConfig == nil {
		return ""
	}
	return p.UnsupportedConfig(cfg)
}

// record is one E2EISH record of an e2e test log.
type record struct {
	line      int
	Op        string           `json:"op"`
	Plugin    string           `json:"plugin"`
	Topology  []map[string]any `json:"topology"`
	Values    json.RawMessage  `json:"values"`
	Namespace string           `json:"namespace"`
	Name      string           `json:"name"`
	Container string           `json:"container"`
	CPU       string           `json:"cpu"`
	Object    json.RawMessage  `json:"object"`
	Pods      json.RawMessage  `json:"pods"`
	All       bool             `json:"all"`
	Code      string           `json:"code"`
	Result    string           `json:"result"`
	Expr      string           `json:"expr"`
	Command   string           `json:"command"`
	NUMA      *bool            `json:"numa"`
	Cmdline   *string          `json:"cmdline"`
	Reason    string           `json:"reason"`
	MayFail   bool             `json:"mayFail"`
}

// syncPod is a pod of a sync record.
type syncPod struct {
	corev1.Pod
	Running []string          `json:"running"`
	IDs     map[string]string `json:"ids"`
}

func (r *record) String() string {
	return fmt.Sprintf("line %d: %s", r.line, r.Op)
}

func readRecords(path string) ([]*record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var (
		records []*record
		scanner = bufio.NewScanner(f)
		line    = 0
	)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		line++
		// Output of a command without a final newline may come before a
		// record on its line.
		i := strings.Index(scanner.Text(), "E2EISH {")
		if i < 0 {
			continue
		}
		text := scanner.Text()[i+len("E2EISH "):]
		r := &record{line: line}
		if err := json.Unmarshal([]byte(text), r); err != nil {
			return nil, fmt.Errorf("%s:%d: bad record: %w", path, line, err)
		}
		records = append(records, r)
	}
	return records, scanner.Err()
}

// launchRecord is the topology and the helm configuration of a launch
// record.
type launchRecord struct {
	topology Topology
	*launchConfig
}

// segment is a run of the records of a log, from one launch, whose launches
// all have the same overrides read when the binary starts.
type segment struct {
	from, to int // records[from:to]
	env      []string
}

// segments splits a log at the launches which change the overrides read when
// the binary starts. A segment can start fresh at its first launch: the sync
// record after a launch has the pods then running.
func segments(t *testing.T, plugin *Plugin, records []*record) ([]segment, map[*record]*launchRecord) {
	t.Helper()

	var (
		segs     []segment
		launches = map[*record]*launchRecord{}
		key      *string
	)
	for i, r := range records {
		if r.Op != "launch" {
			continue
		}
		l := parseLaunch(t, plugin, r)
		launches[r] = l
		var env []string
		for _, name := range initEnvVars {
			if v := l.env[name]; v != "" {
				env = append(env, name+"="+v)
			}
		}
		k := strings.Join(env, "\x00")
		if key != nil && *key == k {
			continue
		}
		key = &k
		// An expect-launch-failure goes with the launch after it.
		from := i
		for from > 0 && records[from-1].Op == "expect-launch-failure" {
			from--
		}
		if len(segs) > 0 {
			segs[len(segs)-1].to = from
		} else {
			from = 0
		}
		segs = append(segs, segment{from: from, to: len(records), env: env})
	}
	if len(segs) == 0 {
		segs = []segment{{from: 0, to: len(records)}}
	}
	return segs, launches
}

// replayRecords replays the records of one e2e test log. Overrides read
// when the binary starts need a child process, so a log with launches of
// different such overrides replays in segments, each in its own process.
func replayRecords(t *testing.T, plugin *Plugin, records []*record) {
	segs, launches := segments(t, plugin, records)
	if len(segs) == 1 {
		if len(segs[0].env) > 0 && Reexec(t, segs[0].env...) {
			return
		}
		replaySegment(t, plugin, records, segs[0], launches)
		return
	}
	for i, seg := range segs {
		t.Run(fmt.Sprintf("segment%d", i), func(t *testing.T) {
			if len(seg.env) > 0 && Reexec(t, seg.env...) {
				return
			}
			replaySegment(t, plugin, records, seg, launches)
		})
	}
}

// replaySegment replays a segment of the records of a log on a script. The
// records before it only set up the machine and the py_consts.
func replaySegment(t *testing.T, plugin *Plugin, records []*record, seg segment, launches map[*record]*launchRecord) {
	t.Helper()

	var (
		s          = NewScript(t, plugin)
		failLaunch = false
		skipIDs    map[string]bool // containers running at the skip
	)
	s.requirePython = true

	for i, r := range records[:seg.to] {
		// Before the segment, only the machine and the py_consts matter.
		if i < seg.from && r.Op != "machine" && r.Op != "consts" {
			continue
		}
		replayRecord(t, s, r, launches, &failLaunch, &skipIDs)
	}
}

// replayRecord replays one record on the script.
func replayRecord(t *testing.T, s *Script, r *record, launches map[*record]*launchRecord, failLaunch *bool, skipIDs *map[string]bool) {
	t.Helper()

	s.at = r.String()
	unsupported := s.unsupported
	pods := func() []*syncPod {
		t.Helper()
		var pods []*syncPod
		require.NoError(t, json.Unmarshal(r.Pods, &pods), "%s: bad pods", r)
		return pods
	}

	switch r.Op {
	case "launch":
		l := launches[r]
		how := launchOK
		switch {
		case *failLaunch:
			how = launchFails
			*failLaunch = false
		case r.MayFail:
			how = launchMayFail
		}
		s.launch(l.topology, l.launchConfig, how)

	case "expect-launch-failure":
		*failLaunch = true

	case "machine":
		if r.NUMA != nil {
			s.SetNUMA(*r.NUMA)
		}
		if r.Cmdline != nil {
			s.SetCmdline(*r.Cmdline)
		}

	case "sync":
		require.NotNil(t, s.h, "%s: a sync with no policy launched", r)
		s.sync(pods())

	case "terminate":
		s.Terminate()

	case "create":
		if string(r.Object) == "null" || len(r.Object) == 0 {
			if s.skipping != "" {
				return
			}
			unsupported("an object the e2e test could not log")
		}
		var meta struct{ Kind string }
		require.NoError(t, json.Unmarshal(r.Object, &meta), "%s: bad object", r)
		if meta.Kind != "Pod" {
			if s.skipping != "" {
				return
			}
			unsupported("creating a %s", meta.Kind)
		}
		pod := &corev1.Pod{}
		require.NoError(t, json.Unmarshal(r.Object, pod), "%s: bad pod", r)
		s.createPod(r.Namespace, pod)

	case "delete":
		if r.All {
			s.DeleteAllPodsIn(r.Namespace)
			return
		}
		var names []string
		require.NoError(t, json.Unmarshal(r.Pods, &names), "%s: bad pods", r)
		s.DeletePods(r.Namespace, names...)

	case "consts":
		s.Consts(r.Code)

	case "verify":
		s.verify(r.Expr, r.Result == "ok")

	case "skip":
		s.Skip(r.Reason)
		*skipIDs = runningIDs(pods())

	case "resume":
		running := pods()
		for _, p := range running {
			for ctr, id := range p.IDs {
				if !(*skipIDs)[id] {
					unsupported("%s/%s/%s, created in the block skipped for %q, still runs",
						p.Namespace, p.Name, ctr, s.skipping)
				}
			}
		}
		s.resume(running)

	case "unsupported":
		// Older versions of e2eish.bash took the framework's own fuser
		// --kill for a command which kills containers.
		if strings.HasPrefix(r.Command, "fuser ") {
			Log.Infof("===== ignoring %q", r.Command)
			return
		}
		if s.skipping != "" {
			return
		}
		unsupported("%s", r.Command)

	default:
		unsupported("unknown record")
	}
}

// runningIDs gives the IDs of the containers of the pods.
func runningIDs(pods []*syncPod) map[string]bool {
	ids := map[string]bool{}
	for _, p := range pods {
		for _, id := range p.IDs {
			ids[id] = true
		}
	}
	return ids
}

// forgetGone deletes the pods and stops the containers which do not run any
// more, as the e2e test deleted or stopped them in a skipped block.
func (h *Harness) forgetGone(pods []*syncPod) {
	h.T.Helper()

	running := map[string][]string{}
	for _, p := range pods {
		running[p.Namespace+"/"+p.Name] = p.Running
	}
	for _, name := range slices.Sorted(maps.Keys(h.Pods)) {
		pod := h.Pods[name]
		ctrs, ok := running[pod.GetNamespace()+"/"+name]
		if !ok {
			h.DeletePods(name)
			continue
		}
		for _, c := range pod.GetContainers() {
			if !slices.Contains(ctrs, c.GetName()) && c.GetState() == cache.ContainerStateRunning {
				h.StopContainer(c)
			}
		}
	}
}

// parseLaunch parses the topology and the helm values of a launch.
func parseLaunch(t *testing.T, plugin *Plugin, r *record) *launchRecord {
	t.Helper()

	topo, err := topologyOfGroups(r.Topology)
	if err != nil {
		t.Skipf("%s: %v", r, err)
	}

	cfg, env, err := plugin.HelmConfig(r.Values)
	require.NoError(t, err, "%s: bad helm values", r)
	return &launchRecord{topology: topo, launchConfig: &launchConfig{config: cfg, env: env}}
}

// syncPods does what the resource manager does for the Synchronize of a
// starting plugin: it caches the running containers of the pods, and syncs
// the policy with them, in the order of the pods and their containers.
func (h *Harness) syncPods(pods []*syncPod) {
	h.T.Helper()

	var ctrs []cache.Container
	for _, p := range pods {
		Log.Infof("===== sync %s/%s %v", p.Namespace, p.Name, p.Running)
		qos := map[corev1.PodQOSClass]string{
			corev1.PodQOSGuaranteed: "",
			corev1.PodQOSBurstable:  "burstable",
			corev1.PodQOSBestEffort: "besteffort",
		}[p.Status.QOSClass]
		if p.Status.QOSClass == "" {
			qos = PodQOS(&p.Pod)
		}
		pod := h.InsertPod(p.Namespace, p.Name, qos, p.Labels, p.Annotations)
		for _, c := range p.Spec.Containers {
			if !slices.Contains(p.Running, c.Name) {
				continue
			}
			ctr := h.InsertContainer(CtrSpec{pod, qos, ResourcesOf(&c)}, c.Name)
			ctr.UpdateState(cache.ContainerStateRunning)
			key := c.Name
			if _, taken := h.Ctrs[key]; taken {
				key = p.Namespace + "/" + p.Name + "/" + c.Name
			}
			h.Ctrs[key] = ctr
			ctrs = append(ctrs, ctr)
		}
	}

	h.Owner.Lock()
	defer h.Owner.Unlock()
	require.NoError(h.T, h.Policy.Sync(ctrs, ctrs), "failed to sync the policy")
}

var (
	errNoPython = errors.New("no python3 to evaluate verify expressions")
	errNoName   = errors.New("a name the replay does not provide")

	// initEnvVars are the overrides read when the binary starts,
	// launchEnvVars those read at each start of the plugin.
	initEnvVars   = []string{"OVERRIDE_SYS_MEMORY_TYPE", "OVERRIDE_SYS_CPUFREQ", "OVERRIDE_SYS_CSTATES"}
	launchEnvVars = []string{"OVERRIDE_SYS_CORE_CPUS", "OVERRIDE_SYS_ATOM_CPUS", "OVERRIDE_SST"}

	// testContainer is what the e2e test sees of the containers.
	testContainer = regexp.MustCompile(`^pod[0-9]*c[0-9]*$`)
)

// libDir is test/e2e/lib.
func libDir() string {
	return filepath.Join(srcDir(), "test", "e2e", "lib")
}

// evalVerify evaluates a verify expression of an e2e test on the replay, with
// the variables the e2e test computes on the VM and the py_consts code. It
// tells if the expression held, the state the variables of stateVars had,
// their names by container, and the snapshot of the allocations it read them
// from, for the caller to cross-check the state against the same read.
func (h *Harness) evalVerify(expr, consts string) (bool, map[string]map[string][]string, map[string]allowed, error) {
	h.T.Helper()

	python, err := exec.LookPath("python3")
	if err != nil {
		return false, nil, nil, errNoPython
	}

	var (
		dir         = h.T.TempDir()
		lib         = libDir()
		dump        = filepath.Join(dir, "topology_dump")
		allowedFile = filepath.Join(dir, "res_allowed")
		snap        = h.allowedSnapshot()
	)
	require.NoError(h.T, os.WriteFile(dump, []byte(h.topologyDump()), 0o644))
	require.NoError(h.T, os.WriteFile(allowedFile, []byte(h.resAllowed(snap)), 0o644))

	tree, err := exec.Command(python, filepath.Join(lib, "topology.py"),
		"-t", dump, "-r", allowedFile, "res_allowed", "-o", "json").Output()
	if err != nil {
		return false, nil, nil, fmt.Errorf("topology.py: %w", err)
	}
	glue, err := os.ReadFile(filepath.Join(lib, "py_allowed.py"))
	require.NoError(h.T, err)

	// The state pyexec gives verify, and the assertion as verify runs it.
	code := "import pprint; pp=pprint.pprint\n" +
		"import re\nallowed=" + strings.TrimSpace(string(tree)) + "\n" +
		string(glue) + "\n" + consts + "\n" +
		"import sys, json\n" +
		"print('E2EISH-STATE ' + json.dumps({v: {c: sorted(s) for c, s in globals()[v].items()} for v in " +
		pyStrings(stateVars) + "}))\n" +
		"try:\n    assert(" + expr + ")\n" +
		"except AssertionError:\n    sys.exit(10)\n" +
		"except NameError as e:\n    print(e)\n    sys.exit(11)\n"
	program := filepath.Join(dir, "verify.py")
	require.NoError(h.T, os.WriteFile(program, []byte(code), 0o644))

	cmd := exec.Command(python, program)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+lib)
	out, err := cmd.CombinedOutput()
	var (
		state = map[string]map[string][]string{}
		last  string
		exit  *exec.ExitError
	)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if data, ok := strings.CutPrefix(line, "E2EISH-STATE "); ok {
			require.NoError(h.T, json.Unmarshal([]byte(data), &state), "bad python state")
			continue
		}
		last = line
	}
	switch {
	case err == nil:
		return true, state, snap, nil
	case errors.As(err, &exit) && exit.ExitCode() == 10:
		return false, state, snap, nil
	case errors.As(err, &exit) && exit.ExitCode() == 11:
		return false, nil, nil, fmt.Errorf("%w: %s", errNoName, last)
	default:
		return false, nil, nil, fmt.Errorf("%w: %s", err, out)
	}
}

// pyStrings is a python list of the strings.
func pyStrings(strs []string) string {
	return "['" + strings.Join(strs, "', '") + "']"
}

// topologyDump is what topology.py bash_topology_dump prints on the VM.
func (h *Harness) topologyDump() string {
	b := &strings.Builder{}
	for _, id := range h.Machine.CPUIDs() {
		c := h.Machine.CPU(id)
		fmt.Fprintf(b, "cpu p:%d d:%d n:%d c:%d t:%s cpu:%d\n", c.PackageID(), c.DieID(),
			c.NodeID(), c.CoreID(), hexMask(c.ThreadCPUSet().List()), id)
	}
	for _, id := range h.Machine.NodeIDs() {
		n := h.Machine.Node(id)
		dist := make([]string, 0, len(n.Distance()))
		for _, d := range n.Distance() {
			dist = append(dist, strconv.Itoa(d))
		}
		info, err := n.MemoryInfo()
		require.NoError(h.T, err)
		fmt.Fprintf(b, "dist n:%d d:%s\n", id, strings.Join(dist, " "))
		fmt.Fprintf(b, "mem n:%d s:%g\n", id, float64(info.MemTotal)/(1<<20))
	}
	return b.String()
}

// allowed is what one read of an allocation gives: the CPUs and the memory
// nodes a container may use, with the memory nodes of an unset cpuset.mems
// resolved to all of them, as res_allowed reports them.
type allowed struct {
	cpus cpuset.CPUSet
	mems []int
}

// allowedSnapshot reads the allocations of the running test containers once.
// A timer of the policy, like the cold start of topology-aware, can change an
// allocation at any point of a verify, so python and the Go reconstruction of
// the state must both work from one read of it, not from two.
func (h *Harness) allowedSnapshot() map[string]allowed {
	snap := map[string]allowed{}
	for _, name := range h.Running() {
		if !testContainer.MatchString(name) {
			continue
		}
		cpus, mems := h.Allowed(name)
		memIDs := h.Machine.NodeIDs()
		if mems != "" {
			set, err := cpuset.Parse(mems)
			require.NoError(h.T, err)
			memIDs = set.List()
		}
		snap[name] = allowed{cpus: cpus, mems: memIDs}
	}
	return snap
}

// resAllowed is what topology.py bash_res_allowed prints on the VM for the
// containers of the snapshot: name/pid, and the CPUs and memory nodes allowed.
func (h *Harness) resAllowed(snap map[string]allowed) string {
	b := &strings.Builder{}
	for i, name := range slices.Sorted(maps.Keys(snap)) {
		a := snap[name]
		fmt.Fprintf(b, "%s/%d c:%s m:%s\n", name, 1000+i, hexMask(a.cpus.List()), hexMask(a.mems))
	}
	return b.String()
}

// hexMask is a bitmask in hex, as /proc and /sys print them.
func hexMask(ids []int) string {
	mask := new(big.Int)
	for _, id := range ids {
		mask.SetBit(mask, id, 1)
	}
	return mask.Text(16)
}

// isolcpus gives the CPUs isolated on a kernel command line. Other
// parameters are not supported.
func isolcpus(cmdline string) (string, error) {
	isolated := cpuset.New()
	for _, param := range strings.Fields(cmdline) {
		value, ok := strings.CutPrefix(param, "isolcpus=")
		if !ok {
			return "", fmt.Errorf("kernel parameter %q", param)
		}
		// isolcpus=[flag,...,]cpu-list: the flags are the words.
		for _, item := range strings.Split(value, ",") {
			if cpus, err := cpuset.Parse(item); err == nil {
				isolated = isolated.Union(cpus)
			}
		}
	}
	return isolated.String(), nil
}
