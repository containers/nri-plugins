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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	oci "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

type weightSupport struct {
	bfq           bool
	deviceWeights bool
}

func checkCgroupV2(root string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return fmt.Errorf("checking cgroup filesystem: %w", err)
	}
	if stat.Type != unix.CGROUP2_SUPER_MAGIC {
		return fmt.Errorf("block I/O plugin requires cgroup v2 at %s", root)
	}
	return nil
}

func detectWeightSupport(root string) (weightSupport, error) {
	// io.bfq.weight is NOT present on the real hierarchy root. Check the
	// mount itself (which might be a delegated subtree) and its children.
	paths := []string{filepath.Join(root, "io.bfq.weight")}
	entries, err := os.ReadDir(root)
	if err != nil {
		return weightSupport{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			paths = append(paths, filepath.Join(root, entry.Name(), "io.bfq.weight"))
		}
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return weightSupport{}, fmt.Errorf("reading %s: %w", path, err)
		}
		// Before Linux 5.4 BFQ only supported the default weight, reported
		// as a single number. Mirror runc's device-weight capability test.
		_, err = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		return weightSupport{bfq: true, deviceWeights: err != nil}, nil
	}
	return weightSupport{}, nil
}

func linuxBlockIOToUnified(parameters *oci.LinuxBlockIO, weights weightSupport) map[string]string {
	unified := make(map[string]string)
	var weightLines []string
	if parameters.Weight != nil && *parameters.Weight != 0 {
		weight := uint64(*parameters.Weight)
		if weights.bfq {
			weightLines = append(weightLines, strconv.FormatUint(weight, 10))
		} else {
			// Match runc/crun's OCI (10..1000) to v2 (1..10000) conversion.
			unified["io.weight"] = strconv.FormatUint(1+(weight-10)*9999/990, 10)
		}
	}
	if len(parameters.WeightDevice) != 0 {
		if weights.bfq && weights.deviceWeights {
			devices := slices.Clone(parameters.WeightDevice)
			slices.SortFunc(devices, func(a, b oci.LinuxWeightDevice) int {
				return compareDevices(a.LinuxBlockIODevice, b.LinuxBlockIODevice)
			})
			for _, device := range devices {
				if device.Weight != nil {
					weightLines = append(weightLines, fmt.Sprintf("%d:%d %d", device.Major, device.Minor, *device.Weight))
				}
			}
		} else {
			// runc omits device weights when BFQ doesn't support them.
			log.Warn("ignoring per-device block I/O weights: BFQ device weights are unavailable")
		}
	}
	if len(weightLines) != 0 {
		unified["io.bfq.weight"] = strings.Join(weightLines, "\n")
	}

	limits := make(map[oci.LinuxBlockIODevice][]string)
	addLimits := func(devices []oci.LinuxThrottleDevice, field string) {
		for _, device := range devices {
			// Zero means no limit in the class format. Unlike CRI-O's OCI
			// path, translate it to v2's "max" rather than writing invalid 0.
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
