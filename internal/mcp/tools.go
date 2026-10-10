package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gorizond/vcs-rag/internal/store"
)

// Searcher — узкий интерфейс store для инструментов (мок в тестах).
type Searcher interface {
	Search(ctx context.Context, vector []float32, f store.Filter, topK int) ([]store.ScoredPoint, error)
}

// Embedder — узкий интерфейс эмбеддингов для инструментов (мок в тестах).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

const (
	toolTimeout = 60 * time.Second // предел на вызов инструмента (таймауты → код timeout)
	defaultTopK = 5
	maxTopK     = 20 // cap из contracts/mcp-tools.md
	maxSnippet  = 400
)

// tools — реализация MCP-инструментов поверх узких интерфейсов.
type tools struct {
	embedder Embedder
	searcher Searcher
}

// --- входы (contracts/mcp-tools.md) ---

type searchCodeInput struct {
	Query    string `json:"query" jsonschema:"текст поискового запроса"`
	VCSType  string `json:"vcs_type,omitempty" jsonschema:"фильтр: gitlab | github"`
	GroupOrg string `json:"group_org,omitempty" jsonschema:"группа GitLab или организация GitHub"`
	Repo     string `json:"repo,omitempty" jsonschema:"путь репозитория"`
	Branch   string `json:"branch,omitempty" jsonschema:"ветка"`
	TopK     int    `json:"top_k,omitempty" jsonschema:"сколько результатов вернуть (по умолчанию 5, максимум 20)"`
}

type metadataQueryInput struct {
	Query    string `json:"query" jsonschema:"текст поискового запроса"`
	Repo     string `json:"repo,omitempty" jsonschema:"путь репозитория"`
	GroupOrg string `json:"group_org,omitempty" jsonschema:"группа GitLab или организация GitHub"`
	State    string `json:"state,omitempty" jsonschema:"состояние: opened | closed | merged"`
	TopK     int    `json:"top_k,omitempty" jsonschema:"сколько результатов вернуть (по умолчанию 5, максимум 20)"`
}

type ciFailuresInput struct {
	Query  string `json:"query" jsonschema:"текст поискового запроса"`
	Repo   string `json:"repo,omitempty" jsonschema:"путь репозитория"`
	Branch string `json:"branch,omitempty" jsonschema:"ветка"`
	TopK   int    `json:"top_k,omitempty" jsonschema:"сколько результатов вернуть (по умолчанию 5, максимум 20)"`
}

// --- элементы ответов (contracts/mcp-tools.md) ---

type codeHit struct {
	Repo      string  `json:"repo"`
	GroupOrg  string  `json:"group_org"`
	Branch    string  `json:"branch"`
	FilePath  string  `json:"file_path"`
	CommitSHA string  `json:"commit_sha"`
	Score     float32 `json:"score"`
	Snippet   string  `json:"snippet"`
}

type prHit struct {
	VCSType      string  `json:"vcs_type"`
	Repo         string  `json:"repo"`
	PRID         int64   `json:"pr_id"`
	SourceBranch string  `json:"source_branch"`
	TargetBranch string  `json:"target_branch"`
	State        string  `json:"state"`
	Score        float32 `json:"score"`
	Snippet      string  `json:"snippet"`
}

type issueHit struct {
	VCSType string  `json:"vcs_type"`
	Repo    string  `json:"repo"`
	IssueID int64   `json:"issue_id"`
	State   string  `json:"state"`
	Score   float32 `json:"score"`
	Snippet string  `json:"snippet"`
}

type ciHit struct {
	VCSType    string  `json:"vcs_type"`
	Repo       string  `json:"repo"`
	Branch     string  `json:"branch"`
	CommitSHA  string  `json:"commit_sha"`
	PipelineID string  `json:"pipeline_id"`
	JobName    string  `json:"job_name"`
	Score      float32 `json:"score"`
	Snippet    string  `json:"snippet"`
}

// registerTools подключает инструменты из contracts/mcp-tools.md.
func registerTools(server *mcp.Server, t *tools) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_code",
		Description: "Семантический поиск по проиндексированному коду",
	}, t.searchCode)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_pull_requests",
		Description: "Семантический поиск по pull/merge requests",
	}, t.searchPRs)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_issues",
		Description: "Семантический поиск по issues (индексация — US4/FR-012)",
	}, t.searchIssues)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_ci_failures",
		Description: "Семантический поиск по логам упавших CI-джобов",
	}, t.searchCIFailures)
}

// searchCode — семантический поиск по коду (T046).
// Out=any: успех — массив codeHit, ошибка — toolError ({"error": {...}}).
func (t *tools) searchCode(ctx context.Context, _ *mcp.CallToolRequest, in searchCodeInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	points, err := t.searcher.Search(ctx, vec, store.Filter{
		VCSType:  in.VCSType,
		GroupOrg: in.GroupOrg,
		Repo:     in.Repo,
		Branch:   in.Branch,
		Type:     "code",
	}, clampTopK(in.TopK))
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	hits := make([]codeHit, 0, len(points))
	for _, p := range points {
		hits = append(hits, codeHit{
			Repo:      payloadString(p.Payload, "repo"),
			GroupOrg:  payloadString(p.Payload, "group_org"),
			Branch:    payloadString(p.Payload, "branch"),
			FilePath:  payloadString(p.Payload, "file_path"),
			CommitSHA: payloadString(p.Payload, "commit_sha"),
			Score:     p.Score,
			Snippet:   snippet(payloadString(p.Payload, "text")),
		})
	}
	return nil, hits, nil
}

// searchPRs — поиск по MR/PR (T071).
func (t *tools) searchPRs(ctx context.Context, _ *mcp.CallToolRequest, in metadataQueryInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	points, err := t.searcher.Search(ctx, vec, store.Filter{
		GroupOrg: in.GroupOrg,
		Repo:     in.Repo,
		Type:     "pull_request",
		State:    in.State,
	}, clampTopK(in.TopK))
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	hits := make([]prHit, 0, len(points))
	for _, p := range points {
		hits = append(hits, prHit{
			VCSType:      payloadString(p.Payload, "vcs_type"),
			Repo:         payloadString(p.Payload, "repo"),
			PRID:         payloadInt64(p.Payload, "pr_id"),
			SourceBranch: payloadString(p.Payload, "source_branch"),
			TargetBranch: payloadString(p.Payload, "target_branch"),
			State:        payloadString(p.Payload, "state"),
			Score:        p.Score,
			Snippet:      snippet(payloadString(p.Payload, "text")),
		})
	}
	return nil, hits, nil
}

// searchIssues — поиск по issues (T072).
func (t *tools) searchIssues(ctx context.Context, _ *mcp.CallToolRequest, in metadataQueryInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	points, err := t.searcher.Search(ctx, vec, store.Filter{
		GroupOrg: in.GroupOrg,
		Repo:     in.Repo,
		Type:     "issue",
		State:    in.State,
	}, clampTopK(in.TopK))
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	hits := make([]issueHit, 0, len(points))
	for _, p := range points {
		hits = append(hits, issueHit{
			VCSType: payloadString(p.Payload, "vcs_type"),
			Repo:    payloadString(p.Payload, "repo"),
			IssueID: payloadInt64(p.Payload, "issue_id"),
			State:   payloadString(p.Payload, "state"),
			Score:   p.Score,
			Snippet: snippet(payloadString(p.Payload, "text")),
		})
	}
	return nil, hits, nil
}

// searchCIFailures — поиск по логам упавших CI-джобов (T073).
func (t *tools) searchCIFailures(ctx context.Context, _ *mcp.CallToolRequest, in ciFailuresInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	vec, err := t.queryVector(ctx, in.Query)
	if err != nil {
		return nil, errorResult(ctx, codeEmbedFailed, err), nil
	}
	points, err := t.searcher.Search(ctx, vec, store.Filter{
		Repo:   in.Repo,
		Branch: in.Branch,
		Type:   "ci_failure",
	}, clampTopK(in.TopK))
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	hits := make([]ciHit, 0, len(points))
	for _, p := range points {
		hits = append(hits, ciHit{
			VCSType:    payloadString(p.Payload, "vcs_type"),
			Repo:       payloadString(p.Payload, "repo"),
			Branch:     payloadString(p.Payload, "branch"),
			CommitSHA:  payloadString(p.Payload, "commit_sha"),
			PipelineID: payloadString(p.Payload, "pipeline_id"),
			JobName:    payloadString(p.Payload, "job_name"),
			Score:      p.Score,
			Snippet:    snippet(payloadString(p.Payload, "text")),
		})
	}
	return nil, hits, nil
}

// queryVector вычисляет эмбеддинг поискового запроса.
func (t *tools) queryVector(ctx context.Context, query string) ([]float32, error) {
	vecs, err := t.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("эмбеддинг запроса: получено %d векторов, ожидался 1", len(vecs))
	}
	return vecs[0], nil
}

// clampTopK применяет дефолт 5 и cap 20 (contracts/mcp-tools.md).
func clampTopK(k int) int {
	switch {
	case k <= 0:
		return defaultTopK
	case k > maxTopK:
		return maxTopK
	default:
		return k
	}
}

// snippet обрезает текст до maxSnippet символов (по рунам, без разрыва UTF-8).
func snippet(text string) string {
	runes := []rune(text)
	if len(runes) <= maxSnippet {
		return text
	}
	return string(runes[:maxSnippet-1]) + "…"
}

// payloadString читает строковое поле payload.
func payloadString(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return s
}

// payloadInt64 читает целочисленное поле payload (int64 из Qdrant, с запасом).
func payloadInt64(p map[string]any, key string) int64 {
	switch v := p[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

// marshalJSON — вспомогательное для тестов: детерминированный JSON значения.
func marshalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("marshal error: %v", err)
	}
	return string(data)
}
