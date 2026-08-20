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

// Package dra provides a policy-agnostic DRA (Dynamic Resource Allocation)
// kubelet plugin used by nri-plugins policies.
//
// This package must never import from github.com/containers/nri-plugins/cmd/
// so that it can be consumed by any policy binary without introducing import
// cycles. See docs/dra/design.md resolved decision 6 for the code-sharing
// verification checklist.
package dra
