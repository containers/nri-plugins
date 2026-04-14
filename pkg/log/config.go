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
	"strings"
	"sync"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log"
)

type config struct {
	sync.RWMutex
	*cfgapi.Config
	debugging map[string]bool
	sources   map[string]struct{}
	maxSrcLen int
}

var cfg = &config{
	Config:    &cfgapi.Config{},
	debugging: make(map[string]bool),
	sources:   make(map[string]struct{}),
}

func Configure(config *cfgapi.Config) error {
	return cfg.configure(config)
}

func EnableDebug(source string) bool {
	return cfg.EnableDebugging(source, true)
}

func (c *config) configure(cfg *cfgapi.Config) error {
	debugging := map[string]bool{}

	for _, value := range cfg.Debug {
		for _, val := range strings.Split(value, ",") {
			src, state := "", "on"

			val = strings.TrimSpace(val)
			split := strings.SplitN(val, ":", 2)
			switch len(split) {
			case 1:
				src = split[0]
			case 2:
				src, state = split[0], split[1]
			default:
				return fmt.Errorf("invalid state debug spec %q", val)
			}

			switch state {
			case "on", "off":
			default:
				return fmt.Errorf("invalid state '%s' in debug spec %q", state, val)
			}

			if src == "all" {
				src = "*"
			}

			debugging[src] = state == "on"
		}
	}

	c.Lock()
	defer c.Unlock()

	c.debugging = debugging
	c.Config = cfg

	return nil
}

func (c *config) Debugging(source string) bool {
	c.RLock()
	defer c.RUnlock()

	if debugging, ok := c.debugging[source]; ok {
		return debugging
	}

	return c.debugging["*"]
}

func (c *config) EnableDebugging(source string, enable bool) bool {
	c.Lock()
	defer c.Unlock()

	orig := c.debugging[source]
	c.debugging[source] = enable
	return orig
}

func (c *config) LogSource() bool {
	c.RLock()
	defer c.RUnlock()
	return c.Config.LogSource
}

func (c *config) SkipHeaders() bool {
	c.RLock()
	defer c.RUnlock()
	return c.Klog.Skip_headers != nil && *c.Klog.Skip_headers
}

func (c *config) MaxSourceLen() int {
	c.RLock()
	defer c.RUnlock()
	return c.maxSrcLen
}

func (c *config) addSource(source string) {
	c.Lock()
	defer c.Unlock()

	c.sources[source] = struct{}{}
	if len(source) > c.maxSrcLen {
		c.maxSrcLen = len(source)
	}
}
