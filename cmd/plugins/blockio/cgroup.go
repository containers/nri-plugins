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
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	oci "github.com/opencontainers/runtime-spec/specs-go"
)

// linuxBlockIOToUnified translates class parameters into cgroup v2 settings.
//
// Weights always go to io.bfq.weight, per-device weights included. That
// interface requires the BFQ scheduler and kernel v5.4 (commit 795fe54c2a8) or
// later, so the plugin neither probes the cgroup hierarchy nor falls back to
// io.weight. On a node without BFQ the runtime fails to apply the settings and
// container creation fails, which the prerequisites document.
func linuxBlockIOToUnified(parameters *oci.LinuxBlockIO) map[string]string {
	unified := make(map[string]string)
	// BFQ accepts the OCI default weight directly in io.bfq.weight.
	var weightLines []string
	if parameters.Weight != nil && *parameters.Weight != 0 {
		weightLines = append(weightLines, strconv.FormatUint(uint64(*parameters.Weight), 10))
	}
	// BFQ with device support also accepts "major:minor weight" entries.
	for _, device := range parameters.WeightDevice {
		if device.Weight != nil {
			weightLines = append(weightLines, fmt.Sprintf("%d:%d %d", device.Major, device.Minor, *device.Weight))
		}
	}
	if len(weightLines) != 0 {
		unified["io.bfq.weight"] = strings.Join(weightLines, "\n")
	}
	// Throttles use io.max regardless of weight support.
	limits := make(map[oci.LinuxBlockIODevice][]string)
	addLimits := func(devices []oci.LinuxThrottleDevice, field string) {
		for _, device := range devices {
			// Zero means no limit in the class format; cgroup v2 requires "max".
			value := "max"
			if device.Rate != 0 {
				value = strconv.FormatUint(device.Rate, 10)
			}
			limits[device.LinuxBlockIODevice] = append(limits[device.LinuxBlockIODevice], field+"="+value)
		}
	}
	addLimits(parameters.ThrottleReadBpsDevice, "rbps")
	addLimits(parameters.ThrottleWriteBpsDevice, "wbps")
	addLimits(parameters.ThrottleReadIOPSDevice, "riops")
	addLimits(parameters.ThrottleWriteIOPSDevice, "wiops")
	devices := make([]oci.LinuxBlockIODevice, 0, len(limits))
	for device := range limits {
		devices = append(devices, device)
	}
	slices.SortFunc(devices, compareDevices)
	var lines []string
	for _, device := range devices {
		lines = append(lines, fmt.Sprintf("%d:%d %s", device.Major, device.Minor, strings.Join(limits[device], " ")))
	}
	if len(lines) != 0 {
		unified["io.max"] = strings.Join(lines, "\n")
	}
	return unified
}

func compareDevices(a, b oci.LinuxBlockIODevice) int {
	if order := cmp.Compare(a.Major, b.Major); order != 0 {
		return order
	}
	return cmp.Compare(a.Minor, b.Minor)
}
