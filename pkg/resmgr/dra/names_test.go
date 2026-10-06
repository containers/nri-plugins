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
	"testing"

	"tags.cncf.io/container-device-interface/pkg/parser"
)

func TestDriverName(t *testing.T) {
	if got := DriverName("balloons"); got != "balloons.nri.io" {
		t.Errorf("DriverName(balloons) = %q, want balloons.nri.io", got)
	}
}

func TestClaimOfCDIDeviceRoundTrip(t *testing.T) {
	for _, index := range []int{0, 1, 12} {
		name := parser.QualifiedName(testDriverName, cdiClass, cdiDeviceName(testClaimUID, index))
		driver, uid, ok := ClaimOfCDIDevice(name)
		if !ok || driver != testDriverName || uid != testClaimUID {
			t.Errorf("ClaimOfCDIDevice(%q) = %q, %q, %v, want %q, %q, true",
				name, driver, uid, ok, testDriverName, testClaimUID)
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
		if driver, uid, ok := ClaimOfCDIDevice(name); ok {
			t.Errorf("ClaimOfCDIDevice(%q) = %q, %q, true, want false", name, driver, uid)
		}
	}
}
