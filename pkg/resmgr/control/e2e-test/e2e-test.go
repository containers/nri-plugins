// Copyright 2023 Intel Corporation. All Rights Reserved.
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

package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/containers/nri-plugins/pkg/instrumentation"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/control"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control"
)

const (
	// EnvVarEnableTestAPIs controls if test APIS are enabled (currently e2e test controller).
	EnvVarEnableTestAPIs = "ENABLE_TEST_APIS"

	// ControllerName is the name of this controller.
	ControllerName = "e2e-test"

	controllerEvent = "ControllerEvent"
	preCreate       = "PreCreate"
	preStart        = "PreStart"
	postStart       = "PostStart"
	postUpdate      = "PostUpdate"
	postStop        = "PostStop"
)

var (
	enableTestAPIs = (os.Getenv(EnvVarEnableTestAPIs) != "")
)

// testctl encapsulates the runtime state of our test controller.
type testctl struct {
	sync.Mutex `json:"-"` // we're lockable
	Log        map[string][]string
	registered bool
}

var log logger.Logger = logger.NewLogger(ControllerName)

// Controller singleton instance.
var singleton *testctl

// getE2ETestController returns the (singleton) e2e test controller instance.
func getE2ETestController() *testctl {
	if singleton == nil {
		singleton = &testctl{}
		singleton.Log = make(map[string][]string)
	}
	return singleton
}

// Start initializes the controller for enforcing decisions.
func (ctl *testctl) Start(cache cache.Cache, cfg *cfgapi.Config) (bool, error) {
	log.Debug("Start called (with test APIs %s)",
		map[bool]string{false: "disabled", true: "enabled"})

	if !enableTestAPIs {
		return false, nil
	}

	ctl.registerHandler()
	ctl.Log[controllerEvent] = append(ctl.Log[controllerEvent], "Start")

	return true, nil
}

// Stop shuts down the controller.
func (ctl *testctl) Stop() {
	log.Debug("Stop called")
	ctl.Log[controllerEvent] = append(ctl.Log[controllerEvent], "Stop")
}

// PreCreateHook handler for the e2e test controller.
func (ctl *testctl) PreCreateHook(c cache.Container) error {
	log.Debug("PreCreateHook called for %s", c.GetName())
	ctl.Log[preCreate] = append(ctl.Log[preCreate], c.GetName())
	return nil
}

// PreStartHook handler for the e2e test controller.
func (ctl *testctl) PreStartHook(c cache.Container) error {
	log.Debug("PreStartHook called for %s", c.GetName())
	ctl.Log[preStart] = append(ctl.Log[preStart], c.GetName())
	return nil
}

// PostStartHook handler for the e2e test controller.
func (ctl *testctl) PostStartHook(c cache.Container) error {
	log.Debug("PostStartHook called for %s", c.GetName())
	ctl.Log[postStart] = append(ctl.Log[postStart], c.GetName())
	return nil
}

// PostUpdateHook handler for the e2e test controller.
func (ctl *testctl) PostUpdateHook(c cache.Container) error {
	log.Debug("PostUpdateHook called for %s", c.GetName())
	ctl.Log[postUpdate] = append(ctl.Log[postUpdate], c.GetName())
	return nil
}

// PostStopHook handler for the e2e test controller.
func (ctl *testctl) PostStopHook(c cache.Container) error {
	log.Debug("PostStopHook called for %s", c.GetName())
	ctl.Log[postStop] = append(ctl.Log[postStop], c.GetName())
	return nil
}

// dumpE2ETestControllerState prints internal info used by e2e testing script.
func (ctl *testctl) dumpE2ETestControllerState(w http.ResponseWriter, req *http.Request) {
	log.Debug("output E2E test controller state...")

	ctl.Lock()
	defer ctl.Unlock()

	log.Debug("snapshot %v", ctl)

	data, err := json.Marshal(ctl)
	if err != nil {
		return
	}

	fmt.Fprintf(w, "%s\r\n", data)
}

func (ctl *testctl) registerHandler() {
	if ctl.registered {
		return
	}
	mux := instrumentation.HTTPServer().GetMux()
	mux.HandleFunc("/e2e-test-controller-state", ctl.dumpE2ETestControllerState)
	ctl.registered = true
}

// Register us as a controller.
func init() {
	control.Register(ControllerName, "Test controller", getE2ETestController())
}
