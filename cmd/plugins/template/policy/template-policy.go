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

package template

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/template"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
)

const (
	// PolicyName is the name used to activate this policy implementation.
	PolicyName = "template"
	// PolicyDescription is a short description of this policy.
	PolicyDescription = ""
	// PolicyPath is the path of this policy in the configuration hierarchy.
	PolicyPath = "policy." + PolicyName
)

// policy is our runtime state for this policy.
type policy struct {
	cfg   *cfgapi.Config // our runtime configuration
	cache cache.Cache    // pod/container cache
}

// Make sure policy implements the policy.Backend interface.
var _ policyapi.Backend = &policy{}
var log logger.Logger = logger.NewLogger("policy")

// New creates a new uninitialized template policy instance.
func New() policyapi.Backend {
	return &policy{}
}

// Name returns the name of this policy.
func (p *policy) Name() string {
	return PolicyName
}

// Description returns the description for this policy.
func (p *policy) Description() string {
	return PolicyDescription
}

// Setup initializes the template policy instance.
func (p *policy) Setup(opts *policyapi.BackendOptions) error {
	cfg, ok := opts.Config.(*cfgapi.Config)
	if !ok {
		return fmt.Errorf("config data of wrong type %T", opts.Config)
	}

	p.cfg = cfg
	p.cache = opts.Cache
	return nil
}

// Start prepares this policy for accepting allocation/release requests.
func (p *policy) Start() error {
	log.Info("started...")
	return nil
}

// Reconfigure this policy.
func (p *policy) Reconfigure(newCfg interface{}) error {
	cfg, ok := newCfg.(*cfgapi.Config)
	if !ok {
		return fmt.Errorf("config data of wrong type %T", newCfg)
	}
	p.cfg = cfg
	return nil
}

// Sync synchronizes the state of this policy.
func (p *policy) Sync(add []cache.Container, del []cache.Container) error {
	log.Info("synchronizing state...")
	return nil
}

// AllocateResources is a resource allocation request for this policy.
func (p *policy) AllocateResources(container cache.Container) error {
	log.Info("allocating resources for %s...", container.PrettyName())
	return nil
}

// ReleaseResources is a resource release request for this policy.
func (p *policy) ReleaseResources(container cache.Container) error {
	log.Info("releasing resources of %s...", container.PrettyName())
	return nil
}

// UpdateResources is a resource allocation update request for this policy.
func (p *policy) UpdateResources(c cache.Container) error {
	log.Info("(not) updating container %s...", c.PrettyName())
	return nil
}

// HandleEvent handles policy-specific events.
func (p *policy) HandleEvent(e *events.Policy) (bool, error) {
	log.Info("received policy event %s.%s with data %v...", e.Source, e.Type, e.Data)
	return true, nil
}

// DescribeMetrics generates policy-specific prometheus metrics data descriptors.
func (p *policy) DescribeMetrics() []*prometheus.Desc {
	return nil
}

// PollMetrics provides policy metrics for monitoring.
func (p *policy) PollMetrics() policyapi.Metrics {
	return nil
}

// CollectMetrics generates prometheus metrics from cached/polled policy-specific metrics data.
func (p *policy) CollectMetrics(policyapi.Metrics) ([]prometheus.Metric, error) {
	return nil, nil
}

// GetTopologyZones returns the policy/pool data for 'topology zone' CRDs.
func (p *policy) GetTopologyZones() []*policyapi.TopologyZone {
	return nil
}

// ExportResourceData provides resource data to export for the container.
func (p *policy) ExportResourceData(c cache.Container) map[string]string {
	return nil
}

// Initialize or reinitialize the policy.
func (p *policy) initialize() error {
	return nil
}
