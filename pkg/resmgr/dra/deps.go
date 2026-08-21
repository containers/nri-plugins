/*
Copyright The NRI Plugins Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dra

import (
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/containers/nri-plugins/pkg/log"
)

// DeviceLister provides the DRA device list for a given driver name.
type DeviceLister interface {
	DRADevices(driverName string) ([]resourceapi.Device, error)
}

// Deps holds the dependencies a policy binary must supply when constructing
// a Plugin.
type Deps struct {
	// KubeClient is the Kubernetes client used to publish ResourceSlice objects.
	KubeClient kubernetes.Interface
	// NodeName is the name of the node this plugin runs on.
	NodeName string
	// RegistrarDir is the directory where the plugin registrar socket is created.
	// Defaults to kubeletplugin.KubeletRegistryDir when empty.
	RegistrarDir string
	// PluginDataDir is the directory where the plugin data socket is created.
	// Defaults to kubeletplugin.KubeletPluginsDir+"/"+driverName when empty.
	PluginDataDir string
	// ValidateClasses is a closure that validates the current cpuClass
	// configuration for DRA compatibility.
	ValidateClasses func() error
	// DeviceLister returns the list of DRA devices to publish.
	DeviceLister DeviceLister
	// Logger is the logger used for all plugin log output.
	Logger log.Logger
}
