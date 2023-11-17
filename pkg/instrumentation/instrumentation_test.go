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
	"io/ioutil"
	"net/http"
	"strings"
	"testing"
)

func TestPrometheusConfiguration(t *testing.T) {
	log.EnableDebug(true)

	if cfg.HTTPEndpoint == "" {
		cfg.HTTPEndpoint = ":0"
	}

	Start()

	address := srv.GetAddress()
	if strings.HasSuffix(cfg.HTTPEndpoint, ":0") {
		cfg.HTTPEndpoint = address
	}

	checkPrometheus(t, address, !cfg.PrometheusExport)

	cfg.PrometheusExport = !cfg.PrometheusExport
	reconfigure()
	checkPrometheus(t, address, !cfg.PrometheusExport)

	cfg.PrometheusExport = !cfg.PrometheusExport
	reconfigure()
	checkPrometheus(t, address, !cfg.PrometheusExport)

	cfg.PrometheusExport = !cfg.PrometheusExport
	reconfigure()
	checkPrometheus(t, address, !cfg.PrometheusExport)

	srv.Shutdown(true)

	Stop()
}

func checkPrometheus(t *testing.T, server string, shouldFail bool) {
	rpl, err := http.Get("http://" + server + "/metrics")

	switch shouldFail {
	case false:
		if err != nil {
			t.Errorf("Prometheus HTTP GET failed: %v", err)
			return
		}

		if rpl.StatusCode != 200 {
			t.Errorf("Prometheus HTTP GET failed: %s", rpl.Status)
			return
		}

		_, err = ioutil.ReadAll(rpl.Body)
		rpl.Body.Close()
		if err != nil {
			t.Errorf("failed to read Prometheus response: %v", err)
		}
		return

	case true:
		if err == nil && rpl.StatusCode == 200 {
			t.Errorf("Prometheus HTTP GET should have failed, but it didn't.")
			return
		}
	}
}
