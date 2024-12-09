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

package metrics_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/metrics"
)

func TestMetricsDescriptors(t *testing.T) {
	r := metrics.NewRegistry()
	require.NotNil(t, r, "non-nil registry")

	newTestGauge(t, r, "test1", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	newTestGauge(t, r, "test2", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	newTestGauge(t, r, "test3", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	newTestGauge(t, r, "test4", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))

	var (
		enabled = []string{"*"}
		none    []string
	)

	srv := newTestServer(t, r, enabled, none, 0)
	defer srv.stop()

	descriptors, _ := srv.collect(t)
	require.True(t, descriptors.HasEntry("test1", "gauge"))
	require.True(t, descriptors.HasEntry("test2", "gauge"))
	require.True(t, descriptors.HasEntry("test3", "gauge"))
	require.True(t, descriptors.HasEntry("test4", "gauge"))
}

func TestUnprefixedDefaultCollection(t *testing.T) {
	r := metrics.NewRegistry()
	require.NotNil(t, r, "non-nil registry")

	newTestGauge(t, r, "test1", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	newTestGauge(t, r, "test2", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	newTestGauge(t, r, "test3", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	newTestGauge(t, r, "test4", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))

	var (
		enabled = []string{"*"}
		none    []string
	)

	srv := newTestServer(t, r, enabled, none, 0)
	defer srv.stop()

	_, metrics := srv.collect(t)
	require.Equal(t, "0", metrics.GetValue("test1"))
	require.Equal(t, "0", metrics.GetValue("test2"))
	require.Equal(t, "0", metrics.GetValue("test3"))
	require.Equal(t, "0", metrics.GetValue("test4"))
}

func TestPrefixedDefaultCollection(t *testing.T) {
	r := metrics.NewRegistry()
	require.NotNil(t, r, "non-nil registry")

	newTestGauge(t, r, "test1")
	newTestGauge(t, r, "test2")
	newTestGauge(t, r, "test3")
	newTestGauge(t, r, "test4")

	var (
		enabled = []string{"*"}
		none    []string
	)

	srv := newTestServer(t, r, enabled, none, 0)
	defer srv.stop()

	_, metrics := srv.collect(t)
	require.Equal(t, "0", metrics.GetValue("default_test1"))
	require.Equal(t, "0", metrics.GetValue("default_test2"))
	require.Equal(t, "0", metrics.GetValue("default_test3"))
	require.Equal(t, "0", metrics.GetValue("default_test4"))
}

func TestUpdatedMetricsCollection(t *testing.T) {
	r := metrics.NewRegistry()
	require.NotNil(t, r, "non-nil registry")

	g1 := newTestGauge(t, r, "test1", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	g2 := newTestGauge(t, r, "test2", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	g3 := newTestGauge(t, r, "test3", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	g4 := newTestGauge(t, r, "test4", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))

	var (
		enabled = []string{"*"}
		none    []string
	)

	srv := newTestServer(t, r, enabled, none, 0)
	defer srv.stop()

	_, metrics := srv.collect(t)
	require.Equal(t, "0", metrics.GetValue("test1"))
	require.Equal(t, "0", metrics.GetValue("test2"))
	require.Equal(t, "0", metrics.GetValue("test3"))
	require.Equal(t, "0", metrics.GetValue("test4"))

	g1.gauge.Inc()
	g2.gauge.Set(5)
	g3.gauge.Inc()
	g4.gauge.Set(3)

	_, metrics = srv.collect(t)
	require.Equal(t, "1", metrics.GetValue("test1"))
	require.Equal(t, "5", metrics.GetValue("test2"))
	require.Equal(t, "1", metrics.GetValue("test3"))
	require.Equal(t, "3", metrics.GetValue("test4"))

	g1.gauge.Set(4)
	g2.gauge.Inc()
	g3.gauge.Set(7)
	g4.gauge.Dec()

	_, metrics = srv.collect(t)
	require.Equal(t, "4", metrics.GetValue("test1"))
	require.Equal(t, "6", metrics.GetValue("test2"))
	require.Equal(t, "7", metrics.GetValue("test3"))
	require.Equal(t, "2", metrics.GetValue("test4"))
}

func TestMetricsConfiguration(t *testing.T) {
	r := metrics.NewRegistry()
	require.NotNil(t, r)

	newTestGauge(t, r, "test1", metrics.WithGroup("group1"))
	newTestGauge(t, r, "test2", metrics.WithGroup("group1"),
		metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	newTestGauge(t, r, "test3", metrics.WithGroup("group2"),
		metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	newTestGauge(t, r, "test4", metrics.WithGroup("group2"))

	var (
		enabled = []string{"test1", "group2"}
		none    []string
	)

	srv := newTestServer(t, r, enabled, none, 0)
	defer srv.stop()

	described, metrics := srv.collect(t)
	require.True(t, described.HasEntry("group1_test1", "gauge"))
	require.True(t, described.HasEntry("test3", "gauge"))
	require.True(t, described.HasEntry("group2_test4", "gauge"))

	require.True(t, metrics.HasEntry("group1_test1"), "group1_test1 collected")
	require.False(t, metrics.HasEntry("test2"), "test2 not collected")
	require.True(t, metrics.HasEntry("test3"), "test3 collected")
	require.True(t, metrics.HasEntry("group2_test4"), "group2_test4 collected")
}

func TestMetricsPolling(t *testing.T) {
	r := metrics.NewRegistry()
	require.NotNil(t, r, "non-nil registry")

	g0 := newTestPolled(t, r, "test1", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	g1 := newTestPolled(t, r, "test2", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	g2 := newTestPolled(t, r, "test3", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))
	g3 := newTestPolled(t, r, "test4", metrics.WithCollectorOptions(metrics.WithoutSubsystem()))

	var (
		enabled  []string
		polled   = []string{"*"}
		interval = metrics.MinPollInterval
	)

	srv := newTestServer(t, r, enabled, polled, interval)
	defer srv.stop()

	_, metrics := srv.collect(t)
	require.Equal(t, "0", metrics.GetValue("test1"))
	require.Equal(t, "0", metrics.GetValue("test2"))
	require.Equal(t, "0", metrics.GetValue("test3"))
	require.Equal(t, "0", metrics.GetValue("test4"))

	g0.Set(1)
	g1.Set(2)
	g2.Set(3)
	g3.Set(4)

	_, metrics = srv.collect(t)
	require.Equal(t, "0", metrics.GetValue("test1"))
	require.Equal(t, "0", metrics.GetValue("test2"))
	require.Equal(t, "0", metrics.GetValue("test3"))
	require.Equal(t, "0", metrics.GetValue("test4"))

	g0.Inc()
	g1.Inc()
	g2.Set(7)
	g3.Set(9)

	_, metrics = srv.collect(t)
	require.Equal(t, "0", metrics.GetValue("test1"))
	require.Equal(t, "0", metrics.GetValue("test2"))
	require.Equal(t, "0", metrics.GetValue("test3"))
	require.Equal(t, "0", metrics.GetValue("test4"))

	t.Logf("waiting for metrics poll interval (%s)", interval)
	time.Sleep(interval)

	_, metrics = srv.collect(t)
	require.Equal(t, "2", metrics.GetValue("test1"))
	require.Equal(t, "3", metrics.GetValue("test2"))
	require.Equal(t, "7", metrics.GetValue("test3"))
	require.Equal(t, "9", metrics.GetValue("test4"))
}

type testGauge struct {
	name  string
	gauge prometheus.Gauge
}

func newTestGauge(t *testing.T, r *metrics.Registry, name string, options ...metrics.RegisterOption) *testGauge {
	g := &testGauge{
		name: name,
	}
	g.gauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: name,
			Help: "Test gauge " + name,
		},
	)

	require.NoError(t, r.Register(g.name, g.gauge, options...))

	return g
}

type testPolled struct {
	desc  *prometheus.Desc
	value int
}

func newTestPolled(t *testing.T, r *metrics.Registry, name string, options ...metrics.RegisterOption) *testPolled {
	p := &testPolled{
		desc: prometheus.NewDesc(name, "Help for metric "+name, nil, nil),
	}
	require.NoError(t, r.Register(name, p, options...))
	return p
}

func (p *testPolled) Describe(ch chan<- *prometheus.Desc) {
	ch <- p.desc
}

func (p *testPolled) Collect(ch chan<- prometheus.Metric) {
	m, err := prometheus.NewConstMetric(p.desc, prometheus.GaugeValue, float64(p.value))
	if err != nil {
		return
	}
	ch <- m
}

func (p *testPolled) Set(v int) {
	p.value = v
}

func (p *testPolled) Inc() {
	p.value++
}

type described []string

func (d described) HasEntry(name, kind string) bool {
	for _, e := range d {
		split := strings.Split(e, " ")
		if len(split) >= 2 && split[0] == name && split[1] == kind {
			return true
		}
	}

	return false
}

type collected []string

func (c collected) HasEntry(name string) bool {
	for _, e := range c {
		if !strings.HasPrefix(e, "#") {
			split := strings.SplitN(e, " ", 2)
			if len(split) > 0 && split[0] == name {
				return true
			}
		}
	}

	return false
}

func (c collected) HasValue(name, value string) bool {
	for _, e := range c {
		if strings.HasPrefix(e, "#") {
			continue
		}
		split := strings.SplitN(e, " ", 2)
		if len(split) == 2 && split[0] == name {
			if split[0] == name && split[1] == value {
				return true
			}
		}
	}

	return false
}

func (c collected) GetValue(name string) string {
	for _, e := range c {
		if strings.HasPrefix(e, "#") {
			continue
		}
		split := strings.SplitN(e, " ", 2)
		if len(split) == 2 && split[0] == name {
			if split[0] == name {
				return split[1]
			}
		}
	}

	return ""
}

type testServer struct {
	srv *http.Server
	r   *metrics.Registry
	g   *metrics.Gatherer
}

func newTestServer(t *testing.T, r *metrics.Registry, enabled, polled []string, poll time.Duration) *testServer {
	g, err := r.NewGatherer(
		metrics.WithMetrics(enabled, polled),
		metrics.WithPollInterval(poll),
	)
	require.NoError(t, err)
	require.NotNil(t, g)

	handlerOpts := promhttp.HandlerOpts{
		ErrorLog:      logger.Get("metrics-test"),
		ErrorHandling: promhttp.PanicOnError,
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(g, handlerOpts))

	srv := &http.Server{
		Addr:    ":4321",
		Handler: mux,
	}

	go func() {
		require.Equal(t, http.ErrServerClosed, srv.ListenAndServe())
	}()

	return &testServer{
		srv: srv,
		r:   r,
		g:   g,
	}
}

func (srv *testServer) stop() {
	if srv.srv != nil {
		err := srv.srv.Shutdown(context.Background())
		if err != nil && err != http.ErrServerClosed {
			fmt.Printf("test server shutdown failed: %v\n", err)
		}
	}
	if srv.g != nil {
		srv.g.Stop()
	}
}

func (srv *testServer) collect(t *testing.T) (described, collected) {
	resp, err := http.Get("http://localhost" + srv.srv.Addr + "/metrics")
	require.NoError(t, err)

	defer resp.Body.Close()

	var (
		types   []string
		metrics []string
		scanner = bufio.NewScanner(resp.Body)
	)

	for scanner.Scan() {
		e := scanner.Text()

		switch {
		case strings.HasPrefix(e, "# HELP"):
		case strings.HasPrefix(e, "# TYPE "):
			types = append(types, strings.TrimPrefix(e, "# TYPE "))
		default:
			metrics = append(metrics, e)
		}
	}

	return described(types), collected(metrics)
}
