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

package klogcontrol

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log/klogcontrol"
	"k8s.io/klog/v2"
)

// Control implements runtime control for klog.
type Control struct {
	*flag.FlagSet
}

// Our singleton klog Control instance.
var ctl = &Control{FlagSet: flag.NewFlagSet("klog flags", flag.ContinueOnError)}

// Get returns our singleton klog Control instance.
func Get() *Control {
	return ctl
}

// Configure klog according to the given configuration.
func (c *Control) Configure(cfg *cfgapi.Config) error {
	var errs []error
	c.VisitAll(func(f *flag.Flag) {
		if value, ok := cfg.GetByFlag(f.Name); ok {
			if err := ctl.Set(f.Name, value); err != nil {
				errs = append(errs, klogError("failed to set klog flag %s to %s: %w",
					f.Name, value, err))
			}
		}
	})
	return errors.Join(errs...)
}

// getEnvForFlag returns a default value for the flag from the environment.
func getEnvForFlag(flagName string) (string, string, bool) {
	name := "LOGGER_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
	if value, ok := os.LookupEnv(name); ok {
		return name, value, true
	}
	return "", "", false
}

// klogError returns a package-specific formatted error.
func klogError(format string, args ...interface{}) error {
	return fmt.Errorf("klogcontrol: "+format, args...)
}

// init discovers klog flags and sets up dynamic control for them.
func init() {
	ctl.SetOutput(ioutil.Discard)
	klog.InitFlags(ctl.FlagSet)
	ctl.VisitAll(func(f *flag.Flag) {
		if name, value, ok := getEnvForFlag(f.Name); ok {
			if err := ctl.Set(f.Name, value); err != nil {
				klog.Errorf("klog flag %q: invalid environment default %s=%q: %v",
					f.Name, name, value, err)
			}
		} else {
			// Unless explicitly configured in the environment, by default
			// turn off headers (date, timestamp, etc.) when we're logging
			// to a journald stream.
			if f.Name == "skip_headers" {
				if value, _ := os.LookupEnv("JOURNAL_STREAM"); value != "" {
					klog.Infof("Logging to journald, forcing headers off...")
					ctl.Set(f.Name, "true")
				}
			}
		}
	})
}
