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

package libmem

import "fmt"

var (
	ErrFailedOption    = fmt.Errorf("libmem: failed to apply option")
	ErrInvalidType     = fmt.Errorf("libmem: invalid type")
	ErrInvalidNode     = fmt.Errorf("libmem: invalid node")
	ErrInvalidNodeMask = fmt.Errorf("libmem: invalid NodeMask")
	ErrExpiredOffer    = fmt.Errorf("libmem: expired offer")
	ErrUnknownRequest  = fmt.Errorf("libmem: unknown allocation")
	ErrAlreadyExists   = fmt.Errorf("libmem: allocation already exists")
	ErrNoMem           = fmt.Errorf("libmem: insufficient available memory")
	ErrNoZone          = fmt.Errorf("libmem: failed to find zone")
	ErrInternalError   = fmt.Errorf("libmem: internal error")
)
