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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resapi "k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// genPlugin is a plugin which takes any helm values as its configuration.
func genPlugin() *Plugin {
	return &Plugin{
		Name:   "topology-aware",
		CRD:    "config.nri_topologyawarepolicies.yaml",
		Config: func() any { return &map[string]any{} },
	}
}

// newGenTest is a generated test of one segment, for the steps of a record.
func newGenTest(t *testing.T) *genTest {
	plugin := genPlugin()
	g := &genTest{
		t:       t,
		plugin:  plugin,
		file:    &genFile{topo: "n4c16", plugin: plugin.Name, consts: &genConsts{}},
		name:    "test01-generated",
		host:    "n4c16-fedora-43-containerd",
		renames: map[string]string{},
		pods:    map[string]bool{},
	}
	g.segments = append(g.segments, &genSegment{})
	return g
}

// genOf is the test generated from the records of a log.
func genOf(t *testing.T, records []*record) *genTest {
	g := newGenTest(t)
	g.segments = nil
	g.generate(records)
	return g
}

// jsonOf is the JSON of a value.
func jsonOf(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

// podsOf is the pods of a sync, skip or resume record.
func podsOf(t *testing.T, pods ...*syncPod) json.RawMessage {
	t.Helper()
	if pods == nil {
		pods = []*syncPod{}
	}
	return jsonOf(t, pods)
}

// syncPodOf is a running pod of a sync, skip or resume record, with one
// container of the given name and ID.
func syncPodOf(namespace, name, ctr, id string) *syncPod {
	p := &syncPod{Running: []string{ctr}, IDs: map[string]string{ctr: id}}
	p.Namespace, p.Name = namespace, name
	p.Spec.Containers = []corev1.Container{{Name: ctr}}
	return p
}

// launchOf is a launch record of a topology of two nodes.
func launchOf() *record {
	return &record{
		Op:       "launch",
		Values:   json.RawMessage(`{}`),
		Topology: []map[string]any{{"mem": "2G", "cores": float64(2), "nodes": float64(2)}},
	}
}

// verifyOf is an assertion record which passed.
func verifyOf(expr string) *record {
	return &record{Op: "verify", Result: "ok", Expr: expr}
}

// hasStep tells whether a step of the test calls the given function.
func hasStep(g *genTest, call string) bool {
	for _, seg := range g.segments {
		for _, st := range seg.steps {
			if strings.HasPrefix(st.format, call) {
				return true
			}
		}
	}
	return false
}

// stepCalls are the steps of the test, with the strings of their arguments.
func stepCalls(g *genTest) []string {
	var calls []string
	for _, seg := range g.segments {
		for _, st := range seg.steps {
			args := make([]any, len(st.args))
			for i, a := range st.args {
				if s, ok := a.(genString); ok {
					args[i] = s.s
					continue
				}
				args[i] = a
			}
			calls = append(calls, fmt.Sprintf(st.format, args...))
		}
	}
	return calls
}

// A skipped block the replay cannot resume leaves no skip of its own behind.
func TestGenerateDropsUnresumedSkip(t *testing.T) {
	g := genOf(t, []*record{
		launchOf(),
		verifyOf("len(cpus) == 2"),
		{Op: "skip", Reason: "the e2e test writes a file", Pods: podsOf(t)},
		{Op: "resume", Pods: podsOf(t, syncPodOf("default", "pod0", "pod0c0", "id-of-a-new-container"))},
	})

	require.NotEmpty(t, g.stopped, "the replay ends at the resume")
	require.True(t, hasStep(g, "s.VerifyPy"), "the assertion before the skip stays")
	require.False(t, hasStep(g, "s.Skip"), "the skip without a resume goes")
}

// An empty snapshot of the runtime resumes the replay.
func TestGenerateResumesOnEmptySnapshot(t *testing.T) {
	g := genOf(t, []*record{
		launchOf(),
		verifyOf("len(cpus) == 2"),
		{Op: "skip", Reason: "the e2e test writes a file", Pods: podsOf(t)},
		{Op: "resume", Pods: podsOf(t)},
		verifyOf("len(cpus) == 2"),
	})

	require.Empty(t, g.stopped, "the replay follows the log")
	require.True(t, hasStep(g, "s.ResumeWithRuntimePods"), "the empty snapshot is a resume")
}

// Tests of the same topology and name generate one function name.
func TestGenFileDuplicates(t *testing.T) {
	f := &genFile{topo: "n4c16", plugin: "topology-aware", consts: &genConsts{}}
	f.tests = []*genTest{
		{name: "test01-basic-placement", host: "n4c16-fedora-43-containerd"},
		{name: "test01-basic-placement", host: "n4c16-fedora-43-crio"},
		{name: "test02-prefer-isolated", host: "n4c16-fedora-43-containerd"},
	}

	dups := f.duplicates()
	require.Len(t, dups, 1, "only the repeated test is a duplicate")
	name := f.funcName(f.tests[0])
	require.Contains(t, dups, name)
	require.Equal(t, []string{"n4c16-fedora-43-containerd", "n4c16-fedora-43-crio"}, dups[name])
}

// A delete of a kube-system pod names the pod the replay synced.
func TestDeleteRenamedKubeSystemPod(t *testing.T) {
	g := newGenTest(t)
	logged := "coredns-7c65d6cfc9-abcde"
	sync := &record{Op: "sync", Pods: podsOf(t, syncPodOf("kube-system", logged, "coredns", "id-of-coredns"))}

	require.NotEmpty(t, g.syncPods(sync), "the pod syncs")
	g.addPods(sync)
	g.delete(&record{Op: "delete", Namespace: "kube-system", Pods: jsonOf(t, []string{logged})})

	require.Equal(t, "coredns-1", g.renames[logged], "the pod of a deployment gets a stable name")
	require.Contains(t, strings.Join(stepCalls(g), "\n"), `s.DeletePods("kube-system", "coredns-1")`)
}

// The id widths follow the NUMA mode of the launch. A machine with NUMA
// disabled has one node, whatever its topology says, so a node name of the
// e2e test which names nothing there, like the node00 of a twelve-node
// machine, has to stay a name literal: as an id it would be padded to the
// node0 of the replay and compare equal, making an assertion which held in
// the e2e test fail in the generated one.
func TestWidthsFollowTheNUMAMode(t *testing.T) {
	calls := func(numa bool) string {
		g := genOf(t, []*record{
			{Op: "machine", NUMA: &numa},
			{Op: "launch", Values: json.RawMessage(`{}`), Topology: []map[string]any{
				{"mem": "2G", "cores": float64(1), "nodes": float64(6), "packages": float64(2)}}},
			verifyOf(`nodes["pod0c0"] != {"node00"}`),
		})
		return strings.Join(stepCalls(g), "\n")
	}

	on := calls(true)
	require.Contains(t, on, "s.SetNUMA(true)")
	require.Contains(t, on, "s.NodeIDs(0)", "with NUMA a two-digit node name is an id")

	off := calls(false)
	require.Contains(t, off, "s.SetNUMA(false)")
	require.Contains(t, off, `harness.NewSet("node00")`, "without NUMA the name stays a literal")
	require.NotContains(t, off, "s.NodeIDs(", "and is not compared as an id")
}

// A log which ends inside a skipped block, as a run aborted in one leaves it,
// ends the test at the assertion before the block. The skip would otherwise
// stay in the generated test, which then fails the unmatched-skip check of
// the Script instead of replaying the prefix it can.
func TestGenerateUnresumedSkipAtEndOfLog(t *testing.T) {
	g := genOf(t, []*record{
		launchOf(),
		verifyOf("len(cpus) == 2"),
		{Op: "skip", Reason: "the e2e test writes a file", Pods: podsOf(t)},
	})

	require.NotEmpty(t, g.stopped, "the log ends inside the skip")
	require.True(t, hasStep(g, "s.VerifyPy"), "the assertion before the skip stays")
	require.False(t, hasStep(g, "s.Skip"), "the skip without a resume goes")
}

// Two pods of one snapshot which would get the same stable name stop the
// generation. The harness keys pods by name and derives the sandbox IDs from
// the namespace and the name, so a test of two pods sharing one name would
// lose their identities, and a delete could take the wrong pod.
func TestSyncPodNameCollision(t *testing.T) {
	collide := newGenTest(t)
	// The pod of the deployment is renamed to coredns-1, which is the name
	// the pod after it already has.
	collide.syncPods(&record{Op: "sync", Pods: podsOf(t,
		syncPodOf("kube-system", "coredns-7c65d6cfc9-abcde", "coredns", "id-of-the-deployment-pod"),
		syncPodOf("kube-system", "coredns-1", "coredns", "id-of-coredns-1"))})

	require.Contains(t, collide.stopped, "coredns-1", "the generation stops at the collision")

	// The other order does not collide: the name of the second pod is taken,
	// so the pod of the deployment is numbered past it.
	fine := newGenTest(t)
	pods := fine.syncPods(&record{Op: "sync", Pods: podsOf(t,
		syncPodOf("kube-system", "coredns-1", "coredns", "id-of-coredns-1"),
		syncPodOf("kube-system", "coredns-7c65d6cfc9-abcde", "coredns", "id-of-the-deployment-pod"))})

	require.Empty(t, fine.stopped, "no collision in this order")
	require.Contains(t, pods, `harness.NewPod("coredns-2"`, "the deployment pod is numbered past it")
}

// A runtime pod of a QoS class only its init containers explain still syncs.
// A kubelet computes the class from the init containers too, so dropping them
// would leave a pod the generator reads as guaranteed while the e2e test
// recorded it as burstable, and the generation would stop there, cutting the
// assertions after it.
func TestSyncPodOfInitContainerQoS(t *testing.T) {
	g := newGenTest(t)
	p := syncPodOf("default", "pod0", "pod0c0", "id-of-pod0c0")
	p.Status.QOSClass = corev1.PodQOSBurstable
	// The application container is guaranteed on its own: both resources
	// have a limit equal to the request.
	p.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resapi.MustParse("1"),
			corev1.ResourceMemory: resapi.MustParse("100M"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resapi.MustParse("1"),
			corev1.ResourceMemory: resapi.MustParse("100M"),
		},
	}
	// The init container, which has run to completion, is not.
	p.Spec.InitContainers = []corev1.Container{{
		Name:      "pod0i0",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resapi.MustParse("500m")}},
	}}

	args := g.syncPods(&record{Op: "sync", Pods: podsOf(t, p)})

	require.NotEmpty(t, args, "the pod syncs")
	require.Contains(t, args, "harness.WithInitContainers(", "the init containers are kept")
	require.Contains(t, args, "harness.WithContainers(", "so are the application ones")
}

// A test the generator skips late leaves no mark on its file: its strings do
// not become variables of the file, and its topology is not the one the file
// declares.
func TestGenerateSkippedTestLeavesFileAlone(t *testing.T) {
	plugin := genPlugin()
	f := &genFile{topo: "n4c16", plugin: plugin.Name, consts: &genConsts{}}
	pods := podsOf(t, syncPodOf("default", "pod0", "pod0c0", "id-of-pod0c0"))

	launch := func(nodes int) *record {
		return &record{Op: "launch", Values: json.RawMessage(`{}`),
			Topology: []map[string]any{{"mem": "2G", "cores": float64(2), "nodes": float64(nodes)}}}
	}
	run := func(name string, records ...*record) {
		t.Run(name, func(t *testing.T) {
			g := &genTest{t: t, plugin: plugin, file: f, name: name,
				host: "n4c16-fedora-43-containerd", renames: map[string]string{}}
			g.generate(records)
		})
	}

	// The replay stops with no assertion before it, so the test is skipped.
	run("skipped", launch(4), &record{Op: "sync", Pods: pods},
		&record{Op: "unsupported", Command: "crictl rm all"})
	require.Empty(t, f.tests, "a test with no assertion is skipped")
	require.Nil(t, f.topology, "a skipped test does not decide the topology")

	run("accepted", launch(2), &record{Op: "sync", Pods: pods}, verifyOf("len(cpus) == 2"))
	require.Len(t, f.tests, 1, "only the test with an assertion is in the file")
	require.Empty(t, f.consts.names("genN4c16"),
		"the pods of the one accepted test are not a variable of the file")

	require.Equal(t, Topology{Group{Mem: "2G", Cores: 2, Nodes: 2}}, f.topology,
		"the topology is the accepted test's, not the skipped test's")
}

// The base holds the values more than half of the launches agree on, nested
// maps included, and nothing a bare majority does not reach.
func TestBaseValues(t *testing.T) {
	values := []any{
		map[string]any{"a": "1", "b": "shared", "m": map[string]any{"x": "1", "y": "1"}},
		map[string]any{"a": "2", "b": "shared", "m": map[string]any{"x": "1", "y": "2"}},
		map[string]any{"a": "3", "b": "shared", "m": map[string]any{"x": "1"}},
		map[string]any{"half": "yes"},
	}

	require.Equal(t, map[string]any{
		"b": "shared",
		"m": map[string]any{"x": "1"},
	}, baseValues(values, len(values)),
		"only b and m.x are in more than half of the launches")

	// Two of four is not more than half, so nothing is shared.
	require.Empty(t, baseValues(values[2:], 4), "a bare half shares nothing")
}

// The overlay of a launch holds what it changes from the base, and a null for
// a key the base adds which the chart does not have, so that merging the
// overlay over the base deletes it again.
func TestOverlay(t *testing.T) {
	var (
		chart  = map[string]any{"fromChart": "chart", "m": map[string]any{"x": "chart"}}
		base   = map[string]any{"same": "1", "changed": "base", "added": "base", "m": map[string]any{"x": "base"}}
		values = map[string]any{"same": "1", "changed": "mine", "own": "mine", "m": map[string]any{"x": "mine"}}
	)

	require.Equal(t, map[string]any{
		"changed": "mine",
		"own":     "mine",
		"m":       map[string]any{"x": "mine"},
		// The base adds "added", this launch does not have it, and the chart
		// has no value to fall back to.
		"added": nil,
	}, overlay(values, base, chart), "same is left out, added is deleted")
}

// A string is a raw Go literal unless it holds something a raw literal
// cannot.
func TestGoString(t *testing.T) {
	require.Equal(t, "`plain\nlines`", goString("plain\nlines"))
	require.Equal(t, `"a `+"`"+` tick"`, goString("a ` tick"), "a backtick needs quoting")
	require.Equal(t, `"a \r return"`, goString("a \r return"), "a carriage return needs quoting")
}

// A kube-system pod gets a name which does not change from run to run: the
// VM of a static pod becomes vm, and the random suffix of a deployment or
// daemonset pod the first number no other pod took.
func TestRename(t *testing.T) {
	g := newGenTest(t)
	taken := map[string]bool{}

	require.Equal(t, "etcd-vm", g.rename("etcd-"+g.host, taken), "a static pod is named after the VM")
	require.Equal(t, "coredns-1", g.rename("coredns-7c65d6cfc9-abcde", taken), "a deployment pod")
	require.Equal(t, "coredns-2", g.rename("coredns-7c65d6cfc9-fghij", taken), "the next one of the same deployment")
	require.Equal(t, "kube-proxy-1", g.rename("kube-proxy-abcde", taken), "a daemonset pod")
	require.Equal(t, "coredns-1", g.rename("coredns-7c65d6cfc9-abcde", taken), "a pod renamed before keeps its name")
	require.Equal(t, "nri-resource-policy", g.rename("nri-resource-policy", taken),
		"a name of neither shape is left alone")
}

// A pod of an e2e test template with an explicit zero quantity takes the
// options of NewPod, which keep the quantity.
func TestPodSpecExplicitZeroRequest(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Name, pod.Namespace, pod.Labels = "pod0", "default", map[string]string{"app": "pod0"}
	pod.Spec.Containers = []corev1.Container{{
		Name: "pod0c0",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resapi.MustParse("0")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resapi.MustParse("1")},
		},
	}}

	qos, spec := newGenTest(t).podSpec(pod)
	require.Empty(t, spec, "an explicit zero quantity has no PodSpec")
	require.Empty(t, qos)
}

// A mapping key which YAML 1.1 reads as a boolean or a null is quoted, so
// that the helm values of a generated launch are the ones the e2e test had.
// Every other plain key stays unquoted, which keeps the overlay readable.
func TestFlowYAMLQuotesTypedKeys(t *testing.T) {
	for _, k := range []string{
		"y", "Y", "yes", "Yes", "YES", "n", "N", "no", "No", "NO",
		"true", "True", "TRUE", "false", "False", "FALSE",
		"on", "On", "ON", "off", "Off", "OFF", "null", "Null", "NULL",
	} {
		got := flowYAML(t, map[string]any{k: "v"}, false)
		require.Equal(t, `{"`+k+`": "v"}`, got, "%s is a YAML 1.1 typed key", k)

		// And the quoting survives the conversion a launch puts it through.
		data, err := yaml.YAMLToJSON([]byte(got))
		require.NoError(t, err, "%s: %s", k, got)
		require.JSONEq(t, `{"`+k+`":"v"}`, string(data), "%s round trips", k)
	}

	for _, k := range []string{"config", "cpu", "colocatePods", "a-b", "x.y", "_"} {
		require.Equal(t, "{"+k+": \"v\"}", flowYAML(t, map[string]any{k: "v"}, false),
			"%s needs no quotes", k)
	}
}

// The strings of a skipped block the replay cannot resume go with its steps,
// so a string only one accepted test really uses does not become a variable
// of the file.
func TestGenerateCutSkipDropsItsStrings(t *testing.T) {
	plugin := genPlugin()
	f := &genFile{topo: "n4c16", plugin: plugin.Name, consts: &genConsts{}}
	pods := podsOf(t, syncPodOf("default", "pod0", "pod0c0", "id-of-pod0c0"))

	run := func(name string, records ...*record) {
		t.Run(name, func(t *testing.T) {
			g := &genTest{t: t, plugin: plugin, file: f, name: name,
				host: "n4c16-fedora-43-containerd", renames: map[string]string{}}
			g.generate(records)
		})
	}

	// One test syncs the pods and is accepted whole.
	run("whole", launchOf(), &record{Op: "sync", Pods: pods}, verifyOf("len(cpus) == 2"))

	// The other syncs the same pods only inside a skipped block which the
	// replay cannot resume, so that sync is cut from the test. A sync is not
	// one of the records a skipped block ignores, so it does register its
	// pods before the cut.
	run("cut", launchOf(), verifyOf("len(cpus) == 2"),
		&record{Op: "skip", Reason: "the e2e test writes a file", Pods: podsOf(t)},
		&record{Op: "sync", Pods: pods},
		&record{Op: "resume", Pods: pods})

	require.Len(t, f.tests, 2, "both tests are accepted")
	require.Empty(t, f.consts.names("genN4c16"),
		"the pods of the one test which still uses them are not a variable of the file")
}

// A segment with no steps is left out of the generated test, which would
// otherwise declare the Script of that segment and never use it, and so fail
// to compile.
func TestGenerateSkipsEmptySegment(t *testing.T) {
	g := newGenTest(t)
	g.segments = []*genSegment{
		{steps: []genStep{{format: "s.Terminate()"}}},
		{env: []string{"OVERRIDE_SYS_CSTATES=1"}}, // no steps
	}
	g.file.tests = []*genTest{g}
	g.file.topology = Topology{Group{Mem: "2G", Cores: 2, Nodes: 2}}

	src := string(g.file.source(t, "topologyaware"))
	require.Equal(t, 1, strings.Count(src, "harness.NewScript"),
		"only the segment with steps declares a Script")
	require.NotContains(t, src, "OVERRIDE_SYS_CSTATES",
		"and the empty segment contributes no Reexec either")
}

// A machine change after an expected launch failure does not stop the
// generation: the Script is not running a plugin then, so the replay follows
// it. Without this the assertions after the change are lost.
func TestGenerateMachineAfterLaunchFails(t *testing.T) {
	noNUMA := false
	// The first launch and assertion are there so that the test is accepted
	// either way: a generation which stops with no assertion before it is
	// skipped, and a skipped test would hide the stop.
	g := genOf(t, []*record{
		launchOf(),
		verifyOf("len(cpus) == 2"),
		{Op: "terminate"},
		{Op: "expect-launch-failure"},
		launchOf(),
		{Op: "machine", NUMA: &noNUMA},
		launchOf(),
		verifyOf("len(cpus) == 2"),
	})

	require.Empty(t, g.stopped, "the replay follows a machine change after a failed launch")
	calls := strings.Join(stepCalls(g), "\n")
	require.Contains(t, calls, "s.LaunchFails(")
	require.Contains(t, calls, "s.SetNUMA(false)")
	require.Contains(t, calls, "s.VerifyPy(")
}
