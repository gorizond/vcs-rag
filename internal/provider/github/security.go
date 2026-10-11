// GitHub security findings (T017/US1): the schema-v3 security family has no
// GitHub source this round — the domain is reported unavailable.
package github

import (
	"context"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// ListSecurityFindings reports the security domain as unavailable: GitHub's
// alert APIs (Dependabot, code scanning) expose no finding/scanner/identifier/
// scan context, so the schema-v3 security family has no source here.
func (c *Client) ListSecurityFindings(ctx context.Context, repo provider.Repo) ([]provider.VulnerabilityFinding, error) {
	return nil, provider.ErrDomainUnavailable
}
