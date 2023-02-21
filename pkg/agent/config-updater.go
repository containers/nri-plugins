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

	"github.com/intel/nri-resmgr/pkg/healthz"
	"github.com/intel/nri-resmgr/pkg/log"
	"github.com/intel/nri-resmgr/pkg/resmgr/config"
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
	UpdateConfig(config.RawConfig)
}

// updater implements configUpdater
type updater struct {
	log.Logger
	newConfig chan config.RawConfig
	setConfig SetConfigFn
	configErr error
}

func newConfigUpdater(setConfig SetConfigFn) (configUpdater, error) {
	u := &updater{
		Logger:    log.NewLogger("config-updater"),
		newConfig: make(chan config.RawConfig),
		setConfig: setConfig,
	}

	return u, nil
}

func (u *updater) Start() error {
	u.Info("Registering health-checker")
	healthz.RegisterHealthChecker("config", u.healthCheck)
	u.Info("Starting config-updater")
	go func() {
		var pendingConfig config.RawConfig

		var ratelimit <-chan time.Time

		for {
			select {
			case cfg := <-u.newConfig:
				u.Info("scheduling update after %v rate-limiting timeout...", rateLimitTimeout)
				pendingConfig = cfg
				ratelimit = time.After(rateLimitTimeout)

			case _ = <-ratelimit:
				if pendingConfig != nil {
					err := u.SetConfig(pendingConfig)
					if err != nil {
						u.Error("nri-resmgr configuration error: %v", err)
					}
					pendingConfig = nil
					ratelimit = nil
				}
			}
		}
	}()

	return nil
}

func (u *updater) Stop() {
}

func (u *updater) UpdateConfig(c config.RawConfig) {
	u.newConfig <- c
}

func (u *updater) SetConfig(cfg config.RawConfig) error {
	if u.setConfig != nil {
		u.configErr = u.setConfig(cfg)
	}

	return nil
}

// If the last config update failed we report unhealthy,
// otherwise healthy.
func (u *updater) healthCheck() (healthz.Status, error) {
	if u.configErr != nil {
		return healthz.Degraded, u.configErr
	}
	return healthz.Healthy, nil
}
