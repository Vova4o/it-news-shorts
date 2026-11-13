// Package observability provides structured JSON logging and Prometheus
// metrics without global mutable state.
package observability

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// NewLogger creates a structured JSON logger with UTC timestamps and a service
// field. Sensitive fields must never be passed to logger attributes.
func NewLogger(service, level string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	lvl := parseLevel(level)
	opts := &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				a.Key = "timestamp"
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.UTC().Format(time.RFC3339Nano))
				}
			case slog.LevelKey:
				a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
			}
			return a
		},
	}
	handler := slog.NewJSONHandler(w, opts)
	return slog.New(handler).With(slog.String("service", service))
}

func parseLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
