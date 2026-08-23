// Package logging configures the structured logger (log/slog) shared by
// all three agentbox services. It replaces the ad-hoc stdlib log.Printf
// calls that were previously scattered through cmd/*/main.go and the
// controller's background goroutines — those carried no level, no
// structured fields, and couldn't be filtered or turned down in production
// without editing source.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New builds a slog.Logger from a level name ("debug", "info", "warn",
// "error" — case-insensitive, defaults to "info" for an empty or unknown
// value) and a format name ("text" or "json" — defaults to "text").
// Output always goes to stderr, so it never interleaves with anything a
// service intentionally writes to stdout.
func New(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
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
