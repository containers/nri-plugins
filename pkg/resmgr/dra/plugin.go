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

// Package dra implements a DRA (Dynamic Resource Allocation) kubelet plugin.
//
// The package owns the kubelet-facing plumbing of the driver: registration,
// the gRPC lifecycle, publishing ResourceSlices and the mechanics of CDI spec
// files. It does not know what any of the devices it publishes mean. Which
// devices exist, what their attributes say, which resources a claim gets and
// what ends up in its CDI edits are all decided by the active policy.
//
// The plugin is owned by the resource manager, not by a policy, the same way
// the NRI plugin is: the resource manager constructs it, starts and stops it,
// and passes itself as the Locker so that kubelet-initiated requests serialize
// against the NRI-initiated ones.
package dra

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"

	logger "github.com/containers/nri-plugins/pkg/log"
)

var log = logger.NewLogger("dra")

// Options are the parameters New needs beyond the driver name.
type Options struct {
	// NodeName is the node this plugin publishes resources for. Required.
	NodeName string
	// KubeClient is used to publish ResourceSlices. Required.
	KubeClient kubernetes.Interface
	// Locker is taken for every kubelet request. Required.
	Locker sync.Locker
	// RegistrarDir is where the plugin registration socket is created.
	// Empty selects the kubelet's registry directory.
	RegistrarDir string
	// PluginDataDir is where the plugin's own socket is created.
	// Empty selects the driver's directory under the kubelet plugin directory.
	PluginDataDir string
}

// Plugin is a DRA kubelet plugin.
type Plugin struct {
	driverName    string
	nodeName      string
	kubeClient    kubernetes.Interface
	locker        sync.Locker
	registrarDir  string
	pluginDataDir string

	mu     sync.Mutex // guards helper, the only mutable state
	helper *kubeletplugin.Helper
}

var _ kubeletplugin.DRAPlugin = &Plugin{}

// New creates a plugin for the named DRA driver.
func New(driverName string, opts Options) (*Plugin, error) {
	switch {
	case driverName == "":
		return nil, fmt.Errorf("dra: driver name must not be empty")
	case opts.NodeName == "":
		return nil, fmt.Errorf("dra: node name must not be empty")
	case opts.KubeClient == nil:
		return nil, fmt.Errorf("dra: kube client must not be nil")
	case opts.Locker == nil:
		return nil, fmt.Errorf("dra: locker must not be nil")
	}

	pluginDataDir := opts.PluginDataDir
	if pluginDataDir == "" {
		pluginDataDir = filepath.Join(kubeletplugin.KubeletPluginsDir, driverName)
	}

	return &Plugin{
		driverName:    driverName,
		nodeName:      opts.NodeName,
		kubeClient:    opts.KubeClient,
		locker:        opts.Locker,
		registrarDir:  opts.RegistrarDir,
		pluginDataDir: pluginDataDir,
	}, nil
}

// Start registers the plugin with the kubelet and publishes its resources.
func (p *Plugin) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.helper != nil {
		return fmt.Errorf("dra: plugin already started")
	}

	if err := os.MkdirAll(p.pluginDataDir, 0750); err != nil {
		return fmt.Errorf("dra: failed to create plugin directory %q: %w", p.pluginDataDir, err)
	}

	opts := []kubeletplugin.Option{
		kubeletplugin.DriverName(p.driverName),
		kubeletplugin.NodeName(p.nodeName),
		kubeletplugin.KubeClient(p.kubeClient),
		kubeletplugin.PluginDataDirectoryPath(p.pluginDataDir),
	}
	if p.registrarDir != "" {
		// Only pass this when set: the option assigns unconditionally, so an
		// empty path would replace the kubelet registry directory default.
		opts = append(opts, kubeletplugin.RegistrarDirectoryPath(p.registrarDir))
	}

	log.Infof("registering DRA driver %q for node %q...", p.driverName, p.nodeName)

	helper, err := kubeletplugin.Start(ctx, p, opts...)
	if err != nil {
		return fmt.Errorf("dra: failed to register driver %q: %w", p.driverName, err)
	}

	if err := helper.PublishResources(ctx, p.driverResources()); err != nil {
		helper.Stop()
		return fmt.Errorf("dra: failed to publish resources for driver %q: %w", p.driverName, err)
	}

	p.helper = helper

	return nil
}

// Stop unregisters the plugin and stops serving kubelet requests. Stop must
// not be called with the Locker held: a request in flight holds the Locker
// and Stop waits for it to finish.
func (p *Plugin) Stop() {
	if p == nil {
		return
	}

	p.mu.Lock()
	helper := p.helper
	p.helper = nil
	p.mu.Unlock()

	if helper == nil {
		return
	}

	log.Infof("unregistering DRA driver %q...", p.driverName)
	helper.Stop()
}

// PrepareResourceClaims prepares resources for the given claims.
func (p *Plugin) PrepareResourceClaims(_ context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	p.locker.Lock()
	defer p.locker.Unlock()

	if len(claims) > 0 {
		// This driver publishes no devices yet, so no claim can name it.
		log.Warnf("asked to prepare %d claim(s) for a driver with no devices", len(claims))
	}

	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		result[claim.UID] = kubeletplugin.PrepareResult{}
	}

	return result, nil
}

// UnprepareResourceClaims releases the resources prepared for the given claims.
func (p *Plugin) UnprepareResourceClaims(_ context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	p.locker.Lock()
	defer p.locker.Unlock()

	if len(claims) > 0 {
		log.Warnf("asked to unprepare %d claim(s) for a driver with no devices", len(claims))
	}

	result := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		result[claim.UID] = nil
	}

	return result, nil
}

// HandleError logs errors the kubeletplugin helper runs into in the background.
func (p *Plugin) HandleError(_ context.Context, err error, msg string) {
	log.Errorf("%s: %v", msg, err)
}

// driverResources returns the resources to publish for this driver. Publishing
// an empty slice, instead of no slice at all, states that the driver is alive
// and has no devices, which is what this driver has until a policy provides
// some.
func (p *Plugin) driverResources() resourceslice.DriverResources {
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.nodeName: {
				Slices: []resourceslice.Slice{{}},
			},
		},
	}
}
