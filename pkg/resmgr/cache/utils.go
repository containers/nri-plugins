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
	"os"
	"path"
	"strconv"
	"strings"

	nri "github.com/containerd/nri/pkg/api"
	corev1 "k8s.io/api/core/v1"
	resapi "k8s.io/apimachinery/pkg/api/resource"

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

// Try to estimate CRI resource requirements from NRI resources.
func estimateResourceRequirements(r *nri.LinuxResources, qosClass corev1.PodQOSClass) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}

	cpu := r.GetCpu()
	shares := int64(cpu.GetShares().GetValue())

	// calculate CPU request
	if value := SharesToMilliCPU(shares); value > 0 {
		qty := resapi.NewMilliQuantity(value, resapi.DecimalSI)
		resources.Requests[corev1.ResourceCPU] = *qty
	}

	// get memory limit
	if value := r.GetMemory().GetLimit().GetValue(); value > 0 {
		qty := resapi.NewQuantity(value, resapi.DecimalSI)
		resources.Limits[corev1.ResourceMemory] = *qty
	}

	// calculate CPU limit, set memory request if known
	switch qosClass {
	case corev1.PodQOSGuaranteed:
		resources.Limits[corev1.ResourceCPU] = resources.Requests[corev1.ResourceCPU]
		resources.Requests[corev1.ResourceMemory] = resources.Limits[corev1.ResourceMemory]
	default:
		fallthrough
	case corev1.PodQOSBestEffort, corev1.PodQOSBurstable:
		quota := cpu.GetQuota().GetValue()
		period := int64(cpu.GetPeriod().GetValue())
		if value := QuotaToMilliCPU(quota, period); value > 0 {
			qty := resapi.NewMilliQuantity(value, resapi.DecimalSI)
			resources.Limits[corev1.ResourceCPU] = *qty
		}
	}

	return resources
}

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

	if data, err = os.ReadFile("/proc/meminfo"); err != nil {
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

func init() {
	// TODO: get rid of this eventually, use pkg/sysfs instead...
	getMemoryCapacity()
}
