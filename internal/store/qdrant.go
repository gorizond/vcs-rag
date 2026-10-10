// Package store — персистентность точек в Qdrant (FR-010/011).
package store

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/config"
)

// Store — обёртка над клиентом Qdrant и именем коллекции.
type Store struct {
	client     *qdrant.Client
	collection string
}

// New создаёт клиента по конфигурации Qdrant (URL вида http://host:port).
func New(cfg config.Qdrant) (*Store, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("QDRANT_URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		host = "localhost"
	}
	port := 6334
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("QDRANT_URL: порт %q: %w", p, err)
		}
		port = n
	}
	client, err := qdrant.NewClient(&qdrant.Config{
		Host:   host,
		Port:   port,
		APIKey: cfg.APIKey,
		UseTLS: u.Scheme == "https",
		// Без version-check RPC: клиент создаётся мгновенно и не зависит от
		// доступности Qdrant (MCP стартует при недоступном бэкенде, FR-013).
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant-клиент: %w", err)
	}
	return &Store{client: client, collection: cfg.Collection}, nil
}

// Close закрывает соединения.
func (s *Store) Close() error { return s.client.Close() }

// EnsureCollection проверяет/создаёт коллекцию и сверяет её размерность
// с ожидаемой; несовпадение — отказ ДО первой записи (FR-009, quickstart §7).
func (s *Store) EnsureCollection(ctx context.Context, dims uint64) error {
	exists, err := s.client.CollectionExists(ctx, s.collection)
	if err != nil {
		return fmt.Errorf("qdrant: проверка коллекции: %w", err)
	}
	if !exists {
		err := s.client.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: s.collection,
			VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
				Size:     dims,
				Distance: qdrant.Distance_Cosine,
			}),
		})
		if err != nil {
			return fmt.Errorf("qdrant: создание коллекции: %w", err)
		}
		return nil
	}
	info, err := s.client.GetCollectionInfo(ctx, s.collection)
	if err != nil {
		return fmt.Errorf("qdrant: информация о коллекции: %w", err)
	}
	got := info.GetConfig().GetParams().GetVectorsConfig().GetParams().GetSize()
	if got != dims {
		return fmt.Errorf(
			"qdrant: коллекция %q имеет размерность %d, ожидается %d — требуется переиндексация: удалите коллекцию и перезапустите воркер (quickstart §7)",
			s.collection, got, dims)
	}
	return nil
}
