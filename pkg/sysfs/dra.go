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

package sysfs

import (
	"fmt"
	"strconv"

	idset "github.com/intel/goresctrl/pkg/utils"
	resapi "k8s.io/api/resource/v1beta2"
)

type (
	ID            = idset.ID
	QualifiedName = resapi.QualifiedName
	Attribute     = resapi.DeviceAttribute
)

const (
	// Names for standard CPU device attributes.
	AttrPackage     = QualifiedName("package")
	AttrDie         = QualifiedName("die")
	AttrCluster     = QualifiedName("cluster")
	AttrCore        = QualifiedName("core")
	AttrCoreType    = QualifiedName("coreType")
	AttrLocalMemory = QualifiedName("localMemory")
	AttrIsolated    = QualifiedName("isolated")
	AttrMinFreq     = QualifiedName("minFreq")
	AttrMaxFreq     = QualifiedName("maxFreq")
	AttrBaseFreq    = QualifiedName("baseFreq")
)

// CPUsAsDRADevices returns the given CPUs as DRA devices.
func (sys *system) CPUsAsDRADevices(ids []ID) []resapi.Device {
	devices := make([]resapi.Device, 0, len(ids))
	for _, id := range ids {
		devices = append(devices, *(sys.CPU(id).DRA()))
	}
	return devices
}

// DRA returns the CPU represented as a DRA device.
func (c *cpu) DRA(extras ...map[QualifiedName]Attribute) *resapi.Device {
	dra := &resapi.Device{
		Name: "cpu" + strconv.Itoa(c.ID()),
		Attributes: map[QualifiedName]Attribute{
			AttrPackage:     Attr(c.PackageID()),
			AttrDie:         Attr(c.DieID()),
			AttrCluster:     Attr(c.ClusterID()),
			AttrCore:        Attr(c.CoreID()),
			AttrCoreType:    Attr(c.CoreKind().String()),
			AttrLocalMemory: Attr(c.NodeID()),
			AttrIsolated:    Attr(c.Isolated()),
		},
	}

	if base := c.FrequencyRange().Base; base > 0 {
		dra.Attributes[AttrBaseFreq] = Attr(base)
	}
	if min := c.FrequencyRange().Min; min > 0 {
		dra.Attributes[AttrMinFreq] = Attr(min)
	}
	if max := c.FrequencyRange().Max; max > 0 {
		dra.Attributes[AttrMaxFreq] = Attr(max)
	}

	for idx, cache := range c.GetCaches() {
		dra.Attributes[QualifiedName(fmt.Sprintf("cache%dID", idx))] = Attr(cache.EnumID())
	}

	for _, m := range extras {
		for name, value := range m {
			if _, ok := dra.Attributes[name]; !ok {
				dra.Attributes[name] = value
			}
		}
	}

	return dra
}

// Attr returns an attribute for the given value.
func Attr(value any) Attribute {
	switch v := any(value).(type) {
	case int64:
		return Attribute{IntValue: &v}
	case int:
		val := int64(v)
		return Attribute{IntValue: &val}
	case uint64:
		val := int64(v)
		return Attribute{IntValue: &val}
	case int32:
		val := int64(v)
		return Attribute{IntValue: &val}
	case uint32:
		val := int64(v)
		return Attribute{IntValue: &val}
	case string:
		return Attribute{StringValue: &v}
	case bool:
		return Attribute{BoolValue: &v}
	default:
		val := fmt.Sprintf("<unsupported attribute type %T>", value)
		return Attribute{StringValue: &val}
	}
}
