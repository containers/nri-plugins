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

package engine

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// realisePoolRam realises a pool grant of volatile memory: the region of
// the device with its Whole device, kmem with its memory online movable in
// mode numa (the node fenced first, and again when the node is known only
// after onlining), device_dax in mode dax (never onlined).
func (e *Engine) realisePoolRam(ctx context.Context, req *GrantRequest, memdev string) error {
	region, err := e.ensureRegion(ctx, req, memdev, state.RegionRAM, 0)
	if err != nil {
		return err
	}
	driver := kernel.DriverDeviceDax
	if req.Asked.Mode == state.ModeNUMA {
		driver = kernel.DriverKmem
	}
	dax, err := e.wholeDevice(ctx, req, region, driver)
	if err != nil {
		return err
	}
	got := state.Got{Region: state.RegionRef(region), Dax: dax}
	var dev state.DevNumbers
	if req.Asked.Mode == state.ModeNUMA {
		node, err := e.onlineRegion(ctx, req, memdev, region, dax)
		if err != nil {
			return err
		}
		got.NUMANode = state.NUMANodePtr(node)
	} else {
		if d := e.k.DaxDriver(dax); d != kernel.DriverDeviceDax {
			return fmt.Errorf("%s: %s is bound to %q, not %s", memdev, dax, d, kernel.DriverDeviceDax)
		}
		major, minor, err := e.k.DaxDevNumbers(dax)
		if err != nil {
			return err
		}
		dev = state.DevNumbers{Major: major, Minor: minor}
	}
	got.Size = req.Asked.Size
	return e.realised(ctx, req, got, dev)
}

// onlineRegion fences the node of the memory of a region of mode numa,
// onlines its memory blocks movable and returns its node.
func (e *Engine) onlineRegion(ctx context.Context, req *GrantRequest, memdev, region, dax string) (int, error) {
	fenced := e.k.TargetNode(dax)
	e.fence(ctx, fenced)
	blocks, err := e.k.RegionBlocks(region)
	if err != nil {
		return -1, err
	}
	if len(blocks) == 0 {
		return -1, fmt.Errorf("%s: no memory blocks found for %s", memdev, region)
	}
	node, err := e.onlineMemory(ctx, region, dax, blocks, fenced, func(node int) {
		klog.V(3).Infof("engine: %s (%s): NUMA node %d of %s known only after onlining, fencing it late", memdevText(req.Asked.Source, req.Serial), memdev, node, region)
	})
	if err == nil && node < 0 {
		err = fmt.Errorf("%s: cannot find the NUMA node of %s", memdev, region)
	}
	return node, err
}
