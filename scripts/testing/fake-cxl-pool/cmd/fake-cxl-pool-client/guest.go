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

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// The guest commands prepare and disable hotplugged CXL memory in the VM
// through package kernel. They bind dax devices through sysfs only, never
// with daxctl.

// settleOffline is how long the memory blocks of a new region must stay
// offline before its dax device is switched to device_dax: a udev rule may
// online hot-added blocks again (a variable for tests).
var settleOffline = 500 * time.Millisecond

// offlineTimeout bounds the offlining of the memory blocks of a new region.
const offlineTimeout = 30 * time.Second

// regionInfo describes the first region of a memdev. Mode is numa (kmem),
// dax (device_dax) or "" (no or another driver).
type regionInfo struct {
	Memdev string `json:"memdev"`
	Region string `json:"region"`
	Dax    string `json:"dax"`
	Driver string `json:"driver"`
	Mode   string `json:"mode"`
	Node   int    `json:"node"`
	Device string `json:"device,omitempty"` // /dev/daxX.Y in mode dax
	Blocks []int  `json:"blocks,omitempty"`
}

func (c *cli) cmdGuest(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("guest wait|memdev|region|online|info|disable ...")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "-h", "-help", "--help", "help":
		_, _ = fmt.Fprint(c.stdout, usage)
		return nil
	case "wait":
		fs := c.flagSet("guest wait")
		timeout := fs.Duration("timeout", 30*time.Second, "timeout")
		pos, err := parse(fs, args)
		if err != nil || len(pos) != 1 {
			return firstErr(err, usagef("guest wait DEVICE [--timeout 30s]"))
		}
		serial, err := c.deviceSerial(ctx, pos[0])
		if err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		m, err := c.kernel().WaitMemdev(wctx, serial, kernel.MemdevBound)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(c.stdout, m)
		return nil
	case "memdev":
		fs := c.flagSet("guest memdev")
		pos, err := parse(fs, args)
		if err != nil || len(pos) != 1 {
			return firstErr(err, usagef("guest memdev DEVICE"))
		}
		m, _, err := c.guestMemdev(ctx, pos[0])
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(c.stdout, m)
		return nil
	case "region":
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			_, _ = fmt.Fprint(c.stdout, usage)
			return nil
		}
		if len(args) == 0 || args[0] != "create" {
			return usagef("guest region create DEVICE --mode numa|dax [--decoder decoderX.Y]")
		}
		fs := c.flagSet("guest region create")
		mode := fs.String("mode", "", "numa or dax")
		decoder := fs.String("decoder", "", "root decoder (default: the one of the device's host bridge)")
		timeout := fs.Duration("timeout", 30*time.Second, "timeout")
		pos, err := parse(fs, args[1:])
		if err != nil || len(pos) != 1 || *mode == "" {
			return firstErr(err, usagef("guest region create DEVICE --mode numa|dax [--decoder decoderX.Y]"))
		}
		if *mode != cxlapi.ModeNUMA && *mode != cxlapi.ModeDax {
			return usagef("invalid mode %q (%s or %s)", *mode, cxlapi.ModeNUMA, cxlapi.ModeDax)
		}
		m, serial, err := c.guestMemdev(ctx, pos[0])
		if err != nil {
			return err
		}
		rctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		if err := c.createRegion(rctx, m, *mode, *decoder); err != nil {
			return err
		}
		ri, err := c.regionInfo(ctx, serial)
		if err != nil {
			return err
		}
		return c.printRegionInfo(ri)
	case "online":
		fs := c.flagSet("guest online")
		movable := fs.Bool("movable", false, "online_movable")
		pos, err := parse(fs, args)
		if err != nil || len(pos) != 1 {
			return firstErr(err, usagef("guest online DEVICE [--movable]"))
		}
		serial, err := c.deviceSerial(ctx, pos[0])
		if err != nil {
			return err
		}
		ri, err := c.online(ctx, serial, *movable)
		if err != nil {
			return err
		}
		return c.printRegionInfo(ri)
	case "info":
		fs := c.flagSet("guest info")
		pos, err := parse(fs, args)
		if err != nil || len(pos) != 1 {
			return firstErr(err, usagef("guest info DEVICE"))
		}
		serial, err := c.deviceSerial(ctx, pos[0])
		if err != nil {
			return err
		}
		ri, err := c.regionInfo(ctx, serial)
		if err != nil {
			return err
		}
		return c.printRegionInfo(ri)
	case "disable":
		fs := c.flagSet("guest disable")
		pos, err := parse(fs, args)
		if err != nil || len(pos) != 1 {
			return firstErr(err, usagef("guest disable DEVICE"))
		}
		m, _, err := c.guestMemdev(ctx, pos[0])
		if errors.Is(err, kernel.ErrNotFound) {
			if !c.json() {
				_, _ = fmt.Fprintf(c.stdout, "%s: not present, nothing to disable\n", pos[0])
			}
			return nil
		}
		if err != nil {
			return err
		}
		if err := c.disable(ctx, m); err != nil {
			return err
		}
		if !c.json() {
			_, _ = fmt.Fprintf(c.stdout, "%s: disabled\n", m)
		}
		return nil
	}
	return usagef("unknown guest command %q", sub)
}

// deviceSerial returns the serial in dev (0x hex or decimal), or else the
// serial of the device named dev.
func (c *cli) deviceSerial(ctx context.Context, dev string) (uint64, error) {
	if strings.HasPrefix(dev, "0x") || strings.HasPrefix(dev, "0X") || isDecimal(dev) {
		return cxlapi.ParseSerial(dev)
	}
	d, err := c.client().Device(ctx, dev)
	if err != nil {
		return 0, err
	}
	return cxlapi.ParseSerial(d.Serial)
}

// isDecimal tells whether s is a non-empty string of decimal digits.
func isDecimal(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func (c *cli) printRegionInfo(ri *regionInfo) error {
	if c.json() {
		return c.printJSON(ri)
	}
	w := c.table()
	_, _ = fmt.Fprintln(w, "MEMDEV\tREGION\tDAX\tDRIVER\tMODE\tNODE\tDEVICE\tBLOCKS")
	blocks := ""
	if n := len(ri.Blocks); n > 0 {
		blocks = fmt.Sprintf("memory%d..memory%d", ri.Blocks[0], ri.Blocks[n-1])
	}
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", ri.Memdev, ri.Region, dash(ri.Dax), dash(ri.Driver), dash(ri.Mode), ri.Node, dash(ri.Device), dash(blocks))
	return w.Flush()
}

// kernel returns the kernel of the VM, which logs to stderr with -v.
func (c *cli) kernel() *kernel.Kernel {
	if c.k == nil {
		c.k = kernel.New()
		if c.verbose {
			c.k.Logf = c.logf
		}
	}
	return c.k
}

// logf writes a line to stderr with -v.
func (c *cli) logf(format string, args ...any) {
	if c.verbose {
		_, _ = fmt.Fprintf(c.stderr, format+"\n", args...)
	}
}

// guestMemdev returns the memdev (memN) of the device dev, an error of kind
// kernel.ErrNotFound if the VM has none, and its serial.
func (c *cli) guestMemdev(ctx context.Context, dev string) (string, uint64, error) {
	serial, err := c.deviceSerial(ctx, dev)
	if err != nil {
		return "", 0, err
	}
	memdev, err := c.kernel().FindMemdev(serial)
	return memdev, serial, err
}

// createRegion creates a volatile region on the memdev under the root
// decoder (the one of its host bridge if ""), or reuses its first region,
// and binds the dax device of the region for the mode: device_dax for mode
// dax (its memory never onlined), kmem for mode numa (its memory left
// offline).
func (c *cli) createRegion(ctx context.Context, memdev, mode, decoder string) error {
	k := c.kernel()
	if err := k.EnableMemdev(ctx, memdev); err != nil {
		return err
	}
	region := ""
	if rs := k.Regions(memdev); len(rs) > 0 {
		region = rs[0]
		c.logf("%s: reusing %s", memdev, region)
	} else {
		var err error
		if decoder == "" {
			if decoder, err = k.RootDecoder(memdev, kernel.RegionRAM); err != nil {
				return err
			}
		}
		req := kernel.CreateRegionRequest{Memdev: memdev, Decoder: decoder, Type: kernel.RegionRAM}
		if region, err = k.CreateRegion(ctx, req); err != nil {
			return err
		}
	}
	dax, err := k.WaitDaxRegion(ctx, region)
	if err != nil {
		return err
	}
	// The kernel binds the dax device to kmem by itself. Its driver link
	// exists before kmem has added the memory blocks: wait for them before
	// a switch of drivers.
	if k.DaxDriver(dax) == kernel.DriverKmem {
		if err := k.WaitRegionBlocks(ctx, region); err != nil {
			return err
		}
	}
	if mode == cxlapi.ModeNUMA {
		if err := k.BindDax(ctx, dax, kernel.DriverKmem); err != nil {
			return err
		}
		return k.WaitRegionBlocks(ctx, region)
	}
	if k.DaxDriver(dax) == kernel.DriverKmem {
		blocks, err := k.RegionBlocks(region)
		if err != nil {
			return err
		}
		if err := c.offlineSettled(ctx, blocks); err != nil {
			return err
		}
	}
	return k.BindDax(ctx, dax, kernel.DriverDeviceDax)
}

// offlineSettled offlines memory blocks until they stay offline for
// settleOffline, offlineTimeout at most.
func (c *cli) offlineSettled(ctx context.Context, blocks []int) error {
	k := c.kernel()
	deadline := time.Now().Add(offlineTimeout)
	for {
		if err := k.OfflineBlocks(blocks); err != nil {
			return err
		}
		b, err := k.StayedOffline(ctx, blocks, settleOffline)
		if err != nil || b < 0 {
			return err
		}
		c.logf("memory%d: online again after offlining it (auto-onlined by udev?)", b)
		if time.Now().After(deadline) {
			return fmt.Errorf("memory blocks %v keep coming online after offlining them: is a udev rule onlining hot-added memory?", blocks)
		}
	}
}

// regionInfo returns the first region of the memdev with the serial, its
// first dax device and the memory blocks of the region that exist, an info
// without a region for a memdev without one. It reads them with
// ObserveTopology.
func (c *cli) regionInfo(ctx context.Context, serial uint64) (*regionInfo, error) {
	k := c.kernel()
	snap, err := k.ObserveTopology(ctx)
	if err != nil {
		return nil, err
	}
	m := snap.Memdev(serial)
	if m.State == kernel.MemdevAbsent {
		return nil, fmt.Errorf("no memdev with the serial 0x%x", serial)
	}
	ri := &regionInfo{Memdev: m.Name, Node: -1}
	if len(m.Regions) == 0 {
		return ri, nil
	}
	r := snap.Region(m.Regions[0])
	ri.Region = r.Name
	if ds := snap.DaxDevicesOf(r.Name); len(ds) > 0 {
		ri.Dax, ri.Driver, ri.Node = ds[0].Name, ds[0].Driver, ds[0].TargetNode
	}
	switch ri.Driver {
	case kernel.DriverDeviceDax:
		ri.Mode, ri.Device = cxlapi.ModeDax, filepath.Join(k.DevRoot, ri.Dax)
	case kernel.DriverKmem:
		ri.Mode = cxlapi.ModeNUMA
	}
	for _, b := range r.Range.Blocks(snap.BlockSize) {
		if snap.Block(b).State != kernel.BlockAbsent {
			ri.Blocks = append(ri.Blocks, b)
		}
	}
	return ri, nil
}

// online onlines the memory blocks of the first region of the memdev with
// the serial, into ZONE_MOVABLE with movable, and returns the region info.
// The region must be in mode numa.
func (c *cli) online(ctx context.Context, serial uint64, movable bool) (*regionInfo, error) {
	ri, err := c.regionInfo(ctx, serial)
	switch {
	case err != nil:
		return nil, err
	case ri.Region == "":
		return nil, fmt.Errorf("%s has no region", ri.Memdev)
	case ri.Driver != kernel.DriverKmem:
		return nil, fmt.Errorf("%s: %s is bound to %q, not kmem: create the region in mode numa", ri.Memdev, ri.Dax, ri.Driver)
	case len(ri.Blocks) == 0:
		return nil, fmt.Errorf("%s: no memory blocks found for %s", ri.Memdev, ri.Region)
	}
	if err := c.kernel().OnlineBlocks(ri.Blocks, movable); err != nil {
		return nil, err
	}
	return ri, nil
}

// disable releases the regions of the memdev (offline, disable, destroy)
// and disables it, so that the pool can detach it.
func (c *cli) disable(ctx context.Context, memdev string) error {
	k := c.kernel()
	for _, r := range k.Regions(memdev) {
		if err := k.DestroyRegion(ctx, r); err != nil {
			return err
		}
	}
	return k.DisableMemdev(ctx, memdev)
}
