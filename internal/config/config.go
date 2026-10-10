// Package config загружает конфигурацию сервиса из переменных окружения и
// валидирует её до первого сетевого вызова. В сообщениях об ошибках — только
// ИМЕНА переменных; значения секретов не выводятся.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — конфигурация сервиса целиком.
type Config struct {
	Provider   Provider
	Embeddings Embeddings
	Qdrant     Qdrant
	Worker     Worker
	MCP        MCP
}

// Provider описывает выбранный VCS-провайдер (FR-003): ровно один из двух.
type Provider struct {
	Type string // "gitlab" | "github"

	GitLabGroups  []string // GITLAB_GROUP_ID, через запятую
	GitLabToken   string   // GITLAB_TOKEN (секрет)
	GitLabBaseURL string   // GITLAB_BASE_URL

	GitHubOrgs    []string // GITHUB_ORG, через запятую
	GitHubToken   string   // GITHUB_TOKEN (секрет)
	GitHubBaseURL string   // GITHUB_BASE_URL
}

// Embeddings — провайдер эмбеддингов (FR-009).
type Embeddings struct {
	Provider string // "ollama" | "lm-studio"
	BaseURL  string
	Model    string
	Dim      int
	Batch    int // ponytail: стартовый дефолт, уточняется по фактам
}

// Qdrant — векторное хранилище (FR-010).
type Qdrant struct {
	URL        string
	APIKey     string // секрет, опционален
	Collection string
}

// Worker — параметры фонового цикла (FR-005/006/018).
type Worker struct {
	Interval          time.Duration
	Concurrency       int // ponytail: стартовый дефолт, уточняется по фактам
	MirrorsDir        string
	HistoryMaxCommits int   // ponytail: стартовый дефолт, уточняется по фактам
	MaxFileBytes      int64 // ponytail: стартовый дефолт, уточняется по фактам
	CIWindow          int   // ponytail: стартовый дефолт, уточняется по фактам
}

// MCP — транспорт MCP-сервера (FR-013).
type MCP struct {
	Transport string // "stdio" | "sse"
	Addr      string // адрес для sse
}

// ConfigError — ошибка конфигурации с перечнем имён переменных, которые
// нужно задать (или конфликт, который нужно устранить).
type ConfigError struct {
	Problem string   // краткое описание проблемы
	Missing []string // имена переменных окружения
}

func (e *ConfigError) Error() string {
	if len(e.Missing) == 0 {
		return "конфигурация: " + e.Problem
	}
	return "конфигурация: " + e.Problem + ": " + strings.Join(e.Missing, ", ")
}

// Load читает переменные окружения и возвращает валидированную конфигурацию.
func Load() (*Config, error) {
	cfg := &Config{}

	// --- VCS-провайдер ---
	gitlabGroups := splitList(os.Getenv("GITLAB_GROUP_ID"))
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	githubOrgs := splitList(os.Getenv("GITHUB_ORG"))
	githubToken := os.Getenv("GITHUB_TOKEN")

	hasGitLab := len(gitlabGroups) > 0 || gitlabToken != ""
	hasGitHub := len(githubOrgs) > 0 || githubToken != ""

	switch {
	case hasGitLab && hasGitHub:
		return nil, &ConfigError{Problem: "заданы оба провайдера (GitLab и GitHub), оставьте ровно один"}
	case hasGitLab:
		if len(gitlabGroups) == 0 {
			return nil, &ConfigError{Problem: "для GitLab не задана группа", Missing: []string{"GITLAB_GROUP_ID"}}
		}
		if gitlabToken == "" {
			return nil, &ConfigError{Problem: "для GitLab не задан токен", Missing: []string{"GITLAB_TOKEN"}}
		}
		cfg.Provider.Type = "gitlab"
		cfg.Provider.GitLabGroups = gitlabGroups
		cfg.Provider.GitLabToken = gitlabToken
		cfg.Provider.GitLabBaseURL = envDefault("GITLAB_BASE_URL", "https://gitlab.com")
	case hasGitHub:
		if len(githubOrgs) == 0 {
			return nil, &ConfigError{Problem: "для GitHub не задана организация", Missing: []string{"GITHUB_ORG"}}
		}
		if githubToken == "" {
			return nil, &ConfigError{Problem: "для GitHub не задан токен", Missing: []string{"GITHUB_TOKEN"}}
		}
		cfg.Provider.Type = "github"
		cfg.Provider.GitHubOrgs = githubOrgs
		cfg.Provider.GitHubToken = githubToken
		cfg.Provider.GitHubBaseURL = envDefault("GITHUB_BASE_URL", "https://api.github.com")
	default:
		return nil, &ConfigError{
			Problem: "не задан VCS-провайдер, задайте GitLab или GitHub",
			Missing: []string{"GITLAB_GROUP_ID", "GITLAB_TOKEN", "GITHUB_ORG", "GITHUB_TOKEN"},
		}
	}

	// --- Эмбеддинги ---
	embProvider := os.Getenv("EMBEDDINGS_PROVIDER")
	switch embProvider {
	case "ollama", "lm-studio":
	case "":
		return nil, &ConfigError{
			Problem: "не задан провайдер эмбеддингов (ollama|lm-studio)",
			Missing: []string{"EMBEDDINGS_PROVIDER"},
		}
	default:
		return nil, &ConfigError{Problem: "EMBEDDINGS_PROVIDER: недопустимое значение (ollama|lm-studio)"}
	}
	embBaseURL := os.Getenv("EMBEDDINGS_BASE_URL")
	if embBaseURL == "" {
		if embProvider == "ollama" {
			embBaseURL = "http://localhost:11434"
		} else {
			embBaseURL = "http://localhost:1234"
		}
	}
	embDim, err := envInt("EMBEDDINGS_DIM", 1024)
	if err != nil {
		return nil, err
	}
	embBatch, err := envInt("EMBEDDINGS_BATCH", 32)
	if err != nil {
		return nil, err
	}
	cfg.Embeddings = Embeddings{
		Provider: embProvider,
		BaseURL:  embBaseURL,
		Model:    envDefault("EMBEDDINGS_MODEL", "bge-m3"),
		Dim:      embDim,
		Batch:    embBatch,
	}

	// --- Qdrant ---
	cfg.Qdrant = Qdrant{
		URL:        envDefault("QDRANT_URL", "http://localhost:6334"),
		APIKey:     os.Getenv("QDRANT_API_KEY"),
		Collection: envDefault("QDRANT_COLLECTION", "vcs_rag"),
	}

	// --- Worker ---
	interval, err := envDuration("WORKER_INTERVAL", 60*time.Second)
	if err != nil {
		return nil, err
	}
	concurrency, err := envInt("WORKER_CONCURRENCY", 4)
	if err != nil {
		return nil, err
	}
	historyMax, err := envInt("HISTORY_MAX_COMMITS", 2000)
	if err != nil {
		return nil, err
	}
	maxFileBytes, err := envInt("MAX_FILE_BYTES", 1<<20)
	if err != nil {
		return nil, err
	}
	ciWindow, err := envInt("CI_WINDOW", 50)
	if err != nil {
		return nil, err
	}
	cfg.Worker = Worker{
		Interval:          interval,
		Concurrency:       concurrency,
		MirrorsDir:        envDefault("MIRRORS_DIR", "./mirrors"),
		HistoryMaxCommits: historyMax,
		MaxFileBytes:      int64(maxFileBytes),
		CIWindow:          ciWindow,
	}

	// --- MCP ---
	transport := envDefault("MCP_TRANSPORT", "stdio")
	if transport != "stdio" && transport != "sse" {
		return nil, &ConfigError{Problem: "MCP_TRANSPORT: недопустимое значение (stdio|sse)"}
	}
	cfg.MCP = MCP{
		Transport: transport,
		Addr:      envDefault("MCP_ADDR", ":8080"),
	}

	return cfg, nil
}

// envDefault возвращает значение переменной окружения или дефолт.
func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envInt читает положительное целое; ошибка называет переменную, не значение.
func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, &ConfigError{Problem: name + ": ожидается положительное целое"}
	}
	return n, nil
}

// envDuration читает time.ParseDuration; ошибка называет переменную, не значение.
func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, &ConfigError{Problem: name + ": ожидается положительная длительность (например, 60s)"}
	}
	return d, nil
}

// splitList разбивает список через запятую, обрезая пробелы и пропуская пустые.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
