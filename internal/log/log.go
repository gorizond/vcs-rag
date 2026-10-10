// Package log is a thin wrapper over the stdlib log/slog for structured
// logs (repo, phase, duration, vector count; FR-020).
package log

import (
	"io"
	"log/slog"
)

// New creates a logger over w: json=true selects JSON format, otherwise text.
func New(w io.Writer, json bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if json {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// WithRepo adds the repo attribute.
func WithRepo(l *slog.Logger, repo string) *slog.Logger { return l.With("repo", repo) }

// WithPhase adds the phase attribute.
func WithPhase(l *slog.Logger, phase string) *slog.Logger { return l.With("phase", phase) }
