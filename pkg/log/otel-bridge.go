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
	"sync"
)

var (
	otelLock    = &sync.RWMutex{}
	otelHandler = slog.DiscardHandler
)

func SetOtelHandler(h slog.Handler) slog.Handler {
	otelLock.Lock()
	defer otelLock.Unlock()

	old := otelHandler

	otelHandler = h
	if otelHandler == nil {
		otelHandler = slog.DiscardHandler
	}

	return old
}

func getOtelHandler() slog.Handler {
	otelLock.RLock()
	defer otelLock.RUnlock()

	return otelHandler
}

type OtelHandler struct{}

func (o *OtelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	otelLock.RLock()
	defer otelLock.RUnlock()

	return getOtelHandler().Enabled(ctx, level)
}

func (o *OtelHandler) Handle(ctx context.Context, r slog.Record) error {
	return getOtelHandler().Handle(ctx, r)
}

func (o *OtelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return getOtelHandler().WithAttrs(attrs)
}

func (o *OtelHandler) WithGroup(name string) slog.Handler {
	return getOtelHandler().WithGroup(name)
}
