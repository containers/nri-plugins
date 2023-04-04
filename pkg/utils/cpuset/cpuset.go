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

package cpuset

import (
	"fmt"

	"k8s.io/utils/cpuset"
)

// CPUSet is an alias for k8s.io/utils/cpuset.CPUSet.
type CPUSet = cpuset.CPUSet

var (
	// New is an alias for cpuset.New.
	New = cpuset.New
	// Parse is an alias for cpuset.Parse.
	Parse = cpuset.Parse
)

// MustParse panics if parsing the given cpuset string fails.
func MustParse(s string) cpuset.CPUSet {
	cset, err := cpuset.Parse(s)
	if err != nil {
		panic(fmt.Errorf("failed to parse CPUSet %s: %w", s, err))
	}
	return cset
}
