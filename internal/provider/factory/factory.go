// Package factory assembles a provider.VCSProvider from configuration (T020).
//
// It lives in a separate package: putting New into package provider is
// impossible — provider would import provider/gitlab|github, and those use
// the contract types (an import cycle). Constructors make no network calls.
package factory

import (
	"fmt"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/github"
	"github.com/gorizond/vcs-rag/internal/provider/gitlab"
)

// New creates a VCSProvider from configuration. It makes no network calls.
func New(cfg *config.Config) (provider.VCSProvider, error) {
	switch cfg.Provider.Type {
	case "gitlab":
		return gitlab.New(cfg)
	case "github":
		return github.New(cfg)
	default:
		return nil, fmt.Errorf("unknown VCS provider %q", cfg.Provider.Type)
	}
}
