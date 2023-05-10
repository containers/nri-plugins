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
	"os"
	"os/signal"
	"strings"
	"sync"

	//	"time"

	"golang.org/x/sys/unix"

	pkgcfg "github.com/containers/nri-plugins/pkg/config"
	"github.com/containers/nri-plugins/pkg/healthz"
	"github.com/containers/nri-plugins/pkg/instrumentation"
	"github.com/containers/nri-plugins/pkg/log"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/pidfile"
	"github.com/containers/nri-plugins/pkg/resmgr/agent"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	config "github.com/containers/nri-plugins/pkg/resmgr/config"
	"github.com/containers/nri-plugins/pkg/resmgr/control"
	"github.com/containers/nri-plugins/pkg/resmgr/introspect"
	"github.com/containers/nri-plugins/pkg/resmgr/metrics"
	"github.com/containers/nri-plugins/pkg/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/topology"

	policyCollector "github.com/containers/nri-plugins/pkg/resmgr/policycollector"
)

// ResourceManager is the interface we expose for controlling the CRI resource manager.
type ResourceManager interface {
	// Start starts the resource manager.
	Start() error
	// Stop stops the resource manager.
	Stop()
	// SetConfig dynamically updates the resource manager configuration.
	SetConfig(config.RawConfig) error
	// SendEvent sends an event to be processed by the resource manager.
	SendEvent(event interface{}) error
	// Add-ons for testing.
	//ResourceManagerTestAPI
}

// resmgr is the implementation of ResourceManager.
type resmgr struct {
	logger.Logger
	sync.RWMutex
	cache        cache.Cache        // cached state
	policy       policy.Policy      // resource manager policy
	policySwitch bool               // active policy is being switched
	control      control.Control    // policy controllers/enforcement
	conf         config.RawConfig   // pending for saving in cache
	metrics      *metrics.Metrics   // metrics collector/pre-processor
	events       chan interface{}   // channel for delivering events
	stop         chan interface{}   // channel for signalling shutdown to goroutines
	signals      chan os.Signal     // signal channel
	introspect   *introspect.Server // server for external introspection
	nri          *nriPlugin         // NRI plugins, if we're running as such
	agent        agent.ResourceManagerAgent
}

const (
	topologyLogger = "topology-hints"
)

// NewResourceManager creates a new ResourceManager instance.
func NewResourceManager() (ResourceManager, error) {
	m := &resmgr{Logger: logger.NewLogger("resource-manager")}

	if err := m.setupCache(); err != nil {
		return nil, err
	}

	sysfs.SetSysRoot(opt.HostRoot)
	topology.SetSysRoot(opt.HostRoot)
	topology.SetLogger(logger.Get(topologyLogger))

	m.Info("running as an NRI plugin...")
	nrip, err := newNRIPlugin(m)
	if err != nil {
		return nil, err
	}
	m.nri = nrip

	if err := m.checkOpts(); err != nil {
		return nil, err
	}

	if err := m.loadConfig(); err != nil {
		return nil, err
	}

	if err := m.setupPolicy(); err != nil {
		return nil, err
	}

	if err := m.registerPolicyMetricsCollector(); err != nil {
		return nil, err
	}

	if err := m.setupEventProcessing(); err != nil {
		return nil, err
	}

	if err := m.setupIntrospection(); err != nil {
		return nil, err
	}

	m.setupHealthCheck()

	if err := m.setupAgent(); err != nil {
		return nil, err
	}

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

	if err := m.startEventProcessing(); err != nil {
		return err
	}

	m.startIntrospection()

	if err := m.startAgent(); err != nil {
		return err
	}

	if err := pidfile.Remove(); err != nil {
		return resmgrError("failed to remove stale/old PID file: %v", err)
	}
	if err := pidfile.Write(); err != nil {
		return resmgrError("failed to write PID file: %v", err)
	}

	if opt.ForceConfig == "" {
		// We never store a forced configuration in the cache. However, if we're not
		// running with a forced configuration, and the configuration is pending to
		// get stored in the cache (IOW, it is a new one acquired from an agent), then
		// then store it in the cache now.
		if m.conf != nil {
			m.cache.SetConfig(m.conf)
			m.conf = nil
		}
	}

	m.Info("up and running")

	return nil
}

// Stop stops the resource manager.
func (m *resmgr) Stop() {
	m.Info("shutting down...")

	m.Lock()
	defer m.Unlock()

	if m.signals != nil {
		close(m.signals)
		m.signals = nil
	}

	m.nri.stop()
}

// setupCache creates a cache and reloads its last saved state if found.
func (m *resmgr) setupCache() error {
	var err error

	options := cache.Options{CacheDir: opt.StateDir}
	if m.cache, err = cache.NewCache(options); err != nil {
		return resmgrError("failed to create cache: %v", err)
	}

	// If we ended up loading an existing cache and that cache has
	// an empty configuration saved, remove that configuration now.
	// Policies tend to expect *some* CPU reservation which is not
	// present if the configuration is fully empty. Not having any
	// configuration (in the cache or from the agent) should cause
	// the fallback configuration to be taken into use (until some
	// other configuration is provided by the agent). The fallback
	// configuration is fully controlled by the user and it should
	// have a valid configuration for the policy being started.

	if cfg := m.cache.GetConfig(); cfg != nil && len(cfg) == 0 {
		m.cache.ResetConfig()
	}

	return nil

}

// checkOpts checks the command line options for obvious errors.
func (m *resmgr) checkOpts() error {
	if opt.ForceConfig != "" && opt.FallbackConfig != "" {
		return resmgrError("both fallback (%s) and forced (%s) configurations given",
			opt.FallbackConfig, opt.ForceConfig)
	}

	return nil
}

// setupPolicy sets up policy with the configured/active backend
func (m *resmgr) setupPolicy() error {
	var err error

	active := policy.ActivePolicy()
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
	if m.policy, err = policy.NewPolicy(m.cache, options); err != nil {
		return resmgrError("failed to create policy %s: %v", active, err)
	}

	return nil
}

// setupIntrospection prepares the resource manager for serving external introspection requests.
func (m *resmgr) setupIntrospection() error {
	mux := instrumentation.GetHTTPMux()

	i, err := introspect.Setup(mux, m.policy.Introspect())
	if err != nil {
		return resmgrError("failed to set up introspection service: %v", err)
	}
	m.introspect = i

	return nil
}

// setupHealthCheck prepares the resource manager for serving health-check requests.
func (m *resmgr) setupHealthCheck() {
	mux := instrumentation.GetHTTPMux()
	healthz.Setup(mux)
}

// startIntrospection starts serving the external introspection requests.
func (m *resmgr) startIntrospection() {
	m.introspect.Start()
	m.updateIntrospection()
}

// stopInstrospection stops serving external introspection requests.
func (m *resmgr) stopIntrospection() {
	m.introspect.Stop()
}

// updateIntrospection pushes updated data for external introspection·
func (m *resmgr) updateIntrospection() {
	m.introspect.Set(m.policy.Introspect())
}

// updateTopologyZones updates the 'topology zone' CRDs.
func (m *resmgr) updateTopologyZones() {
	m.Info("updating topology zone CRDs...")

	if m.agent == nil {
		m.Warn("no agent, can't update topology zones")
		return
	}

	err := m.agent.UpdateNrtCR(policy.ActivePolicy(), m.policy.GetTopologyZones())
	if err != nil {
		m.Error("failed to update topology zones: %v", err)
	}
}

// setupAgent sets up the cluster access 'agent', for accessing the cluster/API server.
func (m *resmgr) setupAgent() error {
	if opt.DisableAgent {
		m.Info("cluster access agent is disabled")
		return nil
	}

	a, err := agent.NewResourceManagerAgent(m.SetConfig)
	if err != nil {
		return fmt.Errorf("failed to set up cluster access agent: %w", err)
	}

	m.agent = a
	return nil
}

// startAgent starts the cluster access 'agent'.
func (m *resmgr) startAgent() error {
	if m.agent == nil {
		return nil
	}

	go func() {
		if err := m.agent.Run(); err != nil {
			log.Error("failed to start cluster access agent")
		}
	}()

	return nil
}

// stopAgent stops the cluster access 'agent'.
func (m *resmgr) stopAgent() {
	if m.agent == nil {
		return
	}
}

// registerPolicyMetricsCollector registers policy metrics collector·
func (m *resmgr) registerPolicyMetricsCollector() error {
	pc := &policyCollector.PolicyCollector{}
	pc.SetPolicy(m.policy)
	if pc.HasPolicySpecificMetrics() {
		return pc.RegisterPolicyMetricsCollector()
	}
	m.Info("%s policy has no policy-specific metrics.", policy.ActivePolicy())
	return nil
}

// loadConfig tries to pick and load (initial) configuration from a number of sources.
func (m *resmgr) loadConfig() error {
	//
	// We try to load initial configuration from a number of sources:
	//
	//    1. use forced configuration file if we were given one
	//    2. use configuration from agent, if we can fetch it and it applies
	//    3. use last configuration stored in cache, if we have one and it applies
	//    4. use fallback configuration file if we were given one
	//    5. use empty/builtin default configuration, whatever that is...
	//
	// Notes/TODO:
	//   If the agent is already running at this point, the initial configuration is
	//   obtained by polling the agent via GetConfig(). Unlike for the latter updates
	//   which are pushed by the agent, there is currently no way to report problems
	//   about polled configuration back to the agent. If/once the agent will have a
	//   mechanism to propagate configuration errors back to the origin, this might
	//   become a problem that we'll need to solve.
	//

	if opt.ForceConfig != "" {
		m.Info("using forced configuration %s...", opt.ForceConfig)
		if err := pkgcfg.SetConfigFromFile(opt.ForceConfig); err != nil {
			return resmgrError("failed to load forced configuration %s: %v",
				opt.ForceConfig, err)
		}
		return m.setupConfigSignal(opt.ForceConfigSignal)
	}

	m.Info("trying last cached configuration...")
	if conf := m.cache.GetConfig(); conf != nil {
		err := pkgcfg.SetConfig(conf)
		if err == nil {
			return nil
		}
		m.Error("failed to activate cached configuration: %v", err)
	}

	if opt.FallbackConfig != "" {
		m.Info("using fallback configuration %s...", opt.FallbackConfig)
		if err := pkgcfg.SetConfigFromFile(opt.FallbackConfig); err != nil {
			return resmgrError("failed to load fallback configuration %s: %v",
				opt.FallbackConfig, err)
		}
		return nil
	}

	m.Warn("no initial configuration found")
	return nil
}

// SetConfig pushes new configuration to the resource manager.
func (m *resmgr) SetConfig(conf config.RawConfig) error {
	if conf == nil {
		m.Info("config from agent is empty, ignoring...")
		return resmgrError("config from agent is empty, ignoring...")
	}

	if opt.ForceConfig != "" {
		m.Info("ignoring config from agent because using forced configuration %s", opt.ForceConfig)
		return nil
	}

	m.Info("applying new configuration from agent...")

	return m.setConfig(conf)
}

// setConfigFromFile pushes new configuration to the resource manager from a file.
func (m *resmgr) setConfigFromFile(path string) error {
	m.Info("applying new configuration from file %s...", path)
	return m.setConfig(path)
}

// setConfig activates a new configuration, either from the agent or from a file.
func (m *resmgr) setConfig(v interface{}) error {
	var err error

	m.Lock()
	defer m.Unlock()

	switch cfg := v.(type) {
	case config.RawConfig:
		err = pkgcfg.SetConfig(cfg)
	case string:
		err = pkgcfg.SetConfigFromFile(cfg)
	default:
		err = fmt.Errorf("invalid configuration source/type %T", v)
	}
	if err != nil {
		m.Error("configuration rejected: %v", err)
		return resmgrError("configuration rejected: %v", err)
	}

	if err != nil {
		return err
	}

	m.Info("successfully switched to new configuration")

	// Save succesfully applied configuration from agent in the cache.
	if cfg, ok := v.(config.RawConfig); ok {
		m.cache.SetConfig(cfg)
	}

	err = m.nri.updateContainers()
	if err != nil {
		m.Warn("failed to update containers for new configuration: %v", err)
	}

	return nil
}

// setupConfigSignal sets up a signal handler for reloading forced configuration.
func (m *resmgr) setupConfigSignal(signame string) error {
	if signame == "" || strings.HasPrefix(strings.ToLower(signame), "disable") {
		return nil
	}

	m.Info("setting up signal %s to reload forced configuration", signame)

	sig := unix.SignalNum(signame)
	if int(sig) == 0 {
		return resmgrError("invalid forced configuration reload signal '%s'", signame)
	}

	m.signals = make(chan os.Signal, 1)
	signal.Notify(m.signals, sig)

	go func(signals <-chan os.Signal) {
		for {
			select {
			case _, ok := <-signals:
				if !ok {
					return
				}
			}

			m.Info("reloading forced configuration %s...", opt.ForceConfig)

			if err := m.setConfigFromFile(opt.ForceConfig); err != nil {
				m.Error("failed to reload forced configuration %s: %v",
					opt.ForceConfig, err)
			}
		}
	}(m.signals)

	return nil
}

// rebalance triggers a policy-specific rebalancing cycle of containers.
func (m *resmgr) rebalance(method string) error {
	if m.policy == nil {
		return nil
	}

	changes, err := m.policy.Rebalance()

	if err != nil {
		m.Error("%s: rebalancing of containers failed: %v", method, err)
	}

	if changes {
		// TODO: fix this
	}

	return m.cache.Save()
}
