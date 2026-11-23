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

package state

import (
	"fmt"
	"strconv"

	"github.com/containers/nri-plugins/pkg/cxl/api"
)

// Serial is the serial of a memdev (/sys/bus/cxl/devices/memN/serial), the
// identity of a memdev in every record. Its text is 0x<lower-case hex>.
type Serial uint64

// String returns the serial as 0x<lower-case hex>.
func (s Serial) String() string {
	return api.FormatSerial(uint64(s))
}

// Hex returns the serial as lower-case hex without 0x.
func (s Serial) Hex() string {
	return strconv.FormatUint(uint64(s), 16)
}

// MarshalText returns the serial as 0x<lower-case hex>.
func (s Serial) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// UnmarshalText sets the serial from 0x hex or decimal text.
func (s *Serial) UnmarshalText(text []byte) error {
	v, err := api.ParseSerial(string(text))
	if err != nil {
		return err
	}
	*s = Serial(v)
	return nil
}

// Source is who provides the memory of a memdev: api.SourceLocal or
// api.SourcePool.
type Source string

const (
	SourceLocal Source = api.SourceLocal
	SourcePool  Source = api.SourcePool
)

// Mode is how a container gets the memory of a grant: api.ModeNUMA,
// api.ModeDax, api.ModeBlock or api.ModeFS.
type Mode string

const (
	ModeNUMA  Mode = api.ModeNUMA
	ModeDax   Mode = api.ModeDax
	ModeBlock Mode = api.ModeBlock
	ModeFS    Mode = api.ModeFS
)

// Access is how a node mount maps its file pages: api.AccessDirect or
// api.AccessCached.
type Access string

const (
	AccessDirect Access = api.AccessDirect
	AccessCached Access = api.AccessCached
)

// GrantRef identifies a grant: the claim UID and the index of the
// allocation result in the claim (the n of the CDI device claim-<uid>-<n>).
// The grants of a lease are those of its companion claim.
type GrantRef struct {
	Claim  string `json:"claim"`
	Result int    `json:"result"`
}

// String returns the reference as <claim uid>/<n>.
func (r GrantRef) String() string {
	return fmt.Sprintf("%s/%d", r.Claim, r.Result)
}

// Compare returns -1, 0 or 1 as r sorts before, with or after o.
func (r GrantRef) Compare(o GrantRef) int {
	switch {
	case r.Claim < o.Claim:
		return -1
	case r.Claim > o.Claim:
		return 1
	case r.Result < o.Result:
		return -1
	case r.Result > o.Result:
		return 1
	}
	return 0
}

// RegionRef identifies a Region record: the name of the region (regionN),
// or the noted name while the region is being created.
type RegionRef string

// PodRef identifies a pod.
type PodRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// DevNumbers are the major and minor numbers of a device node.
type DevNumbers struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

// IsZero tells whether the numbers are unknown.
func (d DevNumbers) IsZero() bool {
	return d.Major == 0 && d.Minor == 0
}

// NUMANodeOf returns the NUMA node p points to, -1 for none.
func NUMANodeOf(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// NUMANodePtr returns a pointer to the NUMA node n, nil for a negative n.
func NUMANodePtr(n int) *int {
	if n < 0 {
		return nil
	}
	return &n
}
