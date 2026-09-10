// Copyright 2020 Intel Corporation. All Rights Reserved.
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

package sysfs

import (
	"github.com/containers/nri-plugins/pkg/utils/parse"
)

// ParseFileEntries and the key/value file parsing behind it have nothing to do
// with topology; they live in pkg/utils now. These are kept so that this
// package's interface is unchanged for as long as it is still here.

// PickEntryFn picks a given input line apart into an entry of key and value.
type PickEntryFn = parse.PickEntryFn

// ParseFileEntries parses a sysfs files for the given entries.
func ParseFileEntries(path string, values map[string]any, pickFn PickEntryFn) error {
	return parse.FileEntries(path, values, pickFn)
}
