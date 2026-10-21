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
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Generate writes a Go test for each e2e test log of the plugin in
// $E2EISH_GENERATE, one e2eish-gen-TOPOLOGY_test.go file for each topology,
// in the package directory. Build with the e2eish_generate tag, which leaves
// out the files generated before. The tests run a Script, with the variable
// e2eishPlugin of the package as the plugin. See "Generating unit tests from
// test logs" in test/e2e/README.md.
func Generate(t *testing.T, plugin *Plugin) {
	if os.Getenv("E2EISH_GENERATE") == "" {
		t.Skip("no $E2EISH_GENERATE logs to generate tests from")
	}

	var logs []string
	for _, path := range filepath.SplitList(os.Getenv("E2EISH_GENERATE")) {
		err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && (p == path || d.Name() == "run.sh.output") {
				logs = append(logs, p)
			}
			return err
		})
		require.NoError(t, err)
	}
	slices.Sort(logs)

	pkg := packageName(t)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	dir, err := filepath.Rel(srcDir(), cwd)
	require.NoError(t, err)
	files := map[string]*genFile{}
	failed := 0
	for _, path := range logs {
		records, err := readRecords(path)
		require.NoError(t, err)
		if other := pluginOf(path, records); other != "" && other != plugin.Name {
			continue
		}
		topo, test := topologyOf(t, plugin, path, records), filepath.Base(filepath.Dir(path))
		ok := t.Run(topo+"/"+test, func(t *testing.T) {
			f := files[topo]
			if f == nil {
				f = &genFile{topo: topo, plugin: plugin.Name, consts: &genConsts{}}
				files[topo] = f
			}
			g := &genTest{t: t, plugin: plugin, file: f, name: test,
				host: vmOf(path), renames: map[string]string{}}
			g.generate(records)
		})
		if !ok {
			failed++
		}
	}

	// The file of a topology is written whole, so writing it now would drop
	// the test of every log which failed to generate one.
	if failed > 0 {
		t.Fatalf("%s failed to generate, writing no test file", plural(failed, "log"))
	}

	// Every topology is checked before any file is written, so that a
	// duplicate in one of them leaves the files of the others alone.
	duplicates := 0
	for _, topo := range slices.Sorted(maps.Keys(files)) {
		dups := files[topo].duplicates()
		for _, fn := range slices.Sorted(maps.Keys(dups)) {
			t.Errorf("duplicate generated test: %s of %s is in the logs of %s; "+
				"generate from the logs of one VM of a topology",
				fn, topo, strings.Join(dups[fn], " and "))
			duplicates++
		}
	}
	if duplicates > 0 {
		t.Fatalf("%s, writing no test file", plural(duplicates, "duplicate generated test"))
	}

	for _, topo := range slices.Sorted(maps.Keys(files)) {
		f := files[topo]
		if len(f.tests) == 0 {
			continue
		}
		name := "e2eish-gen-" + topo + "_test.go"
		require.NoError(t, os.WriteFile(name, f.source(t, pkg), 0o644))
		t.Logf("wrote %s to %s", plural(len(f.tests), "test"), filepath.Join(dir, name))
	}
}

// packageName is the name of the package in the current directory.
func packageName(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	require.NotEmpty(t, files, "no test files in the current directory")
	f, err := parser.ParseFile(token.NewFileSet(), files[0], nil, parser.PackageClauseOnly)
	require.NoError(t, err)
	return f.Name.Name
}

// vmOf is the name of the e2e VM of a log, the output directory of its run:
// VM/policies.test-suite/POLICY/TEST/run.sh.output.
func vmOf(path string) string {
	dirs := strings.Split(filepath.ToSlash(path), "/")
	for i, dir := range dirs {
		if dir == "policies.test-suite" && i > 0 {
			return dirs[i-1]
		}
	}
	return ""
}

// topologyOf is the topology of the e2e tests of the plugin whose directory
// the VM of a log is named after, the longest one if more are.
// topologyOf is the topology of the e2e test of a log. A log of a full run
// is under the directory of the VM it ran on, which is named after its
// topology. run_tests.sh -o puts the output of a run in a directory of its
// own, with no VM in the path, and such a log names its topology only by the
// machine its launches asked for.
func topologyOf(t *testing.T, plugin *Plugin, path string, records []*record) string {
	t.Helper()

	dir := filepath.Join(srcDir(), "test", "e2e", "policies.test-suite", plugin.Name)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	if vm := vmOf(path); vm != "" {
		topo := ""
		for _, e := range entries {
			if n := e.Name(); e.IsDir() && (vm == n || strings.HasPrefix(vm, n+"-")) && len(n) > len(topo) {
				topo = n
			}
		}
		if topo != "" {
			return topo
		}
	}

	var launched Topology
	for _, r := range records {
		if r.Op == "launch" {
			launched, err = topologyOfGroups(r.Topology)
			require.NoError(t, err, "%s: %s: bad launch topology", path, r)
			break
		}
	}
	require.NotNil(t, launched,
		"%s: no VM in the path and no launch to take the topology from", path)

	var matched []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		topo, err := readTopology(filepath.Join(dir, e.Name(), "topology.var.json"))
		if err == nil && reflect.DeepEqual(topo, launched) {
			matched = append(matched, e.Name())
		}
	}
	require.Len(t, matched, 1,
		"%s: %d topologies of %s match the machine of its launches (%v), expected one",
		path, len(matched), plugin.Name, matched)

	return matched[0]
}

// genFile is the generated file of a topology.
type genFile struct {
	topo     string
	plugin   string
	topology Topology // of the first launch
	tests    []*genTest
	consts   *genConsts
	idWidths map[string]pyWidths // by topology literal and NUMA mode
}

// genConsts are the configs, pods and py_consts of a file, which become
// variables of the file when more than one test uses them.
type genConsts struct {
	order []genString
	uses  map[genString]map[*genTest]bool
}

func (c *genConsts) use(s genString, g *genTest) {
	if c.uses == nil {
		c.uses = map[genString]map[*genTest]bool{}
	}
	if c.uses[s] == nil {
		c.order = append(c.order, s)
		c.uses[s] = map[*genTest]bool{}
	}
	c.uses[s][g] = true
}

func (c *genConsts) shared(s genString) bool {
	return len(c.uses[s]) > 1
}

// names gives names to the shared strings: the topology variable with the
// kind of the string, numbered if there are more of a kind.
func (c *genConsts) names(prefix string) map[genString]string {
	return numberedNames(prefix, slices.DeleteFunc(slices.Clone(c.order), func(s genString) bool {
		return !c.shared(s)
	}))
}

// numberedNames names strings by their kind, numbered if there are more of a
// kind.
func numberedNames(prefix string, strs []genString) map[genString]string {
	var (
		names = map[genString]string{}
		count = map[string]int{}
		index = map[string]int{}
	)
	for _, s := range strs {
		count[s.kind]++
	}
	for _, s := range strs {
		if names[s] == "" {
			names[s] = prefix + s.kind
			if count[s.kind] > 1 {
				index[s.kind]++
				names[s] += strconv.Itoa(index[s.kind])
			}
		}
	}
	return names
}

// genTest is the generated test of a log.
type genTest struct {
	t          *testing.T
	plugin     *Plugin
	file       *genFile
	name       string
	host       string            // the VM, named in kube-system pods
	renames    map[string]string // stable names for kube-system pods
	segments   []*genSegment
	stopped    string // why the replay ends early
	verifies   int
	goVerifies int             // of verifies, those in Go
	widths     pyWidths        // of the machine last launched on
	pods       map[string]bool // of the harness, by namespace/name
	created    int             // pods created in the segment
	// uses and topos are what the test would contribute to its file, kept
	// here until the file accepts it: a test which is skipped late must not
	// name a string of the file, nor decide which topology the file declares.
	uses  []genString
	topos []Topology
}

// genConfig is the helm values of a launch, as JSON. The file renders them
// as values shared by its launches and an overlay of each.
type genConfig struct {
	values  any
	overlay *genString // nil for none
	noBase  bool       // the overlay is all the values
}

// podName is the name of a pod as the harness has it, the stable name of a
// kube-system pod the replay synced.
func (g *genTest) podName(namespace, name string) string {
	if namespace == "kube-system" {
		if n, ok := g.renames[name]; ok {
			return n
		}
	}
	return name
}

// podKey is a pod of the harness, by namespace and the name it has there.
func (g *genTest) podKey(namespace, name string) string {
	return namespace + "/" + g.podName(namespace, name)
}

// addPods adds the pods of a sync record to those of the harness.
func (g *genTest) addPods(r *record) {
	var pods []*syncPod
	require.NoError(g.t, json.Unmarshal(r.Pods, &pods), "%s: bad pods", r)
	for _, p := range pods {
		g.pods[g.podKey(p.Namespace, p.Name)] = true
	}
}

// delete is the step of a delete record, if it deletes a pod which is there.
func (g *genTest) delete(r *record) {
	ns := r.Namespace
	if r.All {
		if slices.ContainsFunc(slices.Collect(maps.Keys(g.pods)), func(key string) bool {
			return strings.HasPrefix(key, ns+"/")
		}) {
			g.step("s.DeleteAllPodsIn(%s)", strconv.Quote(ns))
		}
		maps.DeleteFunc(g.pods, func(key string, _ bool) bool { return strings.HasPrefix(key, ns+"/") })
		return
	}
	var names []string
	require.NoError(g.t, json.Unmarshal(r.Pods, &names), "%s: bad pods", r)
	args := []string{strconv.Quote(ns)}
	for _, n := range names {
		if key := g.podKey(ns, n); g.pods[key] {
			args = append(args, strconv.Quote(g.podName(ns, n)))
			delete(g.pods, key)
		}
	}
	if len(args) > 1 {
		g.step("s.DeletePods(%s)", strings.Join(args, ", "))
	}
}

// create is the step of a create record: one of a PodSpec if the pod is of
// an e2e test template, one of its options otherwise.
func (g *genTest) create(r *record) {
	pod := g.podOf(r)
	if pod == nil {
		return
	}
	if qos, spec := g.podSpec(pod); spec != "" {
		g.step("s.Create%s(%s)", qos, spec)
	} else if args := g.podArgs(pod, pod.Spec.InitContainers, nil); args != "" {
		g.step("s.CreatePod(%s)", args)
	} else {
		return
	}
	g.pods[g.podKey(pod.Namespace, pod.Name)] = true
	g.created++
}

// genSegment is a segment of a generated test. A step is a statement with
// placeholders for the strings which may become constants.
type genSegment struct {
	env   []string
	steps []genStep
}

type genStep struct {
	kind   string // "py" for VerifyPy, "consts" for Consts, else ""
	format string
	args   []any // genString for strings which may become constants
}

// genString is a string of a kind, which may become a variable: the YAML of
// a Config, the Go elements of a list of Pods, or the code of PyConsts.
type genString struct {
	kind, s string
}

// literal is the Go expression of the string itself.
func (s genString) literal() string {
	switch s.kind {
	case "Pods":
		return "\n" + s.s
	case "Inline":
		return strconv.Quote(s.s)
	}
	return goString(s.s)
}

// ref is the Go expression of the string as the variable name, or of the
// string itself without a name.
func (s genString) ref(name string) string {
	switch {
	case s.kind == "Inline" || name == "":
		return s.literal()
	case s.kind == "Pods":
		return name + "..."
	}
	return name
}

// decl is the Go declaration of the string as the variable name.
func (s genString) decl(name string) string {
	if s.kind == "Pods" {
		return fmt.Sprintf("var %s = []*harness.Pod{\n%s}\n", name, s.s)
	}
	return fmt.Sprintf("const %s = %s\n", name, goString(s.s))
}

// doc is the comment of a variable of the file.
func (s genString) doc(name string) string {
	what := map[string]string{
		"Config":   "is the helm values",
		"Pods":     "are the runtime pods",
		"PyConsts": "is the py_consts code",
	}[s.kind]
	return fmt.Sprintf("// %s %s of more than one test.\n", name, what)
}

func (g *genTest) stop(format string, args ...any) {
	g.stopped = fmt.Sprintf(format, args...)
}

// syncStep is the step of a sync or resume record, which gives the replay the
// pods running in the runtime.
func (g *genTest) syncStep(call, pods string) {
	if pods == "" {
		g.step(call + "()")
		return
	}
	g.step(call+"(%s)", genString{"Pods", pods})
}

func (g *genTest) step(format string, args ...any) {
	g.stepKind("", format, args...)
}

func (g *genTest) stepKind(kind, format string, args ...any) {
	seg := g.segments[len(g.segments)-1]
	seg.steps = append(seg.steps, genStep{kind, format, args})
	for _, a := range args {
		if s, ok := a.(genString); ok {
			g.uses = append(g.uses, s)
		}
	}
}

// generate turns the records of a log into the steps of a test, up to the
// first record the replay cannot follow.
func (g *genTest) generate(records []*record) {
	t := g.t
	segs, launches := segments(t, g.plugin, records)
	for _, r := range records {
		if r.Op == "verify" && r.Result != "ok" {
			t.Skipf("%s: the e2e test failed", r)
		}
	}

	for _, seg := range segs {
		gen := &genSegment{env: seg.env}
		g.segments = append(g.segments, gen)
		var (
			started                        bool // by a launch
			launched, failLaunch, skipping bool
			skipIDs                        map[string]bool
			skipFrom                       int    // the step which starts a skipped block
			usesFrom                       int    // the strings of the steps before it
			skipReason                     string // of the skipped block, for a stop in it
		)
		// The NUMA mode of the kernel command line, which a machine record
		// changes and the next launch takes, and which decides the id widths.
		numa := true
		// The pods of the harness, by namespace/name, to leave out deletes
		// of pods which are not there.
		g.pods, g.created = map[string]bool{}, 0
		for i, r := range records[:seg.to] {
			if i < seg.from && r.Op != "machine" && r.Op != "consts" {
				continue
			}
			if skipping {
				switch r.Op {
				case "verify", "create", "delete", "unsupported":
					continue
				}
			}
			switch r.Op {
			case "launch":
				l := launches[r]
				if why := g.plugin.unsupportedConfig(l.config); why != "" {
					g.stop("%s", why)
					break
				}
				method := "Launch"
				switch {
				case failLaunch:
					method, failLaunch = "LaunchFails", false
				case r.MayFail:
					method = "LaunchMayFail"
				}
				g.topos = append(g.topos, l.topology)
				g.step("s.%s(%s%s)", method, &genTopology{topo: l.topology},
					&genConfig{values: g.values(r.Values)})
				g.widths = g.file.widths(t, l.topology, numa)
				// A launch which fails keeps the pods. One which may fail
				// may keep them.
				if method == "Launch" {
					g.pods = map[string]bool{}
				}
				started = true
				// LaunchFails leaves the Script as it was, so a machine
				// change after one is still something the replay follows.
				if method != "LaunchFails" {
					launched = true
				}
			case "expect-launch-failure":
				failLaunch = true
			case "machine":
				if launched {
					g.stop("changing the machine with the plugin running")
					break
				}
				if r.NUMA != nil {
					g.step("s.SetNUMA(%v)", *r.NUMA)
					numa = *r.NUMA
				}
				if r.Cmdline != nil {
					if _, err := isolcpus(*r.Cmdline); err != nil {
						g.stop("%v", err)
						break
					}
					g.step("s.SetCmdline(%s)", strconv.Quote(*r.Cmdline))
				}
			case "sync":
				pods := g.syncPods(r)
				if g.stopped != "" {
					break
				}
				g.syncStep("s.SyncWithRuntimePods", pods)
				g.addPods(r)
			case "terminate":
				if launched {
					g.step("s.Terminate()")
				}
				launched = false
			case "create":
				g.create(r)
			case "delete":
				// Deletes before the first launch clean up after other tests.
				if !started {
					break
				}
				g.delete(r)
			case "consts":
				g.stepKind("consts", "s.Consts(%s)", genString{"PyConsts", r.Code})
			case "verify":
				if stmts, err := translatePy(r.Expr, g.widths); err == nil {
					for _, st := range stmts {
						g.step("%s", st)
					}
					g.goVerifies++
				} else {
					g.stepKind("py", "s.VerifyPy(%s)", goString(r.Expr))
				}
				g.verifies++
			case "skip":
				skipFrom, usesFrom = len(gen.steps), len(g.uses)
				g.step("s.Skip(%s)", strconv.Quote(r.Reason))
				skipping, skipReason = true, r.Reason
				var pods []*syncPod
				require.NoError(t, json.Unmarshal(r.Pods, &pods), "%s: bad pods", r)
				skipIDs = runningIDs(pods)
			case "resume":
				var pods []*syncPod
				require.NoError(t, json.Unmarshal(r.Pods, &pods), "%s: bad pods", r)
				for _, p := range pods {
					for ctr, id := range p.IDs {
						if !skipIDs[id] && g.stopped == "" {
							g.stop("%s/%s/%s, created in a skipped block, still runs",
								p.Namespace, p.Name, ctr)
						}
					}
				}
				if g.stopped != "" {
					break
				}
				synced := g.syncPods(r)
				if g.stopped != "" {
					break
				}
				g.syncStep("s.ResumeWithRuntimePods", synced)
				running := g.pods
				g.pods = map[string]bool{}
				for _, p := range pods {
					if key := g.podKey(p.Namespace, p.Name); running[key] {
						g.pods[key] = true
					}
				}
				skipping = false
			case "unsupported":
				if !strings.HasPrefix(r.Command, "fuser ") {
					g.stop("%s", strconv.Quote(r.Command))
				}
			default:
				g.stop("an unknown %q record", r.Op)
			}
			if g.stopped != "" {
				break
			}
		}
		if skipping && g.stopped == "" {
			// The records ran out inside the skipped block, as a run aborted
			// in one leaves them. The e2e test went no further, so the block
			// ends the test like one the replay cannot resume.
			g.stop("a block skipped for %s which the e2e test never resumed", strconv.Quote(skipReason))
		}
		if g.stopped != "" {
			if skipping {
				// A skipped block the replay cannot resume is no part of the
				// test, which ends at the assertion before it. Its strings go
				// with its steps, or the file would name one for a test which
				// no longer uses it.
				gen.steps, g.uses = gen.steps[:skipFrom], g.uses[:usesFrom]
			}
			break
		}
	}

	if g.stopped != "" && g.verifies == 0 {
		t.Skipf("no assertion before the replay ends at %s", g.stopped)
	}
	if !slices.ContainsFunc(g.segments, func(seg *genSegment) bool { return len(seg.steps) > 0 }) {
		t.Skip("no steps")
	}
	g.file.accept(g)
	if g.stopped != "" {
		t.Logf("%s, then the replay ends at %s", g.counts(), g.stopped)
	} else {
		t.Logf("%s", g.counts())
	}
}

func (g *genTest) counts() string {
	return fmt.Sprintf("%s (%d Go, %d python)", plural(g.verifies, "assertion"),
		g.goVerifies, g.verifies-g.goVerifies)
}

// widths are the id widths of the machine of a topology, as the NUMA mode of
// the launch leaves them: a machine with NUMA disabled has one node whatever
// its topology says, so its node names are narrower, and a name of the e2e
// test which names nothing there has to stay a name literal rather than
// become an id the replay pads to a name it does have.
func (f *genFile) widths(t *testing.T, topo Topology, numa bool) pyWidths {
	t.Helper()
	key := topologyLiteral(topo)
	if !numa {
		key += " no-numa"
	}
	if w, ok := f.idWidths[key]; ok {
		return w
	}
	n := newTopoNames(NewMachine(t, topo, Host{NoNUMA: !numa}))
	w := pyWidths{cpu: n.cpuW, node: n.nodeW, pkg: n.pkgW}
	if f.idWidths == nil {
		f.idWidths = map[string]pyWidths{}
	}
	f.idWidths[key] = w
	return w
}

// prunePyConsts drops the py_consts of the segments with no python left.
func (f *genFile) prunePyConsts() {
	for _, g := range f.tests {
		used := false
		for _, seg := range g.segments {
			if !slices.ContainsFunc(seg.steps, func(st genStep) bool { return st.kind == "py" }) {
				seg.steps = slices.DeleteFunc(seg.steps, func(st genStep) bool { return st.kind == "consts" })
			}
			used = used || slices.ContainsFunc(seg.steps, func(st genStep) bool { return st.kind == "consts" })
		}
		if !used {
			for s, users := range f.consts.uses {
				if s.kind == "PyConsts" {
					delete(users, g)
				}
			}
		}
	}
	f.consts.order = slices.DeleteFunc(f.consts.order, func(s genString) bool { return len(f.consts.uses[s]) == 0 })
}

// usesRequire tells if a test of the file has a require assertion.
func (f *genFile) usesRequire() bool {
	for _, g := range f.tests {
		for _, seg := range g.segments {
			for _, st := range seg.steps {
				if st.format != "%s" || len(st.args) != 1 {
					continue
				}
				if code, ok := st.args[0].(string); ok && strings.HasPrefix(code, "require.") {
					return true
				}
			}
		}
	}
	return false
}

// genTopology is the topology of a launch. It renders as the topology
// variable of the file when it is the one the file declares, and as a literal
// otherwise. Which of the two it is follows from the tests the file accepted,
// so it is resolved only when the file is rendered.
type genTopology struct {
	topo Topology
}

func topologyLiteral(topo Topology) string {
	b := &strings.Builder{}
	b.WriteString("harness.Topology{")
	for i, g := range topo {
		if i > 0 {
			b.WriteString(", ")
		}
		var fields []string
		add := func(name string, v int) {
			if v != 0 {
				fields = append(fields, fmt.Sprintf("%s: %d", name, v))
			}
		}
		if g.Mem != "" {
			fields = append(fields, "Mem: "+strconv.Quote(g.Mem))
		}
		add("Cores", g.Cores)
		add("Threads", g.Threads)
		add("Nodes", g.Nodes)
		add("Dies", g.Dies)
		add("Packages", g.Packages)
		if len(g.NodeDist) > 0 {
			var dist []string
			for _, node := range slices.Sorted(maps.Keys(g.NodeDist)) {
				dist = append(dist, fmt.Sprintf("%d: %d", node, g.NodeDist[node]))
			}
			fields = append(fields, "NodeDist: map[int]int{"+strings.Join(dist, ", ")+"}")
		}
		b.WriteString("{" + strings.Join(fields, ", ") + "}")
	}
	b.WriteString("}")
	return b.String()
}

// values are the helm values of a launch record, decoded.
func (g *genTest) values(data json.RawMessage) any {
	g.t.Helper()
	var v any
	if len(data) > 0 {
		require.NoError(g.t, json.Unmarshal(data, &v), "bad helm values")
	}
	return v
}

// yamlOf is decoded JSON as YAML.
func yamlOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	out, err := yaml.JSONToYAML(data)
	require.NoError(t, err)
	return string(out)
}

var (
	// plainKey is a mapping key which flow YAML needs no quotes for.
	plainKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)
	// typedKey is a plain key which YAML 1.1 reads as a boolean or a null
	// instead of a string, so that an unquoted "on" becomes the key "true".
	// The helm values of a launch go through yaml.YAMLToJSON, which reads
	// YAML 1.1, so such a key has to be quoted or the generated test would
	// launch with a configuration the e2e test never had.
	typedKey = regexp.MustCompile(`^(y|Y|yes|Yes|YES|n|N|no|No|NO|` +
		`true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF|null|Null|NULL)$`)
)

// flowYAML is decoded JSON as one line of YAML, with a top-level map of one
// key without braces.
func flowYAML(t *testing.T, v any, top bool) string {
	t.Helper()
	switch val := v.(type) {
	case map[string]any:
		var entries []string
		for _, k := range slices.Sorted(maps.Keys(val)) {
			key := k
			if !plainKey.MatchString(k) || typedKey.MatchString(k) {
				key = flowYAML(t, k, false)
			}
			entries = append(entries, key+": "+flowYAML(t, val[k], false))
		}
		if top && len(entries) == 1 {
			return entries[0]
		}
		return "{" + strings.Join(entries, ", ") + "}"
	case []any:
		var items []string
		for _, item := range val {
			items = append(items, flowYAML(t, item, false))
		}
		return "[" + strings.Join(items, ", ") + "]"
	default:
		data, err := json.Marshal(val)
		require.NoError(t, err)
		return string(data)
	}
}

// baseValues are the values more than half of n given ones have,
// recursively for maps.
func baseValues(values []any, n int) any {
	maps_ := map[string][]any{}
	counts := map[string]int{}
	byKey := map[string]any{}
	for _, v := range values {
		if m, ok := v.(map[string]any); ok {
			for k, x := range m {
				if _, isMap := x.(map[string]any); isMap {
					maps_[k] = append(maps_[k], x)
					continue
				}
				data, _ := json.Marshal(x)
				key := k + "\x00" + string(data)
				counts[key]++
				byKey[key] = x
			}
		}
	}
	out := map[string]any{}
	for k, list := range maps_ {
		if len(list) > n/2 {
			out[k] = baseValues(list, n)
		}
	}
	for key, count := range counts {
		if count > n/2 {
			k, _, _ := strings.Cut(key, "\x00")
			if _, taken := out[k]; !taken {
				out[k] = byKey[key]
			}
		}
	}
	return out
}

// overlay is what to merge over base values, merged over the chart values,
// for helm to give the values merged over the chart values.
func overlay(values, base, chart any) map[string]any {
	vm, _ := values.(map[string]any)
	bm, _ := base.(map[string]any)
	cm, _ := chart.(map[string]any)
	out := map[string]any{}
	for k, v := range vm {
		b, ok := bm[k]
		if !ok {
			out[k] = v
			continue
		}
		_, vmap := v.(map[string]any)
		_, bmap := b.(map[string]any)
		switch {
		case vmap && bmap:
			if o := overlay(v, b, cm[k]); len(o) > 0 {
				out[k] = o
			}
		case !reflect.DeepEqual(v, b):
			out[k] = v
		}
	}
	// What the base adds to the chart, the values do not have.
	for k, b := range bm {
		if _, ok := vm[k]; ok {
			continue
		}
		c, inChart := cm[k]
		_, bmap := b.(map[string]any)
		_, cmap := c.(map[string]any)
		switch {
		case !inChart:
			out[k] = nil
		case bmap && cmap:
			if o := overlay(c, b, c); len(o) > 0 {
				out[k] = o
			}
		case !reflect.DeepEqual(b, c):
			out[k] = c
		}
	}
	return out
}

// podOf is the pod of a create record, or nil if the replay cannot create it.
func (g *genTest) podOf(r *record) *corev1.Pod {
	g.t.Helper()
	if string(r.Object) == "null" || len(r.Object) == 0 {
		g.stop("an object the e2e test could not log")
		return nil
	}
	pod := &corev1.Pod{}
	require.NoError(g.t, json.Unmarshal(r.Object, pod), "%s: bad object", r)
	if pod.Kind != "Pod" {
		g.stop("creating a %s", pod.Kind)
		return nil
	}
	if len(pod.Spec.ResourceClaims) > 0 {
		g.stop("a pod with resource claims")
		return nil
	}
	if why := g.plugin.unsupportedPod(pod); why != "" {
		g.stop("%s", why)
		return nil
	}
	pod.Namespace = cmp.Or(pod.Namespace, r.Namespace, "default")
	return pod
}

// podSpec is the QoS class and the Go PodSpec of a pod of an e2e test
// template, or "" if the pod is of none.
func (g *genTest) podSpec(pod *corev1.Pod) (string, string) {
	if pod.Labels["app"] != pod.Name || len(pod.Spec.Containers) == 0 {
		return "", ""
	}
	var all []corev1.Container
	for i, c := range pod.Spec.InitContainers {
		if c.Name != fmt.Sprintf("%sc%di", pod.Name, i) {
			return "", ""
		}
		all = append(all, c)
	}
	for i, c := range pod.Spec.Containers {
		if c.Name != fmt.Sprintf("%sc%d", pod.Name, i) {
			return "", ""
		}
		all = append(all, c)
	}
	res := all[0].Resources
	for _, c := range all {
		if !reflect.DeepEqual(c.Resources, res) {
			return "", ""
		}
	}
	for _, list := range []corev1.ResourceList{res.Requests, res.Limits} {
		for name, q := range list {
			if name != corev1.ResourceCPU && name != corev1.ResourceMemory {
				return "", ""
			}
			// A PodSpec gives "0" the meaning of no quantity at all, so an
			// explicit zero needs the options of NewPod.
			if q.IsZero() {
				return "", ""
			}
		}
	}

	var fields []string
	field := func(name, value, def string) {
		if value == "" {
			value = "0"
		}
		if value != def {
			fields = append(fields, name+": "+strconv.Quote(value))
		}
	}
	quantity := func(list corev1.ResourceList, name corev1.ResourceName) string {
		if q, ok := list[name]; ok {
			return q.String()
		}
		return ""
	}
	if want := fmt.Sprintf("pod%d", g.created); pod.Name != want {
		fields = append(fields, "Name: "+strconv.Quote(pod.Name))
	}
	if pod.Namespace != "default" {
		fields = append(fields, "Namespace: "+strconv.Quote(pod.Namespace))
	}
	if n := len(pod.Spec.Containers); n != 1 {
		fields = append(fields, fmt.Sprintf("ContCount: %d", n))
	}
	if n := len(pod.Spec.InitContainers); n > 0 {
		fields = append(fields, fmt.Sprintf("IContCount: %d", n))
	}

	cpuReq, memReq := quantity(res.Requests, corev1.ResourceCPU), quantity(res.Requests, corev1.ResourceMemory)
	cpuLim, memLim := quantity(res.Limits, corev1.ResourceCPU), quantity(res.Limits, corev1.ResourceMemory)
	qos := "Burstable"
	switch {
	case len(res.Requests) == 0 && len(res.Limits) == 0:
		qos = "BestEffort"
	case cpuReq != "" && memReq != "" && cpuReq == cpuLim && memReq == memLim:
		qos = "Guaranteed"
		field("CPU", cpuReq, DefaultCPU)
		field("Mem", memReq, DefaultMem)
	default:
		field("CPUReq", cpuReq, DefaultCPUReq)
		field("CPULim", cpuLim, DefaultCPULim)
		field("MemReq", memReq, DefaultMemReq)
		field("MemLim", memLim, DefaultMemLim)
	}

	labels := maps.Clone(pod.Labels)
	delete(labels, "app")
	multiline := false
	if len(labels) > 0 {
		fields = append(fields, "Labels: "+mapLiteral(labels))
		multiline = true
	}
	if len(pod.Annotations) > 0 {
		fields = append(fields, "Annotations: "+mapLiteral(pod.Annotations))
		multiline = true
	}
	if !multiline {
		return qos, "harness.PodSpec{" + strings.Join(fields, ", ") + "}"
	}
	return qos, "harness.PodSpec{\n" + strings.Join(fields, ",\n") + ",\n}"
}

// syncPods is the Go elements of a list of the Pods of a sync or resume
// record, with the kube-system pods under stable names, or "" if the replay
// cannot sync them.
func (g *genTest) syncPods(r *record) string {
	g.t.Helper()
	var pods []*syncPod
	require.NoError(g.t, json.Unmarshal(r.Pods, &pods), "%s: bad pods", r)
	taken := map[string]bool{} // names of the pods of the record
	for _, p := range pods {
		if n, ok := g.renames[p.Name]; ok && p.Namespace == "kube-system" {
			taken[n] = true
		}
	}
	b := &strings.Builder{}
	for _, p := range pods {
		if len(p.Spec.ResourceClaims) > 0 {
			g.stop("a pod with resource claims")
			return ""
		}
		if why := g.plugin.unsupportedPod(&p.Pod); why != "" {
			g.stop("%s", why)
			return ""
		}
		pod := p.DeepCopy()
		if pod.Namespace == "kube-system" {
			pod.Name = g.rename(p.Name, taken)
			// The annotations of the cluster and the labels of controller
			// revisions, of no use to a policy, change from run to run.
			maps.DeleteFunc(pod.Annotations, func(k, _ string) bool { return strings.Contains(k, "kubernetes.io/") })
			maps.DeleteFunc(pod.Labels, func(k, _ string) bool {
				return strings.HasSuffix(k, "-hash") || k == "pod-template-generation"
			})
		}
		// A runtime pod has the QoS class its containers give it, the init
		// containers included: a kubelet computes the class from those too,
		// so a completed init container can be the only reason for the class
		// of a pod whose application containers are guaranteed. They are kept
		// for PodQOS to give the recorded class, and a sync starts only the
		// application containers, so they never run in the replay.
		qos := map[corev1.PodQOSClass]string{
			corev1.PodQOSGuaranteed: "",
			corev1.PodQOSBurstable:  "burstable",
			corev1.PodQOSBestEffort: "besteffort",
		}
		if want, ok := qos[p.Status.QOSClass]; ok && want != PodQOS(pod) {
			g.stop("%s/%s, of a QoS class its containers do not give", p.Namespace, p.Name)
			return ""
		}
		running := p.Running
		var all []string
		for _, c := range pod.Spec.Containers {
			all = append(all, c.Name)
		}
		if slices.Equal(running, all) {
			running = nil
		} else if running == nil {
			running = []string{}
		}
		args := g.podArgs(pod, pod.Spec.InitContainers, running)
		if args == "" {
			return ""
		}
		fmt.Fprintf(b, "harness.NewPod(%s),\n", args)
	}
	return b.String()
}

// podArgs is the arguments of NewPod for a pod, or "" if there are no
// options for its containers. running is nil for all containers.
func (g *genTest) podArgs(pod *corev1.Pod, init []corev1.Container, running []string) string {
	args := []string{strconv.Quote(pod.Name)}
	if pod.Namespace != "default" && pod.Namespace != "" {
		args = append(args, fmt.Sprintf("harness.InNamespace(%s)", strconv.Quote(pod.Namespace)))
	}
	if len(pod.Labels) > 0 {
		args = append(args, "harness.WithLabels("+mapLiteral(pod.Labels)+")")
	}
	if len(pod.Annotations) > 0 {
		args = append(args, "harness.WithAnnotations("+mapLiteral(pod.Annotations)+")")
	}
	for _, list := range []struct {
		option string
		ctrs   []corev1.Container
	}{
		{"WithInitContainers", init},
		{"WithContainers", pod.Spec.Containers},
	} {
		if len(list.ctrs) == 0 {
			continue
		}
		ctrs := ""
		for _, c := range list.ctrs {
			ctr := g.container(c)
			if ctr == "" {
				return ""
			}
			ctrs += ctr + ",\n"
		}
		args = append(args, "harness."+list.option+"(\n"+ctrs+")")
	}
	if running != nil {
		names := make([]string, 0, len(running))
		for _, n := range running {
			names = append(names, strconv.Quote(n))
		}
		args = append(args, "harness.WithRunning("+strings.Join(names, ", ")+")")
	}
	if len(args) == 1 {
		return args[0]
	}
	return strings.Join(args, ",\n") + ",\n"
}

// container is the NewContainer call for a container, or "" if there are no
// options for its resources.
func (g *genTest) container(c corev1.Container) string {
	var (
		req, lim = c.Resources.Requests, c.Resources.Limits
		opts     []string
	)
	quantity := func(list corev1.ResourceList, name corev1.ResourceName) string {
		if q, ok := list[name]; ok {
			return strconv.Quote(q.String())
		}
		return `""`
	}
	for _, r := range []struct {
		option string
		list   corev1.ResourceList
	}{
		{"WithRequests", req},
		{"WithLimits", lim},
	} {
		_, cpu := r.list[corev1.ResourceCPU]
		_, mem := r.list[corev1.ResourceMemory]
		if cpu || mem {
			opts = append(opts, fmt.Sprintf("harness.%s(%s, %s)", r.option,
				quantity(r.list, corev1.ResourceCPU), quantity(r.list, corev1.ResourceMemory)))
		}
	}
	extended := map[corev1.ResourceName]bool{}
	for name := range maps.Keys(req) {
		extended[name] = true
	}
	for name := range maps.Keys(lim) {
		extended[name] = true
	}
	delete(extended, corev1.ResourceCPU)
	delete(extended, corev1.ResourceMemory)
	for _, name := range slices.Sorted(maps.Keys(extended)) {
		q, ok := lim[name]
		if r, hasReq := req[name]; !ok {
			q = r
		} else if hasReq && r.Cmp(q) != 0 {
			g.stop("container %s with a request of %s other than its limit", c.Name, name)
			return ""
		}
		opts = append(opts, fmt.Sprintf("harness.WithExtendedResource(%s, %s)",
			strconv.Quote(string(name)), strconv.Quote(q.String())))
	}
	return "harness.NewContainer(" + strings.Join(append([]string{strconv.Quote(c.Name)}, opts...), ", ") + ")"
}

// mapLiteral is the Go literal of a map of strings, sorted.
func mapLiteral(m map[string]string) string {
	var entries []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v := strconv.Quote(m[k])
		if strings.Contains(m[k], "\n") {
			v = goString(m[k])
		}
		entries = append(entries, strconv.Quote(k)+": "+v)
	}
	if len(entries) == 1 {
		return "map[string]string{" + entries[0] + "}"
	}
	return "map[string]string{\n" + strings.Join(entries, ",\n") + ",\n}"
}

var (
	deploymentPod = regexp.MustCompile(`^(.+)-[0-9a-f]{6,10}-[0-9a-z]{5}$`)
	daemonSetPod  = regexp.MustCompile(`^(.+)-[0-9a-z]{5}$`)
)

// rename gives a kube-system pod a name which does not change from run to
// run: the VM in the name of a static pod becomes vm, and the random suffix
// of the pod of a deployment or a daemonset the first number not taken by
// another pod of the record.
func (g *genTest) rename(name string, taken map[string]bool) string {
	if n, ok := g.renames[name]; ok {
		return n
	}
	n := name
	if base, ok := strings.CutSuffix(name, "-"+g.host); ok && g.host != "" {
		n = base + "-vm"
	} else if m := deploymentPod.FindStringSubmatch(name); m != nil {
		n = numbered(m[1], taken)
	} else if m := daemonSetPod.FindStringSubmatch(name); m != nil {
		n = numbered(m[1], taken)
	}
	if taken[n] {
		// A name of its own, or of a static pod, which another pod of the
		// record has already been renamed to. numbered() never collides. The
		// harness keys pods by name and makes the sandbox IDs of the
		// namespace and the name, so going on would merge the two pods, and a
		// delete could take the wrong one.
		g.stop("two pods of the snapshot which both become %s", strconv.Quote(n))
	}
	g.renames[name] = n
	taken[n] = true
	return n
}

func numbered(base string, taken map[string]bool) string {
	for i := 1; ; i++ {
		if n := fmt.Sprintf("%s-%d", base, i); !taken[n] {
			return n
		}
	}
}

// goString is the Go literal of a string, a raw one if it can be.
func goString(s string) string {
	if utf8.ValidString(s) && !strings.ContainsAny(s, "`\r") {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// camel is a name like test01-basic-placement as Test01BasicPlacement.
func camel(name string) string {
	var b strings.Builder
	for _, w := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		b.WriteString(strings.ToUpper(w[:1]) + w[1:])
	}
	return b.String()
}

// accept adds a test to its file, with the strings it may name and the
// topology of its first launch.
func (f *genFile) accept(g *genTest) {
	f.tests = append(f.tests, g)
	for _, s := range g.uses {
		f.consts.use(s, g)
	}
	if f.topology == nil && len(g.topos) > 0 {
		f.topology = g.topos[0]
	}
}

// configs splits the helm values of the launches of the file into the
// values they all share, which it returns as YAML, and an overlay for each.
// A one-line overlay is a literal. Others may become constants.
func (f *genFile) configs(t *testing.T) string {
	var (
		all     []any
		configs []*genConfig
		users   []*genTest
	)
	for _, g := range f.tests {
		for _, seg := range g.segments {
			for _, st := range seg.steps {
				for _, a := range st.args {
					if c, ok := a.(*genConfig); ok {
						all = append(all, c.values)
						configs = append(configs, c)
						users = append(users, g)
					}
				}
			}
		}
	}
	if len(all) == 0 {
		return ""
	}
	chart, err := readYAML(filepath.Join(srcDir(), "deployment", "helm", f.plugin, "values.yaml"))
	require.NoError(t, err)
	base := baseValues(all, len(all))
	for i, c := range configs {
		o := overlay(c.values, base, chart)
		// Helm must merge the base and the overlay into the same values.
		if want, got := mergeValues(chart, c.values), mergeValues(mergeValues(chart, base), o); !reflect.DeepEqual(want, got) {
			c.noBase = true
			o, _ = c.values.(map[string]any)
		}
		if len(o) == 0 {
			continue
		}
		if flow := flowYAML(t, o, true); len(flow) <= 72 {
			c.overlay = &genString{"Inline", flow}
			continue
		}
		c.overlay = &genString{"Config", "\n" + yamlOf(t, o)}
		f.consts.use(*c.overlay, users[i])
	}
	return "\n" + yamlOf(t, base)
}

// funcName is the name of the generated test function of a test.
func (f *genFile) funcName(g *genTest) string {
	return "TestE2eishGen" + camel(f.topo) + camel(g.name)
}

// duplicates are the VMs of the tests of the file which generate a function
// of the same name, by that name.
func (f *genFile) duplicates() map[string][]string {
	hosts := map[string][]string{}
	for _, g := range f.tests {
		name := f.funcName(g)
		hosts[name] = append(hosts[name], g.host)
	}
	maps.DeleteFunc(hosts, func(_ string, vms []string) bool { return len(vms) < 2 })
	return hosts
}

// source is the formatted Go source of the file.
func (f *genFile) source(t *testing.T, pkg string) []byte {
	t.Helper()

	topoVar := "gen" + camel(f.topo)
	f.prunePyConsts()
	base := f.configs(t)
	names := f.consts.names(topoVar)

	b := &bytes.Buffer{}
	// The tag keeps generated files out of the build which generates them,
	// so they need not build with an API they were not generated for.
	fmt.Fprintf(b, "%s\n// Code generated by TestE2eishGenerate. DO NOT EDIT.\n\n", licenseHeader)
	fmt.Fprintf(b, "//go:build !e2eish_generate\n\n")
	fmt.Fprintf(b, "package %s\n\n", pkg)
	fmt.Fprintf(b, "// The e2e tests of test/e2e/policies.test-suite/%s/%s, from the logs of\n", f.plugin, f.topo)
	fmt.Fprintf(b, "// a test run. See \"Generating unit tests from test logs\" in\n// test/e2e/README.md.\n\n")
	fmt.Fprintf(b, "import (\n\t\"testing\"\n\n")
	if f.usesRequire() {
		fmt.Fprintf(b, "\t\"github.com/stretchr/testify/require\"\n\n")
	}
	fmt.Fprintf(b, "\tharness %q\n)\n\n", "github.com/containers/nri-plugins/pkg/testutils/e2eish")
	fmt.Fprintf(b, "// %s is test/e2e/policies.test-suite/%s/%s/topology.var.json.\n", topoVar, f.plugin, f.topo)
	fmt.Fprintf(b, "var %s = %s\n\n", topoVar, topologyLiteral(f.topology))
	if base != "" {
		fmt.Fprintf(b, "// %sBaseConfig is the helm values most launches share.\n", topoVar)
		fmt.Fprintf(b, "const %sBaseConfig = %s\n\n", topoVar, goString(base))
	}
	for _, s := range f.consts.order {
		if name, ok := names[s]; ok {
			fmt.Fprintf(b, "%s%s\n", s.doc(name), s.decl(name))
		}
	}

	for _, g := range f.tests {
		fmt.Fprintf(b, "// %s is %s/%s.\n", f.funcName(g), f.topo, g.name)
		fmt.Fprintf(b, "func %s(t *testing.T) {\n", f.funcName(g))
		// The configs of only this test are its constants.
		var configs []genString
		for _, seg := range g.segments {
			for _, st := range seg.steps {
				for _, a := range st.args {
					c, ok := a.(*genConfig)
					if ok && c.overlay != nil && c.overlay.kind == "Config" && !f.consts.shared(*c.overlay) &&
						!slices.Contains(configs, *c.overlay) {
						configs = append(configs, *c.overlay)
					}
				}
			}
		}
		locals := numberedNames("", configs)
		for _, s := range configs {
			name := locals[s]
			locals[s] = strings.ToLower(name[:1]) + name[1:]
			b.WriteString(s.decl(locals[s]))
		}
		if len(configs) > 0 {
			b.WriteString("\n")
		}
		for i, seg := range g.segments {
			// A segment with no steps would declare s and never use it.
			if len(seg.steps) == 0 {
				continue
			}
			if len(g.segments) > 1 {
				fmt.Fprintf(b, "t.Run(\"segment%d\", func(t *testing.T) {\n", i)
			}
			if len(seg.env) > 0 {
				var env []string
				for _, kv := range seg.env {
					env = append(env, goString(kv))
				}
				fmt.Fprintf(b, "if harness.Reexec(t, %s) {\nreturn\n}\n", strings.Join(env, ", "))
			}
			b.WriteString("s := harness.NewScript(t, e2eishPlugin)\n")
			for _, st := range seg.steps {
				args := make([]any, len(st.args))
				for j, a := range st.args {
					if tp, ok := a.(*genTopology); ok {
						a = topologyLiteral(tp.topo)
						if reflect.DeepEqual(f.topology, tp.topo) {
							a = topoVar
						}
					}
					if c, ok := a.(*genConfig); ok {
						var files []string
						if !c.noBase {
							files = append(files, topoVar+"BaseConfig")
						}
						if c.overlay != nil {
							files = append(files, c.overlay.ref(cmp.Or(names[*c.overlay], locals[*c.overlay])))
						}
						a = ""
						if len(files) > 0 {
							a = ", " + strings.Join(files, ", ")
						}
					}
					if s, ok := a.(genString); ok {
						if name, ok := names[s]; ok {
							a = s.ref(name)
						} else if name, ok := locals[s]; ok {
							a = s.ref(name)
						} else {
							a = s.literal()
						}
					}
					args[j] = a
				}
				fmt.Fprintf(b, st.format+"\n", args...)
			}
			if i == len(g.segments)-1 && g.stopped != "" {
				fmt.Fprintf(b, "\n// The replay cannot follow the rest of the e2e test: %s.\n", g.stopped)
			}
			if len(g.segments) > 1 {
				b.WriteString("})\n")
			}
		}
		b.WriteString("}\n\n")
	}

	src, err := format.Source(b.Bytes())
	require.NoError(t, err, "bad generated source:\n%s", b.String())
	return src
}

const licenseHeader = `// Copyright The NRI Plugins Authors. All Rights Reserved.
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
`
