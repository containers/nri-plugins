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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func cmdsOf(e *e2eTarget) []string {
	var out []string
	for _, r := range e.recs {
		out = append(out, r.cmds...)
	}
	return out
}

func TestE2ETargetSteps(t *testing.T) {
	e := newE2ETarget("n4c16/test50-x", "x_test.go", "topology-aware")
	topo := Topology{{Mem: "2G", Cores: 2}}
	e.launch(topo, []string{"config: {a: 1}", "config: {b: 2}"}, launchOK)
	e.launch(topo, []string{"config: {a: 1, b: 2}"}, launchMayFail)
	e.create(PodSpec{ContCount: 2, CPU: "500m", Annotations: map[string]string{"k/x": "true"}}, Guaranteed, "pod0")
	e.create(PodSpec{Namespace: "test-ns", CPUReq: "1", CPULim: "0"}, Burstable, "pod1")
	e.create(PodSpec{Name: "sys", Namespace: "kube-system"}, BestEffort, "sys")
	e.deletePods("test-ns", []string{"pod1"})
	e.deleteAll("default")
	e.setNUMA(false)
	e.setCmdline("isolcpus=1 nohz")
	e.terminate()

	require.Empty(t, e.errs)
	require.Equal(t, []string{
		`helm_config="$TEST_DIR/helm-config-1.yaml" helm-launch topology-aware`,
		`helm-terminate`,
		`expect_error=1 helm_config="$TEST_DIR/helm-config-1.yaml" helm-launch topology-aware`,
		`CONTCOUNT=2 CPU=500m ANN0='k/x: "true"' create guaranteed`,
		`create-namespaces test-ns`,
		`namespace=test-ns CPUREQ=1 CPULIM=0 create burstable`,
		`NAME=sys namespace=kube-system create besteffort`,
		`delete-pods -n test-ns pod1`,
		`delete-pods --all`,
		`disable-numa`,
		`vm-set-kernel-cmdline-reboot 'isolcpus=1 nohz'`,
		`helm-terminate`,
	}, cmdsOf(e))
	require.Equal(t, map[string]string{"helm-config-1.yaml": "config:\n  a: 1\n  b: 2\n"}, e.files)
	require.Equal(t, []string{"test-ns"}, e.ns)
	require.Equal(t, []string{"sys"}, e.sysPods)
	require.False(t, e.release)
}

func TestE2ETargetRefuses(t *testing.T) {
	e := newE2ETarget("n4c16/test50-x", "x_test.go", "topology-aware")
	e.launch(Topology{{Cores: 2}}, nil, launchOK)
	e.launch(Topology{{Cores: 4}}, nil, launchOK)
	e.deleteAll("kube-system")
	e.create(PodSpec{Annotations: map[string]string{"k": `a\nb`}}, Guaranteed, "pod0")
	e.consts("A = 1")
	e.consts("A = 2")
	require.Len(t, e.errs, 4)
}

func TestShellQuote(t *testing.T) {
	require.Equal(t, "500m", shellQuote("500m"))
	require.Equal(t, `'a b'`, shellQuote("a b"))
	require.Equal(t, `'it'\''s'`, shellQuote("it's"))
}

func TestE2ETargetCreatePod(t *testing.T) {
	pod, err := NewPod("web",
		InNamespace("test-ns"),
		WithAnnotations(map[string]string{"price": "$5"}),
		WithInitContainers(NewContainer("web-init")),
		WithContainers(NewContainer("web-c0", WithRequests("1", "100M"), WithLimits("1", "100M"))),
	).k8s()
	require.NoError(t, err)

	e := newE2ETarget("n4c16/test50-x", "x_test.go", "topology-aware")
	e.createPod(pod)
	require.Empty(t, e.errs)
	require.Equal(t, []string{"create-namespaces test-ns", "NAME=web namespace=test-ns create web"}, cmdsOf(e))
	tmpl := e.files["web.yaml.in"]
	require.Contains(t, tmpl, "image: quay.io/prometheus/busybox")
	// The e2e tests find the containers by their names on the command line.
	require.Contains(t, tmpl, `- echo web-c0 \$(sleep inf)`)
	require.Contains(t, tmpl, `- echo web-init \$(sleep 5)`)
	require.Contains(t, tmpl, `price: \$5`)
	require.NotContains(t, tmpl, "namespace:")
	require.NotContains(t, tmpl, "status:")
	require.NotContains(t, tmpl, "creationTimestamp")

	for _, p := range []string{"guaranteed", `a\b`} {
		pod, err := NewPod(p, WithAnnotations(map[string]string{"k": p})).k8s()
		require.NoError(t, err)
		e := newE2ETarget("n4c16/test50-x", "x_test.go", "topology-aware")
		e.createPod(pod)
		require.Len(t, e.errs, 1, p)
		require.Empty(t, e.recs, p)
	}
}

func TestE2ETargetFaithful(t *testing.T) {
	topo := Topology{{Cores: 2}}
	newTarget := func() *e2eTarget { return newE2ETarget("n4c16/test50-x", "x_test.go", "topology-aware") }

	// A null drops a chart default, so it must reach helm.
	e := newTarget()
	e.launch(topo, []string{"config: {agent: {a: 1}}", "config: {agent: null}"}, launchOK)
	require.Contains(t, e.files["helm-config-1.yaml"], "agent: null")

	// A failed launch leaves the release installed, as does a launch.
	e = newTarget()
	e.launch(topo, nil, launchFails)
	e.launch(topo, nil, launchOK)
	e.launch(topo, nil, launchOK)
	require.Equal(t, []string{
		`helm_config="$TEST_DIR/helm-config-1.yaml" expect-launch-failure topology-aware`,
		`helm-terminate`,
		`helm_config="$TEST_DIR/helm-config-1.yaml" helm-launch topology-aware`,
		`helm-terminate`,
		`helm_config="$TEST_DIR/helm-config-1.yaml" helm-launch topology-aware`,
	}, cmdsOf(e))
	require.True(t, e.release)

	// Deleting no pods is no command.
	e = newTarget()
	e.deletePods("default", nil)
	require.Empty(t, cmdsOf(e))
	require.Empty(t, e.errs)

	// The machine is restored at the end.
	e = newTarget()
	e.setNUMA(false)
	e.setCmdline("isolcpus=1")
	require.Equal(t, `trap 'enable-numa; vm-set-kernel-cmdline-reboot' EXIT`, e.restoreTrap())
	e.setNUMA(true) // a failure before may leave NUMA disabled
	require.Equal(t, `trap 'enable-numa; vm-set-kernel-cmdline-reboot' EXIT`, e.restoreTrap())
	require.Empty(t, newTarget().restoreTrap())
}

func TestE2ETargetRefusesUnfaithful(t *testing.T) {
	newTarget := func() *e2eTarget { return newE2ETarget("n4c16/test50-x", "x_test.go", "topology-aware") }
	for name, spec := range map[string]struct {
		spec PodSpec
		qos  string
	}{
		"burstable init containers":  {PodSpec{IContCount: 1}, Burstable},
		"besteffort init containers": {PodSpec{IContCount: 1}, BestEffort},
		"no CPU request":             {PodSpec{CPUReq: "0"}, Burstable},
		"no memory request":          {PodSpec{MemReq: "0"}, Burstable},
		"no CPU":                     {PodSpec{CPU: "0"}, Guaranteed},
		"no memory":                  {PodSpec{Mem: "0"}, Guaranteed},
		"an app label":               {PodSpec{Labels: map[string]string{"app": "x"}}, Guaranteed},
	} {
		e := newTarget()
		e.create(spec.spec, spec.qos, "pod0")
		require.Len(t, e.errs, 1, name)
		require.Empty(t, e.recs, name)
	}

	e := newTarget()
	e.consts(`A = "a\n"`)
	require.Len(t, e.errs, 1, "a backslash in py_consts")

	pod := func(cpu string, options ...PodOption) *corev1.Pod {
		options = append(options, WithContainers(NewContainer("webc0", WithRequests(cpu, "100M"))))
		p, err := NewPod("web", options...).k8s()
		require.NoError(t, err)
		return p
	}
	e = newTarget()
	e.createPod(pod("1"))
	e.createPod(pod("1"))
	require.Empty(t, e.errs, "the same pod twice")
	e.createPod(pod("2"))
	require.Len(t, e.errs, 1, "another pod of the same name")

	for name, p := range map[string]*corev1.Pod{
		"kind in a value":     pod("1", WithLabels(map[string]string{"x": "kind"})),
		"a multi-line string": pod("1", WithAnnotations(map[string]string{"x": "a\nb"})),
	} {
		e := newTarget()
		e.createPod(p)
		require.Len(t, e.errs, 1, name)
	}
}
