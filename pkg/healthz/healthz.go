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

package healthz

import (
	"fmt"
	"net/http"
	"sort"
	"sync"

	xhttp "github.com/containers/nri-plugins/pkg/http"
	logger "github.com/containers/nri-plugins/pkg/log"
)

var (
	lock     sync.Mutex
	checkers = map[string]CheckFn{}
	sorted   []string
	// our logger instance
	log = logger.NewLogger("health-check")
)

type CheckFn func() (status Status, details error)

// Status describes the health of a component or the whole.
type Status int

const (
	// just an example, we need to figure out what/if granularity makes any sense
	Healthy Status = iota
	Degraded
	NonFunctional
)

// Setup prepares the given HTTP request multiplexer for serving healthz.
func Setup(mux *xhttp.ServeMux) {
	mux.HandleFunc("/healthz", serve)
}

// serve serves a single HTTP request.
func serve(w http.ResponseWriter, req *http.Request) {
	status, details := check()
	if status == Healthy {
		w.WriteHeader(200)
		_, err := w.Write([]byte("ok"))
		if err != nil {
			log.Errorf("failed to write response: %v", err)
		}
	} else {
		errors := ""
		for _, err := range details {
			errors += fmt.Sprintf("%v\n", err)
		}
		w.WriteHeader(500)
		_, err := w.Write([]byte(errors))
		if err != nil {
			log.Errorf("failed to write response: %v", err)
		}
	}
}

// RegisterHealthChecker registers the given health checker function
func RegisterHealthChecker(name string, fn CheckFn) {
	lock.Lock()
	defer lock.Unlock()

	if _, conflict := checkers[name]; conflict {
		panic(fmt.Sprintf("checker %q already registered", name))
	}

	checkers[name] = fn
	sorted = append(sorted, name)
	sort.Strings(sorted)
}

// check is called (form the HTTP request handler) to perform custom healthcheck
func check() (Status, map[string]error) {
	status := Healthy
	details := map[string]error{}

	lock.Lock()
	defer lock.Unlock()

	for _, name := range sorted {
		if s, err := checkers[name](); s != Healthy {
			if s > status {
				status = s
			}
			if err != nil {
				details[name] = err
				log.Errorf("component %s reported unhealthy: %v", name, err)
			}
		}
	}

	return status, details
}
