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
	"slices"
	"sync"
	"sync/atomic"

	resourceapi "k8s.io/api/resource/v1"
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
	// ClaimAllocated and ClaimReleased commit an allocation change the policy
	// just made for a claim. They are called with the lock held, once per
	// claim, and they are what make the change outlive us and reach the
	// containers it affects. An error from ClaimAllocated means the claim's
	// resources may still be in use by other containers.
	ClaimAllocated() error
	ClaimReleased() error
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
	// Policy allocates the resources of the claims we prepare. Required.
	Policy policy.Policy
	// RegistrarDir is where the plugin registration socket is created.
	// Empty selects the kubelet's registry directory.
	RegistrarDir string
	// PluginDataDir is where the plugin's own socket is created.
	// Empty selects the driver's directory under the kubelet plugin directory.
	PluginDataDir string
	// CDIDir is where the CDI specs of prepared claims are written.
	// Empty selects the CDI directory runtimes watch for generated specs.
	CDIDir string
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
	cdi           *cdiStore

	mu      sync.Mutex // guards helper and devices
	helper  *kubeletplugin.Helper
	devices []resourceapi.Device // the devices published last

	// allowed gates the claim handlers. It is set once, when the owner has
	// synchronized with the container runtime, and never cleared: losing the
	// runtime connection takes the whole process down with it.
	allowed atomic.Bool
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

	store, err := newCDIStore(driverName, opts.CDIDir)
	if err != nil {
		return nil, err
	}

	return &Plugin{
		driverName:    driverName,
		nodeName:      opts.NodeName,
		kubeClient:    opts.KubeClient,
		owner:         opts.Owner,
		policy:        opts.Policy,
		registrarDir:  opts.RegistrarDir,
		pluginDataDir: pluginDataDir,
		cdi:           store,
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

	// This publishes nothing yet, it starts the controller which does. Actual
	// publication failures turn up asynchronously in HandleError, which retries
	// them. Failing here means we could not get as far as trying.
	if err := helper.PublishResources(ctx, p.driverResources()); err != nil {
		helper.Stop()
		return fmt.Errorf("dra: failed to start publishing resources for driver %q: %w",
			p.driverName, err)
	}

	p.helper = helper

	return nil
}

// AllowClaims lets the plugin serve claims. The driver is registered with the
// kubelet as soon as the plugin starts, but a claim must not be allocated while
// the owner is still synchronizing its own state with the container runtime: the
// allocation would run concurrently with that synchronization, and a commit made
// during it does not stick. The owner calls this once it has synchronized.
//
// Until then claims are refused with an error, which costs nothing but a retry:
// the kubelet asks again, and a pod needing a claim cannot start before the
// containers already running have been synchronized anyway.
func (p *Plugin) AllowClaims() {
	if p == nil {
		return
	}

	p.allowed.Store(true)
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

// Publish replaces the published devices with the given ones. Devices published
// before the plugin is started get published when it starts.
func (p *Plugin) Publish(devices []resourceapi.Device) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.devices = make([]resourceapi.Device, len(devices))
	for i := range devices {
		devices[i].DeepCopyInto(&p.devices[i])
	}

	if p.helper == nil {
		return nil
	}

	// This only hands the devices over to the controller which publishes them.
	if err := p.helper.PublishResources(context.Background(), p.driverResources()); err != nil {
		return fmt.Errorf("dra: failed to publish resources for driver %q: %w", p.driverName, err)
	}

	return nil
}

// WatchHealthStatus declines to report device health. Our devices are the
// node's own CPUs and memory: the kubelet already knows whether the node is
// healthy, and there is nothing per-device we could tell it that it does not
// know. Declining makes the kubelet stop asking.
func (p *Plugin) WatchHealthStatus(_ context.Context, _ chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
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
// published last, in a single pool named after our node. The devices are
// passed on as the policy described them, attributes and all: what they mean
// is the policy's business, not ours.
//
// A ResourceSlice holds at most ResourceSliceMaxDevices devices and the helper
// leaves the splitting to us, so the devices are chunked to that limit. An
// oversized slice would be rejected by the apiserver, and the helper treats
// that as recoverable and retries it forever, so it would never get published.
//
// A policy with no devices gets an empty slice published, not no slice at all.
// That states that the driver is alive and currently offers nothing, and it
// replaces whatever we published before, so a policy which stops offering
// devices does not leave a stale slice behind.
func (p *Plugin) driverResources() resourceslice.DriverResources {
	var published []resourceslice.Slice
	for chunk := range slices.Chunk(p.devices, resourceapi.ResourceSliceMaxDevices) {
		published = append(published, resourceslice.Slice{Devices: chunk})
	}
	if published == nil {
		published = []resourceslice.Slice{{}}
	}

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.nodeName: {
				Slices: published,
			},
		},
	}
}
