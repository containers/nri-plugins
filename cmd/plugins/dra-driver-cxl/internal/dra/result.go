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
	"fmt"
	"path/filepath"
	"slices"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/lend"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// The results of a preparation as the readers of the claims get them,
// derived from the records of either driver: the PrepareResult of kubelet,
// a state.DeviceInfo per result for the CDI spec, the lease broker and the
// NRI plugin, and the lend of a companion claim.

// ClaimInfo is a prepared claim with its devices.
type ClaimInfo struct {
	ClaimName string // namespace/name
	Devices   []state.DeviceInfo
	// Lend is the lend of a companion claim.
	Lend *lend.Companion
}

// roots are the directories in the paths of the results: the sysfs mount
// point and the directory of the device nodes.
type roots struct {
	sys, dev string
}

// newRoots returns the roots sys and dev, "/sys" and "/dev" for "".
func newRoots(sys, dev string) roots {
	if sys == "" {
		sys = "/sys"
	}
	if dev == "" {
		dev = "/dev"
	}
	return roots{sys: sys, dev: dev}
}

// prepareResult returns the PrepareResult of a preparation for kubelet,
// with CDI devices of the CDI kind kind; a companion claim has no CDI
// devices.
func prepareResult(p *state.Preparation, kind string) kubeletplugin.PrepareResult {
	res := kubeletplugin.PrepareResult{Devices: []kubeletplugin.Device{}}
	for _, r := range p.Results {
		d := kubeletplugin.Device{
			Requests:   []string{r.Request},
			PoolName:   r.Pool,
			DeviceName: r.Device,
		}
		if p.Lease == "" {
			d.CDIDeviceIDs = []string{kind + "=" + ClaimCDIDeviceName(p.UID, r.Index)}
			if r.Kind == state.ResultLeaseClaim {
				d.CDIDeviceIDs = append(d.CDIDeviceIDs, kind+"="+leaseCDIDeviceName(p.UID))
			}
		}
		if r.ShareID != "" {
			id := types.UID(r.ShareID)
			d.ShareID = &id
		}
		res.Devices = append(res.Devices, d)
	}
	return res
}

// claimInfo returns a preparation in any state with its devices and its
// lend.
func claimInfo(doc *store.Document, p *state.Preparation, rs roots) *ClaimInfo {
	info := &ClaimInfo{ClaimName: p.ClaimName, Devices: []state.DeviceInfo{}, Lend: lendOf(doc, p)}
	for i := range p.Results {
		info.Devices = append(info.Devices, deviceInfo(doc, &p.Results[i], rs))
	}
	return info
}

// deviceInfo returns a result of a preparation as a state.DeviceInfo; a
// result whose grant is gone gives only its result fields.
func deviceInfo(doc *store.Document, r *state.Result, rs roots) state.DeviceInfo {
	d := state.DeviceInfo{RequestName: r.Request, DeviceName: r.Device, Result: r.Index, ResultSettings: r.Settings}
	switch r.Kind {
	case state.ResultRegion:
		d.DeviceType, d.Mode = device.DeviceTypeRegion, api.ModeNUMA
		d.Region = r.Region.Name
		d.SysfsPath = regionPath(rs, r.Region.Name)
		d.NUMANodes = []int{r.Region.NUMANode}
		d.NUMAAffinities = slices.Clone(r.Region.NUMAAffinity)
		d.TotalBytes, d.ConsumedBytes = r.Region.Size, r.Region.Consumed
	case state.ResultDRAM:
		d.DeviceType, d.Mode = device.DeviceTypeDRAM, api.ModeNUMA
		d.NUMANodes = slices.Clone(r.DRAM.NUMANodes)
		d.TotalBytes, d.ConsumedBytes = r.DRAM.Size, r.DRAM.Consumed
	case state.ResultLeaseClaim:
		d.DeviceType = device.DeviceTypeLease
		d.ConsumedBytes = r.LeaseClaim.Consumed
	case state.ResultGrant:
		d.DeviceType = device.DeviceTypeMemdev
		if g := doc.Grant(*r.Grant); g != nil {
			grantInfo(doc, g, &d, rs)
		}
	}
	return d
}

// grantInfo sets what a grant got in the device info of its result.
func grantInfo(doc *store.Document, g *state.Grant, d *state.DeviceInfo, rs roots) {
	d.Mode = string(g.Asked.Mode)
	d.Serial = g.Serial
	d.SysfsPath = regionPath(rs, string(g.Got.Region))
	d.TotalBytes, d.ConsumedBytes = g.Got.Size, int64(g.Got.Size)
	d.NUMAAffinities = numaAffinity(doc, g)
	d.Shared = sharedGrant(g)
	switch g.Asked.Mode {
	case state.ModeNUMA:
		if g.Got.NUMANode != nil {
			d.NUMANodes = []int{*g.Got.NUMANode}
		}
	case state.ModeDax:
		if g.Got.Dax != "" {
			dev := doc.DaxDevOf(g)
			d.DaxDevice = filepath.Join(rs.dev, g.Got.Dax)
			d.DaxMajor, d.DaxMinor = dev.Major, dev.Minor
		}
		d.ReadOnly = g.Asked.ReadOnly
	case state.ModeBlock, state.ModeFS:
		d.Persistent = true
		d.ReadOnly = g.Asked.ReadOnly
		if bd := g.Got.BlockDevice; bd != nil {
			d.BlockDevice, d.BlockMajor, d.BlockMinor = bd.Name, bd.Dev.Major, bd.Dev.Minor
		}
		if g.Asked.Mode == state.ModeFS {
			d.Access, d.Path = string(g.Got.Mount), g.Asked.Path
			if m := doc.MountOf(g); m != nil {
				d.NodeMount, d.FSType = m.Path, m.FSType
			}
		}
	}
}

// sharedGrant tells whether a grant maps its memdev shared.
func sharedGrant(g *state.Grant) bool {
	return g.Asked.Shared || g.Asked.Kind.Shared()
}

// numaAffinity returns the CPU NUMA nodes of the local memdev of a grant,
// nil if they are not known.
func numaAffinity(doc *store.Document, g *state.Grant) []int {
	if g.Asked.Source != state.SourceLocal {
		return nil
	}
	if m := doc.Memdev(g.Serial); m != nil && m.Facts.NUMAAffinity != nil {
		return []int{*m.Facts.NUMAAffinity}
	}
	return nil
}

// regionPath returns the sysfs path of a CXL region, "" for none.
func regionPath(rs roots, name string) string {
	if name == "" {
		return ""
	}
	return filepath.Join(rs.sys, "bus", "cxl", "devices", name)
}

// lendOf returns the lend of a companion claim, nil for another claim: the
// lease and the requester of the first grant with a lend, the lent NUMA
// nodes of its grants in their order, and their lease binds.
func lendOf(doc *store.Document, p *state.Preparation) *lend.Companion {
	if p.Lease == "" {
		return nil
	}
	v := &lend.Companion{Lease: p.Lease}
	identity := false
	for _, r := range p.Results {
		if r.Grant == nil {
			continue
		}
		g := doc.Grant(*r.Grant)
		if g == nil {
			continue
		}
		l := g.Got.Lend
		if l == nil {
			l = g.Intent.Lend
		}
		if l != nil && !identity {
			v.Pod, v.Container, v.FSDir, v.Name = l.Pod, l.Container, l.FSDir, l.Name
			identity = true
		}
		if g.Got.Lend != nil && g.Got.Lend.NUMANode != nil {
			v.NUMANodes = append(v.NUMANodes, *g.Got.Lend.NUMANode)
		}
		if b := g.Got.Bind; b != nil && (b.State == state.BindBound || b.State == state.BindStale) && !slices.Contains(v.Mounts, b.Target) {
			v.Mounts = append(v.Mounts, b.Target)
		}
	}
	return v
}

// deviceMemory returns what a prepared memdev gives a container,
// with its env key from keys, and false for the lease device.
func deviceMemory(d *state.DeviceInfo, keys envKeys) (api.Memory, bool) {
	m := api.Memory{Mode: d.Mode, Size: d.TotalBytes, NUMANode: -1}
	if d.ConsumedBytes > 0 {
		m.Size = uint64(d.ConsumedBytes)
	}
	if len(d.NUMANodes) > 0 {
		m.NUMANode = d.NUMANodes[0]
	}
	switch d.DeviceType {
	case device.DeviceTypeMemdev:
		if d.Serial == 0 {
			return m, false
		}
		m.Key, m.Serial, m.Size = keys.next(api.Key(uint64(d.Serial), 1)), d.Serial.String(), d.TotalBytes
		m.Dax, m.ReadOnly, m.Shared = d.DaxDevice, d.ReadOnly, d.Shared
		switch {
		case d.Persistent && d.Mode == api.ModeBlock:
			m.Path = d.BlockDevice
		case d.Persistent && d.Mode == api.ModeFS:
			m.Path, m.Access, m.FSType = d.Path, d.Access, d.FSType
		}
	case device.DeviceTypeRegion:
		m.Key = keys.next(api.RegionKey(d.Region))
	case device.DeviceTypeDRAM:
		m.Key, m.NUMANodes = keys.next(api.DRAMKey), cpuset.New(d.NUMANodes...).String()
	default:
		return m, false
	}
	return m, true
}

// deviceNUMANode returns the NUMA node of a memdev in mode numa, -1 for
// another device.
func deviceNUMANode(d *state.DeviceInfo) int {
	if d.DeviceType == device.DeviceTypeMemdev && d.Mode == api.ModeNUMA && len(d.NUMANodes) > 0 {
		return d.NUMANodes[0]
	}
	return -1
}

// errNoRecord is the error of a claim whose preparation is gone.
func errNoRecord(uid string) error {
	return fmt.Errorf("claim %s has no record", uid)
}
