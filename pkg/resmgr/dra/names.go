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
	"strings"

	"k8s.io/apimachinery/pkg/types"

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

// ClaimOfCDIDevice returns the driver and the claim UID of a CDI device
// written for a prepared claim. ok is false for any other device.
func ClaimOfCDIDevice(qualifiedName string) (driver string, uid types.UID, ok bool) {
	vendor, class, name, err := parser.ParseQualifiedName(qualifiedName)
	if err != nil || class != cdiClass || !strings.HasPrefix(name, cdiClaimPrefix) {
		return "", "", false
	}

	// The name is claim-<uid>-<index>, and a UID may contain '-' itself.
	rest := strings.TrimPrefix(name, cdiClaimPrefix)
	sep := strings.LastIndexByte(rest, '-')
	if sep < 1 {
		return "", "", false
	}
	index := rest[sep+1:]
	if index == "" || strings.Trim(index, "0123456789") != "" {
		return "", "", false
	}

	return vendor, types.UID(rest[:sep]), true
}
