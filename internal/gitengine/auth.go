package gitengine

import "encoding/base64"

// AuthEnv собирает окружение для сетевых git-операций (clone/fetch) с
// приватными репозиториями: Basic-заголовок через GIT_CONFIG_* (git ≥ 2.31).
// Токен НЕ попадает в URL и не сохраняется в .git/config зеркала — env
// действует только на процесс git и его детей.
//
// Пара username:token — по докам хостов: GitLab принимает любое непустое
// имя (конвенция oauth2), GitHub — x-access-token. Заголовок не привязан к
// хосту, поэтому GHES/self-hosted работают без изменений.
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
