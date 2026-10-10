// Package log — тонкая обёртка над stdlib log/slog для структурированных
// логов (repo, фаза, длительность, число векторов; FR-020).
package log

import (
	"io"
	"log/slog"
)

// New создаёт логгер поверх w: json=true — JSON-формат, иначе текстовый.
func New(w io.Writer, json bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if json {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// WithRepo добавляет атрибут repo.
func WithRepo(l *slog.Logger, repo string) *slog.Logger { return l.With("repo", repo) }

// WithPhase добавляет атрибут phase.
func WithPhase(l *slog.Logger, phase string) *slog.Logger { return l.With("phase", phase) }
