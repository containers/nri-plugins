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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// e2eTarget is the e2e test a Script generates, as its steps run. Its
// steps do nothing on a nil target, the target of other Scripts.
type e2eTarget struct {
	target  string            // TOPOLOGY/testNN-name
	file    string            // the source of the test
	policy  string            // the plugin, as helm-launch takes it
	topo    Topology          // of the launches
	recs    []e2eRec          // the steps, in order
	files   map[string]string // the files besides code.var.sh
	helm    map[string]string // helm values to their file
	ns      []string          // namespaces created
	sysPods []string          // pods created in kube-system
	release bool              // a launch may have left its release installed
	noNUMA  bool              // NUMA was disabled
	cmdline bool              // the kernel command line was set
	errs    []string          // refusals, with file:line
}

// e2eRec is a step: the line of the test calling it, and its commands or
// its verify expression.
type e2eRec struct {
	line   int
	cmds   []string
	verify string
	consts bool // the step is Consts
}

func newE2ETarget(target, file, policy string) *e2eTarget {
	return &e2eTarget{
		target: target, file: file, policy: policy,
		files: map[string]string{}, helm: map[string]string{},
	}
}

// caller is the line of the test calling the step, 0 if none.
func (e *e2eTarget) caller() int {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if f.File == e.file {
			return f.Line
		}
		if !more {
			return 0
		}
	}
}

func (e *e2eTarget) record(cmds ...string) {
	e.recs = append(e.recs, e2eRec{line: e.caller(), cmds: cmds})
}

func (e *e2eTarget) refuse(format string, args ...any) {
	if e == nil {
		return
	}
	e.errs = append(e.errs, fmt.Sprintf("%s:%d: %s", e.file, e.caller(), fmt.Sprintf(format, args...)))
}

// none records a step with no e2e command.
func (e *e2eTarget) none() {
	if e == nil {
		return
	}
	e.record()
}

func (e *e2eTarget) launch(topo Topology, values []string, how int) {
	if e == nil {
		return
	}
	switch {
	case e.topo == nil:
		e.topo = topo
	case !reflect.DeepEqual(e.topo, topo):
		e.refuse("a launch on a second topology")
		return
	}
	var merged any
	for _, v := range values {
		data, err := yaml.YAMLToJSON([]byte(v))
		var given any
		if err == nil {
			err = json.Unmarshal(data, &given)
		}
		if err != nil {
			e.refuse("bad helm values: %v", err)
			return
		}
		if given == nil {
			// A blank or null document is no override, as HelmConfig takes
			// it, and must not discard the overrides before it. A null inside
			// a map stays, helm deleting the key it is given for.
			continue
		}
		merged = overlayValues(merged, given)
	}
	data, err := yaml.Marshal(merged)
	if err != nil {
		e.refuse("bad helm values: %v", err)
		return
	}
	file, ok := e.helm[string(data)]
	if !ok {
		file = fmt.Sprintf("helm-config-%d.yaml", len(e.helm)+1)
		e.helm[string(data)] = file
		e.files[file] = string(data)
	}
	// A failed launch leaves its release installed too, and the release
	// of the next one needs the name.
	var cmds []string
	if e.release {
		cmds = append(cmds, "helm-terminate")
	}
	cfg := `helm_config="$TEST_DIR/` + file + `"`
	switch how {
	case launchFails:
		cmds = append(cmds, cfg+" expect-launch-failure "+e.policy)
	case launchMayFail:
		cmds = append(cmds, "expect_error=1 "+cfg+" helm-launch "+e.policy)
	default:
		cmds = append(cmds, cfg+" helm-launch "+e.policy)
	}
	e.record(cmds...)
	e.release = true
}

// overlayValues merges helm values over others, as mergeValues does, but
// keeps nulls: helm drops the chart values they null.
func overlayValues(base, over any) any {
	b, bok := base.(map[string]any)
	o, ook := over.(map[string]any)
	if !bok || !ook {
		return over
	}
	out := maps.Clone(b)
	for k, v := range o {
		out[k] = overlayValues(out[k], v)
	}
	return out
}

func (e *e2eTarget) terminate() {
	if e == nil {
		return
	}
	e.record("helm-terminate")
	e.release = false
}

// create is create of a pod of a template, named name.
func (e *e2eTarget) create(spec PodSpec, qos, name string) {
	if e == nil {
		return
	}
	var vars []string
	ok := true
	add := func(name, v string) {
		if v == "" {
			return
		}
		if strings.Contains(v, `\`) {
			e.refuse("a backslash in %s, which the template would unescape", name)
			ok = false
		}
		vars = append(vars, name+"="+shellQuote(v))
	}
	// The templates know no "0" but for limits, init containers but for
	// guaranteed pods, nor other app labels.
	for v, val := range map[string]string{"CPU": spec.CPU, "MEM": spec.Mem, "CPUREQ": spec.CPUReq, "MEMREQ": spec.MemReq} {
		if val == "0" {
			e.refuse("%s=0, which the templates take as a quantity", v)
			return
		}
	}
	if spec.IContCount > 0 && qos != Guaranteed {
		e.refuse("init containers in a %s pod, which its template has none of", qos)
		return
	}
	if _, ok := spec.Labels["app"]; ok {
		e.refuse("an app label, which the templates set to the name")
		return
	}
	add("NAME", spec.Name)
	cmds, nsVars := e.namespace(spec.Namespace, name)
	vars = append(vars, nsVars...)
	if spec.ContCount > 0 {
		add("CONTCOUNT", strconv.Itoa(spec.ContCount))
	}
	if spec.IContCount > 0 {
		add("ICONTCOUNT", strconv.Itoa(spec.IContCount))
	}
	switch qos {
	case Guaranteed:
		add("CPU", spec.CPU)
		add("MEM", spec.Mem)
	case Burstable:
		add("CPUREQ", spec.CPUReq)
		add("CPULIM", spec.CPULim)
		add("MEMREQ", spec.MemReq)
		add("MEMLIM", spec.MemLim)
	}
	for i, k := range slices.Sorted(maps.Keys(spec.Labels)) {
		add(fmt.Sprintf("LABEL%d", i), k+": "+strconv.Quote(spec.Labels[k]))
	}
	for i, k := range slices.Sorted(maps.Keys(spec.Annotations)) {
		add(fmt.Sprintf("ANN%d", i), k+": "+strconv.Quote(spec.Annotations[k]))
	}
	if !ok {
		return
	}
	e.record(append(cmds, strings.Join(append(vars, "create", qos), " "))...)
}

// namespace gives the commands to create the namespace of a pod, and the
// template vars of it.
func (e *e2eTarget) namespace(ns, pod string) ([]string, []string) {
	var cmds []string
	switch ns {
	case "", "default":
		return nil, nil
	case "kube-system":
		e.sysPods = append(e.sysPods, pod)
	default:
		if !slices.Contains(e.ns, ns) {
			e.ns = append(e.ns, ns)
			cmds = append(cmds, "create-namespaces "+ns)
		}
	}
	return cmds, []string{"namespace=" + shellQuote(ns)}
}

// e2eImage is the image of the containers of the e2e templates.
const e2eImage = "quay.io/prometheus/busybox"

// createPod is create of a pod from a template of its own, with the
// containers sleeping in the image of the templates, as theirs do.
func (e *e2eTarget) createPod(pod *corev1.Pod) {
	if e == nil {
		return
	}
	name := pod.Name
	if slices.Contains([]string{Guaranteed, Burstable, BestEffort}, name) {
		e.refuse("a pod named like the %s template", name)
		return
	}
	p := pod.DeepCopy()
	p.APIVersion, p.Kind = "v1", "Pod"
	ns := p.Namespace
	p.Namespace = ""
	// The e2e tests find the containers by their names on the command line.
	sleep := func(ctrs []corev1.Container, time string) {
		for i := range ctrs {
			ctrs[i].Image = e2eImage
			ctrs[i].ImagePullPolicy = corev1.PullIfNotPresent
			ctrs[i].Command = []string{"sh", "-c", "echo " + ctrs[i].Name + " $(sleep " + time + ")"}
		}
	}
	sleep(p.Spec.InitContainers, "5")
	sleep(p.Spec.Containers, "inf")
	grace := int64(1)
	p.Spec.TerminationGracePeriodSeconds = &grace

	var obj map[string]any
	data, err := json.Marshal(p)
	if err == nil {
		err = json.Unmarshal(data, &obj)
	}
	if err != nil {
		e.refuse("pod %s: %v", name, err)
		return
	}
	delete(obj, "status")
	if meta, ok := obj["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
	}
	data, err = yaml.Marshal(obj)
	if err != nil {
		e.refuse("pod %s: %v", name, err)
		return
	}
	// instantiate evaluates the template in double quotes, with echo -e.
	tmpl := string(data)
	if strings.Contains(tmpl, `\`) {
		e.refuse("a backslash in pod %s, which the template would unescape", name)
		return
	}
	// create takes the kind from the first line with kind in it, and
	// instantiate drops empty lines, those of block scalars too.
	for _, l := range strings.Split(strings.TrimSuffix(tmpl, "\n"), "\n") {
		switch {
		case strings.Contains(l, "kind") && l != "kind: Pod":
			e.refuse("kind in pod %s other than as its kind, which create would take for it", name)
			return
		case blockScalar.MatchString(l):
			e.refuse("a multi-line string in pod %s, which instantiate would change", name)
			return
		}
	}
	tmpl = strings.NewReplacer(`"`, `\"`, "$", `\$`, "`", "\\`").Replace(tmpl)
	file := name + ".yaml.in"
	if old, ok := e.files[file]; ok && old != tmpl {
		e.refuse("a second pod %s, unlike the first", name)
		return
	}
	e.files[file] = tmpl

	cmds, vars := e.namespace(ns, name)
	vars = append([]string{"NAME=" + shellQuote(name)}, vars...)
	e.record(append(cmds, strings.Join(append(vars, "create", shellQuote(name)), " "))...)
}

var blockScalar = regexp.MustCompile(`(^|[:-] )[|>][-+0-9]*$`)

func canonicalNamespace(ns string) string {
	if ns != "" {
		return ns
	}
	return "default"
}

func nsArg(ns string) string {
	if canonicalNamespace(ns) == "default" {
		return ""
	}
	return " -n " + ns
}

func (e *e2eTarget) deletePods(ns string, names []string) {
	if e == nil {
		return
	}
	if len(names) == 0 {
		e.none()
		return
	}
	e.record("delete-pods" + nsArg(ns) + " " + strings.Join(names, " "))
}

func (e *e2eTarget) deleteAll(ns string) {
	if e == nil {
		return
	}
	if ns == "kube-system" {
		e.refuse("deleting all the pods in kube-system")
		return
	}
	e.record("delete-pods" + nsArg(ns) + " --all")
}

func (e *e2eTarget) setNUMA(enabled bool) {
	if e == nil {
		return
	}
	if enabled {
		e.record("enable-numa")
	} else {
		e.record("disable-numa")
		e.noNUMA = true
	}
}

func (e *e2eTarget) setCmdline(cmdline string) {
	if e == nil {
		return
	}
	e.record("vm-set-kernel-cmdline-reboot " + shellQuote(cmdline))
	e.cmdline = true
}

// restoreTrap restores the machine the test changed when it exits, as
// later tests expect it.
func (e *e2eTarget) restoreTrap() string {
	var cmds []string
	if e.noNUMA {
		cmds = append(cmds, "enable-numa")
	}
	if e.cmdline {
		cmds = append(cmds, "vm-set-kernel-cmdline-reboot")
	}
	if len(cmds) == 0 {
		return ""
	}
	return "trap '" + strings.Join(cmds, "; ") + "' EXIT"
}

func (e *e2eTarget) consts(code string) {
	if e == nil {
		return
	}
	if strings.Contains(code, `\`) {
		e.refuse("a backslash in py_consts, which pyexec would unescape")
		return
	}
	if old, ok := e.files["py_consts.var.py"]; ok && old != code {
		e.refuse("a second py_consts")
		return
	}
	e.files["py_consts.var.py"] = code
	e.recs = append(e.recs, e2eRec{line: e.caller(), consts: true})
}

func (e *e2eTarget) verifyPy(expr string) {
	if e == nil {
		return
	}
	e.recs = append(e.recs, e2eRec{line: e.caller(), verify: expr})
}

var shellPlain = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// shellQuote quotes a word for bash, if it needs quoting.
func shellQuote(s string) string {
	if shellPlain.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// NewE2EScript returns a script for the plugin which is also the source of
// an e2e test, the target, like n4c16/test50-name, of the test suite of the
// plugin. A passing test writes it, with E2EISH_EMIT set. See "Generating
// e2e tests from unit tests" in test/e2e/README.md.
func NewE2EScript(t *testing.T, plugin *Plugin, target string) *Script {
	t.Helper()
	_, file, _, ok := runtime.Caller(1)
	require.True(t, ok, "no caller of NewE2EScript")
	s := NewScript(t, plugin)
	s.e2e = newE2ETarget(target, file, plugin.Name)
	t.Cleanup(s.emitE2E)
	return s
}

var e2eTargetName = regexp.MustCompile(`^[a-z0-9-]+/test[0-9]+-[a-z0-9-]+$`)

// emitE2E writes the e2e test of a passing test, with E2EISH_EMIT set.
func (s *Script) emitE2E() {
	t, e := s.t, s.e2e
	suite := os.Getenv("E2EISH_EMIT")
	if suite == "" || t.Failed() || t.Skipped() {
		return
	}
	var w pyWidths
	if s.h != nil {
		n := newTopoNames(s.h.Machine)
		w = pyWidths{cpu: n.cpuW, node: n.nodeW, pkg: n.pkgW}
	}
	files, summary, err := e.render(t.Name(), w, suite)
	if err != nil {
		t.Errorf("e2e test %s not generated:\n%v", e.target, err)
		return
	}
	writeE2E(t, filepath.Join(suite, e.target), files, summary)
}

// render gives the files of the e2e test and what its script holds, or why
// there are none.
func (e *e2eTarget) render(test string, w pyWidths, suite string) (map[string]string, string, error) {
	errs := slices.Clone(e.errs)
	if strings.Contains(test, "/") {
		errs = append(errs, "a subtest")
	}
	if !e2eTargetName.MatchString(e.target) {
		errs = append(errs, fmt.Sprintf("target %q is not like TOPOLOGY/testNN-name", e.target))
	}
	if e.topo == nil {
		errs = append(errs, "no launch")
	}
	if len(errs) == 0 {
		topoFile := filepath.Join(suite, path.Dir(e.target), "topology.var.json")
		topo, err := readTopology(topoFile)
		switch {
		case err != nil:
			errs = append(errs, err.Error())
		case !reflect.DeepEqual(topo, e.topo):
			errs = append(errs, fmt.Sprintf("the topology is not that of %s", topoFile))
		}
	}
	if len(errs) > 0 {
		return nil, "", errors.New(strings.Join(errs, "\n"))
	}
	doc, stmts, err := e2eSource(e.file, test, w)
	if err != nil {
		return nil, "", err
	}
	lines, err := mergeE2E(stmts, e.recs)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", e.file, err)
	}

	b := &strings.Builder{}
	for _, l := range strings.Split(strings.TrimSuffix(doc, "\n"), "\n") {
		fmt.Fprintln(b, strings.TrimRight("# "+l, " "))
	}
	src, _ := filepath.Rel(srcDir(), e.file)
	fmt.Fprintf(b, "#\n# Generated by test/e2e/emit_tests.sh from %s in\n# %s.\n\n", test, src)
	fmt.Fprint(b, e.body(lines))

	files := maps.Clone(e.files)
	files["code.var.sh"] = b.String()
	return files, e2eSummary(lines), nil
}

// body is the script of the test: what a run before it may have left, the
// steps themselves, and the same cleanup at the end.
//
// The cleanup runs first as well as last because a failed verification ends
// the test before the end of the script, and the e2e framework cleans up no
// custom namespace of its own: create-namespaces keeps a namespace which
// exists, with its pods, and test-user-code deletes the pods of the default
// namespace only. A rerun would then fail to create a pod of the run before
// it, with AlreadyExists.
func (e *e2eTarget) body(lines []string) string {
	var cleanup []string
	if len(e.sysPods) > 0 {
		cleanup = append(cleanup, "delete-pods -n kube-system "+strings.Join(e.sysPods, " "))
	}
	if len(e.ns) > 0 {
		cleanup = append(cleanup, "delete-namespaces "+strings.Join(e.ns, " "))
	}

	b := &strings.Builder{}
	for _, l := range cleanup {
		fmt.Fprintln(b, l)
	}
	fmt.Fprintln(b, "helm-terminate")
	if trap := e.restoreTrap(); trap != "" {
		fmt.Fprintln(b, trap)
	}
	for _, l := range lines {
		fmt.Fprintln(b, l)
	}
	for _, l := range cleanup {
		fmt.Fprintln(b, l)
	}
	if e.release {
		fmt.Fprintln(b, "helm-terminate")
	}
	return b.String()
}

// e2eT is what writeE2E needs of a test.
type e2eT interface {
	Helper()
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
}

// writeE2E writes the files of an e2e test to its dir, and logs the summary
// of its script. A dir with the same files is left alone. One which differs
// is an error, and is replaced only with E2EISH_EMIT_FORCE=1.
func writeE2E(t e2eT, dir string, files map[string]string, summary string) {
	t.Helper()
	old, err := readFiles(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Errorf("failed to read %s: %v", dir, err)
		return
	}
	if err == nil {
		if maps.Equal(old, files) {
			t.Logf("e2e test %s is up to date: %s", dir, summary)
			return
		}
		if os.Getenv("E2EISH_EMIT_FORCE") != "1" {
			t.Errorf("e2e test %s differs from the generated one in %s; E2EISH_EMIT_FORCE=1 replaces it",
				dir, strings.Join(differing(old, files), ", "))
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("failed to remove %s: %v", dir, err)
			return
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("failed to create %s: %v", dir, err)
		return
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(files[name]), 0o644); err != nil {
			t.Errorf("failed to write %s: %v", name, err)
			return
		}
	}
	t.Logf("wrote e2e test %s: %s", dir, summary)
}

// readFiles reads the files of a dir; a subdir reads as name/.
func readFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, ent := range entries {
		if !ent.Type().IsRegular() {
			files[ent.Name()+"/"] = ""
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			return nil, err
		}
		files[ent.Name()] = string(data)
	}
	return files, nil
}

func differing(a, b map[string]string) []string {
	names := map[string]bool{}
	for n := range a {
		names[n] = true
	}
	for n := range b {
		names[n] = true
	}
	var out []string
	for _, n := range slices.Sorted(maps.Keys(names)) {
		va, oka := a[n]
		vb, okb := b[n]
		if oka != okb || va != vb {
			out = append(out, n)
		}
	}
	return out
}
