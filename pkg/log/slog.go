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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log/klogcontrol"
)

const (
	AttrKeyCaller = "PC"
)

type slogger struct {
	*slog.Logger
	source string
	ctx    context.Context
}

var _ Logger = &slogger{}

type Handler struct {
	source string
	slog.Handler
}

var _ slog.Handler = &Handler{}

func NewLogger(source string) Logger {
	if source == "" {
		source = filepath.Base(filepath.Clean(os.Args[0]))
	}
	cfg.addSource(source)

	createFn := func(o *slog.HandlerOptions) slog.Handler {
		return slog.NewTextHandler(os.Stderr, o)
	}
	return &slogger{
		ctx: context.TODO(),
		Logger: slog.New(
			slog.NewMultiHandler(
				NewHandler(source, createFn, &slog.HandlerOptions{}),
				&OtelHandler{},
			),
		),
		source: source,
	}
}

func replaceAttr(group []string, a slog.Attr) slog.Attr {
	if a.Key == AttrKeyCaller {
		return slog.Attr{}
	}
	return a
}

func Get(source string) Logger {
	return NewLogger(source)
}

func Flush() {}

func SetupDebugToggleSignal(sig os.Signal) {
}

func (l *slogger) DebugEnabled() bool {
	return cfg.Debugging(l.source)
}

func (l *slogger) EnableDebug(enable bool) bool {
	return cfg.EnableDebugging(l.source, enable)
}

func (l *slogger) Log(lvl Level, msg string, args ...any) {
	l.Logger.Log(l.ctx, lvl, msg, args...)
}

func (l *slogger) Debug(msg string, args ...any) {
	l.Log(LevelDebug, msg, args...)
}

func (l *slogger) Info(msg string, args ...any) {
	l.Log(LevelInfo, msg, args...)
}

func (l *slogger) Warn(msg string, args ...any) {
	l.Log(LevelWarn, msg, args...)
}

func (l *slogger) Error(msg string, args ...any) {
	l.Log(LevelError, msg, args...)
}

func (l *slogger) Fatal(msg string, args ...any) {
	l.Log(LevelFatal, msg, args...)
	os.Exit(1)
}

func (l *slogger) Panic(msg string, args ...any) {
	l.Log(LevelPanic, msg, args...)
	panic(msg)
}

func callerPC(skip int) uintptr {
	var pcs [1]uintptr
	runtime.Callers(2+skip, pcs[:])
	return pcs[0]
}

func (l *slogger) Debugf(format string, args ...any) {
	l.Log(LevelDebug, fmt.Sprintf(format, args...), AttrKeyCaller, callerPC(1))
}

func (l *slogger) Infof(format string, args ...any) {
	l.Log(LevelInfo, fmt.Sprintf(format, args...), AttrKeyCaller, callerPC(1))
}

func (l *slogger) Warnf(format string, args ...any) {
	l.Log(LevelWarn, fmt.Sprintf(format, args...), AttrKeyCaller, callerPC(1))
}

func (l *slogger) Errorf(format string, args ...any) {
	l.Log(LevelError, fmt.Sprintf(format, args...), AttrKeyCaller, callerPC(1))
}

func (l *slogger) Fatalf(format string, args ...any) {
	l.Log(LevelFatal, fmt.Sprintf(format, args...), AttrKeyCaller, callerPC(1))
}

func (l *slogger) Panicf(format string, args ...any) {
	l.Log(LevelPanic, fmt.Sprintf(format, args...), AttrKeyCaller, callerPC(1))
}

func (l *slogger) LogContext(ctx context.Context, lvl Level, msg string, args ...any) {
	l.Logger.Log(ctx, lvl, msg, args...)
}

func (l *slogger) LogBlock(lvl Level, prefix, format string, args ...any) {
	for _, msg := range strings.Split(fmt.Sprintf(format, args...), "\n") {
		l.Log(lvl, fmt.Sprintf("%s %s", prefix, msg))
	}
}

func (l *slogger) DebugBlock(prefix, format string, args ...any) {
	l.LogBlock(LevelDebug, prefix, format, args...)
}

func (l *slogger) InfoBlock(prefix, format string, args ...any) {
	l.LogBlock(LevelInfo, prefix, format, args...)
}

func (l *slogger) WarnBlock(prefix, format string, args ...any) {
	l.LogBlock(LevelWarn, prefix, format, args...)
}

func (l *slogger) ErrorBlock(prefix, format string, args ...any) {
	l.LogBlock(LevelError, prefix, format, args...)
}

func (l *slogger) WithContext(ctx context.Context) Logger {
	return &slogger{
		ctx:    ctx,
		source: l.source,
		Logger: l.Logger,
	}
}

func (l *slogger) Flush() {
}

func (l *slogger) SlogHandler() slog.Handler {
	return l.Handler()
}

func NewHandler(source string, fn func(o *slog.HandlerOptions) slog.Handler, o *slog.HandlerOptions) *Handler {
	h := &Handler{
		source: source,
	}

	o.AddSource = true
	o.ReplaceAttr = replaceAttr

	h.Handler = fn(o)

	return h
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	if level >= slog.LevelInfo {
		return true
	}
	return cfg.Debugging(h.source)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if cfg.LogSource() {
		r.AddAttrs(slog.Any("source", h.source))

		if cfg.SkipHeaders() {
			var (
				lvl    string
				src    = ""
				sep    = ""
				fields = ""
			)
			r.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case "time", "level", AttrKeyCaller:
				case "source":
					src = fmt.Sprintf("%v", a.Value)
				default:
					fields = sep + fmt.Sprintf("%s=%v", a.Key, a.Value)
					sep = " "
				}
				return true
			})
			switch r.Level {
			case LevelDebug:
				lvl = "D"
			case LevelInfo:
				lvl = "I"
			case LevelWarn:
				lvl = "W"
			case LevelError:
				lvl = "E"
			case LevelFatal:
				lvl = "F"
			case LevelPanic:
				lvl = "P"
			default:
				lvl = r.Level.String()
			}
			var (
				pad = cfg.MaxSourceLen() - len(src)
				pre = ""
				suf = ""
			)

			if pad > 0 {
				cnt := pad / 2
				if (pad & 0x1) != 0 {
					cnt++
				}
				pre = fmt.Sprintf("%*.*s", cnt, cnt, "")
				cnt = pad / 2
				if pad > 0 {
					suf = fmt.Sprintf("%*.*s", cnt, cnt, "")
				}
			}

			fmt.Fprintf(os.Stderr, "%s: [%s%s%s] %s %s\n", lvl, pre, src, suf, r.Message, fields)
			return nil
		}
	}

	var pc uintptr
	if r.PC != 0 {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == AttrKeyCaller && a.Value.Kind() == slog.KindUint64 {
				pc = uintptr(a.Value.Uint64())
				return false
			}
			return true
		})
	}
	if pc != 0 {
		r.PC = pc
	}
	return h.Handler.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		Handler: h.Handler.WithAttrs(attrs),
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{
		Handler: h.Handler.WithGroup(name),
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func init() {
	config := &cfgapi.Config{
		Debug:     strings.Split(os.Getenv(DebugEnvVar), ","),
		LogSource: os.Getenv(LogSourceEnvVar) != "",
		Klog: klogcontrol.Config{
			Skip_headers: boolPtr(os.Getenv(LogSkipHdrsEnvVar) != ""),
		},
	}

	if err := cfg.configure(config); err != nil {
		fmt.Fprintf(os.Stderr, "failed to seed configuration from environment: %v", err)
	}
}
