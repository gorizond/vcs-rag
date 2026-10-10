package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// allEnv lists all variables read by Load; reset before each test case.
var allEnv = []string{
	"GITLAB_GROUP_ID", "GITLAB_TOKEN", "GITLAB_BASE_URL",
	"GITHUB_ORG", "GITHUB_TOKEN", "GITHUB_BASE_URL",
	"EMBEDDINGS_PROVIDER", "EMBEDDINGS_BASE_URL", "EMBEDDINGS_MODEL",
	"EMBEDDINGS_DIM", "EMBEDDINGS_BATCH",
	"QDRANT_URL", "QDRANT_API_KEY", "QDRANT_COLLECTION",
	"WORKER_INTERVAL", "WORKER_META_INTERVAL", "WORKER_MIRROR_INTERVAL",
	"WORKER_CONCURRENCY", "MIRRORS_DIR",
	"HISTORY_MAX_COMMITS", "MAX_FILE_BYTES", "CI_WINDOW",
	"MCP_TRANSPORT", "MCP_ADDR",
	"NATS_URL", "NATS_INSTANCE_ID",
}

// prepareEnv resets the environment and applies the test case.
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
			name: "gitlab: groups, defaults",
			env: map[string]string{
				"GITLAB_GROUP_ID":     " 111, 222 ,,",
				"GITLAB_TOKEN":        "glpat-EXAMPLE-NOT-A-TOKEN",
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
					t.Error("GitHub fields must be empty")
				}
				if cfg.Embeddings.BaseURL != "http://localhost:11434" {
					t.Errorf("Embeddings.BaseURL = %q", cfg.Embeddings.BaseURL)
				}
				if cfg.Embeddings.Model != "bge-m3" || cfg.Embeddings.Dim != 1024 || cfg.Embeddings.Batch != 32 {
					t.Errorf("default embeddings: %+v", cfg.Embeddings)
				}
				if cfg.Qdrant.URL != "http://localhost:6334" || cfg.Qdrant.Collection != "vcs_rag" {
					t.Errorf("default qdrant: %+v", cfg.Qdrant)
				}
				if cfg.Worker.Interval != 60*time.Second || cfg.Worker.Concurrency != 2 ||
					cfg.Worker.MirrorsDir != "./mirrors" || cfg.Worker.HistoryMaxCommits != 2000 ||
					cfg.Worker.MaxFileBytes != 1048576 || cfg.Worker.CIWindow != 50 {
					t.Errorf("default worker: %+v", cfg.Worker)
				}
				// T080: the dedicated lanes default to WORKER_INTERVAL.
				if cfg.Worker.MetaInterval != 60*time.Second || cfg.Worker.MirrorInterval != 60*time.Second {
					t.Errorf("lane intervals = %v/%v, want 60s/60s",
						cfg.Worker.MetaInterval, cfg.Worker.MirrorInterval)
				}
				if cfg.MCP.Transport != "stdio" || cfg.MCP.Addr != ":8080" {
					t.Errorf("default mcp: %+v", cfg.MCP)
				}
				// T108/T117: empty NATS_URL — the refresh mode is off (default instance).
				if cfg.NATS.URL != "" || cfg.NATS.InstanceID != NATSDefaultInstance {
					t.Errorf("default NATS = %+v, want empty URL / %q", cfg.NATS, NATSDefaultInstance)
				}
			},
		},
		{
			name: "nats: url and instance id are loaded",
			env: map[string]string{
				"GITLAB_GROUP_ID":     "1",
				"GITLAB_TOKEN":        "t",
				"EMBEDDINGS_PROVIDER": "ollama",
				"NATS_URL":            "nats://127.0.0.1:4222",
				"NATS_INSTANCE_ID":    "team-a",
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.NATS.URL != "nats://127.0.0.1:4222" {
					t.Errorf("NATS.URL = %q", cfg.NATS.URL)
				}
				if cfg.NATS.InstanceID != "team-a" {
					t.Errorf("NATS.InstanceID = %q, want team-a", cfg.NATS.InstanceID)
				}
			},
		},
		{
			name: "nats: instance id defaults",
			env: map[string]string{
				"GITLAB_GROUP_ID":     "1",
				"GITLAB_TOKEN":        "t",
				"EMBEDDINGS_PROVIDER": "ollama",
				"NATS_URL":            "nats://127.0.0.1:4222",
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.NATS.InstanceID != NATSDefaultInstance {
					t.Errorf("NATS.InstanceID = %q, want %q", cfg.NATS.InstanceID, NATSDefaultInstance)
				}
			},
		},
		{
			name: "github: organization, custom values",
			env: map[string]string{
				"GITHUB_ORG":          "acme, inc",
				"GITHUB_TOKEN":        "ghp-EXAMPLE-NOT-A-TOKEN",
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
					t.Errorf("lm-studio embeddings: %+v", cfg.Embeddings)
				}
				if cfg.Worker.Interval != 90*time.Second {
					t.Errorf("Interval = %v, want 90s", cfg.Worker.Interval)
				}
				// T080: WORKER_INTERVAL propagates to the lane defaults.
				if cfg.Worker.MetaInterval != 90*time.Second || cfg.Worker.MirrorInterval != 90*time.Second {
					t.Errorf("lane intervals = %v/%v, want 90s/90s",
						cfg.Worker.MetaInterval, cfg.Worker.MirrorInterval)
				}
			},
		},
		{
			name: "worker: separate meta/mirror intervals",
			env: map[string]string{
				"GITLAB_GROUP_ID":        "1",
				"GITLAB_TOKEN":           "t",
				"EMBEDDINGS_PROVIDER":    "ollama",
				"WORKER_META_INTERVAL":   "15m",
				"WORKER_MIRROR_INTERVAL": "2m",
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Worker.Interval != 60*time.Second {
					t.Errorf("Interval = %v, want the 60s default", cfg.Worker.Interval)
				}
				if cfg.Worker.MetaInterval != 15*time.Minute || cfg.Worker.MirrorInterval != 2*time.Minute {
					t.Errorf("lane intervals = %v/%v, want 15m/2m",
						cfg.Worker.MetaInterval, cfg.Worker.MirrorInterval)
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
			name:        "no provider",
			env:         map[string]string{"EMBEDDINGS_PROVIDER": "ollama"},
			wantMissing: []string{"GITLAB_GROUP_ID", "GITLAB_TOKEN", "GITHUB_ORG", "GITHUB_TOKEN"},
		},
		{
			name: "both providers",
			env: map[string]string{
				"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t",
				"GITHUB_ORG": "acme", "GITHUB_TOKEN": "t",
				"EMBEDDINGS_PROVIDER": "ollama",
			},
			wantProblem: "both providers",
		},
		{
			name:        "gitlab: group without token",
			env:         map[string]string{"GITLAB_GROUP_ID": "1", "EMBEDDINGS_PROVIDER": "ollama"},
			wantMissing: []string{"GITLAB_TOKEN"},
		},
		{
			name:        "gitlab: token without group",
			env:         map[string]string{"GITLAB_TOKEN": "t", "EMBEDDINGS_PROVIDER": "ollama"},
			wantMissing: []string{"GITLAB_GROUP_ID"},
		},
		{
			name:        "github: organization without token",
			env:         map[string]string{"GITHUB_ORG": "acme", "EMBEDDINGS_PROVIDER": "ollama"},
			wantMissing: []string{"GITHUB_TOKEN"},
		},
		{
			name:        "embeddings: provider not set",
			env:         map[string]string{"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t"},
			wantMissing: []string{"EMBEDDINGS_PROVIDER"},
		},
		{
			name: "embeddings: invalid provider",
			env: map[string]string{
				"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t",
				"EMBEDDINGS_PROVIDER": "openai",
			},
			wantProblem: "EMBEDDINGS_PROVIDER",
		},
		{
			name: "worker: invalid duration",
			env: map[string]string{
				"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t",
				"EMBEDDINGS_PROVIDER": "ollama", "WORKER_INTERVAL": "abc",
			},
			wantProblem: "WORKER_INTERVAL",
		},
		{
			name: "worker: invalid meta interval",
			env: map[string]string{
				"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t",
				"EMBEDDINGS_PROVIDER": "ollama", "WORKER_META_INTERVAL": "abc",
			},
			wantProblem: "WORKER_META_INTERVAL",
		},
		{
			name: "nats: invalid instance id (mode on)",
			env: map[string]string{
				"GITLAB_GROUP_ID": "1", "GITLAB_TOKEN": "t",
				"EMBEDDINGS_PROVIDER": "ollama",
				"NATS_URL":            "nats://127.0.0.1:4222",
				"NATS_INSTANCE_ID":    "Bad Id",
			},
			wantProblem: "NATS_INSTANCE_ID",
		},
		{
			name: "mcp: invalid transport",
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
				t.Fatalf("Load() = %+v, expected an error", cfg)
			}
			var cfgErr *ConfigError
			if !errors.As(err, &cfgErr) {
				t.Fatalf("err = %T (%v), want *ConfigError", err, err)
			}
			if tc.wantProblem != "" && !strings.Contains(cfgErr.Problem, tc.wantProblem) {
				t.Errorf("Problem = %q does not contain %q", cfgErr.Problem, tc.wantProblem)
			}
			if tc.wantMissing != nil && !slices.Equal(cfgErr.Missing, tc.wantMissing) {
				t.Errorf("Missing = %v, want %v", cfgErr.Missing, tc.wantMissing)
			}
		})
	}
}

// TestSecretNotLeaked: token values do not appear in the error text.
func TestSecretNotLeaked(t *testing.T) {
	const gitlabSecret = "glpat-EXAMPLE-NOT-A-TOKEN"
	const githubSecret = "ghp-EXAMPLE-NOT-A-TOKEN"
	prepareEnv(t, map[string]string{
		"GITLAB_GROUP_ID":     "1",
		"GITLAB_TOKEN":        gitlabSecret,
		"GITHUB_ORG":          "acme",
		"GITHUB_TOKEN":        githubSecret,
		"EMBEDDINGS_PROVIDER": "ollama",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("error expected (both providers)")
	}
	if strings.Contains(err.Error(), gitlabSecret) || strings.Contains(err.Error(), githubSecret) {
		t.Errorf("secret leaked into the error message: %v", err)
	}
}

func TestSplitList(t *testing.T) {
	if got, want := splitList("a, b,,c"), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("splitList = %v, want %v", got, want)
	}
	if got := splitList("  ,  "); len(got) != 0 {
		t.Errorf("splitList of an empty list = %v, want empty", got)
	}
}
