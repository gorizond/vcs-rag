// GitLab users (T020/US2): one account by its numeric provider id.
package gitlab

import (
	"context"
	"fmt"
	"strconv"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// GetUser returns one user account by its numeric GitLab id (GET /users/:id;
// that endpoint does not accept usernames). A 403/404 is the domain being off —
// provider.ErrDomainUnavailable.
func (c *Client) GetUser(ctx context.Context, id string) (provider.User, error) {
	uid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return provider.User{}, fmt.Errorf("gitlab: user id %q: %w", id, err)
	}
	u, _, err := c.api.Users.GetUser(uid, nil, gitlab.WithContext(ctx))
	if err != nil {
		if isDomainUnavailable(err) {
			return provider.User{}, provider.ErrDomainUnavailable
		}
		return provider.User{}, fmt.Errorf("gitlab: user %s: %w", id, err)
	}
	return provider.User{
		ID:    strconv.FormatInt(u.ID, 10),
		Login: u.Username,
		Name:  u.Name,
		State: u.State,
	}, nil
}
