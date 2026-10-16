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

package cxl

import "errors"

var (
	// ErrMissingFile is returned when a sysfs or procfs file required by a scan
	// does not exist. The returned error also matches fs.ErrNotExist.
	ErrMissingFile = errors.New("cxl: missing file")
	// ErrUnreadableFile is returned when a required file exists but cannot be read.
	ErrUnreadableFile = errors.New("cxl: unreadable file")
	// ErrMissingLine is returned when a file exists and is readable, but contains
	// no line in the expected format.
	ErrMissingLine = errors.New("cxl: missing line")
)
