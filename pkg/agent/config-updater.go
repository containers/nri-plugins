/*
Copyright 2019 Intel Corporation

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"time"

	"github.com/intel/nri-resmgr/pkg/log"
)

const (
	// configuration update rate-limiting timeout
	rateLimitTimeout = 2 * time.Second
	// setConfigTimeout is the duration we wait at most for a SetConfig reply
	setConfigTimeout = 5 * time.Second
	// retryTimeout is the timeout after we retry sending configuration updates upon failure
	retryTimeout = 5 * time.Second
)

// configUpdater handles sending configuration to nri-resmgr
type configUpdater interface {
	Start() error
	Stop()
	UpdateConfig(*resmgrConfig)
}

// updater implements configUpdater
type updater struct {
	log.Logger
	newConfig    chan *resmgrConfig
	notifyConfig func(*resmgrConfig) error
}

func newConfigUpdater() (configUpdater, error) {
	u := &updater{Logger: log.NewLogger("config-updater")}

	u.newConfig = make(chan *resmgrConfig)

	return u, nil
}

func (u *updater) Start() error {
	u.Info("Starting config-updater")
	go func() {
		var pendingConfig *resmgrConfig

		var ratelimit <-chan time.Time

		for {
			select {
			case cfg := <-u.newConfig:
				u.Info("scheduling update after %v rate-limiting timeout...", rateLimitTimeout)
				pendingConfig = cfg
				ratelimit = time.After(rateLimitTimeout)

			case _ = <-ratelimit:
				if pendingConfig != nil {
					mgrErr, err := u.setConfig(pendingConfig)
					if err != nil {
						u.Error("failed to send configuration update: %v", err)
						ratelimit = time.After(retryTimeout)
					} else {
						if mgrErr != nil {
							u.Error("nri-resmgr configuration error: %v", mgrErr)
						}
						pendingConfig = nil
						ratelimit = nil
					}
				}
			}
		}
	}()

	return nil
}

func (u *updater) Stop() {
}

func (u *updater) UpdateConfig(c *resmgrConfig) {
	u.newConfig <- c
}

func (u *updater) setConfig(cfg *resmgrConfig) (error, error) {
	u.Info("*** should set configuration")

	return nil, nil
}
