package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// maxRateLimitPause — верхняя граница паузы при лимите VCS (FR-015).
const maxRateLimitPause = 5 * time.Minute

// processSafely выполняет per-repo обработку с изоляцией отказов (FR-014):
// паника не рушит линию, ошибка логируется со структурой (repo, phase) и НЕ
// меняет state — повтор в следующем проходе; rate-limit ошибки выдерживают
// паузу min(retryAfter, maxRateLimitPause) до повтора (FR-015).
// Возвращает ошибку обработки (nil — успех или штатная отмена контекста).
func processSafely(ctx context.Context, lg *slog.Logger, repo provider.Repo, phase string, fn func() error) (err error) {
	name := repo.Namespace + "/" + repo.Path
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			lg.Error("паника в обработке репозитория", "repo", name, "phase", phase, "panic", fmt.Sprint(r))
		}
	}()

	err = fn()
	if err == nil || ctx.Err() != nil {
		return nil // штатное завершение — не шумим
	}
	if retryAfter, ok := provider.IsRateLimit(err); ok {
		pause := retryAfter
		if pause <= 0 || pause > maxRateLimitPause {
			pause = maxRateLimitPause
		}
		lg.Warn("лимит VCS: пауза перед повтором", "repo", name, "phase", phase,
			"retry_after", retryAfter.String(), "pause", pause.String())
		select {
		case <-ctx.Done():
		case <-time.After(pause):
		}
		return err
	}
	lg.Error("обработка репозитория не удалась", "repo", name, "phase", phase, "err", err)
	return err
}
