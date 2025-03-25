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

package main

import (
	"os"
	"strings"

	"github.com/containers/nri-plugins/pkg/udev"
	"sigs.k8s.io/yaml"

	logger "github.com/containers/nri-plugins/pkg/log"
)

var (
	log = logger.Get("udev")
)

func main() {
	var (
		filters = parseFilters()
		events  = make(chan *udev.Event, 64)
	)

	m, err := udev.NewMonitor(udev.WithFilters(filters...))
	if err != nil {
		log.Fatalf("failed to create udev event reader: %v", err)
	}

	m.Start(events)

	for evt := range events {
		dump(evt)
	}
}

func parseFilters() []map[string]string {
	var filters []map[string]string

	for _, arg := range os.Args[1:] {
		if !strings.Contains(arg, "=") {
			continue
		}

		filter := map[string]string{}
		for _, expr := range strings.Split(arg, ",") {
			kv := strings.SplitN(expr, "=", 2)
			if len(kv) != 2 {
				log.Fatalf("invalid filter expression %s (in %s)", expr, arg)
			}
			filter[strings.ToUpper(kv[0])] = kv[1]
		}
		if len(filter) > 0 {
			log.Info("using parsed filter: %v", filter)
			filters = append(filters, filter)
		}
	}

	return filters
}

func dump(e *udev.Event) {
	dump, err := yaml.Marshal(e)
	if err != nil {
		log.Errorf("failed to marshal event: %v\n", err)
		return
	}
	log.InfoBlock("monitor ", "%s", dump)
}
