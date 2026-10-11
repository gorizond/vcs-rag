// GitHub users (T021/US2): one account by login (or numeric account id).
package github

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// GetUser returns one user account (GET /users/{login}; GitHub accepts the
// numeric account id as well). The public user object carries no account state —
// State is left empty. A 403/404 is the domain being off —
// provider.ErrDomainUnavailable.
func (c *Client) GetUser(ctx context.Context, id string) (provider.User, error) {
	u, _, err := c.api.Users.Get(ctx, id)
	if err != nil {
		if isSecurityUnavailable(err) {
			return provider.User{}, provider.ErrDomainUnavailable
		}
		return provider.User{}, fmt.Errorf("github: user %s: %w", id, err)
	}
	return provider.User{
		ID:    strconv.FormatInt(u.GetID(), 10),
		Login: u.GetLogin(),
		Name:  u.GetName(),
	}, nil
}
