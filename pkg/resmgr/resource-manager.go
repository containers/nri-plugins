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
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/pidfile"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/control"
	"github.com/containers/nri-plugins/pkg/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/topology"
	goresctrlpath "github.com/intel/goresctrl/pkg/path"
	"sigs.k8s.io/yaml"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
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
	sync.RWMutex
	agent   *agent.Agent
	cfg     cfgapi.ResmgrConfig
	cache   cache.Cache      // cached state
	policy  policy.Policy    // resource manager policy
	control control.Control  // policy controllers/enforcement
	events  chan interface{} // channel for delivering events
	stop    chan interface{} // channel for signalling shutdown to goroutines
	nri     *nriPlugin       // NRI plugins, if we're running as such
	running bool
}

const (
	topologyLogger = "topology-hints"
)

var (
	log = logger.Get("resource-manager")
)

// NewResourceManager creates a new ResourceManager instance.
func NewResourceManager(backend policy.Backend, agt *agent.Agent) (ResourceManager, error) {
	topology.SetLogger(logger.Get(topologyLogger))

	if opt.HostRoot != "" {
		sysfs.SetSysRoot(opt.HostRoot)
		topology.SetSysRoot(opt.HostRoot)
		goresctrlpath.SetPrefix(opt.HostRoot)
	}

	if opt.MetricsTimer != 0 {
		log.Warn("WARNING: obsolete metrics-interval flag given, ignoring...")
		log.Warn("WARNING: use the CR-based configuration interface instead")
		log.Warn("WARNING: this flag will be removed in a future release")
	}

	m := &resmgr{
		agent: agt,
	}

	if err := m.setupCache(); err != nil {
		return nil, err
	}

	log.Info("running as an NRI plugin...")
	nrip, err := newNRIPlugin(m)
	if err != nil {
		return nil, err
	}
	m.nri = nrip

	if err := m.setupPolicy(backend); err != nil {
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
	log.Infof("starting agent, waiting for initial configuration...")
	err := m.agent.Start(m.updateConfig)
	if err != nil {
		return err
	}
	return nil
}

func (m *resmgr) updateConfig(newCfg interface{}) (bool, error) {
	if newCfg == nil {
		return false, fmt.Errorf("can't run without effective configuration...")
	}

	cfg, ok := newCfg.(cfgapi.ResmgrConfig)
	if !ok {
		if !m.running {
			return true, fmt.Errorf("got initial configuration of unexpected type %T", newCfg)
		} else {
			return false, fmt.Errorf("got configuration of unexpected type %T", newCfg)
		}
	}

	meta := cfg.GetObjectMeta()
	dump, _ := yaml.Marshal(cfg)

	if !m.running {
		log.Infof("acquired initial configuration %s (generation %d):",
			meta.GetName(), meta.GetGeneration())
		log.InfoBlock("  <initial config> ", "%s", dump)

		if err := m.start(cfg); err != nil {
			return true, fmt.Errorf("failed to start with initial configuration: %v", err)
		}

		m.running = true
		return false, nil
	}

	log.Infof("configuration update %s (generation %d):", meta.GetName(), meta.GetGeneration())
	log.InfoBlock("  <updated config> ", "%s", dump)

	return false, m.reconfigure(cfg)
}

// Start resource management once we acquired initial configuration.
func (m *resmgr) start(cfg cfgapi.ResmgrConfig) error {
	log.Info("starting resource manager...")

	m.cfg = cfg

	mCfg := cfg.CommonConfig()
	if err := logger.Configure(&mCfg.Log); err != nil {
		log.Warnf("failed to configure logger: %v", err)
	}

	if err := m.policy.Start(m.cfg.PolicyConfig()); err != nil {
		return err
	}

	if err := instrumentation.Reconfigure(&mCfg.Instrumentation); err != nil {
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

	log.Info("up and running")

	return nil
}

// Stop stops the resource manager.
func (m *resmgr) Stop() {
	log.Info("shutting down...")

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

	if err := m.cache.ResetActivePolicy(); err != nil {
		log.Warnf("failed to reset active policy: %v", err)
	}

	if err := m.cache.SetActivePolicy(backend.Name()); err != nil {
		log.Warnf("failed to set active policy: %v", err)
	}

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
	if zones := m.policy.GetTopologyZones(); len(zones) != 0 {
		log.Info("updating topology zones...")
		if err := m.agent.UpdateNrtCR(m.policy.ActivePolicy(), zones); err != nil {
			log.Error("failed to update topology zones: %v", err)
		}
	}
}

func (m *resmgr) reconfigure(cfg cfgapi.ResmgrConfig) error {
	apply := func(cfg cfgapi.ResmgrConfig) error {
		mCfg := cfg.CommonConfig()

		if err := logger.Configure(&mCfg.Log); err != nil {
			log.Warnf("failed to configure logger: %v", err)
		}
		if err := instrumentation.Reconfigure(&mCfg.Instrumentation); err != nil {
			return err
		}
		if err := m.control.StartStopControllers(&mCfg.Control); err != nil {
			log.Warnf("failed to restart controllers: %v", err)
		}

		err := m.policy.Reconfigure(cfg.PolicyConfig())
		if err != nil {
			return err
		}

		err = m.nri.updateContainers()
		if err != nil {
			log.Warnf("failed to apply configuration to containers: %v", err)
		}

		return nil
	}

	m.Lock()
	defer m.Unlock()

	log.Infof("activating new configuration...")
	err := apply(cfg)
	if err == nil {
		m.cfg = cfg
		return nil
	}

	log.Errorf("failed to apply update: %v", err)

	revertErr := apply(m.cfg)
	if revertErr != nil {
		log.Warnf("failed to revert configuration: %v", revertErr)
	}

	return err
}
