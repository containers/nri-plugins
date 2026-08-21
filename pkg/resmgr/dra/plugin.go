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

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

var errNotImplemented = errors.New("dra plugin: not yet implemented")

// Plugin is the DRA kubelet plugin.
type Plugin struct {
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

// HandleError handles background errors from the kubelet plugin helper.
// Recoverable errors (errors.Is(err, kubeletplugin.ErrRecoverable)) are
// logged at Warn level; all other errors are logged at Error level.
func (p *Plugin) HandleError(_ context.Context, err error, msg string) {
	logger := p.deps.Logger
	if errors.Is(err, kubeletplugin.ErrRecoverable) {
		logger.Warnf("%s: %v", msg, err)
	} else {
		logger.Errorf("%s: %v", msg, err)
	}
}
