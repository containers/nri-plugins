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

package log

import (
	"fmt"
	"os"
	"strings"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log"
	"github.com/containers/nri-plugins/pkg/log/klogcontrol"
	"github.com/containers/nri-plugins/pkg/utils"
)

const (
	// DefaultLevel is the default logging severity level.
	DefaultLevel = LevelInfo
	// debugEnvVar is the environment variable used to seed debugging flags.
	debugEnvVar = "LOGGER_DEBUG"
	// logSourceEnvVar is the environment variable used to seed source logging.
	logSourceEnvVar = "LOGGER_LOG_SOURCE"
	// configModule is our module name in the runtime configuration.
	configModule = "logger"
)

// srcmap tracks debugging settings for sources.
type srcmap map[string]bool

var (
	// klog control
	klogctl = klogcontrol.Get()
)

// parse parses the given string and updates the srcmap accordingly.
func (m *srcmap) parse(value string) error {
	if *m == nil {
		*m = make(srcmap)
	}
	if value = strings.TrimSpace(value); value == "" {
		return nil
	}

	prev, state, src := "", "", ""
	for _, entry := range strings.Split(value, ",") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		statesrc := strings.Split(entry, ":")
		switch len(statesrc) {
		case 2:
			state, src = statesrc[0], strings.TrimSpace(statesrc[1])
		case 1:
			state, src = "", strings.TrimSpace(statesrc[0])
		default:
			return loggerError("invalid state spec '%s' in source map", entry)
		}
		if state != "" {
			prev = state
		} else {
			state = prev
			if state == "" {
				state = "on"
			}
		}

		if src == "all" {
			src = "*"
		}

		enabled, err := utils.ParseEnabled(state)
		if err != nil {
			return loggerError("invalid state '%s' in source map", state)
		}
		(*m)[src] = enabled
	}

	return nil
}

// String returns a string representation of the srcmap.
func (m *srcmap) String() string {
	off := ""
	on := ""
	for src, state := range *m {
		if state {
			if on == "" {
				on = src
			} else {
				on += "," + src
			}
		} else {
			if off == "" {
				off = src
			} else {
				off += "," + src
			}
		}
	}

	switch {
	case on == "" && off == "":
		return ""
	case off == "":
		return "on:" + on
	case on == "":
		return "off:" + off
	}
	return "on:" + on + "," + "off:" + off
}

// Configure updates the logging configuration.
func Configure(cfg *cfgapi.Config) error {
	deflog.Info("logger configuration update %+v", cfg)

	log.Lock()
	defer log.Unlock()

	prefix := cfg.LogSource
	if toStderr := cfg.Klog.Logtostderr; toStderr != nil && *toStderr {
		if skipHeaders := cfg.Klog.Skip_headers; skipHeaders != nil && *skipHeaders {
			prefix = true
		}
	}

	debugFlags := make(srcmap)
	for _, value := range cfg.Debug {
		if err := debugFlags.parse(value); err != nil {
			Default().Error("failed to parse debug setting %q: %v", value, err)
			return fmt.Errorf("failed to parse debug setting %q: %v", value, err)
		}
	}

	log.setDbgMap(debugFlags)
	log.setPrefix(prefix)

	return klogctl.Configure(&cfg.Klog)
}

// Initialize debug logging from the environment.
func init() {
	cfg := &cfgapi.Config{
		LogSource: os.Getenv(logSourceEnvVar) != "",
	}
	if value, ok := os.LookupEnv(debugEnvVar); ok {
		debugFlags := make(srcmap)
		if err := debugFlags.parse(value); err != nil {
			Default().Error("failed to parse %s %q: %v", debugEnvVar,
				value, err)
		} else {
			cfg.Debug = []string{debugFlags.String()}
			Default().Info("seeded debug flags ($%s): %s", debugEnvVar, debugFlags.String())
		}
	}

	err := Configure(cfg)
	if err != nil {
		Default().Error("initial logging configuration failed: %v", err)
	}
}
