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
	"strings"
)

type slogger struct {
	l Logger
}

var _ slog.Handler = &slogger{}

// SetSlogLogger sets up the default logger for the slog package.
func SetSlogLogger(source string) {
	var l Logger

	if source == "" {
		l = Default()
	} else {
		l = log.get(source)
	}

	slog.SetDefault(slog.New(l.SlogHandler()))
}

func (l logger) SlogHandler() slog.Handler {
	return &slogger{l: l}
}

func (s *slogger) Enabled(_ context.Context, level slog.Level) bool {
	switch level {
	case slog.LevelDebug:
		return log.level <= LevelDebug
	case slog.LevelInfo:
		return log.level <= LevelInfo
	case slog.LevelWarn:
		return log.level <= LevelWarn
	case slog.LevelError:
		return log.level <= LevelError
	}
	return level >= slog.LevelInfo
}

func (s *slogger) Handle(_ context.Context, r slog.Record) error {
	switch r.Level {
	case slog.LevelDebug:
		s.l.Debug("%s", strings.TrimPrefix(r.Message, r.Level.String()+" "))
	case slog.LevelInfo:
		s.l.Info("%s", strings.TrimPrefix(r.Message, r.Level.String()+" "))
	case slog.LevelWarn:
		s.l.Warn("%s", strings.TrimPrefix(r.Message, r.Level.String()+" "))
	case slog.LevelError:
		s.l.Error("%s", strings.TrimPrefix(r.Message, r.Level.String()+" "))
	}
	return nil
}

func (s *slogger) WithAttrs(_ []slog.Attr) slog.Handler {
	return s
}

func (s *slogger) WithGroup(_ string) slog.Handler {
	return s
}
