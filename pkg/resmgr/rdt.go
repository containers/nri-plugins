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

package resmgr

import (
	"fmt"

	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/rdt"
	logger "github.com/containers/nri-plugins/pkg/log"
)

type rdtControl struct {
	resmgr   *resmgr
	hostRoot string
}

func newRdtControl(resmgr *resmgr, hostRoot string) *rdtControl {
	rdt.SetLogger(logger.Get("goresctrl"))

	if hostRoot != "" {
		rdt.SetPrefix(opt.HostRoot)
	}

	return &rdtControl{
		resmgr:   resmgr,
		hostRoot: hostRoot,
	}
}

func (c *rdtControl) configure(cfg *rdt.Config) error {
	if cfg == nil {
		return nil
	}

	if cfg.Enable {
		nativeCfg, force, err := cfg.ToGoresctrl()
		if err != nil {
			return err
		}

		if err := rdt.Initialize(""); err != nil {
			return fmt.Errorf("failed to initialize goresctrl/rdt: %w", err)
		}
		log.Info("goresctrl/rdt initialized")

		if nativeCfg != nil {
			if err := rdt.SetConfig(nativeCfg, force); err != nil {
				return fmt.Errorf("failed to configure goresctrl/rdt: %w", err)
			}
			log.Info("goresctrl/rdt configuration updated")
		}
	}

	c.resmgr.cache.ConfigureRDTControl(cfg.Enable)

	return nil
}
