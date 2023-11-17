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

package resmgr

import (
	"sync"

	//	"time"

	"github.com/containers/nri-plugins/pkg/healthz"
	"github.com/containers/nri-plugins/pkg/instrumentation"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/pidfile"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/control"
	"github.com/containers/nri-plugins/pkg/resmgr/metrics"
	"github.com/containers/nri-plugins/pkg/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/topology"
	goresctrlpath "github.com/intel/goresctrl/pkg/path"

	policyCollector "github.com/containers/nri-plugins/pkg/resmgr/policycollector"
)

// ResourceManager is the interface we expose for controlling the CRI resource manager.
type ResourceManager interface {
	// Start starts the resource manager.
	Start() error
	// Stop stops the resource manager.
	Stop()
	// SendEvent sends an event to be processed by the resource manager.
	SendEvent(event interface{}) error
	// Add-ons for testing.
	//ResourceManagerTestAPI
}

// resmgr is the implementation of ResourceManager.
type resmgr struct {
	logger.Logger
	sync.RWMutex
	cache        cache.Cache      // cached state
	policy       policy.Policy    // resource manager policy
	policySwitch bool             // active policy is being switched
	control      control.Control  // policy controllers/enforcement
	metrics      *metrics.Metrics // metrics collector/pre-processor
	events       chan interface{} // channel for delivering events
	stop         chan interface{} // channel for signalling shutdown to goroutines
	nri          *nriPlugin       // NRI plugins, if we're running as such
}

const (
	topologyLogger = "topology-hints"
)

// NewResourceManager creates a new ResourceManager instance.
func NewResourceManager(backend policy.Backend) (ResourceManager, error) {
	m := &resmgr{Logger: logger.NewLogger("resource-manager")}

	if err := m.setupCache(); err != nil {
		return nil, err
	}

	sysfs.SetSysRoot(opt.HostRoot)
	topology.SetSysRoot(opt.HostRoot)
	topology.SetLogger(logger.Get(topologyLogger))

	if opt.HostRoot != "" {
		goresctrlpath.SetPrefix(opt.HostRoot)
	}

	m.Info("running as an NRI plugin...")
	nrip, err := newNRIPlugin(m)
	if err != nil {
		return nil, err
	}
	m.nri = nrip

	if err := m.setupPolicy(backend); err != nil {
		return nil, err
	}

	if err := m.registerPolicyMetricsCollector(); err != nil {
		return nil, err
	}

	if err := m.setupEventProcessing(); err != nil {
		return nil, err
	}

	if err := m.setupControllers(); err != nil {
		return nil, err
	}

	m.setupHealthCheck()

	return m, nil
}

// Start starts the resource manager.
func (m *resmgr) Start() error {
	m.Info("starting...")

	m.Lock()
	defer m.Unlock()

	if err := m.nri.start(); err != nil {
		return err
	}

	if err := m.startControllers(); err != nil {
		return err
	}

	if err := m.startEventProcessing(); err != nil {
		return err
	}

	if err := pidfile.Remove(); err != nil {
		return resmgrError("failed to remove stale/old PID file: %v", err)
	}
	if err := pidfile.Write(); err != nil {
		return resmgrError("failed to write PID file: %v", err)
	}

	m.Info("up and running")

	return nil
}

// Stop stops the resource manager.
func (m *resmgr) Stop() {
	m.Info("shutting down...")

	m.Lock()
	defer m.Unlock()

	m.nri.stop()
}

// setupCache creates a cache and reloads its last saved state if found.
func (m *resmgr) setupCache() error {
	var err error

	options := cache.Options{CacheDir: opt.StateDir}
	if m.cache, err = cache.NewCache(options); err != nil {
		return resmgrError("failed to create cache: %v", err)
	}

	return nil

}

// setupPolicy sets up policy with the configured/active backend
func (m *resmgr) setupPolicy(backend policy.Backend) error {
	var err error

	active := backend.Name()
	cached := m.cache.GetActivePolicy()

	if active != cached {
		if cached != "" {
			if err := m.cache.ResetActivePolicy(); err != nil {
				return resmgrError("failed to reset cached policy %q: %v", cached, err)
			}
		}
		m.cache.SetActivePolicy(active)
		m.policySwitch = true
	}

	options := &policy.Options{SendEvent: m.SendEvent}
	if m.policy, err = policy.NewPolicy(backend, m.cache, options); err != nil {
		return resmgrError("failed to create policy %s: %v", active, err)
	}

	return nil
}

// setupHealthCheck prepares the resource manager for serving health-check requests.
func (m *resmgr) setupHealthCheck() {
	mux := instrumentation.HTTPServer().GetMux()
	healthz.Setup(mux)
}

// setupControllers sets up the resource controllers.
func (m *resmgr) setupControllers() error {
	var err error

	if m.control, err = control.NewControl(); err != nil {
		return resmgrError("failed to create resource controller: %v", err)
	}

	return nil
}

// startControllers start the resource controllers.
func (m *resmgr) startControllers() error {
	if err := m.control.StartStopControllers(m.cache, opt.EnableTestAPIs); err != nil {
		return resmgrError("failed to start resource controllers: %v", err)
	}

	return nil
}

// updateTopologyZones updates the 'topology zone' CRDs.
func (m *resmgr) updateTopologyZones() {
	m.Warn("no agent, can't update topology zones")
	//err := m.agent.UpdateNrtCR(m.policy.ActivePolicy(), m.policy.GetTopologyZones())
	//if err != nil {
	//	m.Error("failed to update topology zones: %v", err)
	//}
}

// registerPolicyMetricsCollector registers policy metrics collector·
func (m *resmgr) registerPolicyMetricsCollector() error {
	pc := &policyCollector.PolicyCollector{}
	pc.SetPolicy(m.policy)
	if pc.HasPolicySpecificMetrics() {
		return pc.RegisterPolicyMetricsCollector()
	}
	m.Info("%s policy has no policy-specific metrics.", m.policy.ActivePolicy())
	return nil
}
