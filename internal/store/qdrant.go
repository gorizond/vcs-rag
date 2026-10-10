// Package store — point persistence in Qdrant (FR-010/011).
package store

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/config"
)

// Store — a wrapper over the Qdrant client and the collection name.
type Store struct {
	client     *qdrant.Client
	collection string
}

// New creates a client from the Qdrant config (URL like http://host:port).
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
			return nil, fmt.Errorf("QDRANT_URL: port %q: %w", p, err)
		}
		port = n
	}
	client, err := qdrant.NewClient(&qdrant.Config{
		Host:   host,
		Port:   port,
		APIKey: cfg.APIKey,
		UseTLS: u.Scheme == "https",
		// No version-check RPC: the client is created instantly and does not depend
		// on Qdrant availability (MCP starts when the backend is unavailable, FR-013).
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant client: %w", err)
	}
	return &Store{client: client, collection: cfg.Collection}, nil
}

// Close closes the connections.
func (s *Store) Close() error { return s.client.Close() }

// EnsureCollection checks/creates the collection and compares its dimension
// with the expected one; a mismatch fails BEFORE the first write (FR-009, quickstart §7).
func (s *Store) EnsureCollection(ctx context.Context, dims uint64) error {
	exists, err := s.client.CollectionExists(ctx, s.collection)
	if err != nil {
		return fmt.Errorf("qdrant: collection check: %w", err)
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
			return fmt.Errorf("qdrant: collection creation: %w", err)
		}
		return nil
	}
	info, err := s.client.GetCollectionInfo(ctx, s.collection)
	if err != nil {
		return fmt.Errorf("qdrant: collection info: %w", err)
	}
	got := info.GetConfig().GetParams().GetVectorsConfig().GetParams().GetSize()
	if got != dims {
		return fmt.Errorf(
			"qdrant: collection %q has dimension %d, expected %d — reindex required: drop the collection and restart the worker (quickstart §7)",
			s.collection, got, dims)
	}
	return nil
}
