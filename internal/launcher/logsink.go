package launcher

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// logSink is a slog.Handler whose destination is set later: the session is
// established, and logs, before the session directory that holds the log
// exists. Records before Set are dropped.
type logSink struct {
	target *atomic.Pointer[slog.Handler]
	attrs  []slog.Attr
	group  string
}

func newLogSink() *logSink {
	return &logSink{target: new(atomic.Pointer[slog.Handler])}
}

// Set directs every handler derived from s to h.
func (s *logSink) Set(h slog.Handler) { s.target.Store(&h) }

func (s *logSink) handler() slog.Handler {
	p := s.target.Load()
	if p == nil {
		return nil
	}
	h := *p
	if s.group != "" {
		h = h.WithGroup(s.group)
	}
	if len(s.attrs) > 0 {
		h = h.WithAttrs(s.attrs)
	}
	return h
}

// Enabled implements slog.Handler.
func (s *logSink) Enabled(ctx context.Context, l slog.Level) bool {
	h := s.handler()
	return h != nil && h.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (s *logSink) Handle(ctx context.Context, r slog.Record) error {
	if h := s.handler(); h != nil {
		return h.Handle(ctx, r)
	}
	return nil
}

// WithAttrs implements slog.Handler.
func (s *logSink) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logSink{target: s.target, attrs: append(append([]slog.Attr(nil), s.attrs...), attrs...), group: s.group}
}

// WithGroup implements slog.Handler. Groups nest only one level deep,
// which is all tele uses.
func (s *logSink) WithGroup(name string) slog.Handler {
	return &logSink{target: s.target, attrs: s.attrs, group: name}
}
