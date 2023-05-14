// Copyright 2019-2020 Intel Corporation. All Rights Reserved.
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

package instrumentation

import (
	"fmt"

	promcli "github.com/prometheus/client_golang/prometheus"

	"github.com/containers/nri-plugins/pkg/http"
	"github.com/containers/nri-plugins/pkg/instrumentation/metrics"
	logger "github.com/containers/nri-plugins/pkg/log"
)

const (
	// ServiceName is our service name in external tracing and metrics services.
	ServiceName = "NRI-Resource-Plugin"
)

// Our logger instance.
var log = logger.NewLogger("instrumentation")

// Our instrumentation service instance.
var svc = newService()

// RegisterGatherer registers a prometheus metrics gatherer.
func RegisterGatherer(g promcli.Gatherer) {
	metrics.RegisterGatherer(g)
}

// GetHTTPMux returns our HTTP request mux for external services.
func GetHTTPMux() *http.ServeMux {
	if svc == nil {
		return nil
	}
	return svc.http.GetMux()
}

// Start our internal instrumentation services.
func Start() error {
	if svc == nil {
		return instrumentationError("cannot start, no instrumentation service instance")
	}
	return svc.Start()
}

// Stop stops our internal instrumentation services.
func Stop() {
	if svc != nil {
		svc.Stop()
	}
}

// Restart restarts our internal instrumentation services.
func Restart() error {
	if svc == nil {
		return instrumentationError("cannot restart, no instrumentation service instance")
	}
	return svc.Restart()
}

// instrumentationError produces a formatted instrumentation-specific error.
func instrumentationError(format string, args ...interface{}) error {
	return fmt.Errorf("instrumentation: "+format, args...)
}
