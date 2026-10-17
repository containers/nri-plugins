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

package dra

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"tags.cncf.io/container-device-interface/pkg/parser"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
)

func TestDriverName(t *testing.T) {
	if got := DriverName("balloons"); got != "balloons.nri.io" {
		t.Errorf("DriverName(balloons) = %q, want balloons.nri.io", got)
	}
}

func TestClaimOfCDIDeviceRoundTrip(t *testing.T) {
	for _, index := range []int{0, 1, 12} {
		name := parser.QualifiedName(testDriverName, cdiClass, cdiDeviceName(testClaimUID, index))
		driver, uid, i, ok := ClaimOfCDIDevice(name)
		if !ok || driver != testDriverName || uid != testClaimUID || i != index {
			t.Errorf("ClaimOfCDIDevice(%q) = %q, %q, %d, %v, want %q, %q, %d, true",
				name, driver, uid, i, ok, testDriverName, testClaimUID, index)
		}
	}
}

func TestClaimOfCDIDeviceRejected(t *testing.T) {
	for _, name := range []string{
		"",
		"/dev/null",
		"claim-" + string(testClaimUID) + "-0",
		testDriverName + "/gpu=claim-" + string(testClaimUID) + "-0",
		testDriverName + "/device=gpu0",
		testDriverName + "/device=claim-" + string(testClaimUID),
		testDriverName + "/device=claim-" + string(testClaimUID) + "-",
		testDriverName + "/device=claim-" + string(testClaimUID) + "-x1",
		testDriverName + "/device=claim--0",
		testDriverName + "/device=claim-0",
	} {
		if driver, uid, index, ok := ClaimOfCDIDevice(name); ok {
			t.Errorf("ClaimOfCDIDevice(%q) = %q, %q, %d, true, want false", name, driver, uid, index)
		}
	}
}

// testContainer reports the given CDI devices and environment.
type testContainer struct {
	cache.Container
	cdiDevices []string
	env        []string
}

func (c *testContainer) GetCDIDevices() []string { return c.cdiDevices }
func (c *testContainer) GetEnvList() []string    { return c.env }

func TestContainerClaims(t *testing.T) {
	const otherUID = types.UID("0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0")

	device := func(driver string, uid types.UID, index int) string {
		return parser.QualifiedName(driver, cdiClass, cdiDeviceName(uid, index))
	}

	for _, tc := range []struct {
		name       string
		cdiDevices []string
		env        []string
		expected   map[types.UID][]int
	}{
		{
			name: "CDI devices",
			cdiDevices: []string{
				device(testDriverName, testClaimUID, 1),
				device(testDriverName, testClaimUID, 0),
				device(testDriverName, testClaimUID, 0),
				device(testDriverName, otherUID, 0),
				device(otherDriverName, testClaimUID, 2),
				"vendor.com/gpu=gpu0",
			},
			expected: map[types.UID][]int{testClaimUID: {0, 1}, otherUID: {0}},
		},
		{
			name: "variables, when the runtime reports no CDI devices",
			env: []string{
				"PATH=/bin",
				deviceEnv(testClaimUID, 1),
				"DRA_DEVICE_x=" + device(testDriverName, otherUID, 0),
				"DRA_DEVICE_" + string(testClaimUID) + "_0=" + device(testDriverName, testClaimUID, 2),
			},
			expected: map[types.UID][]int{testClaimUID: {1}},
		},
		{
			name:       "CDI devices win over variables",
			cdiDevices: []string{device(testDriverName, testClaimUID, 0)},
			env:        []string{deviceEnv(testClaimUID, 1)},
			expected:   map[types.UID][]int{testClaimUID: {0}},
		},
		{
			name:     "nothing of ours",
			env:      []string{"PATH=/bin"},
			expected: map[types.UID][]int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &testContainer{cdiDevices: tc.cdiDevices, env: tc.env}
			if claims := ContainerClaims(c, testDriverName); !reflect.DeepEqual(claims, tc.expected) {
				t.Errorf("ContainerClaims() = %v, expected %v", claims, tc.expected)
			}
		})
	}
}
