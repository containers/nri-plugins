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
	// reconcileInterval is how often the background reconciler retries failed
	// RemovePodSandbox removals and reaps orphans from a previous process.
	reconcileInterval = 30 * time.Second

	// telemetryShutdownTimeout bounds how long onClose waits for the telemetry
	// stack (MeterProvider flush + HTTP server) to drain before exiting.
	telemetryShutdownTimeout = 5 * time.Second
)

// plugin implements the NRI plugin interface for resctrl monitoring groups.
type plugin struct {
	stub stub.Stub

	// The runtime serializes NRI events, sends Synchronize before any other
	// event, and disconnects a plugin whose handler times out (onClose exits).
	// opMu makes that explicit and orders the background reconciler against
	// the handlers. It guards config, mgr, pendingRemoval, and sandboxes.
	opMu           sync.Mutex
	config         *pluginConfig
	mgr            resctrlManager
	pendingRemoval map[string]struct{}            // keys whose Remove failed, retried by the reconciler
	sandboxes      map[string]map[string]struct{} // canonical pod UID -> live sandbox IDs

	// lifeMu guards the state onClose tears down; onClose must not wait on a
	// stuck handler holding opMu. Lock order: opMu, then lifeMu.
	lifeMu         sync.Mutex
	stopReconciler chan struct{} // closed to stop the background reconciler
	telemetry      *telemetryState
	metrics        *monitor.Registration
	configErr      error // set when Configure fails, so onClose exits non-zero
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
func (p *plugin) Configure(ctx context.Context, config, runtime, version string) (_ stub.EventMask, retErr error) {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	defer func() {
		if retErr != nil {
			p.lifeMu.Lock()
			p.configErr = retErr
			p.lifeMu.Unlock()
		}
	}()
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
	p.lifeMu.Lock()
	defer p.lifeMu.Unlock()
	if p.telemetry == nil {
		// Telemetry is optional: keep managing mon_groups without it.
		if err := p.startTelemetry(ctx); err != nil {
			log.Errorf("telemetry disabled: %v", err)
		}
	}
	return 0, nil
}

// onClose handles losing connection to container runtime.
func (p *plugin) onClose() {
	p.lifeMu.Lock()
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
	configErr := p.configErr
	p.lifeMu.Unlock()
	if configErr != nil {
		log.Errorf("Connection to the runtime lost after configuration failed (%v), exiting...", configErr)
		os.Exit(1)
	}
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

	// Configuration is fixed once telemetry or the reconciler has started.
	// Before then (--config file, then the NRI server's config) it may still
	// select a non-default root. Callers hold opMu, except main before the stub
	// runs.
	p.lifeMu.Lock()
	started := p.telemetry != nil || p.stopReconciler != nil
	p.lifeMu.Unlock()
	if started {
		return errors.New("setConfig: configuration cannot change after startup; restart the plugin")
	}

	if p.config == nil || cfg.ResctrlPath != p.config.ResctrlPath {
		mgr, err := monitor.New(monitor.Options{
			ResctrlRoot:      cfg.ResctrlPath,
			KeyValidator:     monitor.PodUIDValidator,
			KeyCanonicalizer: monitor.CanonicalizePodUID,
		})
		if err != nil {
			return fmt.Errorf("setConfig: failed to create monitor manager: %w", err)
		}
		p.mgr = mgr
	}
	p.config = &cfg

	log.Debugf("configuration: resctrlPath=%s namespaces=%v labelSelector=%v",
		cfg.ResctrlPath, cfg.Namespaces, cfg.LabelSelector)
	return nil
}

// Synchronize is called at plugin startup with the current set of pods and containers.
// It reconciles in-memory state with what exists on the resctrl filesystem.
func (p *plugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	log.Infof("synchronizing state: %d pods, %d containers", len(pods), len(containers))

	mgr := p.mgr

	// Build a lookup from sandbox ID to pod (containers reference
	// pods by sandbox ID, not by Kubernetes UID).
	podBySandboxID := make(map[string]*api.PodSandbox, len(pods))
	for _, pod := range pods {
		podBySandboxID[pod.GetId()] = pod
	}

	// A pod sandbox can be alive with no running container (for example between
	// container restarts). Record every live sandbox so the reconciler protects
	// its existing mon_group even though it never appears in mgr.List(); the
	// container loop below then creates missing groups and (re)assigns PIDs.
	// Filtered pods are recorded too, so UUID-named groups that other tooling
	// owns for them are not reaped.
	p.sandboxes = make(map[string]map[string]struct{}, len(pods))
	for _, pod := range pods {
		p.addSandbox(pod)
	}
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

	// Defensive: NRI currently synchronizes once per process.
	//
	// Synchronize's pod list is the runtime's authoritative liveness snapshot. A
	// sandbox torn down while the plugin was disconnected never delivers
	// RemovePodSandbox, so its mon_group stays tracked; Manager.Reconcile
	// preserves tracked entries regardless of the live set and so cannot reap it.
	// Remove any tracked key absent from the live set here, using the
	// authoritative snapshot, so a missed teardown is cleaned up on reconnect.
	for _, key := range mgr.List() {
		if _, ok := p.sandboxes[key]; ok {
			continue
		}
		switch err := mgr.Remove(key); {
		case err == nil:
			p.clearPendingRemoval(key)
			log.Infof("Synchronize: reaped stale mon_group %s (missed teardown)", key)
		case errors.Is(err, monitor.ErrNotTracked):
			p.clearPendingRemoval(key)
		default:
			log.Warnf("Synchronize: failed to reap stale mon_group %s: %v", key, err)
			p.markPendingRemoval(key)
		}
	}

	if err := mgr.Reconcile(p.reconcileLiveSet(mgr)); err != nil {
		log.Warnf("Synchronize: reconcile failed: %v", err)
	}

	// Start the background reconciler to retry failed RemovePodSandbox
	// removals and reap orphans from a previous process.
	p.startReconciler()

	log.Infof("synchronization complete: tracking %d pods", len(mgr.List()))
	return nil, nil
}

// startReconciler launches a background goroutine that periodically retries
// failed removals and removes orphaned mon_group directories. This covers a
// Remove that fails in RemovePodSandbox (e.g., kernel busy) and leaves the
// directory behind.
func (p *plugin) startReconciler() {
	p.lifeMu.Lock()
	defer p.lifeMu.Unlock()
	if p.stopReconciler != nil {
		// Already running from a previous Synchronize call.
		return
	}
	// onClose nils p.stopReconciler, so the goroutine selects on its own copy.
	stop := make(chan struct{})
	p.stopReconciler = stop
	go func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				p.opMu.Lock()
				p.reconcile(p.mgr)
				p.opMu.Unlock()
			}
		}
	}()
	log.Debugf("background reconciler started (interval=%s)", reconcileInterval)
}

// reconcile retries removals that previously failed and reaps untracked orphan
// directories. A failed Remove leaves its key tracked, so Reconcile(List())
// would treat it as live forever; retrying Remove is what actually frees the
// RMID once the kernel releases the directory. The caller must hold opMu.
func (p *plugin) reconcile(mgr resctrlManager) {
	for _, key := range p.pendingRemovalKeys() {
		switch err := mgr.Remove(key); {
		case err == nil, errors.Is(err, monitor.ErrNotTracked):
			p.clearPendingRemoval(key)
		default:
			log.Warnf("reconciler: retry remove %s failed: %v", key, err)
		}
	}
	// Reconcile against the tracked keys plus the live sandbox set: a
	// container-less sandbox is protected only by p.sandboxes (it is never passed
	// to EnsureGroup, so mgr.List() omits it), and reconciling without it would
	// reap its existing mon_group and hand its replacement container a fresh
	// RMID.
	if err := mgr.Reconcile(p.reconcileLiveSet(mgr)); err != nil {
		log.Warnf("reconciler: %v", err)
	}
}

// addSandbox records a live sandbox under its pod's canonical UID, the form
// mgr.List() reports. The caller must hold opMu.
func (p *plugin) addSandbox(pod *api.PodSandbox) {
	uid := monitor.CanonicalizePodUID(pod.GetUid())
	if p.sandboxes == nil {
		p.sandboxes = make(map[string]map[string]struct{})
	}
	if p.sandboxes[uid] == nil {
		p.sandboxes[uid] = make(map[string]struct{})
	}
	p.sandboxes[uid][pod.GetId()] = struct{}{}
}

// reconcileLiveSet returns the union of the Manager's tracked keys and the live
// pod UIDs for use as the reconcile live list. The caller must hold opMu.
func (p *plugin) reconcileLiveSet(mgr resctrlManager) []string {
	live := make(map[string]struct{}, len(p.sandboxes))
	for k := range p.sandboxes {
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

// markPendingRemoval records a key whose Remove failed so the reconciler
// retries it. The pendingRemoval helpers assume the caller holds opMu.
func (p *plugin) markPendingRemoval(key string) {
	if p.pendingRemoval == nil {
		p.pendingRemoval = make(map[string]struct{})
	}
	p.pendingRemoval[key] = struct{}{}
}

func (p *plugin) clearPendingRemoval(key string) {
	delete(p.pendingRemoval, key)
}

func (p *plugin) pendingRemovalKeys() []string {
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
	p.opMu.Lock()
	defer p.opMu.Unlock()
	podUID := pod.GetUid()
	ctrName := pprintCtr(pod, ctr)

	log.Debugf("PostCreateContainer %s: pid=%d (expected 0)", ctrName, ctr.GetPid())

	p.addSandbox(pod)
	if !p.shouldMonitorPod(pod) {
		log.Debugf("PostCreateContainer %s: pod filtered out, skipping", ctrName)
		return nil
	}

	rdtClass := getRDTClass(ctr)
	if _, err := p.mgr.EnsureGroup(podUID, rdtClass); err != nil {
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
	p.opMu.Lock()
	defer p.opMu.Unlock()
	podUID := pod.GetUid()
	ctrName := pprintCtr(pod, ctr)
	pid := int(ctr.GetPid())

	log.Debugf("StartContainer %s: pid=%d", ctrName, pid)

	p.addSandbox(pod)
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
		mgr := p.mgr
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
	p.opMu.Lock()
	defer p.opMu.Unlock()
	podUID := pod.GetUid()
	ctrName := pprintCtr(pod, ctr)
	pid := int(ctr.GetPid())

	log.Debugf("PostStartContainer %s: pid=%d", ctrName, pid)

	p.addSandbox(pod)
	if !p.shouldMonitorPod(pod) {
		return nil
	}

	if pid > 0 {
		// Same class re-validation as StartContainer: never reassign a task's
		// control group when this container's RDT class does not match the
		// pod's mon_group (the off-class sidecar case).
		mgr := p.mgr
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
// spike. The mon_group is removed in RemovePodSandbox with the pod's last
// sandbox (a missed teardown is reaped on the next Synchronize).
// Because the NRI stub derives its event subscription from the implemented
// handler interfaces, omitting StopContainer also unsubscribes the plugin from
// STOP_CONTAINER events entirely.

// RunPodSandbox only records the new sandbox, so that removing an older sandbox
// of the same pod does not tear down the pod's mon_group.
func (p *plugin) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.addSandbox(pod)
	return nil
}

// RemovePodSandbox is called when a pod sandbox is torn down. kubelet also
// removes older sandboxes of a still-live pod (same UID, e.g. after a node
// reboot or pause-container death), so the mon_group is removed only with the
// pod's last sandbox.
func (p *plugin) RemovePodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	podUID := pod.GetUid()
	uid := monitor.CanonicalizePodUID(podUID)

	if ids := p.sandboxes[uid]; ids != nil {
		delete(ids, pod.GetId())
		if len(ids) > 0 {
			log.Infof("RemovePodSandbox %s/%s: removed sandbox %s, keeping mon_group for the pod's live sandbox",
				pod.GetNamespace(), pod.GetName(), pod.GetId())
			return nil
		}
	}

	// The pod is gone, so stop protecting its key in the reconciler's live set;
	// otherwise a failed Remove below could never be reaped.
	delete(p.sandboxes, uid)

	// Attempt removal unconditionally rather than gating on shouldMonitorPod: a
	// pod may have been monitored under a configuration that was later changed to
	// exclude it. Gating here would strand its mon_group, because Remove would
	// never run and the key would linger in the Manager, so the reconciler would
	// keep treating it as live and never reap it. Remove is idempotent and
	// reports ErrNotTracked for a pod that was never monitored.
	switch err := p.mgr.Remove(podUID); {
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

// shouldMonitorPod checks namespace and label filters.
func (p *plugin) shouldMonitorPod(pod *api.PodSandbox) bool {
	cfg := p.config
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
