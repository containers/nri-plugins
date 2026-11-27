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

package config

import (
	"fmt"
)

// PoolOpaque decodes into out the object of the controller key in
// pool.opaque of the resolved configuration, strictly, and reports whether
// there is one. The objects of other keys are ignored.
//   - out: a pointer to the controller's struct
func PoolOpaque(r *Resolved, key string, out any) (bool, error) {
	if r == nil || r.Config.Pool == nil {
		return false, nil
	}
	raw, ok := r.Config.Pool.Opaque[key]
	if !ok {
		return false, nil
	}
	if err := strictUnmarshal(raw, out); err != nil {
		return true, fmt.Errorf("%spool.opaque.%s: %w", errPrefix, key, err)
	}
	return true, nil
}
