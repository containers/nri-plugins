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

package topologyaware

// TestE2eishReplay replays the E2EISH records of the e2e test logs in
// testdata/e2eish-replay and $E2EISH_REPLAY against the policy alone. See
// "Replaying test logs as unit tests" in test/e2e/README.md.

import (
	"bufio"
	"cmp"
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
	"sigs.k8s.io/yaml"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/utils"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

func TestE2eishReplay(t *testing.T) {
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
			records, err := readE2eishRecords(path)
			require.NoError(t, err)
			replayE2eish(t, records)
		})
	}
}

// e2eishRecord is one E2EISH record of an e2e test log.
type e2eishRecord struct {
	line      int
	Op        string           `json:"op"`
	Topology  []map[string]any `json:"topology"`
	Values    json.RawMessage  `json:"values"`
	Namespace string           `json:"namespace"`
	Name      string           `json:"name"`
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
}

// e2eishSyncPod is a pod of a sync record.
type e2eishSyncPod struct {
	corev1.Pod
	Running []string          `json:"running"`
	IDs     map[string]string `json:"ids"`
}

func (r *e2eishRecord) String() string {
	return fmt.Sprintf("line %d: %s", r.line, r.Op)
}

func readE2eishRecords(path string) ([]*e2eishRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var (
		records []*e2eishRecord
		scanner = bufio.NewScanner(f)
		line    = 0
	)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		line++
		text, ok := strings.CutPrefix(scanner.Text(), "E2EISH ")
		if !ok {
			continue
		}
		r := &e2eishRecord{line: line}
		if err := json.Unmarshal([]byte(text), r); err != nil {
			return nil, fmt.Errorf("%s:%d: bad record: %w", path, line, err)
		}
		records = append(records, r)
	}
	return records, scanner.Err()
}

// e2eishLaunch is the topology and helm values of a launch record.
type e2eishLaunch struct {
	topology e2eTopology
	config   *cfgapi.Config
	env      map[string]string // the extraEnv of the helm values
}

// replayE2eish replays the records of one e2e test log.
func replayE2eish(t *testing.T, records []*e2eishRecord) {
	// The e2e deployment enables test hints.
	t.Setenv(utils.EnvVarEnableTestAPIs, "1")

	var (
		e          *e2eish
		launches   = map[*e2eishRecord]*e2eishLaunch{}
		initEnv    map[string]string // of the first launch
		host       = e2eHost{}       // as the machine records change it
		launched   = false
		failLaunch = false
		skipping   *e2eishRecord   // the skip record of the block being skipped
		skipIDs    map[string]bool // containers running at its start
		skipped    = 0             // verifies in skipped blocks
		consts     = ""
		verified   = 0
	)

	// Variables pkg/sysfs reads when the binary starts need a child process,
	// set as for the first launch.
	for _, r := range records {
		if r.Op != "launch" {
			continue
		}
		l := parseE2eishLaunch(t, r)
		launches[r] = l
		if initEnv == nil {
			initEnv = map[string]string{}
			for _, name := range e2eishInitEnv {
				initEnv[name] = l.env[name]
			}
		}
	}
	var childEnv []string
	for _, name := range e2eishInitEnv {
		if v := initEnv[name]; v != "" {
			childEnv = append(childEnv, name+"="+v)
		}
	}
	if len(childEnv) > 0 && e2eReexec(t, childEnv...) {
		return
	}

	unsupported := func(r *e2eishRecord, format string, args ...any) {
		t.Helper()
		t.Skipf("%s: %s, after %d verified and %d skipped assertions", r,
			fmt.Sprintf(format, args...), verified, skipped)
	}
	runningIDs := func(r *e2eishRecord) ([]*e2eishSyncPod, map[string]bool) {
		t.Helper()
		var pods []*e2eishSyncPod
		require.NoError(t, json.Unmarshal(r.Pods, &pods), "%s: bad pods", r)
		ids := map[string]bool{}
		for _, p := range pods {
			for _, id := range p.IDs {
				ids[id] = true
			}
		}
		return pods, ids
	}

	for _, r := range records {
		// A skipped block changes no pods and checks nothing for the replay.
		if skipping != nil {
			switch r.Op {
			case "verify":
				skipped++
				continue
			case "create", "delete", "unsupported":
				continue
			}
		}

		switch r.Op {
		case "launch":
			l := launches[r]
			for _, name := range e2eishInitEnv {
				if l.env[name] != initEnv[name] {
					unsupported(r, "%s other than at the first launch", name)
				}
			}
			// The plugin discovers the machine at each start, with the
			// overrides of its helm values.
			for _, name := range e2eishLaunchEnv {
				t.Setenv(name, l.env[name])
			}
			host.Caches = l.env["OVERRIDE_SYS_CACHES"]
			if e == nil {
				e = newE2eishWithCaches(t, l.topology, host.Caches)
			}
			e.machine = e2eMachine(t, l.topology, host)
			if failLaunch {
				e.launchFails(l.config)
				failLaunch = false
			} else {
				e.launch(l.config)
				launched = true
			}

		case "machine":
			// The node reboots, which restarts a running plugin with no
			// launch record.
			if launched {
				unsupported(r, "changing the machine with the plugin running")
			}
			if r.NUMA != nil {
				host.NoNUMA = !*r.NUMA
			}
			if r.Cmdline != nil {
				isolated, err := e2eishIsolated(*r.Cmdline)
				if err != nil {
					unsupported(r, "%v", err)
				}
				host.Isolated = isolated
			}

		case "sync":
			require.NotNil(t, e, "%s: a sync with no policy launched", r)
			var pods []*e2eishSyncPod
			require.NoError(t, json.Unmarshal(r.Pods, &pods), "%s: bad pods", r)
			for _, p := range pods {
				if len(p.Spec.ResourceClaims) > 0 {
					unsupported(r, "a pod with resource claims")
				}
			}
			e.syncPods(pods)

		case "expect-launch-failure":
			failLaunch = true

		case "terminate":
			launched = false

		case "create":
			if string(r.Object) == "null" || len(r.Object) == 0 {
				unsupported(r, "an object the e2e test could not log")
			}
			var meta struct{ Kind string }
			require.NoError(t, json.Unmarshal(r.Object, &meta), "%s: bad object", r)
			if meta.Kind != "Pod" {
				unsupported(r, "creating a %s", meta.Kind)
			}
			pod := &corev1.Pod{}
			require.NoError(t, json.Unmarshal(r.Object, pod), "%s: bad pod", r)
			if len(pod.Spec.ResourceClaims) > 0 {
				unsupported(r, "a pod with resource claims")
			}
			require.NotNil(t, e, "%s: creating a pod with no policy launched", r)
			e.createFromPod(cmp.Or(pod.Namespace, r.Namespace), pod)

		case "delete":
			if e == nil {
				continue
			}
			var names []string
			ns := r.Namespace
			if !r.All {
				require.NoError(t, json.Unmarshal(r.Pods, &names), "%s: bad pods", r)
			} else {
				names = nil
				for name, pod := range e.pods {
					if pod.GetNamespace() == ns {
						names = append(names, name)
					}
				}
				slices.Sort(names)
			}
			for _, name := range names {
				if pod, ok := e.pods[name]; ok && pod.GetNamespace() == ns {
					e.deletePods(name)
				}
			}

		case "consts":
			consts = r.Code

		case "verify":
			require.NotNil(t, e, "%s: a verify with no policy launched", r)
			held, err := e.evalVerify(r.Expr, consts)
			if errors.Is(err, errE2eishNoPython) || errors.Is(err, errE2eishNoName) {
				unsupported(r, "%v", err)
			}
			require.NoError(t, err, "%s: failed to evaluate %q", r, r.Expr)
			e2eLog.Infof("===== verify %q: e2e %s, replay held %v", r.Expr, r.Result, held)
			require.Equal(t, r.Result == "ok", held,
				"%s: %q is %v in the replay, %s in the e2e test", r, r.Expr, held, r.Result)
			verified++

		case "skip":
			require.Nil(t, skipping, "%s: a skip within the skip of %s", r, skipping)
			e2eLog.Infof("===== skip: %s", r.Reason)
			skipping = r
			_, skipIDs = runningIDs(r)

		case "resume":
			require.NotNil(t, skipping, "%s: a resume with no skip", r)
			pods, _ := runningIDs(r)
			for _, p := range pods {
				for ctr, id := range p.IDs {
					if !skipIDs[id] {
						unsupported(r, "%s/%s/%s, created in the block skipped at %s, still runs",
							p.Namespace, p.Name, ctr, skipping)
					}
				}
			}
			if e != nil {
				e.forgetGone(pods)
			}
			e2eLog.Infof("===== resume after skipping %s", skipping)
			skipping = nil

		case "unsupported":
			// Older versions of e2eish.bash took the framework's own fuser
			// --kill for a command which kills containers.
			if strings.HasPrefix(r.Command, "fuser ") {
				e2eLog.Infof("===== ignoring %q", r.Command)
				continue
			}
			unsupported(r, "%s", r.Command)

		default:
			unsupported(r, "unknown record")
		}
	}

	require.Nil(t, skipping, "the skip of %s has no resume", skipping)
	t.Logf("%d verified assertions, %d skipped", verified, skipped)
}

// forgetGone deletes the pods and stops the containers which do not run any
// more, as the e2e test deleted or stopped them in a skipped block.
func (e *e2eish) forgetGone(pods []*e2eishSyncPod) {
	e.t.Helper()

	running := map[string][]string{}
	for _, p := range pods {
		running[p.Namespace+"/"+p.Name] = p.Running
	}
	for _, name := range slices.Sorted(maps.Keys(e.pods)) {
		pod := e.pods[name]
		ctrs, ok := running[pod.GetNamespace()+"/"+name]
		if !ok {
			e.deletePods(name)
			continue
		}
		for _, c := range pod.GetContainers() {
			if !slices.Contains(ctrs, c.GetName()) && c.GetState() == cache.ContainerStateRunning {
				e.stopContainer(c)
			}
		}
	}
}

// parseE2eishLaunch parses the topology and the helm values of a launch.
func parseE2eishLaunch(t *testing.T, r *e2eishRecord) *e2eishLaunch {
	t.Helper()

	topo := e2eTopology{}
	for _, g := range r.Topology {
		group := e2eGroup{}
		for key, value := range g {
			n, _ := value.(float64)
			switch key {
			case "mem":
				group.Mem, _ = value.(string)
			case "cores":
				group.Cores = int(n)
			case "threads":
				group.Threads = int(n)
			case "nodes":
				group.Nodes = int(n)
			case "dies":
				group.Dies = int(n)
			case "packages":
				group.Packages = int(n)
			case "node-dist":
				group.NodeDist = map[int]int{}
				for node, d := range value.(map[string]any) {
					id, err := strconv.Atoi(node)
					require.NoError(t, err, "%s: bad node-dist", r)
					group.NodeDist[id] = int(d.(float64))
				}
			default:
				t.Skipf("%s: topology key %q is not supported", r, key)
			}
		}
		topo = append(topo, group)
	}

	var values struct {
		Config   json.RawMessage   `json:"config"`
		ExtraEnv map[string]string `json:"extraEnv"`
	}
	require.NoError(t, json.Unmarshal(r.Values, &values), "%s: bad helm values", r)

	// The chart renders an extraEnv value unquoted into the YAML of the
	// daemonset, which takes off one more level of quotes.
	for name, value := range values.ExtraEnv {
		var rendered struct{ Value string }
		if err := yaml.Unmarshal([]byte("value: "+value), &rendered); err == nil {
			values.ExtraEnv[name] = rendered.Value
		}
	}

	// The defaults of the chart and the CRD. ControllableInterrupts stays
	// unset, so the replay never touches host IRQs.
	preferIsolated := true
	cfg := &cfgapi.Config{
		ReservedResources:  cfgapi.Constraints{cfgapi.CPU: "750m"},
		PinCPU:             true,
		PinMemory:          true,
		PreferIsolated:     &preferIsolated,
		DefaultCPUPriority: cfgapi.PriorityNone,
		UnlimitedBurstable: cfgapi.CPUTopologyLevelPackage,
	}
	if len(values.Config) > 0 {
		require.NoError(t, json.Unmarshal(values.Config, cfg), "%s: bad policy config", r)
	}
	cfg.ControllableInterrupts = nil

	return &e2eishLaunch{topology: topo, config: cfg, env: values.ExtraEnv}
}

// createFromPod creates a pod and starts its containers as the kubelet would
// for the pod object. Containers which fail allocation are left stale.
func (e *e2eish) createFromPod(ns string, pod *corev1.Pod) {
	e.t.Helper()
	e2eLog.Infof("===== create %s/%s", ns, pod.Name)

	qos := podQOS(pod)
	p := e.insertPod(ns, pod.Name, qos, pod.Annotations)
	for _, c := range pod.Spec.InitContainers {
		ctr, err := e.createContainer(ctrSpec{p, qos, containerResourcesOf(&c)}, c.Name)
		if err != nil {
			e2eLog.Warnf("===== %s failed: %v", c.Name, err)
			continue
		}
		e.stopContainer(ctr)
	}
	for _, c := range pod.Spec.Containers {
		if _, err := e.createContainer(ctrSpec{p, qos, containerResourcesOf(&c)}, c.Name); err != nil {
			e2eLog.Warnf("===== %s failed: %v", c.Name, err)
		}
	}
}

// podQOS gives the QoS class of a pod, "" for guaranteed.
func podQOS(pod *corev1.Pod) string {
	var (
		ctrs       = append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...)
		requested  = false
		guaranteed = true
	)
	for _, c := range ctrs {
		r := c.Resources
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			lim, hasLim := r.Limits[name]
			req, hasReq := r.Requests[name]
			requested = requested || hasLim || hasReq
			if !hasReq {
				req, hasReq = lim, hasLim
			}
			if !hasLim || !hasReq || req.Cmp(lim) != 0 {
				guaranteed = false
			}
		}
	}
	switch {
	case !requested:
		return "besteffort"
	case guaranteed:
		return ""
	default:
		return "burstable"
	}
}

// containerResourcesOf gives the resources of a container. A request left
// out is the limit.
func containerResourcesOf(c *corev1.Container) containerResources {
	r := c.Resources
	get := func(list corev1.ResourceList, name corev1.ResourceName, milli bool) int64 {
		q, ok := list[name]
		if !ok {
			return 0
		}
		if milli {
			return q.MilliValue()
		}
		return q.Value()
	}
	res := containerResources{
		cpuReq: get(r.Requests, corev1.ResourceCPU, true),
		cpuLim: get(r.Limits, corev1.ResourceCPU, true),
		memReq: get(r.Requests, corev1.ResourceMemory, false),
		memLim: get(r.Limits, corev1.ResourceMemory, false),
	}
	if _, ok := r.Requests[corev1.ResourceCPU]; !ok {
		res.cpuReq = res.cpuLim
	}
	if _, ok := r.Requests[corev1.ResourceMemory]; !ok {
		res.memReq = res.memLim
	}
	return res
}

// syncPods does what the resource manager does for the Synchronize of a
// starting plugin: it caches the running containers of the pods, and syncs
// the policy with them, in the order of the pods and their containers.
func (e *e2eish) syncPods(pods []*e2eishSyncPod) {
	e.t.Helper()

	var ctrs []cache.Container
	for _, p := range pods {
		e2eLog.Infof("===== sync %s/%s %v", p.Namespace, p.Name, p.Running)
		qos := map[corev1.PodQOSClass]string{
			corev1.PodQOSGuaranteed: "",
			corev1.PodQOSBurstable:  "burstable",
			corev1.PodQOSBestEffort: "besteffort",
		}[p.Status.QOSClass]
		if p.Status.QOSClass == "" {
			qos = podQOS(&p.Pod)
		}
		pod := e.insertPod(p.Namespace, p.Name, qos, p.Annotations)
		for _, c := range p.Spec.Containers {
			if !slices.Contains(p.Running, c.Name) {
				continue
			}
			ctr := e.insertContainer(ctrSpec{pod, qos, containerResourcesOf(&c)}, c.Name)
			ctr.UpdateState(cache.ContainerStateRunning)
			key := c.Name
			if _, taken := e.ctrs[key]; taken {
				key = p.Namespace + "/" + p.Name + "/" + c.Name
			}
			e.ctrs[key] = ctr
			ctrs = append(ctrs, ctr)
		}
	}

	e.owner.Lock()
	defer e.owner.Unlock()
	require.NoError(e.t, e.policy.Sync(ctrs, ctrs), "failed to sync the policy")
}

var (
	errE2eishNoPython = errors.New("no python3 to evaluate verify expressions")
	errE2eishNoName   = errors.New("a name the replay does not provide")

	// e2eishInitEnv are the overrides pkg/sysfs reads when the binary
	// starts, e2eishLaunchEnv those it reads at each discovery.
	e2eishInitEnv   = []string{"OVERRIDE_SYS_MEMORY_TYPE", "OVERRIDE_SYS_CPUFREQ"}
	e2eishLaunchEnv = []string{"OVERRIDE_SYS_CORE_CPUS", "OVERRIDE_SYS_ATOM_CPUS"}

	// e2eishTestContainer is what the e2e test sees of the containers.
	e2eishTestContainer = regexp.MustCompile(`^pod[0-9]*c[0-9]*$`)
)

// e2eishLibDir is test/e2e/lib.
func e2eishLibDir() string {
	return filepath.Join("..", "..", "..", "..", "test", "e2e", "lib")
}

// evalVerify evaluates a verify expression of an e2e test on the replay, with
// the variables the e2e test computes on the VM and the py_consts code. It
// tells if the expression held.
func (e *e2eish) evalVerify(expr, consts string) (bool, error) {
	e.t.Helper()

	python, err := exec.LookPath("python3")
	if err != nil {
		return false, errE2eishNoPython
	}

	var (
		dir     = e.t.TempDir()
		lib     = e2eishLibDir()
		dump    = filepath.Join(dir, "topology_dump")
		allowed = filepath.Join(dir, "res_allowed")
	)
	require.NoError(e.t, os.WriteFile(dump, []byte(e.topologyDump()), 0o644))
	require.NoError(e.t, os.WriteFile(allowed, []byte(e.resAllowed()), 0o644))

	tree, err := exec.Command(python, filepath.Join(lib, "topology.py"),
		"-t", dump, "-r", allowed, "res_allowed", "-o", "json").Output()
	if err != nil {
		return false, fmt.Errorf("topology.py: %w", err)
	}
	glue, err := os.ReadFile(filepath.Join(lib, "py_allowed.py"))
	require.NoError(e.t, err)

	// The state pyexec gives verify, and the assertion as verify runs it.
	code := "import pprint; pp=pprint.pprint\n" +
		"import re\nallowed=" + strings.TrimSpace(string(tree)) + "\n" +
		string(glue) + "\n" + consts + "\n" +
		"import sys\n" +
		"try:\n    assert(" + expr + ")\n" +
		"except AssertionError:\n    sys.exit(10)\n" +
		"except NameError as e:\n    print(e)\n    sys.exit(11)\n"
	program := filepath.Join(dir, "verify.py")
	require.NoError(e.t, os.WriteFile(program, []byte(code), 0o644))

	cmd := exec.Command(python, program)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+lib)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 10:
		return false, nil
	case errors.As(err, &exit) && exit.ExitCode() == 11:
		return false, fmt.Errorf("%w: %s", errE2eishNoName, strings.TrimSpace(string(out)))
	default:
		return false, fmt.Errorf("%w: %s", err, out)
	}
}

// topologyDump is what topology.py bash_topology_dump prints on the VM.
func (e *e2eish) topologyDump() string {
	b := &strings.Builder{}
	for _, id := range e.machine.CPUIDs() {
		c := e.machine.CPU(id)
		fmt.Fprintf(b, "cpu p:%d d:%d n:%d c:%d t:%s cpu:%d\n", c.PackageID(), c.DieID(),
			c.NodeID(), c.CoreID(), e2eishMask(c.ThreadCPUSet().List()), id)
	}
	for _, id := range e.machine.NodeIDs() {
		n := e.machine.Node(id)
		dist := make([]string, 0, len(n.Distance()))
		for _, d := range n.Distance() {
			dist = append(dist, strconv.Itoa(d))
		}
		info, err := n.MemoryInfo()
		require.NoError(e.t, err)
		fmt.Fprintf(b, "dist n:%d d:%s\n", id, strings.Join(dist, " "))
		fmt.Fprintf(b, "mem n:%d s:%g\n", id, float64(info.MemTotal)/(1<<20))
	}
	return b.String()
}

// resAllowed is what topology.py bash_res_allowed prints on the VM for the
// running test containers: name/pid, and the CPUs and memory nodes allowed.
func (e *e2eish) resAllowed() string {
	b := &strings.Builder{}
	for i, name := range e.running() {
		if !e2eishTestContainer.MatchString(name) {
			continue
		}
		cpus, mems := e.allowed(name)
		memIDs := e.machine.NodeIDs()
		if mems != "" {
			set, err := cpuset.Parse(mems)
			require.NoError(e.t, err)
			memIDs = set.List()
		}
		fmt.Fprintf(b, "%s/%d c:%s m:%s\n", name, 1000+i, e2eishMask(cpus.List()), e2eishMask(memIDs))
	}
	return b.String()
}

// e2eishMask is a bitmask in hex, as /proc and /sys print them.
func e2eishMask(ids []int) string {
	mask := new(big.Int)
	for _, id := range ids {
		mask.SetBit(mask, id, 1)
	}
	return mask.Text(16)
}

// e2eishIsolated gives the CPUs isolated on a kernel command line. Other
// parameters are not supported.
func e2eishIsolated(cmdline string) (string, error) {
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
