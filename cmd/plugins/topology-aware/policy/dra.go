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
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"

	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
)

// buildDRAPlugin constructs p.draPlugin from the current policy
// configuration (p.cfg, p.cpuClasses, p.cache) and the given backend
// options. Called once from Setup() when cfg.DRAEnabled() is true.
//
// Any required externally-supplied dependency being unavailable at this
// point in time — no kube client yet, no node name, no cpuClass
// configuration — is treated as "DRA not ready yet" rather than a hard
// Setup() failure: this logs a warning and leaves p.draPlugin nil. Every
// other Backend lifecycle method already nil-checks p.draPlugin, so this
// degrades to "DRA disabled" behavior rather than crashing.
//
// Genuine construction failures (CDI writer setup, dra.New's own
// dependency validation) are returned as errors, since those indicate a
// real misconfiguration rather than a timing issue.
//
// opts.KubeClientFn (see the BackendOptions doc) may return a nil
// kubernetes.Interface — either because opts.KubeClientFn itself is nil
// (defensive; should not happen in production wiring) or because the
// agent has no kube client yet (local-config mode, or too early in
// startup). Both are treated identically: no kube client, no DRA plugin
// this Setup() call.
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
	if p.cpuClasses == nil {
		log.Warnf("dra: no cpuClass configuration, DRA plugin not started")
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
		// ValidateClasses closure captures p, not a config snapshot: it
		// must observe whatever p.cfg is live at call time, including
		// after a later Reconfigure() swaps p.cfg for a new *Config.
		// DRASharedCounters() is the nil-safe getter (see the
		// ValidateClasses nil-guard note in the plan's Context section):
		// a Reconfigure that removes the dra: section would otherwise
		// panic on p.cfg.DRA.SharedCounters inside deps.WithLock.
		ValidateClasses: func() error {
			return cpuclass.ValidateCPUClassesForDRA(p.cfg.CPUClasses, p.cfg.DRASharedCounters())
		},
		DeviceLister:   adapter,
		ClaimAllocator: adapter,
		CDIWriter:      cdiWriter,
		ClaimStore:     dra.NewCacheClaimStore(p.cache),
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
