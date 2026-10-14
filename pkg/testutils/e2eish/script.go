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
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/containers/nri-plugins/pkg/utils"
)

// Script runs the steps of an e2e test, one method for each kind of record
// of its log. Replay drives it from the records of a log, a generated test
// calls it directly. Helm values are given as YAML, pods as Pods.
type Script struct {
	t        *testing.T
	plugin   *Plugin
	h        *Harness
	host     Host // as the machine steps change it
	launched bool
	failed   bool // a launch which may fail did
	skipping string
	at       string // the record being replayed, for messages
	consts   string
	// requirePython makes a verify with no python3 skip the test, as a
	// replay checks nothing else then.
	requirePython bool
	pods          int // created, for the names of PodSpecs
	verified      int
	skipped       int
	reads         int // of the state, by the accessors of the Go assertions
}

// NewScript returns a script for the plugin. A test with overrides read when
// the binary starts runs the script in a Reexec child.
func NewScript(t *testing.T, plugin *Plugin) *Script {
	// The e2e deployment enables test hints.
	t.Setenv(utils.EnvVarEnableTestAPIs, "1")
	s := &Script{t: t, plugin: plugin}
	t.Cleanup(func() {
		if t.Failed() || t.Skipped() {
			return
		}
		if s.skipping != "" {
			t.Errorf("the skip of %q has no resume", s.skipping)
		}
		if r := s.verifyReport(); r != "" {
			t.Log(r)
		}
	})
	return s
}

// verifyReport tells how many python assertions were verified and skipped,
// and how many reads of the state the Go assertions made, "" if a test did
// neither. A require assertion does not go through the Script, so the reads
// of the accessors are all the Script sees of the Go ones: a run with no
// python3 verifies no python assertion at all and the reads are what tells
// that it checked anything.
func (s *Script) verifyReport() string {
	r := ""
	if s.verified+s.skipped > 0 {
		noun := "assertions"
		if s.verified == 1 {
			noun = "assertion"
		}
		r = fmt.Sprintf("%d python %s verified, %d skipped", s.verified, noun, s.skipped)
	}
	if s.reads > 0 {
		noun := "reads"
		if s.reads == 1 {
			noun = "read"
		}
		if r != "" {
			r += ", "
		}
		r += fmt.Sprintf("%d Go state %s", s.reads, noun)
	}
	return r
}

// Harness is the harness of the script, nil before the first launch.
func (s *Script) Harness() *Harness {
	return s.h
}

func (s *Script) unsupported(format string, args ...any) {
	s.t.Helper()
	msg := fmt.Sprintf(format, args...)
	if s.at != "" {
		msg = s.at + ": " + msg
	}
	if r := s.verifyReport(); r != "" {
		msg += ", after " + r
	}
	s.t.Skip(msg)
}

// Launch is helm-launch of the plugin on the topology, with helm values
// files merged in order, as helm merges them.
func (s *Script) Launch(topo Topology, values ...string) {
	s.t.Helper()
	s.launch(topo, s.helmConfig(values), launchOK)
}

// LaunchMayFail is helm-launch with expect_error=1.
func (s *Script) LaunchMayFail(topo Topology, values ...string) {
	s.t.Helper()
	s.launch(topo, s.helmConfig(values), launchMayFail)
}

// LaunchFails is expect-launch-failure and the helm-launch after it.
func (s *Script) LaunchFails(topo Topology, values ...string) {
	s.t.Helper()
	s.launch(topo, s.helmConfig(values), launchFails)
}

func (s *Script) helmConfig(values []string) *launchConfig {
	s.t.Helper()
	var files []json.RawMessage
	for _, v := range values {
		data, err := yaml.YAMLToJSON([]byte(v))
		require.NoError(s.t, err, "bad helm values")
		files = append(files, data)
	}
	cfg, env, err := s.plugin.HelmConfig(files...)
	require.NoError(s.t, err, "bad helm values")
	return &launchConfig{config: cfg, env: env}
}

// launchConfig is the configuration and the extra environment of a launch.
type launchConfig struct {
	config any
	env    map[string]string
}

const (
	launchOK = iota
	launchMayFail
	launchFails
)

func (s *Script) launch(topo Topology, l *launchConfig, how int) {
	s.t.Helper()

	if why := s.plugin.unsupportedConfig(l.config); why != "" {
		s.unsupported("%s", why)
	}
	// Overrides read when the binary starts are those of the process.
	for _, name := range initEnvVars {
		if l.env[name] != os.Getenv(name) {
			s.unsupported("%s other than at the start of the process", name)
		}
	}
	// The plugin discovers the machine at each start, with the overrides
	// of its helm values.
	for _, name := range launchEnvVars {
		s.t.Setenv(name, l.env[name])
	}
	// The SST mock keeps its state in a directory of the test.
	s.t.Setenv("OVERRIDE_SST_STATE_DIR", s.t.TempDir())
	s.host.Caches = l.env["OVERRIDE_SYS_CACHES"]
	if s.h == nil {
		s.h = New(s.t, s.plugin, topo, s.host)
	}
	s.h.Machine = NewMachine(s.t, topo, s.host)
	s.failed = false
	switch how {
	case launchFails:
		s.h.LaunchFails(l.config)
	case launchMayFail:
		s.launched = s.h.LaunchMayFail(l.config)
		s.failed = !s.launched
	default:
		s.h.Launch(l.config)
		s.launched = true
	}
}

// SetNUMA enables or disables NUMA on the kernel command line, for the next
// launch.
func (s *Script) SetNUMA(enabled bool) {
	s.t.Helper()
	// The node reboots, which restarts a running plugin with no launch.
	if s.launched {
		s.unsupported("changing the machine with the plugin running")
	}
	s.host.NoNUMA = !enabled
}

// SetCmdline sets the kernel command line, for the next launch. Only
// isolcpus is supported.
func (s *Script) SetCmdline(cmdline string) {
	s.t.Helper()
	if s.launched {
		s.unsupported("changing the machine with the plugin running")
	}
	isolated, err := isolcpus(cmdline)
	if err != nil {
		s.unsupported("%v", err)
	}
	s.host.Isolated = isolated
}

// SyncWithRuntimePods is the Synchronize of a starting plugin, with the pods
// running in the runtime then.
func (s *Script) SyncWithRuntimePods(pods ...*Pod) {
	s.t.Helper()
	require.NotNil(s.t, s.h, "a sync with no policy launched")
	s.sync(s.syncPods(pods))
}

func (s *Script) syncPods(pods []*Pod) []*syncPod {
	s.t.Helper()
	var list []*syncPod
	for _, p := range pods {
		sp, err := p.syncPod()
		require.NoError(s.t, err)
		list = append(list, sp)
	}
	return list
}

func (s *Script) sync(pods []*syncPod) {
	s.t.Helper()
	for _, p := range pods {
		if len(p.Spec.ResourceClaims) > 0 {
			s.unsupported("a pod with resource claims")
		}
		if why := s.plugin.unsupportedPod(&p.Pod); why != "" {
			s.unsupported("%s", why)
		}
	}
	s.h.syncPods(pods)
}

// Terminate is helm-terminate.
func (s *Script) Terminate() {
	s.launched = false
}

// CreateGuaranteed is create guaranteed, of the pod of the spec.
func (s *Script) CreateGuaranteed(spec PodSpec) {
	s.t.Helper()
	s.createSpec(spec, Guaranteed)
}

// CreateBurstable is create burstable, of the pod of the spec.
func (s *Script) CreateBurstable(spec PodSpec) {
	s.t.Helper()
	s.createSpec(spec, Burstable)
}

// CreateBestEffort is create besteffort, of the pod of the spec.
func (s *Script) CreateBestEffort(spec PodSpec) {
	s.t.Helper()
	s.createSpec(spec, BestEffort)
}

func (s *Script) createSpec(spec PodSpec, qos string) {
	s.t.Helper()
	if spec.Name == "" {
		spec.Name = fmt.Sprintf("pod%d", s.pods)
	}
	s.CreatePod(spec.Name, spec.pod(qos)...)
}

// CreatePod is kubectl create of a pod.
func (s *Script) CreatePod(name string, options ...PodOption) {
	s.t.Helper()
	pod, err := NewPod(name, options...).k8s()
	require.NoError(s.t, err)
	s.createPod(pod.Namespace, pod)
}

// createPod creates a pod in the namespace, unless the pod has its own.
func (s *Script) createPod(ns string, p *corev1.Pod) {
	s.t.Helper()
	s.pods++
	if s.skipping != "" {
		return
	}
	if p.Kind != "" && p.Kind != "Pod" {
		s.unsupported("creating a %s", p.Kind)
	}
	if len(p.Spec.ResourceClaims) > 0 {
		s.unsupported("a pod with resource claims")
	}
	if why := s.plugin.unsupportedPod(p); why != "" {
		s.unsupported("%s", why)
	}
	require.NotNil(s.t, s.h, "creating a pod with no policy launched")
	if s.failed {
		s.unsupported("creating a pod after a launch which failed")
	}
	s.h.CreateFromPod(cmp.Or(p.Namespace, ns), p)
}

// DeletePods is kubectl delete of pods in the namespace. Pods not there are
// left alone.
func (s *Script) DeletePods(ns string, names ...string) {
	s.t.Helper()
	if s.h == nil || s.skipping != "" {
		return
	}
	for _, name := range names {
		if pod, ok := s.h.Pods[name]; ok && pod.GetNamespace() == ns {
			s.h.DeletePods(name)
		}
	}
}

// DeleteAllPodsIn is kubectl delete of all the pods in the namespace.
func (s *Script) DeleteAllPodsIn(ns string) {
	s.t.Helper()
	if s.h == nil || s.skipping != "" {
		return
	}
	var names []string
	for name, pod := range s.h.Pods {
		if pod.GetNamespace() == ns {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	s.DeletePods(ns, names...)
}

// Consts is the py_consts code for the verify expressions after it.
func (s *Script) Consts(code string) {
	s.consts = code
}

// VerifyPy is verify of a python expression which held in the e2e test.
func (s *Script) VerifyPy(expr string) {
	s.t.Helper()
	s.verify(expr, true)
}

// verify checks that the expression holds in the replay exactly when it held
// in the e2e test, and that the accessors give the state python saw. An
// expression is skipped with no python3, or with names the replay does not
// provide.
func (s *Script) verify(expr string, held bool) {
	s.t.Helper()
	if s.skipping != "" {
		s.skipped++
		return
	}
	require.NotNil(s.t, s.h, "a verify with no policy launched")
	ok, state, snap, err := s.h.evalVerify(expr, s.consts)
	if errors.Is(err, errNoPython) && s.requirePython {
		s.unsupported("%v", err)
	}
	if errors.Is(err, errNoPython) || errors.Is(err, errNoName) {
		Log.Infof("===== verify %q: skipped, %v", expr, err)
		s.skipped++
		return
	}
	require.NoError(s.t, err, "failed to evaluate %q", expr)
	s.crossCheck(state, snap)
	Log.Infof("===== verify %q: e2e held %v, replay held %v", expr, held, ok)
	require.Equal(s.t, held, ok, "%q is %v in the replay, %v in the e2e test", expr, ok, held)
	s.verified++
}

// crossCheck requires that the accessors give the state python computed, for
// every container python has one of. Both sides come from the snapshot of the
// allocations python was given, not from a second read of them: a timer of the
// policy, like the cold start of topology-aware, can change an allocation
// while python runs, and the two sides would differ with both of them right.
func (s *Script) crossCheck(state map[string]map[string][]string, snap map[string]allowed) {
	s.t.Helper()
	names := newTopoNames(s.h.Machine)
	for ctr := range state["cpus"] {
		a, ok := snap[ctr]
		require.True(s.t, ok, "%s has a python state but no allocation in the snapshot", ctr)
		st := names.state(a.cpus, a.mems)
		for _, v := range stateVars {
			want := append([]string{}, state[v][ctr]...)
			got := append([]string{}, slices.Sorted(maps.Keys(st[v]))...)
			require.Equal(s.t, want, got, "%s[%q] in Go and in python", v, ctr)
		}
	}
}

// Skip starts a block of the e2e test the replay cannot follow. The pod
// changes and assertions of the block are ignored.
func (s *Script) Skip(reason string) {
	s.t.Helper()
	require.Empty(s.t, s.skipping, "a skip of %q within the skip of %q", reason, s.skipping)
	Log.Infof("===== skip: %s", reason)
	s.skipping = reason
}

// ResumeWithRuntimePods ends a skipped block, with the pods running in the
// runtime then. The pods and containers gone since the skip are deleted.
func (s *Script) ResumeWithRuntimePods(pods ...*Pod) {
	s.t.Helper()
	s.resume(s.syncPods(pods))
}

func (s *Script) resume(pods []*syncPod) {
	s.t.Helper()
	require.NotEmpty(s.t, s.skipping, "a resume with no skip")
	if s.h != nil {
		s.h.forgetGone(pods)
	}
	Log.Infof("===== resume after skipping %s", s.skipping)
	s.skipping = ""
}
