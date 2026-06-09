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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"sigs.k8s.io/yaml"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
)

const (
	// reconcileInterval is how often the background reconciler checks for
	// orphaned mon_groups left behind by failed StopContainer removals.
	reconcileInterval = 30 * time.Second
)

// plugin implements the NRI plugin interface for resctrl monitoring groups.
type plugin struct {
	stub           stub.Stub
	config         *pluginConfig
	state          *podState
	rdt            *resctrlOps
	mu             sync.Mutex    // serializes ensureMonGroup to prevent TOCTOU races
	stopReconciler chan struct{} // closed to stop the background reconciler
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
}

func newPlugin() *plugin {
	cfg := &pluginConfig{
		ResctrlPath: defaultResctrlPath,
	}
	return &plugin{
		config: cfg,
		state:  newPodState(),
		rdt:    newResctrlOps(cfg.ResctrlPath),
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
	return 0, nil
}

// onClose handles losing connection to container runtime.
func (p *plugin) onClose() {
	if p.stopReconciler != nil {
		close(p.stopReconciler)
	}
	log.Infof("Connection to the runtime lost, exiting...")
	os.Exit(0)
}

// setConfig applies new plugin configuration.
func (p *plugin) setConfig(data []byte) error {
	log.Tracef("setConfig: parsing\n---8<---\n%s\n--->8---", data)
	cfg := pluginConfig{
		ResctrlPath: defaultResctrlPath,
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("setConfig: cannot parse configuration: %w", err)
	}
	resctrlPath := filepath.Clean(cfg.ResctrlPath)
	if resctrlPath == "" || !filepath.IsAbs(resctrlPath) {
		return fmt.Errorf("setConfig: resctrlPath must be an absolute path, got %q", cfg.ResctrlPath)
	}
	cfg.ResctrlPath = resctrlPath
	p.config = &cfg
	p.rdt = newResctrlOps(cfg.ResctrlPath)
	log.Debugf("configuration: resctrlPath=%s namespaces=%v labelSelector=%v",
		cfg.ResctrlPath, cfg.Namespaces, cfg.LabelSelector)
	return nil
}

// Synchronize is called at plugin startup with the current set of pods and containers.
// It reconciles in-memory state with what exists on the resctrl filesystem.
func (p *plugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	log.Infof("synchronizing state: %d pods, %d containers", len(pods), len(containers))

	// Build a lookup from sandbox ID to pod (containers reference
	// pods by sandbox ID, not by Kubernetes UID).
	podBySandboxID := make(map[string]*api.PodSandbox, len(pods))
	for _, pod := range pods {
		podBySandboxID[pod.GetId()] = pod
	}

	// Create mon_groups for running containers that don't have one,
	// and write their PIDs to ensure monitoring is active after restart.
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
		if err := p.ensureMonGroup(podUID, ctr.GetId(), rdtClass); err != nil {
			log.Warnf("Synchronize: failed to create mon_group for pod %s: %v", podUID, err)
			continue
		}
		// Use canonical form for state lookups (ensureMonGroup stores canonical).
		u, _ := uuid.Parse(podUID)
		canonicalUID := u.String()
		pid := int(ctr.GetPid())
		if pid > 0 {
			monGroupDir := p.state.getMonGroupDir(canonicalUID)
			if err := p.rdt.writeTaskPID(monGroupDir, pid); err != nil {
				log.Warnf("Synchronize: failed to write PID %d for pod %s: %v", pid, podUID, err)
			} else {
				log.Debugf("Synchronize: assigned pid %d for pod %s", pid, podUID)
			}
		}
	}

	// Remove orphaned mon_groups from a previous plugin instance.
	p.rdt.cleanOrphanedMonGroups(p.state)

	// Start the background reconciler to periodically clean up orphaned
	// mon_groups that could not be removed during StopContainer.
	p.startReconciler()

	log.Infof("synchronization complete: tracking %d pods", p.state.podCount())
	return nil, nil
}

// startReconciler launches a background goroutine that periodically removes
// orphaned mon_group directories. This handles the case where removeMonGroup
// fails in StopContainer (e.g., kernel busy) and the directory lingers.
func (p *plugin) startReconciler() {
	if p.stopReconciler != nil {
		// Already running from a previous Synchronize call.
		return
	}
	p.stopReconciler = make(chan struct{})
	go func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stopReconciler:
				return
			case <-ticker.C:
				p.rdt.cleanOrphanedMonGroups(p.state)
			}
		}
	}()
	log.Debugf("background reconciler started (interval=%s)", reconcileInterval)
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
	if err := p.ensureMonGroup(podUID, ctr.GetId(), rdtClass); err != nil {
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
//
// If the PID is not available (should not happen at this stage), we fall back
// to PostStartContainer which will write PIDs after the process starts.
func (p *plugin) StartContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	podUID := pod.GetUid()
	if u, err := uuid.Parse(podUID); err == nil {
		podUID = u.String()
	}
	ctrName := pprintCtr(pod, ctr)
	pid := int(ctr.GetPid())

	log.Debugf("StartContainer %s: pid=%d", ctrName, pid)

	if !p.shouldMonitorPod(pod) {
		return nil
	}

	monGroupDir := p.state.getMonGroupDir(podUID)
	if monGroupDir == "" {
		log.Debugf("StartContainer %s: no mon_group (pod not tracked), skipping", ctrName)
		return nil
	}

	if pid > 0 {
		if err := p.rdt.writeTaskPID(monGroupDir, pid); err != nil {
			log.Warnf("StartContainer %s: failed to write PID %d to tasks: %v", ctrName, pid, err)
		} else {
			log.Infof("StartContainer %s: assigned pid %d to mon_group %s (pre-start, no threads yet)", ctrName, pid, monGroupDir)
		}
	} else {
		log.Warnf("StartContainer %s: PID not available at pre-start, will retry in PostStartContainer", ctrName)
	}

	return nil
}

// PostStartContainer is called after the container process has been started.
// This is a fallback: if StartContainer did not have the PID, we write the
// init PID here. The init PID is sufficient because all child threads inherit
// the RMID.
func (p *plugin) PostStartContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	podUID := pod.GetUid()
	if u, err := uuid.Parse(podUID); err == nil {
		podUID = u.String()
	}
	ctrName := pprintCtr(pod, ctr)
	pid := int(ctr.GetPid())

	log.Debugf("PostStartContainer %s: pid=%d", ctrName, pid)

	if !p.shouldMonitorPod(pod) {
		return nil
	}

	monGroupDir := p.state.getMonGroupDir(podUID)
	if monGroupDir == "" {
		return nil
	}

	if pid > 0 {
		if err := p.rdt.writeTaskPID(monGroupDir, pid); err != nil {
			log.Warnf("PostStartContainer %s: failed to write PID %d to tasks: %v", ctrName, pid, err)
		} else {
			log.Infof("PostStartContainer %s: assigned pid %d to mon_group %s", ctrName, pid, monGroupDir)
		}
	} else {
		log.Warnf("PostStartContainer %s: PID=0, cannot assign to mon_group (runtime did not provide PID via NRI)", ctrName)
	}

	return nil
}

// StopContainer is called when a container is being stopped.
func (p *plugin) StopContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) ([]*api.ContainerUpdate, error) {
	podUID := pod.GetUid()
	if u, err := uuid.Parse(podUID); err == nil {
		podUID = u.String()
	}
	ctrName := pprintCtr(pod, ctr)

	log.Debugf("StopContainer %s", ctrName)

	monGroupDir := p.state.getMonGroupDir(podUID)
	if monGroupDir == "" {
		return nil, nil
	}

	p.state.removeContainer(podUID, ctr.GetId())

	if p.state.podHasNoContainers(podUID) {
		log.Infof("StopContainer %s: last container, removing mon_group %s", ctrName, monGroupDir)
		if err := p.rdt.removeMonGroup(monGroupDir); err != nil {
			log.Warnf("StopContainer %s: failed to remove mon_group (will be cleaned on next restart): %v", ctrName, err)
		}
		p.state.removePod(podUID)
	}

	return nil, nil
}

// ensureMonGroup creates the mon_group directory if it doesn't exist and registers
// the container in the in-memory state.
//
// Limitation: all containers in a pod share a single mon_group under the first
// container's RDT class. If an allocation plugin assigns different classes to
// containers in the same pod, subsequent containers use the first class.
func (p *plugin) ensureMonGroup(podUID, containerID, rdtClass string) error {
	u, err := uuid.Parse(podUID)
	if err != nil {
		return fmt.Errorf("invalid pod UID %q", podUID)
	}
	podUID = u.String()

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state.getMonGroupDir(podUID) != "" {
		// Mon_group already exists for this pod. Just add the container.
		p.state.addContainer(podUID, containerID)
		return nil
	}

	monGroupDir, err := p.rdt.createMonGroup(rdtClass, podUID)
	if err != nil {
		return err
	}

	p.state.addPod(podUID, monGroupDir)
	p.state.addContainer(podUID, containerID)
	log.Infof("created mon_group %s for pod %s", monGroupDir, podUID)
	return nil
}

// shouldMonitorPod checks namespace and label filters.
func (p *plugin) shouldMonitorPod(pod *api.PodSandbox) bool {
	if len(p.config.Namespaces) > 0 {
		ns := pod.GetNamespace()
		found := false
		for _, allowed := range p.config.Namespaces {
			if ns == allowed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(p.config.LabelSelector) > 0 {
		labels := pod.GetLabels()
		for k, v := range p.config.LabelSelector {
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
