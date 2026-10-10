package gitengine

import "encoding/base64"

// AuthEnv builds the environment for network git operations (clone/fetch)
// with private repositories: a Basic header via GIT_CONFIG_* (git ≥ 2.31).
// The token NEVER appears in the URL and is not persisted to the mirror's
// .git/config — the env applies to the git process and its children only.
//
// The username:token pair follows host docs: GitLab accepts any non-empty
// name (the oauth2 convention), GitHub — x-access-token. The header is not
// host-scoped, so GHES/self-hosted work unchanged.
func AuthEnv(vcsType, token string) []string {
	if token == "" {
		return nil
	}
	user := "oauth2"
	if vcsType == "github" {
		user = "x-access-token"
	}
	cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + cred,
	}
}
