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

package kernel

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Port: a CXL port of the decoder path from the CXL root to a memdev, with
// its stack of HDM decoders: portN (a host bridge or a switch, shared by
// the memdevs under it) or the endpoint port endpointN of the memdev.
//
// Identity: the name portN. Every memdev under a port shares its stack.
// State: the decoder stack, the regions of decoders 0..decoders_committed-1
// in commit order, "" for a committed decoder without a region (a HOLE).
// The stack is read from decoders_committed and decoderP.K/region only: the
// start, size and target list of a freed decoder keep stale values.
// Kernel rule: Linux commits the decoders of a port in order and allocates
// only the decoder after the last committed one (drivers/cxl/core/region.c
// match_free_decoder, check_commit_order). A decoder released below the
// last committed one stays counted in decoders_committed, and the port
// refuses every new region, of any memdev under it, with EBUSY ("cxl
// region: create_region: regionN: failed to set target0 to memM", kernel
// "failed to attach decoderX.0 to regionN: -16"), until every region above
// it is destroyed. Then the kernel reaps the released decoders and the
// port works again.
// Transitions of the stack: CreateRegion pushes the region on every port
// from the root to the endpoint; DestroyRegion of the top pops it and every
// hole below; of another entry leaves a HOLE.
// Invariants: the regions of a stack are distinct; a hole has a region
// above it.

// Port is the observation of a port and its decoder stack.
type Port struct {
	// Name is portN.
	Name string `json:"name"`
	// Committed is its decoders_committed.
	Committed int `json:"committed"`
	// Decoders are its committed decoders in commit order.
	Decoders []Decoder `json:"decoders,omitempty"`
}

// Decoder is a committed HDM decoder of a port.
type Decoder struct {
	// Name is decoderP.K.
	Name string `json:"name"`
	// Region is the region of the decoder, "" for none (a hole).
	Region string `json:"region"`
}

// Hole returns the hole in the decoders of the port, if any.
func (p Port) Hole() (DecoderHole, bool) {
	h := DecoderHole{Port: p.Name, Committed: p.Committed}
	lowest := -1
	for i, d := range p.Decoders {
		if d.Region == "" {
			h.Free = append(h.Free, d.Name)
			if lowest < 0 {
				lowest = i
			}
		}
	}
	if lowest < 0 {
		return h, false
	}
	for _, d := range p.Decoders[lowest+1:] {
		if d.Region != "" {
			h.Blocking = append(h.Blocking, d.Region)
		}
	}
	return h, true
}

// Above returns the regions on the decoders above the highest decoder of
// a region in regions, nil if the port has none of them.
func (p Port) Above(regions []string) []string {
	var above []string
	ours := false
	for _, d := range p.Decoders {
		switch {
		case slices.Contains(regions, d.Region):
			ours = true
		case ours && d.Region != "" && !slices.Contains(above, d.Region):
			above = append(above, d.Region)
		}
	}
	return above
}

// DecoderHole is a port of the decoder path from the CXL root to a memdev
// whose HDM decoders were released out of order: a region that used
// decoder K of the port was destroyed while a region on a higher decoder of
// the same port lives on (see Port).
type DecoderHole struct {
	// Port is the port: portN (a host bridge or a switch) or the endpoint
	// port endpointN of the memdev.
	Port string
	// Committed is the port's decoders_committed.
	Committed int
	// Free are the decoders below Committed without a region.
	Free []string
	// Blocking are the regions on decoders above the lowest free one: the
	// port takes new regions again once all of them are destroyed.
	Blocking []string
}

func (h DecoderHole) String() string {
	s := fmt.Sprintf("%s has %d committed decoders, %s without a region", h.Port, h.Committed, strings.Join(h.Free, ","))
	if len(h.Blocking) > 0 {
		s += " below " + strings.Join(h.Blocking, ",")
	}
	return s
}

var portDirRe = regexp.MustCompile(`^port\d+$`)

// endpointPorts returns the sysfs directories of the ports of the decoder
// path from the CXL root to the endpoint, nearest to the root first: its
// ancestors in the device tree (.../root0/port2/port3/endpoint6), and the
// endpoint port itself, whose decoders are committed in order too.
func (k *Kernel) endpointPorts(ep string) []string {
	real, err := filepath.EvalSymlinks(k.cxlDev(ep))
	if err != nil {
		return nil
	}
	ports := []string{real}
	for dir := filepath.Dir(real); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if portDirRe.MatchString(filepath.Base(dir)) {
			ports = append(ports, dir)
		}
	}
	slices.Reverse(ports)
	return ports
}

// portID returns N of a port portN or endpointN, the X of its decoders
// decoderX.Y.
func portID(name string) string {
	return strings.TrimPrefix(strings.TrimPrefix(name, "port"), "endpoint")
}

// memdevPorts returns the sysfs directories of the ports of the decoder
// path from the CXL root to the endpoint of the memdev (see endpointPorts),
// nil for a memdev without one.
func (k *Kernel) memdevPorts(memdev string) []string {
	ep, err := k.endpoint(memdev)
	if err != nil {
		return nil
	}
	return k.endpointPorts(ep)
}

// readPort returns the port in the sysfs directory dir. A committed
// decoder whose region cannot be read is an error: nothing could be said of
// what lies above it.
func readPort(dir string) (Port, error) {
	p := Port{Name: filepath.Base(dir)}
	n, err := readUint(filepath.Join(dir, "decoders_committed"))
	if err != nil {
		return p, fmt.Errorf("the decoders of %s: %w", p.Name, err)
	}
	p.Committed = int(n)
	prefix := "decoder" + portID(p.Name) + "."
	for id := 0; id < p.Committed; id++ {
		name := prefix + strconv.Itoa(id)
		region, err := readTrim(filepath.Join(dir, name, "region"))
		if err != nil {
			return p, fmt.Errorf("the decoders of %s: %w", p.Name, err)
		}
		p.Decoders = append(p.Decoders, Decoder{Name: name, Region: region})
	}
	return p, nil
}

// Ports returns the ports of the decoder path from the CXL root to the
// endpoint of the memdev, the endpoint port last; none for a memdev without
// an endpoint. A port whose decoders cannot be read is an error.
func (k *Kernel) Ports(memdev string) ([]Port, error) {
	var out []Port
	for _, dir := range k.memdevPorts(memdev) {
		p, err := readPort(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// DecoderHoles returns the ports of the decoder path from the CXL root to
// the memdev's endpoint, the endpoint port included, whose decoders were
// released out of order (see DecoderHole), nearest to the root first. A
// memdev without an endpoint has none; a port whose decoders cannot be read
// is an error.
func (k *Kernel) DecoderHoles(memdev string) ([]DecoderHole, error) {
	ports, err := k.Ports(memdev)
	if err != nil {
		return nil, err
	}
	var holes []DecoderHole
	for _, p := range ports {
		if h, ok := p.Hole(); ok {
			holes = append(holes, h)
		}
	}
	return holes, nil
}

// RegionsAbove returns the regions of other memdevs on the decoders above
// those of the memdev's regions in the ports of the decoder path from the
// CXL root to the memdev's endpoint, nearest to the root first: while they
// live, destroying the memdev's regions leaves a hole in those ports (see
// DecoderHole). The endpoint port is not read: its decoders hold the
// memdev's own regions only. A memdev without an endpoint or regions has
// none; a port whose decoders cannot be read is an error, never "nothing
// above".
func (k *Kernel) RegionsAbove(memdev string) ([]string, error) {
	own := k.Regions(memdev)
	if len(own) == 0 {
		return nil, nil
	}
	var above []string
	for _, dir := range k.memdevPorts(memdev) {
		if !portDirRe.MatchString(filepath.Base(dir)) {
			continue
		}
		p, err := readPort(dir)
		if err != nil {
			return nil, err
		}
		for _, r := range p.Above(own) {
			if !slices.Contains(above, r) {
				above = append(above, r)
			}
		}
	}
	return above, nil
}
