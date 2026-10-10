package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// allEnv — все переменные, которые читает Load; перед каждым кейсом сбрасываются.
var allEnv = []string{
	"GITLAB_GROUP_ID", "GITLAB_TOKEN", "GITLAB_BASE_URL",
	"GITHUB_ORG", "GITHUB_TOKEN", "GITHUB_BASE_URL",
	"EMBEDDINGS_PROVIDER", "EMBEDDINGS_BASE_URL", "EMBEDDINGS_MODEL",
	"EMBEDDINGS_DIM", "EMBEDDINGS_BATCH",
	"QDRANT_URL", "QDRANT_API_KEY", "QDRANT_COLLECTION",
	"WORKER_INTERVAL", "WORKER_CONCURRENCY", "MIRRORS_DIR",
	"HISTORY_MAX_COMMITS", "MAX_FILE_BYTES", "CI_WINDOW",
	"MCP_TRANSPORT", "MCP_ADDR",
}

// prepareEnv сбрасывает среду и применяет кейс.
func prepareEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range allEnv {
		t.Setenv(name, "")
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
}

func TestLoadValid(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, cfg *Config)
	}{
		{
			name: "gitlab: группы, дефолты",
			env: map[string]string{
				"GITLAB_GROUP_ID":     " 111, 222 ,,",
				"GITLAB_TOKEN":        "glpat-token",
				"EMBEDDINGS_PROVIDER": "ollama",
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Provider.Type != "gitlab" {
					t.Fatalf("Type = %q, want gitlab", cfg.Provider.Type)
				}
				if want := []string{"111", "222"}; !slices.Equal(cfg.Provider.GitLabGroups, want) {
					t.Errorf("GitLabGroups = %v, want %v", cfg.Provider.GitLabGroups, want)
				}
				if cfg.Provider.GitLabBaseURL != "https://gitlab.com" {
					t.Errorf("GitLabBaseURL = %q", cfg.Provider.GitLabBaseURL)
				}
				if cfg.Provider.GitHubToken != "" || len(cfg.Provider.GitHubOrgs) != 0 {
					t.Error("GitHub-поля должны быть пустыми")
				}
				if cfg.Embeddings.BaseURL != "http://localhost:11434" {
					t.Errorf("Embeddings.BaseURL = %q", cfg.Embeddings.BaseURL)
				}
				if cfg.Embeddings.Model != "bge-m3" || cfg.Embeddings.Dim != 1024 || cfg.Embeddings.Batch != 32 {
					t.Errorf("эмбеддинги по умолчанию: %+v", cfg.Embeddings)
				}
				if cfg.Qdrant.URL != "http://localhost:6334" || cfg.Qdrant.Collection != "vcs_rag" {
					t.Errorf("qdrant по умолчанию: %+v", cfg.Qdrant)
				}
				if cfg.Worker.Interval != 60*time.Second || cfg.Worker.Concurrency != 4 ||
					cfg.Worker.MirrorsDir != "./mirrors" || cfg.Worker.HistoryMaxCommits != 2000 ||
					cfg.Worker.MaxFileBytes != 1048576 || cfg.Worker.CIWindow != 50 {
					t.Errorf("worker по умолчанию: %+v", cfg.Worker)
				}
				if cfg.MCP.Transport != "stdio" || cfg.MCP.Addr != ":8080" {
					t.Errorf("mcp по умолчанию: %+v", cfg.MCP)
				}
			},
		},
		{
			name: "github: организация, кастомные значения",
			env: map[string]string{
				"GITHUB_ORG":          "acme, inc",
				"GITHUB_TOKEN":        "ghp-token",
				"GITHUB_BASE_URL":     "https://ghe.example.com/api/v3",
				"EMBEDDINGS_PROVIDER": "lm-studio",
				"WORKER_INTERVAL":     "90s",
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Provider.Type != "github" {
					t.Fatalf("Type = %q, want github", cfg.Provider.Type)
				}
				if want := []string{"acme", "inc"}; !slices.Equal(cfg.Provider.GitHubOrgs, want) {
					t.Errorf("GitHubOrgs = %v, want %v", cfg.Provider.GitHubOrgs, want)
				}
				if cfg.Provider.GitHubBaseURL != "https://ghe.example.com/api/v3" {
					t.Errorf("GitHubBaseURL = %q", cfg.Provider.GitHubBaseURL)
				}
				if cfg.Embeddings.Provider != "lm-studio" || cfg.Embeddings.BaseURL != "http://localhost:1234" {
					t.Errorf("эмбеддинги lm-studio: %+v", cfg.Embeddings)
				}
				if cfg.Worker.Interval != 90*time.Second {
					t.Errorf("Interval = %v, want 90s", cfg.Worker.Interval)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prepareEnv(t, tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			tc.check(t, cfg)
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantProblem string
		wantMissing []string
	}{
		{
			name:        "нет провайдера",
			env:         map[string]string{"EMBEDDINGS_PROVIDER": "ollama"},
			wantMissing: []string{"GITLAB_GROUP_ID", "GITLAB_TOKEN", "GITHUB_ORG", "GITHUB_TOKEN"},
		},
		{
			name: "оба провайдера",
			env: map[string]string{
				"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t",
				"GITHUB_ORG": "acme", "GITHUB_TOKEN": "t",
				"EMBEDDINGS_PROVIDER": "ollama",
			},
			wantProblem: "оба провайдера",
		},
		{
			name:        "gitlab: группа без токена",
			env:         map[string]string{"GITLAB_GROUP_ID": "1", "EMBEDDINGS_PROVIDER": "ollama"},
			wantMissing: []string{"GITLAB_TOKEN"},
		},
		{
			name:        "gitlab: токен без группы",
			env:         map[string]string{"GITLAB_TOKEN": "t", "EMBEDDINGS_PROVIDER": "ollama"},
			wantMissing: []string{"GITLAB_GROUP_ID"},
		},
		{
			name:        "github: организация без токена",
			env:         map[string]string{"GITHUB_ORG": "acme", "EMBEDDINGS_PROVIDER": "ollama"},
			wantMissing: []string{"GITHUB_TOKEN"},
		},
		{
			name:        "эмбеддинги: провайдер не задан",
			env:         map[string]string{"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t"},
			wantMissing: []string{"EMBEDDINGS_PROVIDER"},
		},
		{
			name: "эмбеддинги: недопустимый провайдер",
			env: map[string]string{
				"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t",
				"EMBEDDINGS_PROVIDER": "openai",
			},
			wantProblem: "EMBEDDINGS_PROVIDER",
		},
		{
			name: "worker: некорректная длительность",
			env: map[string]string{
				"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t",
				"EMBEDDINGS_PROVIDER": "ollama", "WORKER_INTERVAL": "abc",
			},
			wantProblem: "WORKER_INTERVAL",
		},
		{
			name: "mcp: недопустимый транспорт",
			env: map[string]string{
				"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t",
				"EMBEDDINGS_PROVIDER": "ollama", "MCP_TRANSPORT": "ws",
			},
			wantProblem: "MCP_TRANSPORT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prepareEnv(t, tc.env)
			cfg, err := Load()
			if err == nil {
				t.Fatalf("Load() = %+v, ожидалась ошибка", cfg)
			}
			var cfgErr *ConfigError
			if !errors.As(err, &cfgErr) {
				t.Fatalf("err = %T (%v), ожидался *ConfigError", err, err)
			}
			if tc.wantProblem != "" && !strings.Contains(cfgErr.Problem, tc.wantProblem) {
				t.Errorf("Problem = %q, не содержит %q", cfgErr.Problem, tc.wantProblem)
			}
			if tc.wantMissing != nil && !slices.Equal(cfgErr.Missing, tc.wantMissing) {
				t.Errorf("Missing = %v, want %v", cfgErr.Missing, tc.wantMissing)
			}
		})
	}
}

// TestSecretNotLeaked: значения токенов не попадают в текст ошибки.
func TestSecretNotLeaked(t *testing.T) {
	const gitlabSecret = "glpat-SUPERSECRET-123"
	const githubSecret = "ghp-ANOTHER-SECRET-456"
	prepareEnv(t, map[string]string{
		"GITLAB_GROUP_ID":     "1",
		"GITLAB_TOKEN":        gitlabSecret,
		"GITHUB_ORG":          "acme",
		"GITHUB_TOKEN":        githubSecret,
		"EMBEDDINGS_PROVIDER": "ollama",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("ожидалась ошибка (оба провайдера)")
	}
	if strings.Contains(err.Error(), gitlabSecret) || strings.Contains(err.Error(), githubSecret) {
		t.Errorf("утечка секрета в тексте ошибки: %v", err)
	}
}

func TestSplitList(t *testing.T) {
	if got, want := splitList("a, b,,c"), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("splitList = %v, want %v", got, want)
	}
	if got := splitList("  ,  "); len(got) != 0 {
		t.Errorf("splitList пустого списка = %v, want пусто", got)
	}
}
