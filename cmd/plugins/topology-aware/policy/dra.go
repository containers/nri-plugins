// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package topologyaware

import (
	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"

	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
)

// buildDRAPlugin constructs p.draPlugin from the current policy
// configuration (p.cfg, p.cpuClasses, p.cache) and the given backend
// options. Called once from Setup() when cfg.DRAEnabled() is true, and
// never from Reconfigure() (which refuses any change to DRAEnabled outright
// rather than tearing down/rebuilding p.draPlugin).
//
// A missing kube client or node name is treated as "DRA not ready yet"
// rather than a hard Setup() failure: this logs a warning and leaves
// p.draPlugin nil. Every other Backend lifecycle method already nil-checks
// p.draPlugin, so this degrades to "DRA disabled" rather than crashing.
//
// An empty (nil) p.cpuClasses is different: the plugin is still built, just
// with an empty device set (every *cpuclass.Handler method the adapter
// calls is nil-receiver-safe, see policyDRAAdapter's doc comment). Since
// buildDRAPlugin only ever runs once, leaving p.draPlugin nil here would
// prevent a later Reconfigure() that adds cpuClasses from ever enabling DRA.
//
// Genuine construction failures (CDI writer setup, dra.New's own
// dependency validation) are returned as errors, since those indicate a
// real misconfiguration rather than a timing issue.
func (p *policy) buildDRAPlugin(opts *policyapi.BackendOptions) error {
	if opts.KubeClientFn == nil {
		log.Warnf("dra: no KubeClientFn provided, DRA plugin not started")
		return nil
	}
	kubeClient := opts.KubeClientFn()
	if kubeClient == nil {
		log.Warnf("dra: no kube client available yet, DRA plugin not started")
		return nil
	}
	if opts.NodeName == "" {
		log.Warnf("dra: node name not known yet, DRA plugin not started")
		return nil
	}

	adapter := &policyDRAAdapter{p: p}

	cdiWriter, err := dra.NewCDIWriter(DRADriverName, p.cdiDir)
	if err != nil {
		return policyError("failed to create DRA CDI writer: %w", err)
	}

	deps := dra.Deps{
		KubeClient: kubeClient,
		NodeName:   opts.NodeName,
		// ValidateClasses captures p, not a config snapshot, so it observes
		// whatever p.cfg is live when called, including after Reconfigure()
		// swaps it. DRASharedCounters() is the nil-safe getter — without it,
		// a Reconfigure that removes the dra: section would panic on
		// p.cfg.DRA.SharedCounters here.
		ValidateClasses: func() error {
			return cpuclass.ValidateCPUClassesForDRA(p.cfg.CPUClasses, p.cfg.DRASharedCounters())
		},
		// ValidateCPUsInPool mirrors the check allocateClaim performs via
		// poolForCPUs at container-creation time, so a claim whose picked
		// CPUs straddle more than one leaf pool (e.g. a punit spanning an
		// entire package while leaf pools are NUMA or L3 nodes) is rejected
		// at Prepare time instead of being persisted and always failing
		// later when its container is created.
		ValidateCPUsInPool: func(cpus *libcpu.CpuMask) error {
			_, err := p.poolForCPUs(cpus)
			return err
		},
		DeviceLister:   adapter,
		ClaimAllocator: adapter,
		CDIWriter:      cdiWriter,
		ClaimStore:     dra.NewCacheClaimStore(p.cache),
		ClaimUnprepare: p.unprepareDRAClaim,
		WithLock:       opts.WithLock,
		Logger:         log,
	}

	plugin, err := dra.New(DRADriverName, deps)
	if err != nil {
		return policyError("failed to create DRA plugin: %w", err)
	}

	p.draPlugin = plugin

	return nil
}

// triggerDRARepublish enqueues a DRA ResourceSlice republication when
// PCT-based HP capacity is active. Called (deferred) from AllocateResources,
// ReleaseResources, and UpdateResources so that any change to non-DRA HP CPU
// usage (hpUsed) caused by those NRI-path allocations is reflected in the
// next ResourceSlice.
// The enqueue is non-blocking and safe to call while holding the resmgr lock;
// the actual publish runs in the DRA plugin's republisherLoop goroutine, which
// acquires the lock itself after the NRI handler releases it.
func (p *policy) triggerDRARepublish() {
	if p.draPlugin == nil {
		return
	}
	if p.cpuClasses == nil || !p.cpuClasses.PctActive() {
		return
	}
	p.draPlugin.TriggerRepublish()
}
