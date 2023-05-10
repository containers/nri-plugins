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

package instrumentation

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/containers/nri-plugins/pkg/config"
	"github.com/containers/nri-plugins/pkg/utils"
)

// Sampling defines how often trace samples are taken.
type Sampling float64

const (
	// Disabled is the sampling ratio to disable tracing altogether.
	Disabled Sampling = 0.0
	// Production is the sampling ratio for production environments.
	Production Sampling = 0.1
	// Testing is the sampling ration for test environments.
	Testing Sampling = 1.0

	// defaultReportPeriod is the default report period
	defaultReportPeriod = "15s"
	// defaultHTTPEndpoint is the default HTTP endpoint serving Prometheus /metrics.
	defaultHTTPEndpoint = ""
	// defaultPrometheusExport is the default state for Prometheus exporting.
	defaultPrometheusExport = "false"
)

// options encapsulates our configurable instrumentation parameters.
type options optstruct

type optstruct struct {
	// Sampling is the sampling frequency for traces.
	Sampling Sampling
	// TracingCollector is the endpoint for collecting tracing data.
	TracingCollector string

	// ReportPeriod is the OpenCensus view reporting period.
	ReportPeriod time.Duration
	// HTTPEndpoint is our HTTP endpoint, used among others to export Prometheus /metrics.
	HTTPEndpoint string
	// PrometheusExport defines whether we export /metrics to/for Prometheus.
	PrometheusExport bool `json:"PrometheusExport"`
}

// UnmarshalJSON is a resetting JSON unmarshaller for options.
func (o *options) UnmarshalJSON(raw []byte) error {
	ostruct := optstruct{}
	if err := json.Unmarshal(raw, &ostruct); err != nil {
		return instrumentationError("failed to unmashal options: %v", err)
	}
	*o = options(ostruct)
	return nil
}

// Our instrumentation options.
var opt = defaultOptions().(*options)

// MarshalJSON is the JSON marshaller for Sampling values.
func (s Sampling) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON is the JSON unmarshaller for Sampling values.
func (s *Sampling) UnmarshalJSON(raw []byte) error {
	var obj interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return instrumentationError("failed to unmarshal Sampling value: %v", err)
	}
	switch v := obj.(type) {
	case string:
		if err := s.Parse(v); err != nil {
			return err
		}
	case float64:
		*s = Sampling(v)
	default:
		return instrumentationError("invalid Sampling value of type %T: %v", obj, obj)
	}
	return nil
}

// Parse parses the given string to a Sampling value.
func (s *Sampling) Parse(value string) error {
	switch strings.ToLower(value) {
	case "disabled":
		*s = Disabled
	case "testing":
		*s = Testing
	case "production":
		*s = Production
	default:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return instrumentationError("invalid Sampling value '%s': %v", value, err)
		}
		*s = Sampling(f)
	}
	return nil
}

// String returns the Sampling value as a string.
func (s Sampling) String() string {
	switch s {
	case Disabled:
		return "disabled"
	case Production:
		return "production"
	case Testing:
		return "testing"
	}
	return strconv.FormatFloat(float64(s), 'f', -1, 64)
}

// Ratio returns the sampling ratio for the Sampling value.
func (s Sampling) Ratio() float64 {
	return float64(s)
}

// parseEnv parses the environment for default values.
func parseEnv(name, defval string, parsefn func(string) error) {
	if envval := os.Getenv(name); envval != "" {
		err := parsefn(envval)
		if err == nil {
			return
		}
		log.Error("invalid environment %s=%q: %v, using default %q", name, envval, err, defval)
	}
	if err := parsefn(defval); err != nil {
		log.Error("invalid default %s=%q: %v", name, defval, err)
	}
}

// defaultOptions returns a new options instance, all initialized to defaults.
func defaultOptions() interface{} {
	o := &options{}

	type param struct {
		defval  string
		parsefn func(string) error
	}

	params := map[string]param{
		"HTTP_ENDPOINT": {
			defaultHTTPEndpoint,
			func(v string) error { o.HTTPEndpoint = v; return nil },
		},
		"PROMETHEUS_EXPORT": {
			defaultPrometheusExport,
			func(v string) error {
				enabled, err := utils.ParseEnabled(v)
				if err != nil {
					return err
				}
				o.PrometheusExport = enabled
				return nil
			},
		},
		"REPORT_PERIOD": {
			defaultReportPeriod,
			func(v string) error {
				d, err := time.ParseDuration(v)
				if err != nil {
					return err
				}
				o.ReportPeriod = d
				return nil
			},
		},
	}

	for envvar, p := range params {
		parseEnv(envvar, p.defval, p.parsefn)
	}

	return o
}

// configNotify is our configuration udpate notification handler.
func configNotify(event config.Event, source config.Source) error {
	log.Info("instrumentation configuration is now %v", opt)

	log.Info("reconfiguring...")
	if err := svc.reconfigure(); err != nil {
		log.Error("failed to restart instrumentation: %v", err)
	}

	return nil
}

// Register us for for configuration handling.
func init() {
	config.Register("instrumentation", "Instrumentation for traces and metrics.",
		opt, defaultOptions, config.WithNotify(configNotify))
}
