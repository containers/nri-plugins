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

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/containers/nri-plugins/pkg/log"
)

var errNotImplemented = errors.New("dra plugin: not yet implemented")

// Plugin is the DRA kubelet plugin. Its fields and methods are added
// incrementally in subsequent plan steps.
type Plugin struct {
	helper *kubeletplugin.Helper
}

// New constructs a Plugin with the given driver name and dependencies.
// Returns ErrNotImplemented until the implementation is complete.
func New(driverName string, deps Deps) (*Plugin, error) {
	return nil, errNotImplemented
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
// The logger used is the package default until Task 4 wires in deps.Logger.
func (p *Plugin) HandleError(_ context.Context, err error, msg string) {
	logger := log.Default()
	if errors.Is(err, kubeletplugin.ErrRecoverable) {
		logger.Warnf("%s: %v", msg, err)
	} else {
		logger.Errorf("%s: %v", msg, err)
	}
}
