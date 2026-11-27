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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"time"

	"context"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// The address line probe: a local memdev whose memory aliases (offsets X
// and X+P reach the same memory, as on a device or platform that ignores
// an address bit) has only P distinct bytes. Every new region of a local
// memdev is probed when Options.ProbeAddressing is set; a region whose
// memory aliases is made again with the distinct bytes only, and that
// usable size is recorded (state.Probe) in Options.ProbesFile, by which
// every later region of the memdev is made smaller and the publisher
// publishes it.

// loadProbes returns the probe results of the file path by serial, none
// when it does not exist.
func loadProbes(path string) (map[state.Serial]state.Probe, error) {
	out := map[state.Serial]state.Probe{}
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	byHex := map[string]state.Probe{}
	if err := json.Unmarshal(data, &byHex); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for hex, p := range byHex {
		serial, err := strconv.ParseUint(hex, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: serial %q: %w", path, hex, err)
		}
		out[state.Serial(serial)] = p
		klog.Warningf("engine: local memdev 0x%x: using only its first %d bytes, found by the address line probe of %s at %s: %s (remove %s to probe all of it again)",
			serial, p.UsableSize, p.Region, p.Time.Format(time.RFC3339), p.Result, path)
	}
	return out, nil
}

// Probes returns the results of the address line probes of the local
// memdevs by serial.
func (e *Engine) Probes() map[state.Serial]state.Probe {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	return maps.Clone(e.probes)
}

// saveProbe records the probe result of the memdev with the serial and
// writes every result to Options.ProbesFile, by hex serial.
func (e *Engine) saveProbe(serial state.Serial, p state.Probe) error {
	e.qmu.Lock()
	e.probes[serial] = p
	byHex := make(map[string]state.Probe, len(e.probes))
	for _, s := range slices.Sorted(maps.Keys(e.probes)) {
		byHex[strconv.FormatUint(uint64(s), 16)] = e.probes[s]
	}
	e.qmu.Unlock()
	if e.opts.ProbesFile == "" {
		return nil
	}
	data, err := json.MarshalIndent(byHex, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", e.opts.ProbesFile, err)
	}
	return store.WriteFileAtomic(e.opts.ProbesFile, data)
}

// usableSize returns the bytes of a local memdev that its region gets: the
// smaller of its configured usable size (Facts.Usable) and the probed one,
// rounded down to kernel.RegionAlign; 0 for all of it.
func (e *Engine) usableSize(req *GrantRequest) uint64 {
	size := req.Facts.UsableSize
	e.qmu.Lock()
	p, ok := e.probes[req.Serial]
	e.qmu.Unlock()
	if ok && p.UsableSize > 0 && (size == 0 || p.UsableSize < size) {
		size = p.UsableSize
	}
	return size / kernel.RegionAlign * kernel.RegionAlign
}

// probeRegion runs the address line probe on the region of a local memdev
// whose Whole device dax spans it and is bound to device_dax, unless
// Options.ProbeAddressing is off. It returns the usable size when the
// memory aliases (0 when it does not, or when the architecture has no
// probe), and an error when the probe found memory that does not hold what
// was written.
func (e *Engine) probeRegion(ctx context.Context, req *GrantRequest, memdev, region, dax string) (uint64, error) {
	if !e.opts.ProbeAddressing {
		return 0, nil
	}
	s := req.Serial.String()
	start := time.Now()
	size, err := e.k.DaxDeviceSize(dax)
	if err != nil {
		return 0, err
	}
	res, err := e.k.ProbeAliasing(ctx, dax)
	if errors.Is(err, kernel.ErrProbeUnsupported) {
		klog.V(4).Infof("engine: local memdev %s (%s): %v", s, memdev, err)
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("memdev %s (%s): probing %s for address aliasing: %w", s, memdev, region, err)
	}
	klog.Infof("engine: local memdev %s (%s): address line probe of %s (%d bytes): %s in %s", s, memdev, region, size, res, since(start))
	if res.Offset == 0 {
		if res.Garbage >= 0 {
			return 0, fmt.Errorf("memdev %s (%s): %s: %s", s, memdev, region, res)
		}
		return 0, nil
	}
	usable := res.Offset / kernel.RegionAlign * kernel.RegionAlign
	if usable == 0 {
		return 0, fmt.Errorf("memdev %s (%s): %s: %s: less than %d MiB is usable", s, memdev, region, res, kernel.RegionAlign>>20)
	}
	klog.Warningf("engine: local memdev %s (%s): %s: %s; using only the first %d bytes of the memdev from now on", s, memdev, region, res, usable)
	if err := e.saveProbe(req.Serial, state.Probe{UsableSize: usable, Result: res.String(), Region: region, Time: time.Now()}); err != nil {
		klog.Errorf("engine: saving the probe result: %v", err)
	}
	return usable, nil
}

// aliasedBelowError is the error of the region of a local memdev whose
// memory aliases and that lies below regions of other memdevs: it is not
// released to be made again smaller, which would leave a hole in the
// decoders; a later Prepare does that once nothing lies above it.
type aliasedBelowError struct {
	memdev, region string
	serial         state.Serial
	usable         uint64
}

// Error returns the error in words.
func (e *aliasedBelowError) Error() string {
	return fmt.Sprintf("memdev %s (%s): %s aliases: it is created again with its first %d bytes only once no region of another memory device lies above it in the decoders",
		e.serial.String(), e.memdev, e.region, e.usable)
}
