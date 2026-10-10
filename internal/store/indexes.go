package store

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// indexFields — keyword-индексы payload. Первые пять — контракт
// (data-model §3/QueryFilter); commit_sha и file_path — расширение для
// prune/удаления файлов (отмечено в отчёте фазы).
var indexFields = []string{"vcs_type", "group_org", "repo", "type", "branch", "commit_sha", "file_path"}

// EnsureIndexes создаёт keyword-индексы (идемпотентно — повторный вызов
// для существующего индекса безвреден).
func (s *Store) EnsureIndexes(ctx context.Context) error {
	for _, field := range indexFields {
		if _, err := s.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: s.collection,
			FieldName:      field,
			FieldType:      qdrant.FieldType_FieldTypeKeyword.Enum(),
			Wait:           qdrant.PtrOf(true),
		}); err != nil {
			return fmt.Errorf("qdrant: индекс %q: %w", field, err)
		}
	}
	return nil
}
