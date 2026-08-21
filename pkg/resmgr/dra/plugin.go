/*
Copyright The NRI Plugins Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-logr/logr"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
)

var errNotImplemented = errors.New("dra plugin: not yet implemented")

// Plugin is the DRA kubelet plugin.
type Plugin struct {
	mu         sync.Mutex
	driverName string
	deps       Deps
	helper     *kubeletplugin.Helper
}

// New constructs a Plugin with the given driver name and dependencies.
// Returns an error if any required dependency is missing.
func New(driverName string, deps Deps) (*Plugin, error) {
	if driverName == "" {
		return nil, fmt.Errorf("dra plugin: driverName must not be empty")
	}
	if deps.KubeClient == nil {
		return nil, fmt.Errorf("dra plugin: KubeClient must not be nil")
	}
	if deps.NodeName == "" {
		return nil, fmt.Errorf("dra plugin: NodeName must not be empty")
	}
	if deps.ValidateClasses == nil {
		return nil, fmt.Errorf("dra plugin: ValidateClasses must not be nil")
	}
	if deps.DeviceLister == nil {
		return nil, fmt.Errorf("dra plugin: DeviceLister must not be nil")
	}
	if deps.Logger == nil {
		return nil, fmt.Errorf("dra plugin: Logger must not be nil")
	}
	return &Plugin{driverName: driverName, deps: deps}, nil
}

// PrepareResourceClaims is a stub that satisfies kubeletplugin.DRAPlugin.
// Real allocation logic is added in Step 7.
func (p *Plugin) PrepareResourceClaims(_ context.Context, _ []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	return nil, errNotImplemented
}

// UnprepareResourceClaims is a stub that satisfies kubeletplugin.DRAPlugin.
// Real deallocation logic is added in Step 7.
func (p *Plugin) UnprepareResourceClaims(_ context.Context, _ []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	return nil, errNotImplemented
}

// Start registers this plugin with the kubelet and begins serving DRA
// requests. It validates cpuClass configuration, creates the plugin data
// directory, injects a logr.Logger into the context, and calls
// kubeletplugin.Start. Returns an error if the plugin is already started,
// if ValidateClasses fails, or if the kubelet plugin cannot be started.
func (p *Plugin) Start(ctx context.Context) error {
	p.mu.Lock()
	alreadyStarted := p.helper != nil
	p.mu.Unlock()
	if alreadyStarted {
		return fmt.Errorf("dra plugin: already started")
	}
	if err := p.deps.ValidateClasses(); err != nil {
		return fmt.Errorf("dra plugin: ValidateClasses failed: %w", err)
	}
	// Resolve the plugin data directory default before creating it: an empty
	// string passed to os.MkdirAll would fail immediately.
	pluginDataDir := p.deps.PluginDataDir
	if pluginDataDir == "" {
		pluginDataDir = filepath.Join(kubeletplugin.KubeletPluginsDir, p.driverName)
	}
	if err := os.MkdirAll(pluginDataDir, 0750); err != nil {
		return fmt.Errorf("dra plugin: create plugin data dir %q: %w", pluginDataDir, err)
	}
	ctx = logr.NewContext(ctx, newLogr(p.deps.Logger))
	opts := []kubeletplugin.Option{
		kubeletplugin.DriverName(p.driverName),
		kubeletplugin.KubeClient(p.deps.KubeClient),
		kubeletplugin.NodeName(p.deps.NodeName),
		kubeletplugin.PluginDataDirectoryPath(pluginDataDir),
		kubeletplugin.GRPCVerbosity(-1),
	}
	// Only override the registrar directory when explicitly set; passing an
	// empty string would clobber kubeletplugin's built-in KubeletRegistryDir
	// default (the option setter stores the value unconditionally).
	if p.deps.RegistrarDir != "" {
		opts = append(opts, kubeletplugin.RegistrarDirectoryPath(p.deps.RegistrarDir))
	}
	helper, err := kubeletplugin.Start(ctx, p, opts...)
	if err != nil {
		return fmt.Errorf("dra plugin: kubeletplugin.Start: %w", err)
	}
	p.mu.Lock()
	p.helper = helper
	p.mu.Unlock()
	return nil
}

// Stop shuts down the kubelet plugin and releases resources. It is
// idempotent: calling Stop on an already-stopped Plugin is safe.
func (p *Plugin) Stop() {
	p.mu.Lock()
	h := p.helper
	p.helper = nil
	p.mu.Unlock()
	if h != nil {
		h.Stop()
	}
}

// PublishResources validates classes, lists DRA devices, paginates them into
// ResourceSlice objects (at most resourceapi.ResourceSliceMaxDevices per
// slice), and hands the resulting DriverResources to the helper for
// publishing. Even zero devices produce one empty slice so the pool remains
// visible. Returns an error if the plugin has not been started yet.
func (p *Plugin) PublishResources(ctx context.Context) error {
	if err := p.deps.ValidateClasses(); err != nil {
		return fmt.Errorf("dra plugin: ValidateClasses failed: %w", err)
	}
	p.mu.Lock()
	h := p.helper
	p.mu.Unlock()
	if h == nil {
		return fmt.Errorf("dra plugin: PublishResources called before Start")
	}
	devices, err := p.deps.DeviceLister.DRADevices(p.driverName)
	if err != nil {
		return fmt.Errorf("dra plugin: DRADevices: %w", err)
	}
	resources := buildDriverResources(p.deps.NodeName, devices)
	if err := h.PublishResources(ctx, resources); err != nil {
		return fmt.Errorf("dra plugin: helper.PublishResources: %w", err)
	}
	return nil
}

// buildDriverResources paginates devices into ResourceSlice objects and
// returns a DriverResources ready for Helper.PublishResources. The pool name
// is the node name. At most resourceapi.ResourceSliceMaxDevices devices are
// placed per slice; even zero devices produce one empty slice.
func buildDriverResources(nodeName string, devices []resourceapi.Device) resourceslice.DriverResources {
	maxPerSlice := resourceapi.ResourceSliceMaxDevices
	var slices []resourceslice.Slice
	if len(devices) == 0 {
		slices = []resourceslice.Slice{{}}
	} else {
		for i := 0; i < len(devices); i += maxPerSlice {
			end := i + maxPerSlice
			if end > len(devices) {
				end = len(devices)
			}
			slices = append(slices, resourceslice.Slice{
				Devices: devices[i:end],
			})
		}
	}
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {Slices: slices},
		},
	}
}

// HandleError handles background errors from the kubelet plugin helper.
// Recoverable errors (errors.Is(err, kubeletplugin.ErrRecoverable)) are
// logged at Warn level; all other errors are logged at Error level.
func (p *Plugin) HandleError(_ context.Context, err error, msg string) {
	if errors.Is(err, kubeletplugin.ErrRecoverable) {
		p.deps.Logger.Warnf("%s: %v", msg, err)
	} else {
		p.deps.Logger.Errorf("%s: %v", msg, err)
	}
}
