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
)

const (
	DebugEnvVar       = "LOGGER_DEBUG"
	LogSourceEnvVar   = "LOGGER_LOG_SOURCE"
	LogSkipHdrsEnvVar = "LOGGER_SKIP_HEADERS"
)

type Level = slog.Level

const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
	LevelFatal = slog.LevelError + 4
	LevelPanic = slog.LevelError + 8
)

type Logger interface {
	Log(lvl Level, msg string, args ...any)
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	Fatal(msg string, args ...any)
	Panic(msg string, args ...any)

	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Panicf(format string, args ...any)

	LogBlock(lvl Level, prefix, format string, args ...any)
	DebugBlock(prefix string, format string, args ...any)
	InfoBlock(prefix string, format string, args ...any)
	WarnBlock(prefix string, format string, args ...any)
	ErrorBlock(prefix string, format string, args ...any)

	WithContext(ctx context.Context) Logger

	Enabled(ctx context.Context, lvl Level) bool

	DebugEnabled() bool
	EnableDebug(bool) bool

	Flush()
	SlogHandler() slog.Handler
}
