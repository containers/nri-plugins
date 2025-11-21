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

package metrics

import (
	"path"

	logger "github.com/containers/nri-plugins/pkg/log"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/metric"
)

var (
	enabled  []string
	provider *metric.MeterProvider
	nop      = noop.NewMeterProvider()
	log      = logger.Get("metrics")
)

// SetProvider sets up the OpenTelemetry meter provider we use.
func SetProvider(p *metric.MeterProvider) {
	provider = p
}

// Configure which metrics are enabled for collection.
func Configure(enable []string) {
	enabled = enable
}

// meterProvider wraps a meter provider for limited configurability.
// In particular it allows us to transparently prefix metric names
// and use a no-op provider for disabled metric groups.
type meterProvider struct {
	*metric.MeterProvider
	group string
}

// meter wraps a meter to implement limited configurability.
type meter struct {
	otelmetric.Meter
	group      string
	omitGroup  bool
	subsys     string
	omitSubsys bool
}

// meterOption is an option for a meter.
type meterOption struct {
	otel  []otelmetric.MeterOption
	local func(*meter)
}

// Provider returns a provider for the given metric group.
func Provider(group string) *meterProvider {
	return &meterProvider{
		MeterProvider: provider,
		group:         group,
	}
}

// WithOmitGroup prevents instruments of a meter from being prefixed
// with a group name.
func WithOmitGroup() *meterOption {
	return &meterOption{
		local: func(m *meter) {
			m.omitGroup = true
		},
	}
}

// WithOmitSubsystem prevents instruments of a meter from being prefixed
// with a subsystem name.
func WithOmitSubsystem() *meterOption {
	return &meterOption{
		local: func(m *meter) {
			m.omitSubsys = true
		},
	}
}

// WithMeterOptions sets OpenTelemetry options for a meter.
func WithMeterOptions(options ...otelmetric.MeterOption) *meterOption {
	return &meterOption{
		otel: options,
	}
}

// Meter returns a meter for the given subsystem with the provided options.
func (mp *meterProvider) Meter(subsys string, options ...*meterOption) otelmetric.Meter {
	var (
		otelopts []otelmetric.MeterOption
		m        = &meter{
			subsys: subsys,
		}
	)

	if mp != nil {
		m.group = mp.group
	}

	for _, opt := range options {
		if opt.local != nil {
			opt.local(m)
		}
		if opt.otel != nil {
			otelopts = append(otelopts, opt.otel...)
		}
	}

	if mp == nil || mp.MeterProvider == nil || !IsEnabled(m.group, m.subsys) {
		log.Infof("metric %s in group %s is disabled", m.subsys, m.group)
		m.Meter = nop.Meter(subsys, otelopts...)
	} else {
		log.Infof("metric %s in group %s is enabled", m.subsys, m.group)
		m.Meter = mp.MeterProvider.Meter(subsys, otelopts...)
	}

	return m
}

// meterName returns the name of a meter, possibly prefixing the
// name with a group and a subsystem.
func (m *meter) meterName(name string) string {
	n, sep := "", ""

	if !m.omitGroup && m.group != "" {
		n, sep = m.group, "."
	}
	if !m.omitSubsys && m.subsys != "" {
		n += sep + m.subsys
		sep = "."
	}
	return n + sep + name
}

// Int64Gauge returns the corresponding instrument for the meter.
func (m *meter) Int64Gauge(name string, options ...otelmetric.Int64GaugeOption) (otelmetric.Int64Gauge, error) {
	return m.Meter.Int64Gauge(m.meterName(name), options...)
}

// Int64ObservableGauge returns the corresponding instrument for the meter.
func (m *meter) Int64ObservableGauge(name string, options ...otelmetric.Int64ObservableGaugeOption) (otelmetric.Int64ObservableGauge, error) {
	return m.Meter.Int64ObservableGauge(m.meterName(name), options...)
}

// Float64Gauge returns the corresponding instrument for the meter.
func (m *meter) Float64Gauge(name string, options ...otelmetric.Float64GaugeOption) (otelmetric.Float64Gauge, error) {
	return m.Meter.Float64Gauge(m.meterName(name), options...)
}

// Float64ObservableGauge returns the corresponding instrument for the meter.
func (m *meter) Float64ObservableGauge(name string, options ...otelmetric.Float64ObservableGaugeOption) (otelmetric.Float64ObservableGauge, error) {
	return m.Meter.Float64ObservableGauge(m.meterName(name), options...)
}

// Int64Counter returns the corresponding instrument for the meter.
func (m *meter) Int64Counter(name string, options ...otelmetric.Int64CounterOption) (otelmetric.Int64Counter, error) {
	return m.Meter.Int64Counter(m.meterName(name), options...)
}

// Int64ObservableCounter returns the corresponding instrument for the meter.
func (m *meter) Int64ObservableCounter(name string, options ...otelmetric.Int64ObservableCounterOption) (otelmetric.Int64ObservableCounter, error) {
	return m.Meter.Int64ObservableCounter(m.meterName(name), options...)
}

// Float64Counter returns the corresponding instrument for the meter.
func (m *meter) Float64Counter(name string, options ...otelmetric.Float64CounterOption) (otelmetric.Float64Counter, error) {
	return m.Meter.Float64Counter(m.meterName(name), options...)
}

// Float64ObservableCounter returns the corresponding instrument for the meter.
func (m *meter) Float64ObservableCounter(name string, options ...otelmetric.Float64ObservableCounterOption) (otelmetric.Float64ObservableCounter, error) {
	return m.Meter.Float64ObservableCounter(m.meterName(name), options...)
}

// Int64UpDownCounter returns the corresponding instrument for the meter.
func (m *meter) Int64UpDownCounter(name string, options ...otelmetric.Int64UpDownCounterOption) (otelmetric.Int64UpDownCounter, error) {
	return m.Meter.Int64UpDownCounter(m.meterName(name), options...)
}

// Int64ObservableUpDownCounter returns the corresponding instrument for the meter.
func (m *meter) Int64ObservableUpDownCounter(name string, options ...otelmetric.Int64ObservableUpDownCounterOption) (otelmetric.Int64ObservableUpDownCounter, error) {
	return m.Meter.Int64ObservableUpDownCounter(m.meterName(name), options...)
}

// Float64UpDownCounter returns the corresponding instrument for the meter.
func (m *meter) Float64UpDownCounter(name string, options ...otelmetric.Float64UpDownCounterOption) (otelmetric.Float64UpDownCounter, error) {
	return m.Meter.Float64UpDownCounter(m.meterName(name), options...)
}

// Float64ObservableUpDownCounter returns the corresponding instrument for the meter.
func (m *meter) Float64ObservableUpDownCounter(name string, options ...otelmetric.Float64ObservableUpDownCounterOption) (otelmetric.Float64ObservableUpDownCounter, error) {
	return m.Meter.Float64ObservableUpDownCounter(m.meterName(name), options...)
}

// IsEnabled returns true if the given metric group or subsystem is enabled.
func IsEnabled(group, subsys string) bool {
	for _, glob := range enabled {
		if matches(glob, group, subsys) {
			return true
		}
	}
	return false
}

// matches returns true if the given glob matches the group and subsystem.
func matches(glob, group, subsys string) bool {
	if glob == group || glob == subsys {
		return true
	}
	name := group + "/" + subsys
	if glob == name {
		return true
	}

	ok, err := path.Match(glob, group)
	if err != nil {
		log.Warnf("invalid glob pattern %q: %v", glob, err)
		return false
	}
	if ok {
		return true
	}

	ok, _ = path.Match(glob, subsys)
	if ok {
		return true
	}

	ok, _ = path.Match(glob, name)
	return ok
}
