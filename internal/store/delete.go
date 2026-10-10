package store

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// DeleteByFilter удаляет точки по фильтру (Wait=true).
func (s *Store) DeleteByFilter(ctx context.Context, f *qdrant.Filter) error {
	if _, err := s.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: s.collection,
		Points:         qdrant.NewPointsSelectorFilter(f),
		Wait:           qdrant.PtrOf(true),
	}); err != nil {
		return fmt.Errorf("qdrant: delete: %w", err)
	}
	return nil
}

// DeleteCodeFile удаляет все code-точки файла на ветке (перед перезаписью
// содержимого новым коммитом и при удалении файла).
func (s *Store) DeleteCodeFile(ctx context.Context, repo, branch, filePath string) error {
	return s.DeleteByFilter(ctx, codeFilter(repo, branch, qdrant.NewMatch("file_path", filePath)))
}

// DeleteBranch удаляет все code-точки исчезнувшей ветки (data-model §2).
func (s *Store) DeleteBranch(ctx context.Context, repo, branch string) error {
	return s.DeleteByFilter(ctx, codeFilter(repo, branch))
}

// DeleteCodeCommits удаляет code-точки rewritten-коммитов ветки (prune
// после force-push, FR-016).
func (s *Store) DeleteCodeCommits(ctx context.Context, repo, branch string, shas []string) error {
	if len(shas) == 0 {
		return nil
	}
	return s.DeleteByFilter(ctx, codeFilter(repo, branch, qdrant.NewMatchKeywords("commit_sha", shas...)))
}

// codeFilter собирает must-условия для code-точек репозитория/ветки.
func codeFilter(repo, branch string, extra ...*qdrant.Condition) *qdrant.Filter {
	must := []*qdrant.Condition{
		qdrant.NewMatch("type", "code"),
		qdrant.NewMatch("repo", repo),
		qdrant.NewMatch("branch", branch),
	}
	return &qdrant.Filter{Must: append(must, extra...)}
}
