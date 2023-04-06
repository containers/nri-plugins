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

package cache

import (
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/containers/nri-plugins/pkg/cgroups"
	"github.com/containers/nri-plugins/pkg/kubernetes"
)

var (
	memoryCapacity int64

	SharesToMilliCPU = kubernetes.SharesToMilliCPU
	QuotaToMilliCPU  = kubernetes.QuotaToMilliCPU
	MilliCPUToShares = kubernetes.MilliCPUToShares
	MilliCPUToQuota  = kubernetes.MilliCPUToQuota
)

// IsPodQOSClassName returns true if the given class is one of the Pod QOS classes.
func IsPodQOSClassName(class string) bool {
	switch corev1.PodQOSClass(class) {
	case corev1.PodQOSBestEffort, corev1.PodQOSBurstable, corev1.PodQOSGuaranteed:
		return true
	}
	return false
}

// getMemoryCapacity parses memory capacity from /proc/meminfo (mimicking cAdvisor).
func getMemoryCapacity() int64 {
	var data []byte
	var err error

	if memoryCapacity > 0 {
		return memoryCapacity
	}

	if data, err = ioutil.ReadFile("/proc/meminfo"); err != nil {
		return -1
	}

	for _, line := range strings.Split(string(data), "\n") {
		keyval := strings.Split(line, ":")
		if len(keyval) != 2 || keyval[0] != "MemTotal" {
			continue
		}

		valunit := strings.Split(strings.TrimSpace(keyval[1]), " ")
		if len(valunit) != 2 || valunit[1] != "kB" {
			return -1
		}

		memoryCapacity, err = strconv.ParseInt(valunit[0], 10, 64)
		if err != nil {
			return -1
		}

		memoryCapacity *= 1024
		break
	}

	return memoryCapacity
}

// cgroupParentToQOS tries to map Pod cgroup parent to QOS class.
func cgroupParentToQOS(dir string) corev1.PodQOSClass {
	var qos corev1.PodQOSClass

	// The parent directory naming scheme depends on the cgroup driver in use.
	// Thus, rely on substring matching
	split := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	switch {
	case len(split) < 2:
		qos = corev1.PodQOSClass("")
	case strings.Index(split[1], strings.ToLower(string(corev1.PodQOSBurstable))) != -1:
		qos = corev1.PodQOSBurstable
	case strings.Index(split[1], strings.ToLower(string(corev1.PodQOSBestEffort))) != -1:
		qos = corev1.PodQOSBestEffort
	default:
		qos = corev1.PodQOSGuaranteed
	}

	return qos
}

// findContainerDir brute-force searches for a container cgroup dir.
func findContainerDir(podCgroupDir, podID, ID string) string {
	var dirs []string

	if podCgroupDir == "" {
		return ""
	}

	cpusetDir := cgroups.Cpuset.Path()

	dirs = []string{
		path.Join(cpusetDir, podCgroupDir, ID),
		// containerd, systemd
		path.Join(cpusetDir, podCgroupDir, "cri-containerd-"+ID+".scope"),
		// containerd, cgroupfs
		path.Join(cpusetDir, podCgroupDir, "cri-containerd-"+ID),
		// crio, systemd
		path.Join(cpusetDir, podCgroupDir, "crio-"+ID+".scope"),
		// crio, cgroupfs
		path.Join(cpusetDir, podCgroupDir, "crio-"+ID),
	}

	for _, dir := range dirs {
		if info, err := os.Stat(dir); err == nil {
			if info.Mode().IsDir() {
				return strings.TrimPrefix(dir, cpusetDir)
			}
		}
	}

	return ""
}

func isSupportedQoSComputeResource(name corev1.ResourceName) bool {
	return name == corev1.ResourceCPU || name == corev1.ResourceMemory
}

func init() {
	// TODO: get rid of this eventually, use pkg/sysfs instead...
	getMemoryCapacity()
}
