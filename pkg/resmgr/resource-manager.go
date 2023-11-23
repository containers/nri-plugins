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
	"fmt"
	"sync"

	//	"time"

	"github.com/containers/nri-plugins/pkg/agent"
	"github.com/containers/nri-plugins/pkg/healthz"
	"github.com/containers/nri-plugins/pkg/instrumentation"
	"github.com/containers/nri-plugins/pkg/log"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/pidfile"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/control"
	"github.com/containers/nri-plugins/pkg/resmgr/metrics"
	"github.com/containers/nri-plugins/pkg/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/topology"
	goresctrlpath "github.com/intel/goresctrl/pkg/path"
	"sigs.k8s.io/yaml"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"

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
}

type Config = cfgapi.CommonConfig

// resmgr is the implementation of ResourceManager.
type resmgr struct {
	logger.Logger
	sync.RWMutex
	agent   *agent.Agent
	cfg     cfgapi.ResmgrConfig
	cache   cache.Cache      // cached state
	policy  policy.Policy    // resource manager policy
	control control.Control  // policy controllers/enforcement
	metrics *metrics.Metrics // metrics collector/pre-processor
	events  chan interface{} // channel for delivering events
	stop    chan interface{} // channel for signalling shutdown to goroutines
	nri     *nriPlugin       // NRI plugins, if we're running as such
	running bool
}

const (
	topologyLogger = "topology-hints"
)

// NewResourceManager creates a new ResourceManager instance.
func NewResourceManager(backend policy.Backend, agt *agent.Agent) (ResourceManager, error) {
	topology.SetLogger(logger.Get(topologyLogger))

	if opt.HostRoot != "" {
		sysfs.SetSysRoot(opt.HostRoot)
		topology.SetSysRoot(opt.HostRoot)
		goresctrlpath.SetPrefix(opt.HostRoot)
	}

	m := &resmgr{
		Logger: logger.NewLogger("resource-manager"),
		agent:  agt,
	}

	if err := m.setupCache(); err != nil {
		return nil, err
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

// Start the resource manager.
func (m *resmgr) Start() error {
	m.Infof("starting agent, waiting for initial configuration...")
	err := m.agent.Start(m.updateConfig)
	if err != nil {
		return err
	}
	return nil
}

func (m *resmgr) updateConfig(newCfg interface{}) error {
	if newCfg == nil {
		return fmt.Errorf("can't run without effective configuration...")
	}

	cfg, ok := newCfg.(cfgapi.ResmgrConfig)
	if !ok {
		if !m.running {
			m.Fatalf("got initial configuration of unexpected type %T", newCfg)
		} else {
			return fmt.Errorf("got configuration of unexpected type %T", newCfg)
		}
	}

	meta := cfg.GetObjectMeta()
	dump, _ := yaml.Marshal(cfg)

	if !m.running {
		m.Infof("acquired initial configuration %s (generation %d):",
			meta.GetName(), meta.GetGeneration())
		m.InfoBlock("  <initial config> ", "%s", dump)

		if err := m.start(cfg); err != nil {
			m.Fatalf("failed to start with initial configuration: %v", err)
		}

		m.running = true
		return nil
	}

	m.Infof("configuration update %s (generation %d):", meta.GetName(), meta.GetGeneration())
	m.InfoBlock("  <updated config> ", "%s", dump)

	return m.reconfigure(cfg)
}

// Start resource management once we acquired initial configuration.
func (m *resmgr) start(cfg cfgapi.ResmgrConfig) error {
	m.Info("starting resource manager...")

	m.cfg = cfg

	mCfg := cfg.CommonConfig()
	log.Configure(&mCfg.Log)
	instrumentation.Reconfigure(&mCfg.Instrumentation)

	if err := m.policy.Start(m.cfg.PolicyConfig()); err != nil {
		return err
	}

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

	m.cache.ResetActivePolicy()
	m.cache.SetActivePolicy(backend.Name())

	p, err := policy.NewPolicy(backend, m.cache, &policy.Options{SendEvent: m.SendEvent})
	if err != nil {
		return resmgrError("failed to create policy %s: %v", backend.Name(), err)
	}
	m.policy = p

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

	if m.control, err = control.NewControl(m.cache); err != nil {
		return resmgrError("failed to create resource controller: %v", err)
	}

	return nil
}

// startControllers start the resource controllers.
func (m *resmgr) startControllers() error {
	cfg := m.cfg.CommonConfig()
	if err := m.control.StartStopControllers(&cfg.Control); err != nil {
		return resmgrError("failed to start resource controllers: %v", err)
	}

	return nil
}

// updateTopologyZones updates the 'topology zone' CRDs.
func (m *resmgr) updateTopologyZones() {
	m.Info("updating topology zones...")
	err := m.agent.UpdateNrtCR(m.policy.ActivePolicy(), m.policy.GetTopologyZones())
	if err != nil {
		m.Error("failed to update topology zones: %v", err)
	}
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

func (m *resmgr) reconfigure(cfg cfgapi.ResmgrConfig) error {
	apply := func(cfg cfgapi.ResmgrConfig) error {
		mCfg := cfg.CommonConfig()

		log.Configure(&mCfg.Log)
		instrumentation.Reconfigure(&mCfg.Instrumentation)
		m.control.StartStopControllers(&mCfg.Control)

		err := m.policy.Reconfigure(cfg.PolicyConfig())
		if err != nil {
			return err
		}

		err = m.nri.updateContainers()
		if err != nil {
			m.Warnf("failed to apply configuration to containers: %v", err)
		}

		return nil
	}

	m.Lock()
	defer m.Unlock()

	m.Infof("activating new configuration...")
	err := apply(cfg)
	if err == nil {
		m.cfg = cfg
		return nil
	}

	m.Errorf("failed to apply update: %v", err)

	revertErr := apply(m.cfg)
	if revertErr != nil {
		m.Warnf("failed to revert configuration: %v", revertErr)
	}

	return err
}
