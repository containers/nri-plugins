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
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/blockio"
)

//
// Notes:
//   This is now just a placeholder, mostly to keep our RDT and block I/O
//   control implementation aligned, since both of them are handled using
//   goresctrl in the runtime currently. However unlike for RDT, we can't
//   easily split class configuration and class translation to (cgroup io
//   control) parameters between two processes (NRI plugins and runtime),
//   because class configuration is just an in-process mapping of names
//   to parameters. We'll need more work to bring block I/O control up to
//   the same level as RDT, for instance by adding block I/O cgroup v2
//   support to goresctrl, doing class name to parameter conversion here,
//   and using the v2 unified NRI field to pass those to the runtime (and
//   check if this works properly with runc/crun).

type blkioControl struct {
	resmgr   *resmgr
	hostRoot string
}

func newBlockioControl(resmgr *resmgr, hostRoot string) *blkioControl {
	return &blkioControl{
		resmgr:   resmgr,
		hostRoot: hostRoot,
	}
}

func (c *blkioControl) configure(cfg *blockio.Config) error {
	if cfg == nil {
		return nil
	}

	c.resmgr.cache.ConfigureBlockIOControl(cfg.Enable)

	return nil
}
