package worker

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// summary — счётчики одного прохода линии (T060, FR-020): репозитории,
// успехи/ошибки, проиндексированные коммиты, prune, длительность.
type summary struct {
	phase   string
	start   time.Time
	repos   atomic.Int64
	ok      atomic.Int64
	failed  atomic.Int64
	indexed atomic.Int64
	pruned  atomic.Int64
}

// newSummary начинает проход линии.
func newSummary(phase string) *summary {
	return &summary{phase: phase, start: time.Now()}
}

// countRepo фиксирует результат обработки одного репозитория.
func (s *summary) countRepo(err error) {
	s.repos.Add(1)
	if err != nil {
		s.failed.Add(1)
		return
	}
	s.ok.Add(1)
}

// addIndexed фиксирует проиндексированные коммиты (линия index).
func (s *summary) addIndexed(n int) { s.indexed.Add(int64(n)) }

// addPruned фиксирует удалённые точки/коммиты (prune в index/metadata).
func (s *summary) addPruned(n int) { s.pruned.Add(int64(n)) }

// log пишет единую сводку прохода (FR-020); существующие логи не заменяет.
func (s *summary) log(logger *slog.Logger) {
	logger.Info("сводка прохода: "+s.phase,
		"repos", s.repos.Load(),
		"ok", s.ok.Load(),
		"errors", s.failed.Load(),
		"indexed_commits", s.indexed.Load(),
		"pruned", s.pruned.Load(),
		"duration", time.Since(s.start).Round(time.Millisecond).String(),
	)
}
