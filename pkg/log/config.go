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
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log/klogcontrol"
)

type config struct {
	sync.RWMutex
	*cfgapi.Config
	sources    map[string]struct{}
	debugging  map[string]bool
	forceDebug bool
	sourceLen  int
}

var (
	cfg = &config{
		Config:    &cfgapi.Config{},
		debugging: make(map[string]bool),
		sources:   make(map[string]struct{}),
	}
	sigCh = make(chan os.Signal, 1)

	out     io.Writer = os.Stderr
	outLock sync.Mutex
)

func SetOutput(w io.Writer) io.Writer {
	outLock.Lock()
	defer outLock.Unlock()

	old := out
	out = w
	return old
}

func Configure(config *cfgapi.Config) error {
	return cfg.configure(config)
}

func EnableDebug(source string) bool {
	return cfg.EnableDebug(source, true)
}

func Debugging(source string) bool {
	return cfg.Debugging(source)
}

func (c *config) EnableDebug(source string, enable bool) bool {
	c.Lock()
	defer c.Unlock()

	old := c.debugging[source]
	c.debugging[source] = enable
	return old
}

func (c *config) Debugging(source string) bool {
	c.RLock()
	defer c.RUnlock()

	if debugging, ok := c.debugging[source]; ok {
		return debugging
	}

	return c.debugging["*"] || c.forceDebug
}

func (c *config) ForceDebug(enable bool) bool {
	c.Lock()
	defer c.Unlock()

	old := c.forceDebug
	c.forceDebug = enable

	return old
}

func (c *config) DebugForced() bool {
	c.RLock()
	defer c.RUnlock()

	return c.forceDebug
}

func SetupDebugToggleSignal(sig os.Signal) {
	signal.Notify(sigCh, sig)

	go func(signals <-chan os.Signal) {
		for range signals {
			cfg.ForceDebug(!cfg.DebugForced())
			deflogger.Warnf("forced full debugging: %v", cfg.DebugForced())
		}
	}(sigCh)
}

func ClearDebugToggleSignal() {
	close(sigCh)
	cfg.ForceDebug(false)
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

	return c.sourceLen
}

func (c *config) PadSource(source string) (pre, post int) {
	pad := cfg.MaxSourceLen() - len(source)

	pre = pad / 2
	post = pad / 2

	if pad > 0 && (pad&0x1) != 0 {
		return pre + 1, post
	}
	return pre, post
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
				return fmt.Errorf("invalid debug spec %q", val)
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

func (c *config) addSource(source string) {
	c.Lock()
	defer c.Unlock()

	c.sources[source] = struct{}{}
	if len(source) > c.sourceLen {
		c.sourceLen = len(source)
	}
}

func ConfigFromEnv() *cfgapi.Config {
	boolPtr := func(v bool) *bool { return &v }
	return &cfgapi.Config{
		Debug:     strings.Split(os.Getenv(DebugEnvVar), ","),
		LogSource: os.Getenv(LogSourceEnvVar) != "",
		Klog: klogcontrol.Config{
			Skip_headers: boolPtr(os.Getenv(LogSkipHdrsEnvVar) != ""),
		},
	}
}

func init() {
	if err := cfg.configure(ConfigFromEnv()); err != nil {
		fmt.Fprintf(os.Stderr, "failed to seed configuration from environment: %v\n", err)
	}
}
