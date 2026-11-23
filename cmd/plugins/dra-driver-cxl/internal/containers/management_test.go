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

package containers

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/containerd/nri/pkg/api"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/steering"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// claimSettings returns the container settings of a result whose claim
// ns/<claim> request <request> sets the steering.
func claimSettings(claim, request string, s config.Steering) config.ResultSettings {
	origin := "claim ns/" + claim + " request " + request
	rs := config.ResultSettings{Container: &config.Container{Steering: &s}, Origins: map[string]string{}}
	if s.Policy != "" {
		rs.Origins[config.FieldPolicy] = origin
	}
	if s.MinStep != "" {
		rs.Origins[config.FieldMinStep] = origin
	}
	if s.MaxStep != "" {
		rs.Origins[config.FieldMaxStep] = origin
	}
	return rs
}

// optIn returns the container settings of a result with the steering
// policy from a claim.
func optIn(policy string) config.ResultSettings {
	return config.ResultSettings{
		Container: &config.Container{Steering: &config.Steering{Policy: policy}},
		Origins:   map[string]string{config.FieldPolicy: "claim ns/x request mem"},
	}
}

// nriClaims are prepared claims of cxl.nri.io:
//   - split: result 0 a numa slice on node 2, result 1 a dax slice, result
//     2 DRAM on nodes 0-1, result 3 a region on node 4
//   - auto: a numa slice on node 2 that opts in with policy auto
//   - none: a numa slice on node 2 that says policy none
//   - dram: DRAM on NUMA node 0 with policy first-dram and maxStep 64Mi
//   - lease: the lease device
func nriClaims() claimTable {
	auto := claimSettings("auto", "mem", config.Steering{Policy: config.PolicyAuto})
	return claimTable{
		"split": {ClaimName: "ns/split", Devices: []state.DeviceInfo{
			{Result: 0, RequestName: "numa", DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeNUMA, NUMANodes: []int{2}, ConsumedBytes: 1 << 30},
			{Result: 1, RequestName: "dax", DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeDax, ConsumedBytes: 1 << 30},
			{Result: 2, RequestName: "dram", DeviceType: device.DeviceTypeDRAM, Mode: cxlapi.ModeNUMA, NUMANodes: []int{1, 0}, ConsumedBytes: 2 << 30},
			{Result: 3, RequestName: "region", DeviceType: device.DeviceTypeRegion, Mode: cxlapi.ModeNUMA, NUMANodes: []int{4}, ConsumedBytes: 1 << 30},
		}},
		"auto": {ClaimName: "ns/auto", Devices: []state.DeviceInfo{
			{RequestName: "mem", DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeNUMA, NUMANodes: []int{2}, ConsumedBytes: 1 << 30, ResultSettings: auto},
		}},
		"none": {ClaimName: "ns/none", Devices: []state.DeviceInfo{
			{RequestName: "mem", DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeNUMA, NUMANodes: []int{2}, ConsumedBytes: 1 << 30,
				ResultSettings: claimSettings("none", "mem", config.Steering{Policy: config.PolicyNone})},
		}},
		"dram": {ClaimName: "ns/dram", Devices: []state.DeviceInfo{
			{RequestName: "mem", DeviceType: device.DeviceTypeDRAM, Mode: cxlapi.ModeNUMA, NUMANodes: []int{0}, ConsumedBytes: 1 << 30,
				ResultSettings: claimSettings("dram", "mem", config.Steering{Policy: config.PolicyFirstDRAM, MaxStep: "64Mi"})},
		}},
		"lease": {ClaimName: "ns/lease", Devices: []state.DeviceInfo{{RequestName: "lease", DeviceType: device.DeviceTypeLease}}},
	}
}

// nriPoolResults are prepared results of pool claims: pool result 0 a numa
// device on node 3, result 1 a dax device; poolauto a numa device on node 3
// that opts in with policy auto.
func nriPoolResults() claimTable {
	return claimTable{
		"pool": {ClaimName: "ns/pool", Devices: []state.DeviceInfo{
			{Result: 0, RequestName: "numa", DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeNUMA, NUMANodes: []int{3}, ConsumedBytes: 256 << 20},
			{Result: 1, RequestName: "dax", DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeDax, ConsumedBytes: 256 << 20},
		}},
		"poolauto": {ClaimName: "ns/poolauto", Devices: []state.DeviceInfo{
			{Result: 0, RequestName: "mem", DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeNUMA, NUMANodes: []int{3}, ConsumedBytes: 256 << 20,
				ResultSettings: claimSettings("poolauto", "mem", config.Steering{Policy: config.PolicyAuto})},
		}},
	}
}

// fenceClaims are prepared claims: DRAM, a dax slice, a numa slice of a
// local memdev and a region, and DRAM and a numa slice that opt in to
// steering.
func fenceClaims() claimTable {
	return claimTable{
		"dram-auto": {ClaimName: "ns/dram-auto", Devices: []state.DeviceInfo{
			{DeviceType: device.DeviceTypeDRAM, Mode: cxlapi.ModeNUMA, NUMANodes: []int{0}, ConsumedBytes: 1 << 30, ResultSettings: optIn(config.PolicyAuto)},
		}},
		"numa-auto": {ClaimName: "ns/numa-auto", Devices: []state.DeviceInfo{
			{DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeNUMA, NUMANodes: []int{2}, ConsumedBytes: 1 << 30, ResultSettings: optIn(config.PolicyAuto)},
		}},
		"dram": {ClaimName: "ns/dram", Devices: []state.DeviceInfo{
			{DeviceType: device.DeviceTypeDRAM, Mode: cxlapi.ModeNUMA, NUMANodes: []int{0}, ConsumedBytes: 1 << 30},
		}},
		"dax": {ClaimName: "ns/dax", Devices: []state.DeviceInfo{
			{DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeDax, NUMANodes: []int{2}, ConsumedBytes: 1 << 30},
		}},
		"numa": {ClaimName: "ns/numa", Devices: []state.DeviceInfo{
			{DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeNUMA, NUMANodes: []int{2}, ConsumedBytes: 1 << 30},
		}},
		"region": {ClaimName: "ns/region", Devices: []state.DeviceInfo{
			{DeviceType: device.DeviceTypeRegion, Mode: cxlapi.ModeNUMA, NUMANodes: []int{4}, ConsumedBytes: 1 << 30},
		}},
	}
}

// newDecider returns a plugin whose results are the claims of nriClaims and
// nriPoolResults, with the defaults.
func newDecider(t *testing.T, defaults config.Container) *Plugin {
	t.Helper()
	e := newEnvWith(t, envOptions{fencing: defaults.Fencing, defaults: defaults})
	e.local.set(nriClaims())
	e.pool.set(nriPoolResults())
	return e.p
}

func TestResultMarkers(t *testing.T) {
	uid := "0a1b2c3d-0000-1111-2222-333344445555"
	uid2 := "6a1b2c3d-0000-1111-2222-333344445555"
	env := []string{"PATH=/bin", cxlapi.ResultMarker(uid, 0) + "=1", "CXL_C1EE0001_SIZE=1", cxlapi.ResultMarker(uid2, 1) + "=1",
		cxlapi.ResultMarker(uid, 0) + "=1", cxlapi.ResultMarker(uid, 2) + "=1", "CXL_CLAIM_u1_01=1", "CXL_CLAIM__1=1",
		"CXL_CLAIM_u1=1", "CXL_CLAIM_u1_x=1", "CXL_CLAIM_u1_+1=1", "CXL_CLAIM_u1_-1=1"}
	if got := cxlapi.ResultMarker(uid, 3); got != "CXL_CLAIM_0a1b2c3d_0000_1111_2222_333344445555_3" {
		t.Fatalf("marker %s", got)
	}
	if got := extractResults(env); !reflect.DeepEqual(got, []resultRef{{uid, 0}, {uid2, 1}, {uid, 2}}) {
		t.Fatalf("results %v", got)
	}
	if got := extractResults([]string{"PATH=/bin"}); got != nil {
		t.Fatalf("results %v of no markers", got)
	}
}

// TestContainerResults checks that a container gets the memory and the
// claimed nodes of the results that it uses only (a claim with requests
// used by different containers), of both drivers.
func TestContainerResults(t *testing.T) {
	p := newDecider(t, config.Container{})
	for _, tc := range []struct {
		refs    []resultRef
		mem     steering.MemoryTypes
		claimed string
	}{
		{[]resultRef{{"split", 0}}, steering.MemoryTypes{CXLNUMANodes: []int{2}, CXLQuota: 1 << 30}, "2"},
		{[]resultRef{{"split", 1}}, steering.MemoryTypes{}, ""},
		{[]resultRef{{"split", 2}}, steering.MemoryTypes{DRAMNUMANodes: []int{0, 1}, DRAMQuota: 2 << 30}, ""},
		{[]resultRef{{"split", 3}}, steering.MemoryTypes{CXLNUMANodes: []int{4}, CXLQuota: 1 << 30}, "4"},
		{[]resultRef{{"split", 0}, {"split", 2}, {"pool", 0}},
			steering.MemoryTypes{DRAMNUMANodes: []int{0, 1}, CXLNUMANodes: []int{2, 3}, DRAMQuota: 2 << 30, CXLQuota: 1<<30 + 256<<20}, "2-3"},
		{[]resultRef{{"pool", 1}, {"lease", 0}}, steering.MemoryTypes{}, ""},
		// unknown claims and results
		{[]resultRef{{"split", 7}, {"gone", 0}, {"pool", 2}}, steering.MemoryTypes{}, ""},
	} {
		results := p.results(testPod("p"), "ns/pod/ctr", tc.refs)
		if mem := steeredMemory(results); fmt.Sprint(mem) != fmt.Sprint(tc.mem) {
			t.Errorf("%v: memory %+v, expected %+v", tc.refs, mem, tc.mem)
		}
		if got := claimedNodes(results).String(); got != tc.claimed {
			t.Errorf("%v: claimed nodes %q, expected %q", tc.refs, got, tc.claimed)
		}
	}
	// without the drivers
	none := &Plugin{}
	if got := none.results(testPod("p"), "ns/pod/ctr", []resultRef{{"split", 0}}); got != nil {
		t.Fatalf("results %+v", got)
	}
}

// The marker of a claim of another namespace, or of a lease claim whose
// recorded reservedFor does not have the pod, is not the container's: its
// results are left out.
func TestForeignClaimMarker(t *testing.T) {
	e := newEnvWith(t, envOptions{})
	e.local.set(nriClaims())
	e.pool.set(nriPoolResults())
	noErr(t, e.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		doc.Preparations = append(doc.Preparations,
			state.Preparation{UID: "split", Driver: device.DriverName, ClaimName: "other/split", State: state.PreparationPrepared},
			state.Preparation{UID: "lease", Driver: device.DriverName, ClaimName: "ns/lease", State: state.PreparationPrepared})
		doc.LeaseClaims = append(doc.LeaseClaims, state.LeaseClaim{UID: "lease", Namespace: "ns", Name: "lease",
			Pods: []state.PodRef{{Namespace: "ns", Name: "pod-q", UID: "q"}}, State: state.LeaseClaimOpen})
		return nil
	}))
	refs := []resultRef{{"split", 0}, {"lease", 0}, {"pool", 0}}
	logs := klogtest.Capture(t, 3)
	if got := e.p.results(testPod("p"), "ns/pod-p/ctr", refs); len(got) != 1 || !reflect.DeepEqual(got[0].numaNodes, []int{3}) {
		t.Fatalf("results %+v of another pod's claims", got)
	}
	for _, want := range []string{"ignoring the marker of claim split: the claim other/split is not of namespace ns",
		"ignoring the marker of claim lease: pod ns/pod-p is not in the reservedFor of the claim ns/lease"} {
		if !strings.Contains(logs(), want) {
			t.Errorf("no %q in\n%s", want, logs())
		}
	}
	other := testPod("p")
	other.Namespace = "other"
	if got := e.p.results(other, "other/pod-p/ctr", refs[:1]); len(got) != 1 {
		t.Fatalf("results %+v of the pod's own claim", got)
	}
	if got := e.p.results(testPod("q"), "ns/pod-q/ctr", refs[1:2]); len(got) != 1 {
		t.Fatalf("results %+v of a pod in the reservedFor", got)
	}
}

func TestManage(t *testing.T) {
	auto := config.Container{Steering: &config.Steering{Policy: config.PolicyAuto}}
	fencingNone := config.Container{Fencing: config.FencingNone}
	for _, tc := range []struct {
		name       string
		defaults   config.Container
		refs       []resultRef
		management string // or the error
		claimed    string
		policy     string // steering policy of the plan
		text       string // steering of the log
		fencing    string // fencing of the log
	}{
		{name: "no results", management: "FENCED", text: "none (default)", fencing: "allowed-mems (default)"},
		{name: "numa claim, default none", refs: []resultRef{{"split", 0}}, management: "FENCED", claimed: "2", text: "none (default)"},
		{name: "numa claim, fencing none", defaults: fencingNone, refs: []resultRef{{"split", 0}}, management: "UNFENCED", claimed: "2",
			fencing: "none (node default)"},
		{name: "numa claim, auto", refs: []resultRef{{"auto", 0}}, management: "STEERED", claimed: "2", policy: "first-cxl",
			text: "auto from claim ns/auto request mem"},
		{name: "numa and dram, auto", refs: []resultRef{{"auto", 0}, {"split", 2}}, management: "STEERED", claimed: "2", policy: "start-interleaved"},
		{name: "dram, first-dram", refs: []resultRef{{"dram", 0}}, management: "STEERED", policy: "first-dram"},
		{name: "dram, default none", refs: []resultRef{{"split", 2}}, management: "FENCED"},
		{name: "node default auto", defaults: auto, refs: []resultRef{{"split", 0}, {"split", 3}}, management: "STEERED", claimed: "2,4",
			policy: "first-cxl", text: "auto (node default)"},
		{name: "node default auto, claim none", defaults: auto, refs: []resultRef{{"none", 0}}, management: "FENCED", claimed: "2",
			text: "none from claim ns/none request mem"},
		{name: "node default auto, no numa memory", defaults: auto, refs: []resultRef{{"split", 1}, {"lease", 0}}, management: "FENCED"},
		{name: "pool numa, default none", refs: []resultRef{{"pool", 0}}, management: "FENCED", claimed: "3"},
		{name: "pool numa, auto", refs: []resultRef{{"poolauto", 0}}, management: "STEERED", claimed: "3", policy: "first-cxl"},
		{name: "pool numa, node default auto", defaults: auto, refs: []resultRef{{"pool", 0}}, management: "STEERED", claimed: "3", policy: "first-cxl"},
		{name: "local and pool numa, auto", refs: []resultRef{{"auto", 0}, {"poolauto", 0}}, management: "STEERED", claimed: "2-3", policy: "first-cxl"},
		{name: "conflict", refs: []resultRef{{"auto", 0}, {"none", 0}},
			management: "container ns/pod/ctr: container.steering.policy: claim ns/auto request mem says auto, claim ns/none request mem says none"},
		{name: "conflict with node default", defaults: auto, refs: []resultRef{{"dram", 0}, {"none", 0}},
			management: "container ns/pod/ctr: container.steering.policy: claim ns/dram request mem says first-dram, claim ns/none request mem says none"},
	} {
		p := newDecider(t, tc.defaults)
		d, err := p.decide("ns/pod/ctr", p.results(testPod("p"), "ns/pod/ctr", tc.refs), true)
		if err != nil {
			if err.Error() != tc.management {
				t.Errorf("%s: %v, expected %s", tc.name, err, tc.management)
			}
			continue
		}
		if string(d.management) != tc.management || d.claimed.String() != tc.claimed {
			t.Errorf("%s: management %s claimed %q, expected %s %q", tc.name, d.management, d.claimed, tc.management, tc.claimed)
		}
		if (d.plan != nil) != (tc.policy != "") || (d.plan != nil && d.policy.String() != tc.policy) {
			t.Errorf("%s: plan %v policy %s, expected %q", tc.name, d.plan, d.policy, tc.policy)
		}
		if tc.text != "" && d.eff.PolicyText() != tc.text {
			t.Errorf("%s: steering %q, expected %q", tc.name, d.eff.PolicyText(), tc.text)
		}
		if tc.fencing != "" && d.eff.FencingText() != tc.fencing {
			t.Errorf("%s: fencing %q, expected %q", tc.name, d.eff.FencingText(), tc.fencing)
		}
	}

	// an invalid effective steering names the origins
	p := newDecider(t, config.Container{})
	results := []result{{settings: claimSettings("a", "r", config.Steering{MaxStep: "1Gi"})}}
	_, err := p.decide("ns/pod/ctr", results, true)
	wantErr(t, err, "container ns/pod/ctr: MemoryConfig: container.steering.maxStep needs a policy other than none (policy none (default), maxStep 1Gi from claim ns/a request r)")
	// no plan of a quota that the steering package refuses
	results = []result{{deviceType: device.DeviceTypeDRAM, mode: cxlapi.ModeNUMA, numaNodes: []int{0},
		settings: claimSettings("a", "r", config.Steering{Policy: config.PolicyFirstDRAM, MinStep: "1Gi"})}}
	_, err = p.decide("ns/pod/ctr", results, true)
	wantErr(t, err, "NRI CreateContainer: ctr=ns/pod/ctr: failed to build memory steering plan")
	if d, err := p.decide("ns/pod/ctr", results, false); err != nil || d.management != ManagementSteered || d.plan != nil {
		t.Fatalf("without a plan: %+v %v", d, err)
	}
}

// TestCreateContainerManagement creates containers that use results of
// claims through NRI: the management, the plan and the refusal of
// conflicting settings.
func TestCreateContainerManagement(t *testing.T) {
	e := newFenceEnv(t, "0-1", 2)
	e.local.set(nriClaims())
	e.pool.set(nriPoolResults())
	pod := testPod("u1")
	management := func(id string) Management { return e.record(id).Management }

	// a numa claim is not steered by default: fenced with its node, no plan
	ctr := &api.Container{Id: "c1", PodSandboxId: pod.GetId(), Name: "a", Env: []string{cxlapi.ResultMarker("split", 0) + "=1"}}
	if got := e.createMems(pod, ctr); got != "0-2" || management("c1") != ManagementFenced || e.record("c1").Plan != nil {
		t.Fatalf("adjustment %s, management %s", got, management("c1"))
	}
	// no results, and the dax result of the same claim: no node
	if got := e.createMems(pod, withClaims(testCtr(pod, "c2", "b", 0))); got != "0-1" {
		t.Fatalf("adjustment %s of a container without results", got)
	}
	ctr = &api.Container{Id: "c3", PodSandboxId: pod.GetId(), Name: "c", Env: []string{cxlapi.ResultMarker("split", 1) + "=1"}}
	if got := e.createMems(pod, ctr); got != "0-1" || management("c3") != ManagementFenced {
		t.Fatalf("adjustment %s of the dax result", got)
	}
	// opted in: steered, a plan, no adjustment
	if got := e.createMems(pod, withClaims(testCtr(pod, "c4", "d", 0), "auto")); got != "-" || management("c4") != ManagementSteered ||
		e.record("c4").Plan == nil {
		t.Fatalf("adjustment %s, management %s of an opted-in container", got, management("c4"))
	}
	// pool numa memory is steered on opt-in
	if got := e.createMems(pod, withClaims(testCtr(pod, "c5", "e", 0), "poolauto")); got != "-" || management("c5") != ManagementSteered {
		t.Fatalf("adjustment %s, management %s of an opted-in pool container", got, management("c5"))
	}
	// conflicting policies: not created; the record stays with the defaults
	_, _, err := e.p.CreateContainer(context.Background(), pod, withClaims(testCtr(pod, "c6", "f", 0), "auto", "none"))
	wantErr(t, err, "container ns/pod-u1/f: container.steering.policy: claim ns/auto request mem says auto, claim ns/none request mem says none")
	if c := e.record("c6"); c.Plan != nil || c.Management != ManagementFenced || c.Lifecycle != LifecycleCreated {
		t.Fatalf("record of a refused container %+v", c)
	}
	e.expectUpdates()
}

// TestCharCreateContainerLogLines pins the error line of a CreateContainer
// whose results conflict and the lines of a container whose DRAM and
// region memory is not steered.
func TestCharCreateContainerLogLines(t *testing.T) {
	e := newFenceEnv(t, "0-1", 2)
	e.local.set(nriClaims())
	e.pool.set(nriPoolResults())
	logs := klogtest.Capture(t, 3)
	pod := testPod("u1")

	_, _, err := e.p.CreateContainer(context.Background(), pod, withClaims(testCtr(pod, "c6", "f", 0), "auto", "none"))
	const conflict = "NRI CreateContainer: container ns/pod-u1/f: container.steering.policy: claim ns/auto request mem says auto, claim ns/none request mem says none"
	if err == nil || !strings.Contains(logs(), conflict) {
		t.Fatalf("error %v, no line %q:\n%s", err, conflict, logs())
	}

	// DRAM and a region in mode numa with the default steering none: the
	// claims reserve capacity only (with policy auto the DRAM is steered)
	ctr := &api.Container{Id: "c7", PodSandboxId: pod.GetId(), Name: "g",
		Env: []string{cxlapi.ResultMarker("split", 2) + "=1", cxlapi.ResultMarker("split", 3) + "=1"}}
	if got := e.createMems(pod, ctr); got != "0-1,4" {
		t.Fatalf("adjustment %s", got)
	}
	for _, line := range []string{
		"NRI CreateContainer: ctr=ns/pod-u1/g: memory fenced (steering none (default), fencing allowed-mems (default))",
		"NRI CreateContainer: ctr=ns/pod-u1/g: dram and region memory is not steered: the claims reserve capacity only",
		"NRI CreateContainer: ctr=ns/pod-u1/g: cpuset.mems=0-1,4 (fenced NUMA nodes 2)",
	} {
		if !strings.Contains(logs(), line) {
			t.Errorf("no line %q:\n%s", line, logs())
		}
	}

	// the lines that the e2e tests and the documentation quote
	auto := claimSettings("a", "r", config.Steering{Policy: config.PolicyAuto})
	e.local.set(claimTable{"a": {ClaimName: "ns/a", Devices: []state.DeviceInfo{
		{RequestName: "r", DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeNUMA, NUMANodes: []int{2}, ConsumedBytes: 1 << 30, ResultSettings: auto},
	}}})
	plain := &api.PodSandbox{Id: "sb-plain", Uid: "uplain", Namespace: "ns", Name: "plain"}
	e.createMems(plain, withClaims(testCtr(plain, "c10", "main", 0), "x"))
	e.createMems(pod, withClaims(testCtr(pod, "c11", "steered", 0), "a"))
	none := newEnvWith(t, envOptions{fencing: config.FencingNone})
	none.createMems(pod, testCtr(pod, "c12", "none", 0))
	out := logs()
	for _, want := range []string{
		"NRI CreateContainer: ctr=ns/plain/main: memory fenced (steering none (default), fencing allowed-mems (default))",
		"NRI CreateContainer: ctr=ns/pod-u1/steered: memory steered (steering auto from claim ns/a request r, fencing allowed-mems (default))",
		"NRI CreateContainer: ctr=ns/pod-u1/none: memory none (steering none (default), fencing none (node default))",
		"NRI CreateContainer: container ns/pod-u1/f: container.steering.policy: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no line %q:\n%s", want, out)
		}
	}
	mems := regexp.MustCompile(`NRI CreateContainer: ctr=ns/plain/main: cpuset\.mems=[0-9,-]* `)
	if line := lineWith(out, "ctr=ns/plain/main: cpuset.mems="); !mems.MatchString(line) {
		t.Errorf("line %q does not match %q", line, mems)
	}

	// a numa claim of steering auto without memory to steer
	e.local.set(fenceClaims())
	if got := e.createMems(pod, withClaims(testCtr(pod, "c8", "h", 0), "dax")); got != "0-1" {
		t.Fatalf("adjustment %s", got)
	}
	n := newEnvWith(t, envOptions{defaults: config.Container{Steering: &config.Steering{Policy: config.PolicyAuto}}})
	n.local.set(fenceClaims())
	n.createMems(pod, withClaims(testCtr(pod, "c9", "i", 0), "dax"))
	if line := lineWith(logs(), "ctr=ns/pod-u1/i: steering policy auto"); !strings.HasSuffix(line,
		"NRI CreateContainer: ctr=ns/pod-u1/i: steering policy auto: no numa memory to steer") {
		t.Errorf("line %q:\n%s", line, logs())
	}
}

func TestFormatBytes(t *testing.T) {
	for in, want := range map[uint64]string{
		0:               "0B",
		1023:            "1023B",
		1536:            "1.5KiB",
		128 << 20:       "128.0MiB",
		(5 << 30) / 2:   "2.5GiB",
		uint64(1) << 40: "1024.0GiB",
	} {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d) = %q, expected %q", in, got, want)
		}
	}
}
