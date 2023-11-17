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
	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type (
	BalloonsOptions  = cfgapi.Config
	BalloonDef       = cfgapi.BalloonDef
	CPUTopologyLevel = cfgapi.CPUTopologyLevel
)

var (
	CPUTopologyLevelCount     = cfgapi.CPUTopologyLevelCount
	defaultPinCPU             = true
	defaultPinMemory          = true
	defaultReservedNamespaces = []string{metav1.NamespaceSystem}
)

const (
	CPUTopologyLevelUndefined = cfgapi.CPUTopologyLevelUndefined
	CPUTopologyLevelSystem    = cfgapi.CPUTopologyLevelSystem
	CPUTopologyLevelPackage   = cfgapi.CPUTopologyLevelPackage
	CPUTopologyLevelDie       = cfgapi.CPUTopologyLevelDie
	CPUTopologyLevelNuma      = cfgapi.CPUTopologyLevelNuma
	CPUTopologyLevelCore      = cfgapi.CPUTopologyLevelCore
	CPUTopologyLevelThread    = cfgapi.CPUTopologyLevelThread
)

func setOmittedDefaults(cfg *cfgapi.Config) {
	if cfg == nil {
		return
	}

	if cfg.PinCPU == nil {
		cfg.PinCPU = &defaultPinCPU
	}

	if cfg.PinMemory == nil {
		cfg.PinMemory = &defaultPinMemory
	}

	if cfg.ReservedPoolNamespaces == nil {
		cfg.ReservedPoolNamespaces = make([]string, len(defaultReservedNamespaces))
		copy(cfg.ReservedPoolNamespaces, defaultReservedNamespaces)
	}
}
