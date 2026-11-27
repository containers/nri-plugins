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
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/steering"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// Container is the plugin's record of a container of a pod on the node:
// how the plugin manages its memory, the cpuset.mems of its spec as the
// plugin knows it, the NUMA nodes of its allocation results and the nodes
// lent to it.
//
// Identity: ID, the container ID. A restart of the container of a pod is a
// new ID of the same (PodUID, Name), which a lease is bound to; the newest
// record of the pair is its container.
//
// States: Lifecycle (LifecycleTable): UNSEEN, created while the plugin was
// not connected and found by Synchronize, its CreateContainer adjustment was
// never applied; CREATED, its CreateContainer seen, the spec holds
// cpuset.mems, the cgroup may not exist; TASK_CREATED, StartContainer seen,
// the runtime made the cgroup from the spec and an update reaches the spec
// only; STARTED, the runtime marked it started (PostStartContainer, or
// running at Synchronize): updates reach the cgroup; GONE, stopped or
// removed, the record leaves the table. Management (ManagementTable),
// decided once when the record is made: STEERED, a steering manager of this
// process writes its cpuset.mems and memory.high (or will, with its Plan; or
// its settings do not combine any more), never fenced and never lent to;
// FENCED, the plugin owns its cpuset.mems through NRI: wantMems; UNFENCED
// (fencing none): its cpuset.mems is left as the runtime made it, a lend
// changes only one that has a cpuset.mems of its own.
//
// Transitions: LifecycleTable and ManagementTable below; a transition not in
// them is refused. A lend or a revoke changes no state; its rules: a lend to
// a STEERED container is refused (ErrSteered); a lend to a container that
// has not started (UNSEEN, CREATED) is done when the container was lent the
// node at its creation (the READY numa leases of its pod and name, seeded),
// an error otherwise; a revoke of a seeded node drops its lease, and of a
// FENCED container takes the node off its spec; a lend or a revoke of a
// started container (TASK_CREATED, STARTED) recomputes wantMems and sends
// one UpdateContainers outside the table mutex when its cpuset.mems
// changes.
//
// Invariants: a FENCED container's cpuset.mems is wantMems or an update is
// pending (I11); Claimed are the NUMA nodes of the CXL numa memory of the
// results that it uses; a lent node has a lease; Plan only while STEERED
// and not started; manager only while STEERED.
//
// Persistence: none, the table is in memory; Synchronize builds it again
// after a restart, the leases of the store seed the lends, and a Steered
// record of the store names a container whose manager was of a previous
// process.
//
// Locks and waits: the plugin's table mutex guards the record; the update
// token is held from the computation of wantMems to the record of the
// update's result.
type Container struct {
	ID, PodUID, PodNamespace, PodName, Name string
	Lifecycle                               Lifecycle
	Management                              Management
	// Pid is the init pid at its start, 0 before.
	Pid uint32
	// CgroupDir is the container's own cgroup v2 directory, "" until its
	// start resolves it.
	CgroupDir string
	// SpecMems is the cpuset.mems of its spec as the plugin knows it.
	SpecMems string
	// Base are its own memory nodes without the nodes lent to any
	// container.
	Base cpuset.CPUSet
	// Claimed are the NUMA nodes of the CXL numa memory of its results;
	// OwnClaimed those of them that its cpuset.mems had before any lend.
	Claimed, OwnClaimed cpuset.CPUSet
	// Lends are the nodes lent to it with their leases.
	Lends lentNUMANodes
	// Plan is the steering plan of a steered container until its start.
	Plan *steering.Plan
	// Steering is its effective steering policy with its origin, as the
	// log lines give it: "auto from claim ns/a request r".
	Steering string

	// unsteered is its management when its steering does not start.
	unsteered Management
	// manager steers its memory from its start until it stops.
	manager *steering.Manager
	// starting is the manager whose Start runs, and startEnd the reason
	// why its steering ended before startSteering made it the manager.
	starting *steering.Manager
	startEnd error
	// seq orders the records by their creation.
	seq uint64
}

// Lifecycle is the phase of a container in the runtime.
type Lifecycle string

// The lifecycle of a container.
const (
	// LifecycleNone is a container that the plugin has no record of.
	LifecycleNone        Lifecycle = "NONE"
	LifecycleUnseen      Lifecycle = "UNSEEN"
	LifecycleCreated     Lifecycle = "CREATED"
	LifecycleTaskCreated Lifecycle = "TASK_CREATED"
	LifecycleStarted     Lifecycle = "STARTED"
	LifecycleGone        Lifecycle = "GONE"
)

// Management is how the plugin manages the memory of a container.
type Management string

// The management of the memory of a container.
const (
	// ManagementUndecided is a record that is being made.
	ManagementUndecided Management = "UNDECIDED"
	ManagementSteered   Management = "STEERED"
	ManagementFenced    Management = "FENCED"
	ManagementUnfenced  Management = "UNFENCED"
)

// Trigger names the NRI event that causes a transition.
type Trigger = fsm.Trigger

const (
	// TriggerCreate is the NRI CreateContainer of a container.
	TriggerCreate Trigger = "NRI CreateContainer"
	// TriggerStart is the NRI StartContainer of a container: its task is
	// created.
	TriggerStart Trigger = "NRI StartContainer"
	// TriggerPostStart is the NRI PostStartContainer of a container: the
	// runtime marked it started.
	TriggerPostStart Trigger = "NRI PostStartContainer"
	// TriggerStop is the NRI StopContainer or RemoveContainer of a
	// container.
	TriggerStop Trigger = "NRI StopContainer"
	// TriggerSynchronize is the NRI Synchronize of the plugin: the
	// containers that the runtime has.
	TriggerSynchronize Trigger = "NRI Synchronize"
	// TriggerSteeringEnded is the end of the steering of a running
	// container whose process refuses a memory policy.
	TriggerSteeringEnded Trigger = "steering ended"
)

// Triggers are the triggers of the tables, in the order of the
// documentation.
var Triggers = []Trigger{TriggerCreate, TriggerStart, TriggerPostStart, TriggerStop, TriggerSynchronize,
	TriggerSteeringEnded, TriggerLendsRestored, TriggerNRIStart, TriggerNRIClose, TriggerStopping}

// LifecycleTable is the transition table of Container.Lifecycle.
var LifecycleTable = fsm.Table[Lifecycle]{
	Object: "Container",
	Field:  "Lifecycle",
	States: fsm.List(LifecycleNone, LifecycleUnseen, LifecycleCreated, LifecycleTaskCreated, LifecycleStarted,
		LifecycleGone),
	Rows: []fsm.Transition[Lifecycle]{
		{
			From: fsm.List(LifecycleNone), To: LifecycleCreated, Triggers: fsm.List(TriggerCreate),
			When: "the runtime creates the container",
			Effects: "the management decided; fenced: an adjustment of cpuset.mems iff wantMems differs from " +
				"the spec; the nodes of its READY numa leases recorded as lent (seeded); also when the " +
				"settings of its results do not combine (the creation fails, the record stays with the " +
				"management of the defaults until the container stops)",
		},
		{
			From: fsm.List(LifecycleNone), To: LifecycleUnseen, Triggers: fsm.List(TriggerSynchronize),
			When:  "Synchronize lists a created, running or paused container that the plugin has no record of",
			Guard: "the first Synchronize of the process",
			Effects: "the management decided from its results; not steered by this process; a manager of a " +
				"previous process (its Steered record) released (memory.high max); seeded; no adjustment " +
				"possible",
		},
		{
			From: fsm.List(LifecycleUnseen), To: LifecycleStarted, Triggers: fsm.List(TriggerSynchronize),
			When:    "Synchronize lists the container running or paused",
			Effects: "its pid, cgroup and own nodes read; its claimed nodes in its cpuset.mems are its own",
		},
		{
			From: fsm.List(LifecycleNone, LifecycleUnseen, LifecycleCreated), To: LifecycleTaskCreated,
			Triggers: fsm.List(TriggerStart),
			When:     "the runtime has created the task of the container (a record is made for a container that the plugin has none of, with the management of the defaults)",
			Effects: "its pid, cgroup and own nodes read; steered: the Steered record committed (d, bounded) " +
				"before the manager starts; a manager that does not start makes it fenced or none; updates " +
				"from here reach its spec only",
		},
		{
			From: fsm.List(LifecycleUnseen, LifecycleCreated, LifecycleTaskCreated), To: LifecycleStarted,
			Triggers: fsm.List(TriggerPostStart),
			When:     "the runtime has marked the container started",
			Effects: "its pid, cgroup and own nodes read if StartContainer was not seen; the OnContainerStarted " +
				"callbacks (the broker lends the nodes of its READY numa leases again); fenced: the repair " +
				"pass of the one container (wantMems against its cgroup) once fencing is ready",
		},
		{
			From: fsm.List(LifecycleStarted), Stays: true, Triggers: fsm.List(TriggerPostStart),
			When: "Synchronize found the container running before its PostStartContainer",
			Effects: "the OnContainerStarted callbacks; fenced: the repair pass of the one container once " +
				"fencing is ready",
		},
		{
			From: fsm.List(LifecycleUnseen, LifecycleCreated, LifecycleTaskCreated, LifecycleStarted), To: LifecycleGone,
			Triggers: fsm.List(TriggerStop),
			When:     "the runtime stops or removes the container",
			Effects: "the record leaves the table; the OnContainerGone callbacks (the broker keeps its leases " +
				"while the pod lives or a newer instance exists); its manager stopped (memory.high max), then " +
				"its Steered record removed (d, bounded)",
		},
	},
	Refused: []fsm.Refusal[Lifecycle]{
		{From: fsm.List(LifecycleGone), Why: "a gone container is out of the table; its restart is a new record"},
		{From: fsm.List(LifecycleStarted), To: fsm.List(LifecycleNone, LifecycleUnseen, LifecycleCreated, LifecycleTaskCreated),
			Why: "a started container never goes back"},
	},
}

// ManagementTable is the transition table of Container.Management.
var ManagementTable = fsm.Table[Management]{
	Object: "Container",
	Field:  "Management",
	States: fsm.List(ManagementUndecided, ManagementSteered, ManagementFenced, ManagementUnfenced),
	Rows: []fsm.Transition[Management]{
		{
			From: fsm.List(ManagementUndecided), To: ManagementSteered, Triggers: fsm.List(TriggerCreate),
			When:  "the effective settings of its results steer and there is DRAM or CXL numa memory to steer",
			Guard: "the settings of the results combine and a plan is built (else the creation fails)",
			Effects: "the plan kept until its start; no adjustment: it has every node until its manager " +
				"writes its cgroup",
		},
		{
			From: fsm.List(ManagementUndecided), To: ManagementSteered, Triggers: fsm.List(TriggerSynchronize),
			When:    "the settings of the results of a container that Synchronize finds do not combine",
			Effects: "taken as steered: neither fenced nor lent to",
		},
		{
			From: fsm.List(ManagementUndecided), To: ManagementFenced,
			Triggers: fsm.List(TriggerCreate, TriggerSynchronize, TriggerStart),
			When: "not steered (by this process) and fencing allowed-mems; the defaults for a container " +
				"whose creation failed or that the plugin has no record of",
			Effects: "wantMems: its own nodes without the fenced nodes, its claimed and its lent nodes",
		},
		{
			From: fsm.List(ManagementUndecided), To: ManagementUnfenced,
			Triggers: fsm.List(TriggerCreate, TriggerSynchronize, TriggerStart),
			When:     "not steered (by this process) and fencing none",
			Effects:  "its cpuset.mems is left as the runtime made it",
		},
		{
			From: fsm.List(ManagementSteered), To: ManagementFenced, Triggers: fsm.List(TriggerStart),
			When: "its manager does not start (no cgroup, no own nodes, steering.New or Start fails: memory.high " +
				"not max, no memory policy set) and fencing allowed-mems",
			Effects: "its Steered record removed; fenced by the repair pass at its PostStartContainer",
		},
		{
			From: fsm.List(ManagementSteered), To: ManagementUnfenced, Triggers: fsm.List(TriggerStart),
			When:    "its manager does not start and fencing none",
			Effects: "its Steered record removed",
		},
		{
			From: fsm.List(ManagementSteered), To: ManagementFenced, Triggers: fsm.List(TriggerSteeringEnded),
			When: "a process of the running container refuses a memory policy of a later step (EPERM) and " +
				"fencing allowed-mems",
			Effects: "its manager set memory.high to max and stopped; its Steered record removed; fenced " +
				"by the repair pass of the one container",
		},
		{
			From: fsm.List(ManagementSteered), To: ManagementUnfenced, Triggers: fsm.List(TriggerSteeringEnded),
			When: "a process of the running container refuses a memory policy of a later step and fencing none",
			Effects: "its manager set memory.high to max and stopped; its cpuset.mems back as before the " +
				"steering; its Steered record removed",
		},
	},
	Refused: []fsm.Refusal[Management]{
		{From: fsm.List(ManagementFenced, ManagementUnfenced), To: fsm.List(ManagementSteered),
			Why: "a container is steered only by a plan of its creation; a re-created container is a new record"},
	},
}

// lentNUMANodes maps the nodes lent to a container to their leases (the lease
// ids): devices under one CXL host bridge share a NUMA node, so two leases
// of a container may hold one node, which stays lent until its last lease
// revokes it.
type lentNUMANodes map[int]map[string]bool

// nodes returns the lent nodes that have a lease.
func (l lentNUMANodes) nodes() cpuset.CPUSet {
	nodes := make([]int, 0, len(l))
	for n, leases := range l {
		if len(leases) > 0 {
			nodes = append(nodes, n)
		}
	}
	return cpuset.New(nodes...)
}

// with returns the leases of node after lease lends (lend) or revokes
// (!lend) it.
func (l lentNUMANodes) with(node int, lease string, lend bool) map[string]bool {
	leases := make(map[string]bool, len(l[node])+1)
	for h := range l[node] {
		leases[h] = true
	}
	if lend {
		leases[lease] = true
	} else {
		delete(leases, lease)
	}
	return leases
}

// after returns the lent nodes that have a lease when node has leases.
func (l lentNUMANodes) after(node int, leases map[string]bool) cpuset.CPUSet {
	if len(leases) > 0 {
		return l.nodes().Union(cpuset.New(node))
	}
	return l.nodes().Difference(cpuset.New(node))
}

// set returns the lends with the leases of node.
func (l lentNUMANodes) set(node int, leases map[string]bool) lentNUMANodes {
	if l == nil {
		l = lentNUMANodes{}
	}
	if len(leases) == 0 {
		delete(l, node)
	} else {
		l[node] = leases
	}
	return l
}

// name returns the container as namespace/pod/container.
func (c *Container) name() string {
	return c.PodNamespace + "/" + c.PodName + "/" + c.Name
}

// pod returns the pod of the container.
func (c *Container) pod() state.PodRef {
	return state.PodRef{Namespace: c.PodNamespace, Name: c.PodName, UID: c.PodUID}
}

// started tells whether the task of the container is created: its cgroup
// exists, and its own nodes are read from it.
func (c *Container) started() bool {
	return c.Lifecycle == LifecycleTaskCreated || c.Lifecycle == LifecycleStarted
}

// steered tells whether a steering manager steers the memory of the
// container or will at its start.
func (c *Container) steered() bool {
	return c.Management == ManagementSteered
}
