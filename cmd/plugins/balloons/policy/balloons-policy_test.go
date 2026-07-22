// Copyright 2022 Intel Corporation. All Rights Reserved.
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

package balloons

import (
	"slices"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
)

func TestChangesBalloons(t *testing.T) {
	tcases := []struct {
		name          string
		opts1         *BalloonsOptions
		opts2         *BalloonsOptions
		expectedValue bool
	}{
		{
			name:          "both options are nil",
			expectedValue: false,
		},
		{
			name:          "one option is nil",
			opts2:         &BalloonsOptions{},
			expectedValue: true,
		},
		{
			name: "reserved pool namespaces differ by len",
			opts1: &BalloonsOptions{
				IdleCpuClass:           "icc0",
				ReservedPoolNamespaces: []string{"ns0"},
			},
			opts2: &BalloonsOptions{
				IdleCpuClass:           "icc0",
				ReservedPoolNamespaces: []string{},
			},
			expectedValue: true,
		},
		{
			name: "reserved pool namespaces differ by content",
			opts1: &BalloonsOptions{
				IdleCpuClass:           "icc0",
				ReservedPoolNamespaces: []string{"ns0"},
			},
			opts2: &BalloonsOptions{
				IdleCpuClass:           "icc0",
				ReservedPoolNamespaces: []string{"ns1"},
			},
			expectedValue: true,
		},
		{
			name: "idle cpu classes differ",
			opts1: &BalloonsOptions{
				IdleCpuClass:           "icc0",
				ReservedPoolNamespaces: []string{"ns0"},
			},
			opts2: &BalloonsOptions{
				IdleCpuClass:           "icc1",
				ReservedPoolNamespaces: []string{"ns0"},
			},
			expectedValue: false,
		},
		{
			name: "balloon defs differ",
			opts1: &BalloonsOptions{
				IdleCpuClass:           "icc0",
				ReservedPoolNamespaces: []string{"ns0"},
				BalloonDefs:            []*BalloonDef{},
			},
			opts2: &BalloonsOptions{
				IdleCpuClass:           "icc1",
				ReservedPoolNamespaces: []string{"ns0"},
				BalloonDefs:            []*BalloonDef{},
			},
			expectedValue: false,
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			value := changesBalloons(tc.opts1, tc.opts2)
			if value != tc.expectedValue {
				t.Errorf("Expected return value %v but got %v", tc.expectedValue, value)
			}
		})
	}
}

func TestPodResourceDeviceName(t *testing.T) {
	tcases := []struct {
		name             string
		dev              string
		expectedResource string
		expectedOk       bool
	}{
		{
			name:             "pod resource device",
			dev:              "podresourceapi:telco.com/nic",
			expectedResource: "telco.com/nic",
			expectedOk:       true,
		},
		{
			name:             "pod resource device with a glob pattern",
			dev:              "podresourceapi:tech.com/*",
			expectedResource: "tech.com/*",
			expectedOk:       true,
		},
		{
			name:       "pod resource device without a resource name",
			dev:        "podresourceapi:",
			expectedOk: true,
		},
		{
			name: "ordinary device",
			dev:  "/dev/nvme0n1",
		},
		{
			name: "device name containing but not starting with the prefix",
			dev:  "/dev/podresourceapi:nic",
		},
		{
			name: "empty device",
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			resource, ok := podResourceDeviceName(tc.dev)
			if ok != tc.expectedOk {
				t.Errorf("expected ok %v but got %v", tc.expectedOk, ok)
			}
			if resource != tc.expectedResource {
				t.Errorf("expected resource name %q but got %q", tc.expectedResource, resource)
			}
		})
	}
}

// fakeIDContainer implements only the cache.Container method that
// podResourceHintDevName needs. Calling any other method panics.
type fakeIDContainer struct {
	cache.Container
	id string
}

func (f *fakeIDContainer) GetID() string { return f.id }

func TestPodResourceHintDevName(t *testing.T) {
	tcases := []struct {
		name         string
		c            cache.Container
		resourceName string
		expectedName string
	}{
		{
			name:         "container and resource",
			c:            &fakeIDContainer{id: "ctr0"},
			resourceName: "telco.com/nic",
			expectedName: "__podres_ctr0_telco.com/nic",
		},
		{
			name:         "another resource of the same container",
			c:            &fakeIDContainer{id: "ctr0"},
			resourceName: "tech.com/tpu",
			expectedName: "__podres_ctr0_tech.com/tpu",
		},
		{
			name:         "same resource of another container",
			c:            &fakeIDContainer{id: "ctr1"},
			resourceName: "telco.com/nic",
			expectedName: "__podres_ctr1_telco.com/nic",
		},
		{
			name:         "no resource: common prefix of the devices of a container",
			c:            &fakeIDContainer{id: "ctr0"},
			expectedName: "__podres_ctr0_",
		},
		{
			name:         "no container",
			resourceName: "telco.com/nic",
			expectedName: "__podres_unknown_telco.com/nic",
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			name := podResourceHintDevName(tc.c, tc.resourceName)
			if name != tc.expectedName {
				t.Errorf("expected device name %q but got %q", tc.expectedName, name)
			}
			if !strings.HasPrefix(name, podResourceHintDevPrefix) {
				t.Errorf("device name %q does not start with %q", name, podResourceHintDevPrefix)
			}
			// Devices of a container must be removable by the
			// container's common prefix (see filterOutPrefixDevs).
			if prefix := podResourceHintDevName(tc.c, ""); !strings.HasPrefix(name, prefix) {
				t.Errorf("device name %q does not start with container prefix %q", name, prefix)
			}
		})
	}
}

// TestFilterOutPrefixDevs verifies that the devices of a container can
// be removed from a device list by the container's common prefix,
// leaving devices of other containers and ordinary devices intact.
func TestFilterOutPrefixDevs(t *testing.T) {
	ctr0 := &fakeIDContainer{id: "ctr0"}
	ctr1 := &fakeIDContainer{id: "ctr1"}
	devs := []string{
		podResourceHintDevName(ctr0, "telco.com/nic"),
		"/dev/nvme0n1",
		podResourceHintDevName(ctr1, "telco.com/nic"),
		podResourceHintDevName(ctr0, "tech.com/tpu"),
	}
	expectedDevs := []string{"/dev/nvme0n1", "__podres_ctr1_telco.com/nic"}
	remainingDevs := filterOutPrefixDevs(devs, podResourceHintDevName(ctr0, ""))
	if !slices.Equal(remainingDevs, expectedDevs) {
		t.Errorf("expected remaining devices %v but got %v", expectedDevs, remainingDevs)
	}
}
