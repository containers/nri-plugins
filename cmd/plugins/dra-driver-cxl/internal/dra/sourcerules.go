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

package dra

import (
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// SourceRules are what the two DRA drivers of the process have differently:
// the name of the driver, the source of its devices and the directory of
// their node mounts. The name is the vendor of the driver's CDI kind. The
// source decides how an allocation result maps to a memdev and a grant
// (Plugin.validate), whether the driver publishes the node's ResourceSlices,
// and its CDI spec name.
type SourceRules struct {
	// Name is the DRA driver name.
	Name string
	// Source is the source of the devices of the driver's grants.
	Source state.Source
	// MountsDir is the directory of the node mounts of mode fs of the
	// driver's devices.
	MountsDir string
}

// localRules returns the rules of cxl.nri.io: the local memdevs of the node,
// found in the last scan; node mounts in mountsDir.
func localRules(mountsDir string) SourceRules {
	return SourceRules{Name: device.DriverName, Source: state.SourceLocal, MountsDir: mountsDir}
}

// poolRules returns the rules of the pool driver name: the devices that a
// pool controller attached to the node for a claim, found by the serial of
// the claim's status data; node mounts in mountsDir.
func poolRules(name, mountsDir string) SourceRules {
	return SourceRules{Name: name, Source: state.SourcePool, MountsDir: mountsDir}
}

// publishes tells whether the driver publishes the node's ResourceSlices:
// cxl.nri.io does, the pool driver does not.
func (r SourceRules) publishes() bool {
	return r.Source == state.SourceLocal
}

// CDIKind returns the kind of the CDI devices of the driver.
func (r SourceRules) CDIKind() string {
	return device.CDIKind(r.Name)
}

// CDISpecName returns the name of the CDI spec file of the driver.
func (r SourceRules) CDISpecName() string {
	if r.Source == state.SourceLocal {
		return device.ClaimsCDISpecName
	}
	return device.PoolCDISpecName
}

// serialsOf returns the serials of the grants of a preparation, for the log
// lines.
func serialsOf(r *state.Records, prep *state.Preparation) []string {
	var serials []string
	for _, res := range prep.Results {
		if g := grantOf(r, &res); g != nil {
			serials = append(serials, api.FormatSerial(uint64(g.Serial)))
		}
	}
	return serials
}
