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

// Package dropin checks that pkg/lib/hardware/system is a drop-in
// replacement for pkg/sysfs, by compiling one body of code against both.
//
// viasysfs/api.go and viasystem/api.go reference every exported pkg/sysfs
// symbol. They differ only in the import line and the package clause. Building
// them both proves the two packages present the same surface; the test below
// proves the two files really are the same code, so that the check cannot be
// quietly weakened by editing one of them.

package dropin
