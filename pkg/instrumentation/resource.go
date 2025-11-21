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

package instrumentation

import (
	"fmt"
	"os"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	otelresource "go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"

	"github.com/containers/nri-plugins/pkg/version"
)

var (
	resource *otelresource.Resource
	resOnce  sync.Once
)

func GetResource() (*otelresource.Resource, error) {
	var err error

	resOnce.Do(func() {
		hostname, _ := os.Hostname()
		resource, err = otelresource.Merge(
			otelresource.Default(),
			otelresource.NewWithAttributes(
				semconv.SchemaURL,
				append(
					[]attribute.KeyValue{
						semconv.ServiceName(ServiceName),
						semconv.HostNameKey.String(hostname),
						semconv.ProcessPIDKey.Int64(int64(os.Getpid())),
						attribute.String("Version", version.Version),
						attribute.String("Build", version.Build),
					},
					identity...,
				)...,
			),
		)
	})

	if err != nil {
		return nil, fmt.Errorf("failed to create OTEL resource: %w", err)
	}

	if resource == nil {
		return nil, fmt.Errorf("failed to create OTEL resource")
	}

	return resource, nil
}
