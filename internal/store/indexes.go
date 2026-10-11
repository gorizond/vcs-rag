package store

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// indexFields — keyword payload indexes. The first five are the contract
// (data-model §3/QueryFilter); commit_sha and file_path are the extension
// for prune/file deletion (noted in the phase report); state (F3) is the
// must-match filter of issues/MRs (data-model §7).
var indexFields = []string{"vcs_type", "group_org", "repo", "type", "branch", "commit_sha", "file_path", "state"}

// EnsureIndexes creates keyword indexes (idempotent — a repeated call
// for an existing index is harmless).
func (s *Store) EnsureIndexes(ctx context.Context) error {
	for _, field := range indexFields {
		if _, err := s.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: s.collection,
			FieldName:      field,
			FieldType:      qdrant.FieldType_FieldTypeKeyword.Enum(),
			Wait:           qdrant.PtrOf(true),
		}); err != nil {
			return fmt.Errorf("qdrant: index %q: %w", field, err)
		}
	}
	return nil
}
