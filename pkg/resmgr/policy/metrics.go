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

package policy

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/containers/nri-plugins/pkg/metrics"
)

type PolicyCollector struct {
	policy *policy
}

func (p *policy) newPolicyCollector() *PolicyCollector {
	return &PolicyCollector{
		policy: p,
	}
}

func (c *PolicyCollector) register() error {
	return metrics.Register(c.policy.ActivePolicy(), c, metrics.WithGroup("policy"))
}

func (c *PolicyCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.policy.active.DescribeMetrics() {
		ch <- d
	}
}

func (c *PolicyCollector) Collect(ch chan<- prometheus.Metric) {
	polled := c.policy.active.PollMetrics()

	collected, err := c.policy.active.CollectMetrics(polled)
	if err != nil {
		log.Error("failed to collect metrics: %v", err)
		return
	}

	for _, m := range collected {
		ch <- m
	}
}
