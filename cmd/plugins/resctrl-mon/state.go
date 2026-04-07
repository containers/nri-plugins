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

import "sync"

// podInfo tracks the mon_group directory and container set for a single pod.
type podInfo struct {
	monGroupDir string
	containers  map[string]struct{} // container IDs
}

// podState tracks all pods with active mon_groups.
type podState struct {
	mu   sync.Mutex
	pods map[string]*podInfo // keyed by pod UID
}

func newPodState() *podState {
	return &podState{
		pods: make(map[string]*podInfo),
	}
}

// addPod registers a new pod with its mon_group directory.
// If the pod already exists, the existing entry is preserved.
func (s *podState) addPod(podUID, monGroupDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pods[podUID]; ok {
		return
	}
	s.pods[podUID] = &podInfo{
		monGroupDir: monGroupDir,
		containers:  make(map[string]struct{}),
	}
}

// addContainer adds a container ID to an existing pod's tracking.
func (s *podState) addContainer(podUID, containerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, ok := s.pods[podUID]; ok {
		info.containers[containerID] = struct{}{}
	}
}

// removeContainer removes a container ID from a pod's tracking.
func (s *podState) removeContainer(podUID, containerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, ok := s.pods[podUID]; ok {
		delete(info.containers, containerID)
	}
}

// removePod removes all tracking for a pod.
func (s *podState) removePod(podUID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pods, podUID)
}

// getMonGroupDir returns the mon_group directory for a pod, or empty string.
func (s *podState) getMonGroupDir(podUID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, ok := s.pods[podUID]; ok {
		return info.monGroupDir
	}
	return ""
}

// podHasNoContainers returns true if the pod has no remaining containers.
func (s *podState) podHasNoContainers(podUID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, ok := s.pods[podUID]; ok {
		return len(info.containers) == 0
	}
	return true
}

// hasPod returns true if the pod UID is being tracked.
func (s *podState) hasPod(podUID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pods[podUID]
	return ok
}

// podCount returns the number of tracked pods.
func (s *podState) podCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pods)
}
