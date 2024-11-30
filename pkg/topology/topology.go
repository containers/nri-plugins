// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package topology

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// to mock in tests
var (
	sysRoot        = ""
	log     Logger = &nopLogger{}
)

const (
	// ProviderKubelet is a constant to distinguish that topology hint comes
	// from parameters passed to CRI create/update requests from Kubelet.
	ProviderKubelet = "kubelet"
)

// Hint represents various hints that can be detected from sysfs for the device.
type Hint struct {
	Provider string
	CPUs     string
	NUMAs    string
	Sockets  string
}

// Hints represents set of hints collected from multiple providers.
type Hints map[string]Hint

// Logger is interface we expect from an optional, externally set logger.
type Logger interface {
	Debugf(format string, v ...interface{})
}

// SetSysRoot sets the sysfs root directory to use.
func SetSysRoot(root string) {
	if root != "" {
		sysRoot = filepath.Clean(root)
		if sysRoot != "" && !filepath.IsAbs(sysRoot) {
			a, err := filepath.Abs(sysRoot)
			if err != nil {
				panic(fmt.Errorf("failed to resolve %q to absolute path: %v", sysRoot, err))
			}
			sysRoot = a
		}
		if sysRoot == "/" {
			sysRoot = ""
		}
	} else {
		sysRoot = ""
	}
}

// SetLogger sets the external logger used for (debug) logging.
func SetLogger(l Logger) Logger {
	old := log
	log = l
	return old
}

// ResetLogger resets any externally set logger.
func ResetLogger() {
	log = &nopLogger{}
}

func getDevicesFromVirtual(realDevPath string) (devs []string, err error) {
	relPath, err := filepath.Rel("/sys/devices/virtual", realDevPath)
	if err != nil {
		return nil, fmt.Errorf("unable to find relative path: %w", err)
	}

	if strings.HasPrefix(relPath, "..") {
		return nil, fmt.Errorf("%s is not a virtual device", realDevPath)
	}

	dir, file := filepath.Split(relPath)
	switch dir {
	case "vfio/":
		iommuGroup := filepath.Join(sysRoot, "/sys/kernel/iommu_groups", file, "devices")
		files, err := os.ReadDir(iommuGroup)
		if err != nil {
			return nil, fmt.Errorf("failed to read IOMMU group %s: %w", iommuGroup, err)
		}
		for _, file := range files {
			realDev, err := filepath.EvalSymlinks(filepath.Join(iommuGroup, file.Name()))
			if err != nil {
				return nil, fmt.Errorf("failed to get real path for %s: %w", file.Name(), err)
			}
			devs = append(devs, realDev)
		}
		log.Debugf("devices from virtual %s: %s", realDevPath, strings.Join(devs, ","))
		return devs, nil
	default:
		return nil, nil
	}
}

func getTopologyHint(sysFSPath string) (*Hint, error) {
	log.Debugf("getting topology hint for %s", sysFSPath)

	plainPath := sysFSPath
	if sysRoot != "" {
		relPath, err := filepath.Rel(sysRoot, plainPath)
		if err != nil {
			return nil, fmt.Errorf("internal error: %v", err)
		}
		plainPath = filepath.Join("/", relPath)
	}
	hint := Hint{Provider: plainPath}
	fileMap := map[string]*string{
		// match /sys/devices/system/node/node0/cpulist
		"cpulist": &hint.CPUs,
		// match /sys/devices/pci0000:00/pci_bus/0000:00/cpulistaffinity
		"cpulistaffinity": &hint.CPUs,
		// match /sys/devices/pci0000:00/0000:00:01.0/local_cpulist
		"local_cpulist": &hint.CPUs,
		// match /sys/devices/pci0000:00/0000:00:01.0/numa_node
		"numa_node": &hint.NUMAs,
		// match /sys/devices/system/cpu/cpu0/cache/index0/shared_cpu_list
		"shared_cpu_list": &hint.CPUs,
	}
	if err := readFilesInDirectory(fileMap, sysFSPath); err != nil {
		return nil, err
	}
	// Workarounds for broken information provided by kernel
	if hint.NUMAs == "-1" {
		// non-NUMA aware device or system, ignore it
		hint.NUMAs = ""
	}
	if hint.NUMAs != "" && hint.CPUs == "" {
		// broken topology hint. BIOS reports socket id as NUMA node
		// First, try to get hints from parent device or bus.
		parentHints, er := NewTopologyHints(filepath.Dir(sysFSPath))
		if er == nil {
			cpulist := map[string]bool{}
			numalist := map[string]bool{}
			for _, h := range parentHints {
				if h.CPUs != "" {
					cpulist[h.CPUs] = true
				}
				if h.NUMAs != "" {
					numalist[h.NUMAs] = true
				}
			}
			if cpus := strings.Join(mapKeys(cpulist), ","); cpus != "" {
				hint.CPUs = cpus
			}
			if numas := strings.Join(mapKeys(numalist), ","); numas != "" {
				hint.NUMAs = numas
			}
		}
		// if after parent hints we still don't have CPUs hints, use numa hint as sockets.
		if hint.CPUs == "" && hint.NUMAs != "" {
			hint.Sockets = hint.NUMAs
			hint.NUMAs = ""
		}
	}

	if hint.CPUs != "" || hint.NUMAs != "" || hint.Sockets != "" {
		log.Debugf("  => %s", hint.String())
	}

	return &hint, nil
}

// NewTopologyHints return array of hints for the main device and its
// depended devices (e.g. RAID).
func NewTopologyHints(devPath string) (hints Hints, err error) {
	hints = make(Hints)
	hostDevPath := filepath.Join(sysRoot, devPath)
	realDevPath, err := filepath.EvalSymlinks(hostDevPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get realpath for %s: %w", hostDevPath, err)
	}
	for p := realDevPath; strings.HasPrefix(p, sysRoot+"/sys/devices/"); p = filepath.Dir(p) {
		hint, err := getTopologyHint(p)
		if err != nil {
			return nil, err
		}
		if hint.CPUs != "" || hint.NUMAs != "" || hint.Sockets != "" {
			hints[hint.Provider] = *hint
			break
		}
	}
	fromVirtual, _ := getDevicesFromVirtual(realDevPath)
	deps, _ := filepath.Glob(filepath.Join(realDevPath, "slaves/*"))
	for _, device := range append(deps, fromVirtual...) {
		deviceHints, er := NewTopologyHints(device)
		if er != nil {
			return nil, er
		}
		hints = MergeTopologyHints(hints, deviceHints)
	}
	return hints, err
}

// MergeTopologyHints combines org and hints.
func MergeTopologyHints(org, hints Hints) (res Hints) {
	if org != nil {
		res = org
	} else {
		res = make(Hints)
	}
	for k, v := range hints {
		if _, ok := res[k]; ok {
			continue
		}
		res[k] = v
	}
	return
}

// ResolvePartialHints resolves NUMA-only hints to CPU hints using the given function.
func (hints Hints) ResolvePartialHints(resolve func(NUMAs string) string) {
	for k, h := range hints {
		if h.CPUs == "" && h.NUMAs != "" {
			h.CPUs = resolve(h.NUMAs)
			log.Debugf("partial NUMA hint %q resolved to CPUs %q", h.NUMAs, h.CPUs)
			hints[k] = h
		}
	}
}

// String returns the hints as a string.
func (h *Hint) String() string {
	cpus, nodes, sockets, sep := "", "", "", ""

	if h.CPUs != "" {
		cpus = "CPUs:" + h.CPUs
		sep = ", "
	}
	if h.NUMAs != "" {
		nodes = sep + "NUMAs:" + h.NUMAs
		sep = ", "
	}
	if h.Sockets != "" {
		sockets = sep + "sockets:" + h.Sockets
	}

	return "<hints " + cpus + nodes + sockets + " (from " + h.Provider + ")>"
}

// FindGivenSysFsDevice returns the physical device with the given device type,
// major, and minor numbers.
func FindGivenSysFsDevice(devType string, major, minor int64) (string, error) {
	switch devType {
	case "block", "char":
	case "b":
		devType = "block"
	case "c":
		devType = "char"
	default:
		return "", fmt.Errorf("invalid device type %q", devType)
	}

	realDevPath, err := findSysFsDevice(devType, major, minor)
	if err != nil {
		return "", fmt.Errorf("failed find sysfs device for %s device %d/%d: %w",
			devType, major, minor, err)
	}

	return realDevPath, nil
}

func findSysFsDevice(devType string, major, minor int64) (string, error) {
	devPath := fmt.Sprintf("%s/sys/dev/%s/%d:%d", sysRoot, devType, major, minor)
	realDevPath, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return "", fmt.Errorf("failed to get realpath for %s: %w", devPath, err)
	}
	if sysRoot != "" && strings.HasPrefix(realDevPath, sysRoot) {
		realDevPath = strings.TrimPrefix(realDevPath, sysRoot)
	}
	return realDevPath, nil
}

// readFilesInDirectory small helper to fill struct with content from sysfs entry.
func readFilesInDirectory(fileMap map[string]*string, dir string) error {
	for k, v := range fileMap {
		b, err := os.ReadFile(filepath.Join(dir, k))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("%s: unable to read file %q: %w", dir, k, err)
		}
		*v = strings.TrimSpace(string(b))
	}
	return nil
}

// mapKeys is a small helper that returns slice of keys for a given map.
func mapKeys(m map[string]bool) []string {
	ret := make([]string, len(m))
	i := 0
	for k := range m {
		ret[i] = k
		i++
	}
	return ret
}

type nopLogger struct{}

func (*nopLogger) Debugf(string, ...interface{}) {}
