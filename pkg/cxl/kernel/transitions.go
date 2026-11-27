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

package kernel

import (
	"fmt"
)

// The states of the kernel objects are the kernel's: this package observes
// them, and its operations or kernel events change them. Every object with
// a state has an observed Table of the transitions that the kernel makes,
// next to its state type; an observed change that the table does not have
// is a fact that the design does not know (Table.Check with
// fsm.AnyTrigger).

// stateText returns the name of a state, or "state(n)" for a value without
// a name.
func stateText(names []string, s int) string {
	if s >= 0 && s < len(names) {
		return names[s]
	}
	return fmt.Sprintf("state(%d)", s)
}
