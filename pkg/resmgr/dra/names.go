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
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"

	"tags.cncf.io/container-device-interface/pkg/parser"
)

// draDomain is the DNS domain DRA driver names are formed in. The driver is
// named after the active policy, because the policy decides which devices get
// published and what they mean, so a node running another policy publishes
// devices of another kind.
const draDomain = "nri.io"

// cdiClaimPrefix starts the name of every CDI device written for a claim.
const cdiClaimPrefix = "claim-"

// DriverName returns the name of the DRA driver of the named policy.
func DriverName(policyName string) string {
	return policyName + "." + draDomain
}

// cdiDeviceEnv returns the name of the environment variable set by the CDI
// device of result index of claim uid. The value of the variable is the name
// of the device.
func cdiDeviceEnv(uid types.UID, index int) string {
	return "DRA_DEVICE_" + string(uid) + "_" + strconv.Itoa(index)
}

// ClaimOfCDIDevice returns the driver, the claim UID and the index of the
// allocation result of a CDI device written for a prepared claim. ok is false
// for any other device.
func ClaimOfCDIDevice(qualifiedName string) (driver string, uid types.UID, index int, ok bool) {
	vendor, class, name, err := parser.ParseQualifiedName(qualifiedName)
	if err != nil || class != cdiClass || !strings.HasPrefix(name, cdiClaimPrefix) {
		return "", "", 0, false
	}

	// The name is claim-<uid>-<index>, and a UID may contain '-' itself.
	rest := strings.TrimPrefix(name, cdiClaimPrefix)
	sep := strings.LastIndexByte(rest, '-')
	if sep < 1 {
		return "", "", 0, false
	}
	digits := rest[sep+1:]
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", "", 0, false
	}
	index, err = strconv.Atoi(digits)
	if err != nil {
		return "", "", 0, false
	}

	return vendor, types.UID(rest[:sep]), index, true
}

// ContainerClaims returns the claims of the given driver a container holds,
// with the indexes of the allocation results of each claim it holds. A
// container which names some requests of a claim holds only their results.
func ContainerClaims(c cache.Container, driver string) map[types.UID][]int {
	names := c.GetCDIDevices()
	if len(names) == 0 {
		// The runtime did not report the CDI devices, so read the
		// variables they set. A pod spec can set these too, but it
		// cannot set CDI devices.
		for _, env := range c.GetEnvList() {
			key, value, _ := strings.Cut(env, "=")
			if _, uid, index, ok := ClaimOfCDIDevice(value); ok && key == cdiDeviceEnv(uid, index) {
				names = append(names, value)
			}
		}
	}

	claims := map[types.UID][]int{}
	for _, name := range names {
		d, uid, index, ok := ClaimOfCDIDevice(name)
		if !ok || d != driver || slices.Contains(claims[uid], index) {
			continue
		}
		claims[uid] = append(claims[uid], index)
	}
	for _, indexes := range claims {
		slices.Sort(indexes)
	}

	return claims
}
