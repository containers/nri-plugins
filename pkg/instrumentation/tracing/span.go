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
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// KeyValue is an alias for the opentelemetry KeyValue attribute.
type KeyValue = attribute.KeyValue

// SpanStartOption is applied to a Span in SpanStart.
type SpanStartOption func(*SpanOptions)

// SpanEndOption is applied to a Span in Span.End.
type SpanEndOption func(*Span)

// SpanOptions contains options for starting a Span.
type SpanOptions struct {
	Options []trace.SpanStartOption
}

// WithAttributes sets initial attributes of a Span.
func WithAttributes(attrs ...attribute.KeyValue) SpanStartOption {
	return func(o *SpanOptions) {
		o.Options = append(o.Options, trace.WithAttributes(attrs...))
	}
}

// WithAttributeMap sets initial attributes of a Span from a map.
func WithAttributeMap(attrMap map[string]interface{}) SpanStartOption {
	return func(o *SpanOptions) {
		attrs := make([]attribute.KeyValue, 0, len(attrMap))
		for k, v := range attrMap {
			attrs = append(attrs, Attribute(k, v))
		}
		o.Options = append(o.Options, trace.WithAttributes(attrs...))
	}
}

// WithStatus sets the status for the span.
func WithStatus(err error) SpanEndOption {
	return func(s *Span) {
		s.SetStatus(err)
	}
}

// Span is a (wrapped open-) tracing Span.
type Span struct {
	otel trace.Span
}

// StartSpan starts a new tracing Span. Must be ended with Span.End().
func StartSpan(ctx context.Context, name string, opts ...SpanStartOption) (context.Context, *Span) {
	var t trace.Tracer

	if trc.provider == nil {
		return ctx, &Span{}
	}

	options := &SpanOptions{}
	for _, o := range opts {
		o(options)
	}

	parent := trace.SpanFromContext(ctx)
	if parent != nil && parent.SpanContext().IsValid() {
		t = parent.TracerProvider().Tracer("")
	} else {
		t = otel.Tracer("")
	}

	ctx, span := t.Start(ctx, name, options.Options...)
	return ctx, &Span{otel: span}
}

// SpanFromContext returns the current Span from the context.
func SpanFromContext(ctx context.Context) *Span {
	return &Span{otel: trace.SpanFromContext(ctx)}
}

// SetStatus sets the status of the Span.
func (s *Span) SetStatus(err error) {
	if nilSpan(s) {
		return
	}

	if err != nil {
		s.otel.RecordError(err)
		s.otel.SetStatus(codes.Error, err.Error())
		return
	}

	s.otel.SetStatus(codes.Ok, "")
}

// SetAttributes sets attributes of the Span.
func (s *Span) SetAttributes(attrs ...attribute.KeyValue) {
	if nilSpan(s) {
		return
	}
	s.otel.SetAttributes(attrs...)
}

// End the Span.
func (s *Span) End(opts ...SpanEndOption) {
	if nilSpan(s) {
		return
	}

	for _, o := range opts {
		o(s)
	}

	s.otel.End()
}

// nilSpan returns true if the wrapping if wrapped Span is nil.
func nilSpan(s *Span) bool {
	return s == nil || s.otel == nil
}

// Attribute returns an attribute with the given key and value.
func Attribute(key string, value interface{}) attribute.KeyValue {
	if value == nil {
		return attribute.String(key, "<nil>")
	}

	switch v := value.(type) {
	case string:
		return attribute.String(key, v)
	case []string:
		return attribute.StringSlice(key, v)
	case bool:
		return attribute.Bool(key, v)
	case int64:
		return attribute.Int64(key, v)
	case []int64:
		return attribute.Int64Slice(key, v)
	case float64:
		return attribute.Float64(key, v)
	case []float64:
		return attribute.Float64Slice(key, v)
	default:
		if str, ok := v.(fmt.Stringer); ok {
			return attribute.String(key, str.String())
		}
	}

	return attribute.String(key, fmt.Sprintf("%v", value))
}
