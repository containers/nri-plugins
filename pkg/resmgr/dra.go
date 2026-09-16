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

package resmgr

import (
	"context"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
)

// draDomain is the DNS domain DRA driver names are formed in. The driver is
// named after the active policy, because the policy decides which devices get
// published and what they mean, so a node running another policy publishes
// devices of another kind.
const draDomain = "nri.io"

// draEnabled resolves the tri-state dra.enabled switch: unset is off.
func draEnabled(cfg *cfgapi.DRAConfig) bool {
	return cfg != nil && cfg.Enabled != nil && *cfg.Enabled
}

// setupDRA creates the DRA plugin, unless DRA is disabled or we lack what the
// plugin needs. Missing cluster access leaves DRA off with a warning instead
// of failing startup: a plugin configured from a local file has no kubernetes
// client at all, and refusing to run at all would be the worse outcome.
func (m *resmgr) setupDRA(cfg *cfgapi.DRAConfig) error {
	if !draEnabled(cfg) {
		log.Infof("DRA support is disabled")
		return nil
	}

	nodeName := m.agent.NodeName()
	if nodeName == "" {
		log.Warnf("no node name, running without DRA support")
		return nil
	}

	// The client is checked here rather than left to dra.New, which cannot see
	// it: a nil *client.Client is a non-nil kubernetes.Interface.
	client := m.agent.KubeClient()
	if client == nil {
		log.Warnf("no kubernetes client, running without DRA support")
		return nil
	}

	// We are the plugin's owner: it takes our lock for every kubelet request,
	// serializing those against the NRI ones, and asks us to shut down when it
	// runs into an error it cannot recover from.
	plugin, err := dra.New(m.policy.ActivePolicy()+"."+draDomain, dra.Options{
		NodeName:   nodeName,
		KubeClient: client,
		Owner:      m,
		Policy:     m.policy,
	})
	if err != nil {
		return resmgrError("failed to create DRA plugin: %v", err)
	}
	m.dra = plugin

	return nil
}

// startDRA starts the DRA plugin, if we have one.
func (m *resmgr) startDRA() error {
	if m.dra == nil {
		return nil
	}

	if err := m.dra.Start(context.Background()); err != nil {
		return resmgrError("failed to start DRA plugin: %v", err)
	}

	return nil
}

// reconfigureDRA rejects turning DRA on or off in a running plugin. Both
// directions would have to happen with our lock released, but reconfiguration
// runs with it held: a kubelet request being served holds the lock, so both
// starting and stopping the plugin can wait for one.
//
// Only the resolved switch matters: unset and false are the same to us.
func (m *resmgr) reconfigureDRA(cfg *cfgapi.DRAConfig) error {
	was, now := draEnabled(&m.cfg.CommonConfig().DRA), draEnabled(cfg)
	if now != was {
		return resmgrError("cannot change dra.enabled from %v to %v while running, restart required",
			was, now)
	}

	return nil
}
