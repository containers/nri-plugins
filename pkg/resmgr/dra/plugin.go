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
	"errors"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
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
