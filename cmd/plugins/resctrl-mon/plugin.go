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

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/intel/goresctrl/pkg/monitor"
)

const (
	// reconcileInterval is how often the background reconciler checks for
	// orphaned mon_groups left behind by failed StopContainer removals.
	reconcileInterval = 30 * time.Second

	// telemetryShutdownTimeout bounds how long onClose waits for the telemetry
	// stack (MeterProvider flush + HTTP server) to drain before exiting.
	telemetryShutdownTimeout = 5 * time.Second
)

// plugin implements the NRI plugin interface for resctrl monitoring groups.
type plugin struct {
	stub stub.Stub

	// stateMu guards the reconfigurable lifecycle state below (config, mgr,
	// telemetry, metrics, stopReconciler). The NRI stub dispatches Configure,
	// Synchronize, and the container handlers without a shared lock, so a
	// dynamic config reload could otherwise race a concurrent callback while it
	// swaps these fields. Handlers snapshot them under RLock; setConfig,
	// Configure, and onClose mutate them under Lock. It is never held together
	// with mu: handlers release it before touching the maps, so there is no
	// lock-ordering hazard between the two.
	stateMu        sync.RWMutex
	config         *pluginConfig
	mgr            *monitor.Manager
	stopReconciler chan struct{} // closed to stop the background reconciler
	telemetry      *telemetryState
	metrics        *monitor.Registration

	mu             sync.Mutex          // guards pendingRemoval, liveKeys, and syncRemovals
	pendingRemoval map[string]struct{} // keys whose Remove failed, retried by the reconciler
	liveKeys       map[string]struct{} // monitored pod sandboxes alive as of the last Synchronize
	syncRemovals   map[string]struct{} // non-nil during a Synchronize pass: keys a concurrent RemovePodSandbox tombstoned so the snapshot cannot resurrect them
}

// pluginConfig holds the runtime configuration for the plugin.
type pluginConfig struct {
	// ResctrlPath is the mount point of the resctrl filesystem.
	ResctrlPath string `json:"resctrlPath"`

	// Namespaces filters mon_group creation to pods in these namespaces.
	// Empty list means all namespaces.
	Namespaces []string `json:"namespaces"`

	// LabelSelector filters mon_group creation to pods matching these labels.
	// Empty map means all pods.
	LabelSelector map[string]string `json:"labelSelector"`

	// Telemetry configures the embedded OTel exporter (Prometheus + OTLP).
	Telemetry telemetryConfig `json:"telemetry"`
}

const defaultResctrlPath = "/sys/fs/resctrl"

func newPlugin() *plugin {
	cfg := &pluginConfig{
		ResctrlPath: defaultResctrlPath,
		Telemetry:   defaultTelemetryConfig(),
	}
	mgr, err := monitor.New(monitor.Options{
		ResctrlRoot:      cfg.ResctrlPath,
		KeyValidator:     monitor.PodUIDValidator,
		KeyCanonicalizer: monitor.CanonicalizePodUID,
	})
	if err != nil {
		log.Fatalf("failed to create monitor manager: %v", err)
	}
	return &plugin{
		config: cfg,
		mgr:    mgr,
	}
}

// Configure handles connecting to container runtime's NRI server.
func (p *plugin) Configure(ctx context.Context, config, runtime, version string) (stub.EventMask, error) {
	log.Infof("Connected to %s %s...", runtime, version)
	if err := checkRuntimeVersion(runtime, version); err != nil {
		return 0, err
	}
	if config != "" {
		log.Debugf("loading configuration from NRI server")
		if err := p.setConfig([]byte(config)); err != nil {
			return 0, err
		}
	}
	// Start telemetry now that configuration (from a --config file and/or the
	// NRI server) is finalized. Binding here rather than in main() lets a
	// runtime-provided config disable Prometheus or pick a different port
	// before we bind, instead of fatally exiting on a pre-config port clash.
	//
	// Hold stateMu so the startup cannot race a concurrent reload or container
	// handler; startTelemetry accesses the guarded fields directly and must run
	// with the lock held.
	p.stateMu.Lock()
	var terr error
	if p.telemetry == nil {
		terr = p.startTelemetry(ctx)
	}
	p.stateMu.Unlock()
	if terr != nil {
		return 0, terr
	}
	return 0, nil
}

// onClose handles losing connection to container runtime.
func (p *plugin) onClose() {
	p.stateMu.Lock()
	if p.stopReconciler != nil {
		close(p.stopReconciler)
		p.stopReconciler = nil
	}
	if p.metrics != nil {
		_ = p.metrics.Unregister()
		p.metrics = nil
	}
	if p.telemetry != nil {
		ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		p.telemetry.shutdown(ctx)
		cancel()
		p.telemetry = nil
	}
	p.stateMu.Unlock()
	log.Infof("Connection to the runtime lost, exiting...")
	os.Exit(0)
}

// setConfig applies new plugin configuration.
func (p *plugin) setConfig(data []byte) error {
	log.Tracef("setConfig: parsing\n---8<---\n%s\n--->8---", data)
	cfg := pluginConfig{
		ResctrlPath: defaultResctrlPath,
		Telemetry:   defaultTelemetryConfig(),
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("setConfig: cannot parse configuration: %w", err)
	}
	resctrlPath := filepath.Clean(cfg.ResctrlPath)
	if resctrlPath == "" || !filepath.IsAbs(resctrlPath) {
		return fmt.Errorf("setConfig: resctrlPath must be an absolute path, got %q", cfg.ResctrlPath)
	}
	cfg.ResctrlPath = resctrlPath
	if err := validateTelemetryConfig(&cfg.Telemetry); err != nil {
		return fmt.Errorf("setConfig: %w", err)
	}

	// The resctrl root cannot be changed once the plugin is running: swapping
	// the manager would drop the in-memory tracking (and assigned PIDs) for
	// every live pod with no re-synchronization until the next lifecycle event,
	// and a pending removal bound to the old manager could later delete a
	// same-UID group created in the new one. The initial configuration is
	// applied (from a --config file and/or the NRI server) before telemetry and
	// the reconciler start, so it may still select a non-default root and
	// rebuild the manager; only reject the change once the plugin is running.
	//
	// Serialize the whole reconfiguration against the NRI lifecycle callbacks
	// (Configure/Synchronize/container handlers are dispatched without a shared
	// lock) so swapping config/mgr/telemetry here cannot race a concurrent
	// shouldMonitorPod, EnsureGroup, or RemovePodSandbox. startTelemetry and
	// startReconcilerLocked below run with this lock held and access the guarded
	// fields directly.
	p.stateMu.Lock()
	defer p.stateMu.Unlock()

	running := p.telemetry != nil || p.stopReconciler != nil
	if running && cfg.ResctrlPath != p.config.ResctrlPath {
		return fmt.Errorf("setConfig: resctrlPath cannot be changed on a running plugin (have %q, got %q); restart the plugin to change it",
			p.config.ResctrlPath, cfg.ResctrlPath)
	}

	// Create the monitor manager on the initial configuration only (the root is
	// immutable thereafter, per the guard above). The manager holds the
	// in-memory tracking for every running pod. Build it before mutating state so
	// a failure here leaves the running configuration fully intact.
	rootChanged := p.config == nil || cfg.ResctrlPath != p.config.ResctrlPath
	var newMgr *monitor.Manager
	if rootChanged {
		var err error
		newMgr, err = monitor.New(monitor.Options{
			ResctrlRoot:      cfg.ResctrlPath,
			KeyValidator:     monitor.PodUIDValidator,
			KeyCanonicalizer: monitor.CanonicalizePodUID,
		})
		if err != nil {
			return fmt.Errorf("setConfig: failed to create monitor manager: %w", err)
		}
	}

	// Remember the currently-applied config/manager so a failed reload can be
	// rolled back to the last working state instead of leaving telemetry and
	// the reconciler permanently disabled.
	prevConfig := p.config
	prevMgr := p.mgr

	p.config = &cfg

	// If telemetry is already running, tear it down so it can be rebound to the
	// (possibly new) manager with the new telemetry settings below.
	restartTelemetry := p.telemetry != nil
	if restartTelemetry {
		if p.metrics != nil {
			_ = p.metrics.Unregister()
			p.metrics = nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		p.telemetry.shutdown(ctx)
		cancel()
		p.telemetry = nil
	}

	// Swap the manager only on a root change. The background reconciler pins the
	// manager it was started with, so stop it here and restart it below against
	// the new manager; on an unchanged root it keeps running untouched.
	reconcilerWasRunning := p.stopReconciler != nil
	if rootChanged {
		if p.stopReconciler != nil {
			close(p.stopReconciler)
			p.stopReconciler = nil
		}
		p.mgr = newMgr
	}

	if restartTelemetry {
		if err := p.startTelemetry(context.Background()); err != nil {
			// Roll back to the previously working configuration: the old
			// telemetry/manager have already been torn down, so restore the
			// prior config/manager and bring telemetry (and, if we swapped the
			// manager, the reconciler) back up. This keeps a failed reload
			// (e.g. the new Prometheus port is occupied) from permanently
			// disabling telemetry and reconciliation.
			p.config = prevConfig
			if rootChanged {
				p.mgr = prevMgr
			}
			if rerr := p.startTelemetry(context.Background()); rerr != nil {
				log.Errorf("setConfig: failed to restore telemetry after failed reload: %v", rerr)
			}
			if rootChanged && reconcilerWasRunning {
				p.startReconcilerLocked()
			}
			return fmt.Errorf("setConfig: restart telemetry: %w", err)
		}
	}

	if rootChanged && reconcilerWasRunning {
		p.startReconcilerLocked()
	}

	log.Debugf("configuration: resctrlPath=%s namespaces=%v labelSelector=%v",
		cfg.ResctrlPath, cfg.Namespaces, cfg.LabelSelector)
	return nil
}

// Synchronize is called at plugin startup with the current set of pods and containers.
// It reconciles in-memory state with what exists on the resctrl filesystem.
func (p *plugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	log.Infof("synchronizing state: %d pods, %d containers", len(pods), len(containers))

	// Open a removal-tombstone window before reading the pod snapshot so a
	// RemovePodSandbox racing this pass is remembered and not resurrected by the
	// wholesale setLiveKeys replacement below.
	p.beginSync()

	// Snapshot the manager under the lifecycle lock so a concurrent reload does
	// not swap it mid-synchronization; operate on the snapshot for the rest of
	// this call.
	mgr := p.getManager()

	// Build a lookup from sandbox ID to pod (containers reference
	// pods by sandbox ID, not by Kubernetes UID).
	podBySandboxID := make(map[string]*api.PodSandbox, len(pods))
	for _, pod := range pods {
		podBySandboxID[pod.GetId()] = pod
	}

	// A pod sandbox can be alive with no running container (for example between
	// container restarts). Seed the reconcile live set from the monitored
	// sandboxes so their existing mon_groups survive; the container loop below
	// then creates missing groups and (re)assigns PIDs. Remember this set so the
	// background reconciler keeps protecting container-less sandboxes (which are
	// never passed to EnsureGroup and so never appear in mgr.List()).
	liveKeys := make([]string, 0, len(pods))
	for _, pod := range pods {
		if p.shouldMonitorPod(pod) {
			liveKeys = append(liveKeys, pod.GetUid())
		}
	}
	p.setLiveKeys(liveKeys)
	for _, ctr := range containers {
		pod, ok := podBySandboxID[ctr.GetPodSandboxId()]
		if !ok {
			log.Debugf("Synchronize: container %s has no matching pod, skipping", ctr.GetName())
			continue
		}
		if !p.shouldMonitorPod(pod) {
			continue
		}
		podUID := pod.GetUid()
		rdtClass := getRDTClass(ctr)

		// Assigning a PID to a mon_group writes it into the group's tasks file,
		// which moves the task into the group's parent ctrl_group and rewrites
		// its CLOSID. It must therefore never run when this container's RDT
		// class differs from the class the pod's mon_group was created under, or
		// it would silently overwrite the container's own CAT/MBA allocation
		// (e.g. an off-class sidecar). EnsureGroup reports exactly that mismatch
		// as an error, so on any error skip the container instead of assigning.
		grp, err := mgr.EnsureGroup(podUID, rdtClass)
		if err != nil {
			log.Warnf("Synchronize: not monitoring a container of pod %s: %v", podUID, err)
			continue
		}

		pid := int(ctr.GetPid())
		if pid > 0 {
			if err := mgr.AssignPID(podUID, pid); err != nil {
				log.Warnf("Synchronize: failed to write PID %d for pod %s: %v", pid, podUID, err)
			} else {
				log.Debugf("Synchronize: assigned pid %d for pod %s in %s", pid, podUID, grp.Path())
			}
		}
	}

	// Remove orphaned mon_groups from a previous plugin instance.
	if err := mgr.Reconcile(liveKeys); err != nil {
		log.Warnf("Synchronize: reconcile failed: %v", err)
	}

	// Start the background reconciler to periodically clean up orphaned
	// mon_groups that could not be removed during StopContainer.
	p.startReconciler()

	log.Infof("synchronization complete: tracking %d pods", len(mgr.List()))
	return nil, nil
}

// startReconciler launches a background goroutine that periodically retries
// failed removals and removes orphaned mon_group directories. This handles the
// case where removeMonGroup fails in RemovePodSandbox (e.g., kernel busy) and
// the directory lingers.
func (p *plugin) startReconciler() {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.startReconcilerLocked()
}

// startReconcilerLocked is the body of startReconciler for callers that already
// hold stateMu (setConfig). It mutates stopReconciler and reads mgr, both of
// which are guarded by stateMu.
func (p *plugin) startReconcilerLocked() {
	if p.stopReconciler != nil {
		// Already running from a previous Synchronize call.
		return
	}
	// Capture the channel and manager this goroutine owns. A later config reload
	// may close p.stopReconciler, swap p.mgr, and start a fresh goroutine; binding
	// to these locals keeps this goroutine's select on an immutable channel and
	// pinned to the manager it was started with.
	stop := make(chan struct{})
	p.stopReconciler = stop
	mgr := p.mgr
	go func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				p.reconcile(mgr)
			}
		}
	}()
	log.Debugf("background reconciler started (interval=%s)", reconcileInterval)
}

// reconcile retries removals that previously failed and reaps untracked orphan
// directories. A failed Remove leaves its key tracked, so Reconcile(List())
// would treat it as live forever; retrying Remove is what actually frees the
// RMID once the kernel releases the directory.
func (p *plugin) reconcile(mgr *monitor.Manager) {
	for _, key := range p.pendingRemovalKeys() {
		switch err := mgr.Remove(key); {
		case err == nil, errors.Is(err, monitor.ErrNotTracked):
			p.clearPendingRemoval(key)
		default:
			log.Warnf("reconciler: retry remove %s failed: %v", key, err)
		}
	}
	// Reconcile against the tracked keys plus the live sandbox set: a
	// container-less sandbox is protected only by liveKeys (it is never passed
	// to EnsureGroup, so mgr.List() omits it), and reconciling without it would
	// reap its existing mon_group and hand its replacement container a fresh
	// RMID.
	if err := mgr.Reconcile(p.reconcileLiveSet(mgr)); err != nil {
		log.Warnf("reconciler: %v", err)
	}
}

// setLiveKeys records the set of monitored pod sandboxes that are alive, so the
// background reconciler can protect their mon_groups even when no container has
// caused them to be tracked in the Manager. It closes the tombstone window
// opened by beginSync: a key removed by a concurrent RemovePodSandbox during the
// pass is dropped from the snapshot rather than resurrected.
func (p *plugin) setLiveKeys(keys []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	live := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		// Store the canonical (dashed) UID so the set matches mgr.List() and a
		// later dropLiveKey deletes the entry regardless of which form (compact
		// or dashed) the removal callback reports.
		canon := monitor.CanonicalizePodUID(k)
		if _, removed := p.syncRemovals[canon]; removed {
			continue
		}
		live[canon] = struct{}{}
	}
	p.liveKeys = live
	p.syncRemovals = nil
}

// beginSync opens the removal-tombstone window for a Synchronize pass. Removals
// recorded by dropLiveKey while it is open are remembered so setLiveKeys, which
// commits the pass's (older) snapshot, cannot re-add them.
func (p *plugin) beginSync() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.syncRemovals = make(map[string]struct{})
}

// dropLiveKey removes a sandbox key from the live set once its pod is gone, so
// the reconciler stops protecting it and can reap any leftover mon_group. While
// a Synchronize pass is in flight it also tombstones the key so the pass cannot
// resurrect it from its pre-removal snapshot.
func (p *plugin) dropLiveKey(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	canon := monitor.CanonicalizePodUID(key)
	delete(p.liveKeys, canon)
	if p.syncRemovals != nil {
		p.syncRemovals[canon] = struct{}{}
	}
}

// reconcileLiveSet returns the union of the Manager's tracked keys and the live
// sandbox set for use as the reconcile live list.
func (p *plugin) reconcileLiveSet(mgr *monitor.Manager) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	live := make(map[string]struct{}, len(p.liveKeys))
	for k := range p.liveKeys {
		live[k] = struct{}{}
	}
	for _, k := range mgr.List() {
		live[k] = struct{}{}
	}
	keys := make([]string, 0, len(live))
	for k := range live {
		keys = append(keys, k)
	}
	return keys
}

// markPendingRemoval records a key whose Remove failed so the reconciler retries it.
func (p *plugin) markPendingRemoval(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pendingRemoval == nil {
		p.pendingRemoval = make(map[string]struct{})
	}
	p.pendingRemoval[key] = struct{}{}
}

func (p *plugin) clearPendingRemoval(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pendingRemoval, key)
}

func (p *plugin) pendingRemovalKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, 0, len(p.pendingRemoval))
	for k := range p.pendingRemoval {
		keys = append(keys, k)
	}
	return keys
}

// PostCreateContainer is called after the container is created but before
// it starts executing. The container PID is NOT yet available (pid=0) because
// the init process has not been started. We create the mon_group here so it
// is ready for PID assignment in StartContainer.
func (p *plugin) PostCreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	podUID := pod.GetUid()
	ctrName := pprintCtr(pod, ctr)

	log.Debugf("PostCreateContainer %s: pid=%d (expected 0)", ctrName, ctr.GetPid())

	if !p.shouldMonitorPod(pod) {
		log.Debugf("PostCreateContainer %s: pod filtered out, skipping", ctrName)
		return nil
	}

	rdtClass := getRDTClass(ctr)
	if _, err := p.getManager().EnsureGroup(podUID, rdtClass); err != nil {
		log.Warnf("PostCreateContainer %s: failed to create mon_group: %v", ctrName, err)
		return nil // non-fatal: don't block container creation
	}

	log.Infof("PostCreateContainer %s: mon_group ready, PID will be assigned in StartContainer", ctrName)
	return nil
}

// StartContainer is called just before the container process starts executing.
// At this point the init process has been created (via runc create) and the PID
// is available, but the process is paused and has NOT forked any threads yet.
// This is the ideal moment to write the PID to the resctrl mon_group tasks
// file: the kernel assigns the RMID to this PID, and when the process starts
// and forks threads they all inherit the RMID automatically.
func (p *plugin) StartContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	podUID := pod.GetUid()
	ctrName := pprintCtr(pod, ctr)
	pid := int(ctr.GetPid())

	log.Debugf("StartContainer %s: pid=%d", ctrName, pid)

	if !p.shouldMonitorPod(pod) {
		return nil
	}

	if pid > 0 {
		// Re-validate the container's RDT class against the pod's mon_group
		// before assigning: PostCreateContainer swallows EnsureGroup class
		// mismatches so it does not block container creation. Assignment must
		// never move the task into a different control group and overwrite its
		// allocation (the off-class sidecar case), so EnsureGroup here gates the
		// write and a mismatch skips it rather than reassigning the task.
		mgr := p.getManager()
		if grp, err := mgr.EnsureGroup(podUID, getRDTClass(ctr)); err != nil {
			log.Warnf("StartContainer %s: not assigning PID %d: %v", ctrName, pid, err)
		} else if err := mgr.AssignPID(podUID, pid); err != nil {
			log.Warnf("StartContainer %s: failed to assign PID %d: %v", ctrName, pid, err)
		} else {
			log.Infof("StartContainer %s: assigned pid %d (pre-start, no threads yet) in %s", ctrName, pid, grp.Path())
		}
	} else {
		log.Warnf("StartContainer %s: PID not available at pre-start, will retry in PostStartContainer", ctrName)
	}

	return nil
}

// PostStartContainer is called after the container process has been started.
// This is a fallback: if StartContainer did not have the PID, we write the
// init PID here.
func (p *plugin) PostStartContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	podUID := pod.GetUid()
	ctrName := pprintCtr(pod, ctr)
	pid := int(ctr.GetPid())

	log.Debugf("PostStartContainer %s: pid=%d", ctrName, pid)

	if !p.shouldMonitorPod(pod) {
		return nil
	}

	if pid > 0 {
		// Same class re-validation as StartContainer: never reassign a task's
		// control group when this container's RDT class does not match the
		// pod's mon_group (the off-class sidecar case).
		mgr := p.getManager()
		if grp, err := mgr.EnsureGroup(podUID, getRDTClass(ctr)); err != nil {
			log.Warnf("PostStartContainer %s: not assigning PID %d: %v", ctrName, pid, err)
		} else if err := mgr.AssignPID(podUID, pid); err != nil {
			log.Warnf("PostStartContainer %s: failed to assign PID %d: %v", ctrName, pid, err)
		} else {
			log.Infof("PostStartContainer %s: assigned pid %d in %s", ctrName, pid, grp.Path())
		}
	} else {
		log.Warnf("PostStartContainer %s: PID=0, cannot assign to mon_group (runtime did not provide PID via NRI)", ctrName)
	}

	return nil
}

// StopContainer is intentionally not implemented. A container stop must NOT
// tear down the pod's mon_group: a restart keeps the pod sandbox alive, and
// releasing the RMID would give the replacement container a fresh RMID whose
// hardware counters carry a non-zeroed residual, producing a false energy
// spike. The mon_group is removed in RemovePodSandbox when the pod is truly
// gone (and the reconciler cleans orphans from any missed teardown events).
// Because the NRI stub derives its event subscription from the implemented
// handler interfaces, omitting StopContainer also unsubscribes the plugin from
// STOP_CONTAINER events entirely.

// RemovePodSandbox is called when the pod sandbox is being torn down.
// This is the point at which the mon_group should be cleaned up, because
// the pod (and its UID) will not be reused.
func (p *plugin) RemovePodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	podUID := pod.GetUid()

	// The sandbox is gone, so stop protecting its key in the reconciler's live
	// set; otherwise a failed Remove below could never be reaped.
	p.dropLiveKey(podUID)

	// Attempt removal unconditionally rather than gating on shouldMonitorPod: a
	// pod may have been monitored under a configuration that was later changed to
	// exclude it. Gating here would strand its mon_group, because Remove would
	// never run and the key would linger in the Manager, so the reconciler would
	// keep treating it as live and never reap it. Remove is idempotent and
	// reports ErrNotTracked for a pod that was never monitored.
	switch err := p.getManager().Remove(podUID); {
	case err == nil:
		log.Infof("RemovePodSandbox %s/%s: removed mon_group", pod.GetNamespace(), pod.GetName())
	case errors.Is(err, monitor.ErrNotTracked):
		// Pod was never monitored; nothing to clean up.
	default:
		log.Warnf("RemovePodSandbox %s/%s: failed to remove mon_group (will be retried by reconciler): %v",
			pod.GetNamespace(), pod.GetName(), err)
		p.markPendingRemoval(podUID)
	}
	return nil
}

// getConfig returns the currently-applied configuration. It snapshots the
// pointer under stateMu so a concurrent reload cannot swap p.config mid-read;
// the returned *pluginConfig is treated as immutable (setConfig replaces it
// wholesale rather than mutating in place).
func (p *plugin) getConfig() *pluginConfig {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.config
}

// getManager returns the current monitor manager. Like getConfig it snapshots
// the pointer under stateMu so a concurrent root-change reload cannot swap
// p.mgr while a handler is using it.
func (p *plugin) getManager() *monitor.Manager {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.mgr
}

// shouldMonitorPod checks namespace and label filters.
func (p *plugin) shouldMonitorPod(pod *api.PodSandbox) bool {
	cfg := p.getConfig()
	if len(cfg.Namespaces) > 0 {
		ns := pod.GetNamespace()
		found := slices.Contains(cfg.Namespaces, ns)
		if !found {
			return false
		}
	}
	if len(cfg.LabelSelector) > 0 {
		labels := pod.GetLabels()
		for k, v := range cfg.LabelSelector {
			if labels[k] != v {
				return false
			}
		}
	}
	return true
}

// getRDTClass extracts the RDT class from a container's Linux resources.
func getRDTClass(ctr *api.Container) string {
	if linux := ctr.GetLinux(); linux != nil {
		if res := linux.GetResources(); res != nil {
			if rdt := res.GetRdtClass(); rdt != nil {
				return rdt.GetValue()
			}
		}
	}
	return ""
}

// pprintCtr returns a human-readable container identifier.
func pprintCtr(pod *api.PodSandbox, ctr *api.Container) string {
	return fmt.Sprintf("%s/%s:%s", pod.GetNamespace(), pod.GetName(), ctr.GetName())
}

// checkRuntimeVersion verifies that the container runtime provides PIDs via NRI.
// CRI-O versions before 1.36 do not populate Container.Pid in NRI events,
// making the plugin unable to assign tasks to monitoring groups.
func checkRuntimeVersion(runtime, version string) error {
	if !strings.EqualFold(runtime, "cri-o") {
		return nil
	}
	// Normalize: strip leading "v" and any pre-release/build suffix.
	version = strings.TrimPrefix(version, "v")
	if idx := strings.IndexAny(version, "-+"); idx != -1 {
		version = version[:idx]
	}
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("CRI-O version %q: unable to parse; require >= 1.36 for NRI PID support", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("CRI-O version %q: unable to parse major version: %w", version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("CRI-O version %q: unable to parse minor version: %w", version, err)
	}
	if major < 1 || (major == 1 && minor < 36) {
		return fmt.Errorf("CRI-O %s does not provide container PIDs via NRI (requires >= 1.36)", version)
	}
	return nil
}
