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
	"errors"
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
	"github.com/containers/nri-plugins/pkg/resmgr/policy"
)

var log = logger.NewLogger("dra")

// Owner is what the plugin needs from whoever owns it.
type Owner interface {
	// The plugin takes the lock for every kubelet request.
	sync.Locker
	// RequestShutdown asks the owner to shut down, for errors the plugin cannot
	// recover from. It runs on the very goroutine whose failure it reports, and
	// Stop() waits for that goroutine, so it must not stop us from there: it
	// has to return and leave that to the shutdown it triggers.
	RequestShutdown(reason string)
}

// Options are the parameters New needs beyond the driver name.
type Options struct {
	// NodeName is the node this plugin publishes resources for. Required.
	NodeName string
	// KubeClient is used to publish ResourceSlices. Required.
	KubeClient kubernetes.Interface
	// Owner is who we serialize kubelet requests against and ask to shut us
	// down. Required.
	Owner Owner
	// Policy decides which devices this driver publishes. Required.
	Policy policy.Policy
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
	owner         Owner
	policy        policy.Policy
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
	case opts.Owner == nil:
		return nil, fmt.Errorf("dra: owner must not be nil")
	case opts.Policy == nil:
		return nil, fmt.Errorf("dra: policy must not be nil")
	}

	pluginDataDir := opts.PluginDataDir
	if pluginDataDir == "" {
		pluginDataDir = filepath.Join(kubeletplugin.KubeletPluginsDir, driverName)
	}

	return &Plugin{
		driverName:    driverName,
		nodeName:      opts.NodeName,
		kubeClient:    opts.KubeClient,
		owner:         opts.Owner,
		policy:        opts.Policy,
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

	// Ask the policy for its devices before registering: this is also where the
	// policy validates its DRA configuration, and a policy which cannot say
	// what it offers must not end up as a registered driver.
	resources, err := p.driverResources()
	if err != nil {
		return err
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

	if err := helper.PublishResources(ctx, resources); err != nil {
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
	p.owner.Lock()
	defer p.owner.Unlock()

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
	p.owner.Lock()
	defer p.owner.Unlock()

	if len(claims) > 0 {
		log.Warnf("asked to unprepare %d claim(s) for a driver with no devices", len(claims))
	}

	result := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		result[claim.UID] = nil
	}

	return result, nil
}

// HandleError handles errors the kubeletplugin helper runs into in the
// background. A failed ResourceSlice publication is recoverable, the helper
// retries it, so logging it is enough. Anything else is one of the helper's
// gRPC servers giving up: the driver stays registered with kubelet but can no
// longer serve it, and nothing we do here would bring it back, so we ask our
// owner to shut down.
func (p *Plugin) HandleError(_ context.Context, err error, msg string) {
	if errors.Is(err, kubeletplugin.ErrRecoverable) {
		log.Errorf("%s: %v", msg, err)
		return
	}

	log.Errorf("fatal error: %s: %v", msg, err)
	p.owner.RequestShutdown(fmt.Sprintf("dra: %s: %v", msg, err))
}

// driverResources returns the resources to publish for this driver: the devices
// of the active policy, in a single pool named after our node. The devices are
// passed on as the policy described them, attributes and all — what they mean
// is the policy's business, not ours.
//
// A policy with no devices gets an empty slice published, not no slice at all.
// That states that the driver is alive and currently offers nothing, and it
// replaces whatever we published before, so a policy which stops offering
// devices does not leave a stale slice behind.
func (p *Plugin) driverResources() (resourceslice.DriverResources, error) {
	devices, err := p.policy.DRADevices()
	if err != nil {
		return resourceslice.DriverResources{},
			fmt.Errorf("dra: policy failed to provide DRA devices: %w", err)
	}

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.nodeName: {
				Slices: []resourceslice.Slice{{Devices: devices}},
			},
		},
	}, nil
}
