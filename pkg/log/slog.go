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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type slogger struct {
	source string
	*slog.Logger
	ctx context.Context
}

var _ Logger = &slogger{}

func Get(source string) Logger {
	return NewLogger(source)
}

func NewLogger(source string) Logger {
	if source == "" {
		source = filepath.Base(filepath.Clean(os.Args[0]))
	}

	cfg.addSource(source)

	return &slogger{
		source: source,
		Logger: slog.New(
			slog.NewMultiHandler(
				NewHandler(source),
				NewOtelHandler(source),
			),
		),
		ctx: context.TODO(),
	}
}

func (l *slogger) Log(lvl Level, msg string, attrs ...Attr) {
	l.LogAttrs(l.ctx, lvl, msg, attrs...)
}

func (l *slogger) Debug(msg string, attrs ...Attr) {
	l.Log(LevelDebug, msg, attrs...)
}

func (l *slogger) Info(msg string, attrs ...Attr) {
	l.Log(LevelInfo, msg, attrs...)
}

func (l *slogger) Warn(msg string, attrs ...Attr) {
	l.Log(LevelWarn, msg, attrs...)
}

func (l *slogger) Error(msg string, attrs ...Attr) {
	l.Log(LevelError, msg, attrs...)
}

func (l *slogger) Fatal(msg string, attrs ...Attr) {
	l.Log(LevelFatal, msg, attrs...)
	os.Exit(1)
}

func (l *slogger) Panic(msg string, attrs ...Attr) {
	l.Log(LevelPanic, msg, attrs...)
	panic(msg)
}

func (l *slogger) Debugf(format string, args ...any) {
	l.Log(LevelDebug, fmt.Sprintf(format, args...))
}

func (l *slogger) Infof(format string, args ...any) {
	l.Log(LevelInfo, fmt.Sprintf(format, args...))
}

func (l *slogger) Warnf(format string, args ...any) {
	l.Log(LevelWarn, fmt.Sprintf(format, args...))
}

func (l *slogger) Errorf(format string, args ...any) {
	l.Log(LevelError, fmt.Sprintf(format, args...))
}

func (l *slogger) Fatalf(format string, args ...any) {
	l.Log(LevelFatal, fmt.Sprintf(format, args...))
	os.Exit(1)
}

func (l *slogger) Panicf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.Log(LevelPanic, msg)
	panic(msg)
}

func (l *slogger) LogContext(ctx context.Context, lvl Level, msg string, attrs ...Attr) {
	l.LogAttrs(ctx, lvl, msg, attrs...)
}

func (l *slogger) LogBlock(lvl Level, prefix, format string, args ...any) {
	for _, msg := range strings.Split(fmt.Sprintf(format, args...), "\n") {
		l.Log(lvl, prefix+" "+msg)
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

func (l *slogger) WithGroup(name string) Logger {
	return &slogger{
		ctx:    l.ctx,
		source: l.source,
		Logger: l.Logger.WithGroup(name),
	}
}

func (l *slogger) WithAttrs(args ...any) Logger {
	return &slogger{
		ctx:    l.ctx,
		source: l.source,
		Logger: l.With(args...),
	}
}

func (l *slogger) DebugEnabled() bool {
	return cfg.Debugging(l.source)
}

func (l *slogger) EnableDebug(enable bool) bool {
	return cfg.EnableDebug(l.source, enable)
}

func (l *slogger) Flush() {
}

func (l *slogger) SlogHandler() slog.Handler {
	return l.Handler()
}

type Handler struct {
	sync.Mutex
	source string
	attrs  []Attr
	group  *group
}

var _ slog.Handler = &Handler{}

type group struct {
	name  string
	attrs []Attr
	next  *group
}

func NewHandler(source string) *Handler {
	return &Handler{
		source: source,
	}
}

func (h *Handler) Enabled(_ context.Context, lvl Level) bool {
	if lvl >= LevelInfo {
		return true
	}

	return cfg.Debugging(h.source)
}

func (h *Handler) WithAttrs(attrs []Attr) slog.Handler {
	n := &Handler{
		source: h.source,
	}

	if h.group == nil {
		n.attrs = slices.Clone(h.attrs)
		n.attrs = append(n.attrs, attrs...)
	} else {
		n.group = h.group.Clone()
		n.group.attrs = slices.Clone(attrs)
	}

	return n
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{
		source: h.source,
		attrs:  h.attrs,
		group: &group{
			name: name,
			next: h.group,
		},
	}
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	var (
		msg   = bytes.NewBuffer(make([]byte, 0, 256))
		attrs = bytes.NewBuffer(make([]byte, 0, 256))
		pre   []byte
		post  []byte
	)

	if cfg.SkipHeaders() && cfg.LogSource() {
		h.LegacyFormatMessage(msg, &r)
		pre = []byte("{ ")
		post = []byte("}\n")
	} else {
		h.FormatMessage(msg, &r)
		pre = []byte(" ")
		post = []byte("\n")
	}

	h.FormatAttrs(attrs, &r)
	if attrs.Len() == 0 {
		pre = []byte("")
		post = []byte("\n")
	}

	var err error
	emit := func(buf []byte) {
		if _, e := out.Write(buf); e != nil {
			fmt.Fprintf(os.Stderr, "failed to emit log message: %v\n", err)
			if err == nil {
				err = e
			}
		}
	}

	outLock.Lock()
	defer outLock.Unlock()

	emit(msg.Bytes())
	emit(pre)
	emit(attrs.Bytes())
	emit(post)

	return err
}

func (h *Handler) LegacyFormatMessage(buf *bytes.Buffer, r *slog.Record) {
	pre, post := cfg.PadSource(h.source)
	fmt.Fprintf(buf, "%s:", r.Level.String()[:1])
	fmt.Fprintf(buf, " [%*.*s%s%*.*s]", pre, pre, "", h.source, post, post, "")
	fmt.Fprintf(buf, " %s", r.Message)
}

func (h *Handler) FormatMessage(buf *bytes.Buffer, r *slog.Record) {
	if cfg.SkipHeaders() {
		fmt.Fprintf(buf, "%s:", r.Level.String()[:1])
	} else {
		h.WriteAttr(buf, "", slog.String(slog.LevelKey, r.Level.String()))
		if !r.Time.IsZero() {
			h.WriteAttr(buf, "", slog.Time(slog.TimeKey, r.Time))
		}
	}
	h.WriteAttr(buf, "", slog.String(slog.MessageKey, r.Message))
}

func (h *Handler) FormatAttrs(buf *bytes.Buffer, r *slog.Record) {
	prefix := ""
	groups := []*group{}

	for g := h.group; g != nil; g = g.next {
		groups = append(groups, g)
	}

	for i := len(groups) - 1; i >= 0; i-- {
		g := groups[i]
		if g.name != "" {
			if prefix == "" {
				prefix = g.name + "."
			} else {
				prefix += g.name + "."
			}
		}
		if len(g.attrs) > 0 {
			for _, ga := range g.attrs {
				h.WriteAttr(buf, prefix, ga)
			}
		}
	}

	if r.NumAttrs() > 0 {
		r.Attrs(func(a slog.Attr) bool {
			h.WriteAttr(buf, "", a)
			return true
		})
	}
}

func (h *Handler) WriteAttr(buf *bytes.Buffer, prefix string, a Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}

	sep := " "
	if buf.Len() == 0 {
		sep = ""
	}

	switch a.Value.Kind() {
	case slog.KindString:
		buf.WriteString(sep)
		if a.Key == slog.LevelKey {
			fmt.Fprintf(buf, "%s%s=%s", prefix, a.Key, a.Value.String())
		} else {
			fmt.Fprintf(buf, "%s%s=%q", prefix, a.Key, a.Value.String())
		}
	case slog.KindTime:
		constLenTime := a.Value.Time().Truncate(time.Millisecond).Add(time.Millisecond / 10)
		buf.WriteString(sep)
		fmt.Fprintf(buf, "%s%s=%s", prefix, a.Key, constLenTime.Format(time.RFC3339Nano))
	case slog.KindGroup:
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return
		}
		if a.Key != "" {
			if prefix == "" {
				prefix = a.Key + "."
			} else {
				prefix += a.Key + "."
			}
		}
		for _, ga := range attrs {
			h.WriteAttr(buf, prefix, slog.Any(ga.Key, ga.Value))
		}
	default:
		buf.WriteString(sep)
		fmt.Fprintf(buf, "%s%s=%s", prefix, a.Key, a.Value)
	}
}

func (g *group) Clone() *group {
	if g == nil {
		return nil
	}
	return &group{
		name:  g.name,
		next:  g.next,
		attrs: slices.Clone(g.attrs),
	}
}
