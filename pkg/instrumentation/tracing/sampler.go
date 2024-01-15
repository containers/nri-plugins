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

package tracing

import (
	"sync"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var (
	drop = sdktrace.SamplingResult{
		Decision:   sdktrace.Drop,
		Tracestate: trace.TraceState{},
	}

	_ sdktrace.Sampler = (*sampler)(nil)
)

type sampler struct {
	sync.RWMutex
	sampler sdktrace.Sampler
}

func (s *sampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	s.RLock()
	defer s.RUnlock()

	if s.sampler == nil {
		return drop
	}

	return s.sampler.ShouldSample(p)
}

func (s *sampler) Description() string {
	s.RLock()
	defer s.RUnlock()

	if s.sampler == nil {
		return ""
	}

	return s.sampler.Description()
}

func (s *sampler) setSampler(sampler sdktrace.Sampler) {
	s.Lock()
	defer s.Unlock()

	s.sampler = sampler
}
