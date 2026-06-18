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

package log

import (
	"context"
	"log/slog"
	"slices"
	"sync"
)

var (
	native = &struct {
		sync.RWMutex
		handler slog.Handler
		stamp   uint32
	}{
		handler: slog.DiscardHandler,
	}
)

func SetOtelHandler(h slog.Handler) slog.Handler {
	native.Lock()
	defer native.Unlock()

	h, native.handler = native.handler, h
	if native.handler == nil {
		native.handler = slog.DiscardHandler
	}
	native.stamp++

	return h
}

func GetOtelHandler() (slog.Handler, uint32) {
	native.RLock()
	defer native.RUnlock()

	return getOtelHandler()
}

func getOtelHandler() (slog.Handler, uint32) {
	return native.handler, native.stamp
}

type OtelHandler struct {
	parent  *OtelHandler
	handler slog.Handler
	source  string
	group   *string
	attrs   []Attr
	stamp   uint32
}

func NewOtelHandler(source string) *OtelHandler {
	h, stamp := GetOtelHandler()
	return &OtelHandler{
		source:  source,
		handler: h,
		stamp:   stamp,
	}
}

func (o *OtelHandler) NewChildHandlerWithGroup(group string) *OtelHandler {
	return &OtelHandler{
		parent:  o,
		source:  o.source,
		handler: o.handler.WithGroup(group),
		group:   &group,
		stamp:   o.stamp,
	}
}

func (o *OtelHandler) NewChildHandlerWithAttrs(attrs []Attr) *OtelHandler {
	attrs = slices.Clone(attrs)
	return &OtelHandler{
		parent:  o,
		source:  o.source,
		handler: o.handler.WithAttrs(attrs),
		attrs:   attrs,
		stamp:   o.stamp,
	}
}

func (o *OtelHandler) Enabled(ctx context.Context, level slog.Level) (enabled bool) {
	if level > LevelDebug {
		return o.GetHandler().Enabled(ctx, level)
	}
	return cfg.Debugging(o.source)
}

func (o *OtelHandler) Handle(ctx context.Context, r slog.Record) error {
	return o.GetHandler().Handle(ctx, r)
}

func (o *OtelHandler) WithGroup(name string) slog.Handler {
	return o.NewChildHandlerWithGroup(name)
}

func (o *OtelHandler) WithAttrs(attrs []Attr) slog.Handler {
	return o.NewChildHandlerWithAttrs(attrs)
}

func (o *OtelHandler) GetHandler() slog.Handler {
	native.RLock()
	if o.stamp == native.stamp {
		defer native.RUnlock()
		return o.handler
	}
	native.RUnlock()

	return o.updateHandlers()
}

func (o *OtelHandler) updateHandlers() slog.Handler {
	var (
		parents = []*OtelHandler{o}
		root    *OtelHandler
	)

	native.Lock()
	defer native.Unlock()

	for root = o; root.parent != nil; root = root.parent {
		parents = append(parents, root.parent)
	}

	otel, stamp := native.handler, native.stamp

	root.handler, root.stamp = otel, stamp
	prev := root
	for i := len(parents) - 2; i >= 0; i-- {
		h := parents[i]
		switch {
		case h.group != nil:
			h.handler, h.stamp = prev.handler.WithGroup(*h.group), stamp
			h.stamp = stamp
		case len(h.attrs) > 0:
			h.handler, h.stamp = prev.handler.WithAttrs(h.attrs), stamp
		default:
			h.handler, h.stamp = prev.handler, stamp
		}
		prev = h
	}

	return o.handler
}
