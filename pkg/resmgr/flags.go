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

package resmgr

import (
	"flag"
	"time"

	nri "github.com/containerd/nri/pkg/api"
	"github.com/containers/nri-plugins/pkg/pidfile"
)

const (
	defaultPluginName  = "resource-manager"
	defaultPluginIndex = "90"
)

// Options captures our command line parameters.
type options struct {
	HostRoot          string
	StateDir          string
	PidFile           string
	ResctrlPath       string
	FallbackConfig    string
	ForceConfig       string
	ForceConfigSignal string
	MetricsTimer      time.Duration
	RebalanceTimer    time.Duration
	DisableAgent      bool
	NriPluginName     string
	NriPluginIdx      string
	NriSocket         string
	EnableTestAPIs    bool
}

// ResourceManager command line options.
var opt = options{}

// Register us for command line option processing.
func init() {
	flag.StringVar(&opt.HostRoot, "host-root", "",
		"Directory prefix under which the host's sysfs, etc. are mounted.")
	flag.StringVar(&opt.NriPluginName, "nri-plugin-name", defaultPluginName,
		"NRI plugin name to register.")
	flag.StringVar(&opt.NriPluginIdx, "nri-plugin-index", defaultPluginIndex,
		"NRI plugin index to register.")
	flag.StringVar(&opt.NriSocket, "nri-socket", nri.DefaultSocketPath,
		"NRI unix domain socket path to connect to.")

	flag.StringVar(&opt.PidFile, "pid-file", pidfile.GetPath(),
		"PID file to write daemon PID to")
	flag.DurationVar(&opt.MetricsTimer, "metrics-interval", 0,
		"Interval for polling/gathering runtime metrics data. Use 'disable' for disabling.")
	flag.StringVar(&opt.StateDir, "state-dir", "/var/lib/nri-resource-policy",
		"Permanent storage directory path for the resource manager to store its state in.")
	flag.BoolVar(&opt.EnableTestAPIs, "enable-test-apis", false, "Allow enabling various test APIs (currently only 'e2e-test' test controller).")
	flag.BoolVar(&opt.DisableAgent, "disable-agent", false,
		"Disable K8s cluster agent.")
}
