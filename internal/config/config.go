// Package config loads the service configuration from environment variables and
// validates it before the first network call. Error messages contain only
// variable NAMES; secret values are never printed.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the complete service configuration.
type Config struct {
	Provider   Provider
	Embeddings Embeddings
	Qdrant     Qdrant
	Worker     Worker
	MCP        MCP
	NATS       NATS
}

// NATS is the local JetStream connection for on-demand refresh (T108/T117):
// an empty URL disables the feature — the behavior stays as before.
type NATS struct {
	URL        string // NATS_URL
	InstanceID string // NATS_INSTANCE_ID (default "default"), see RefreshNames
}

// Provider describes the selected VCS provider (FR-003): exactly one of the two.
type Provider struct {
	Type string // "gitlab" | "github"

	GitLabGroups  []string // GITLAB_GROUP_ID, comma-separated
	GitLabToken   string   // GITLAB_TOKEN (secret)
	GitLabBaseURL string   // GITLAB_BASE_URL

	GitHubOrgs    []string // GITHUB_ORG, comma-separated
	GitHubToken   string   // GITHUB_TOKEN (secret)
	GitHubBaseURL string   // GITHUB_BASE_URL
}

// Embeddings is the embeddings provider (FR-009).
type Embeddings struct {
	Provider string // "ollama" | "lm-studio"
	BaseURL  string
	Model    string
	Dim      int
	Batch    int // ponytail: starting default, to be refined based on facts
}

// Qdrant is the vector store (FR-010).
type Qdrant struct {
	URL        string
	APIKey     string // secret, optional
	Collection string
}

// Worker holds background loop parameters (FR-005/006/018).
type Worker struct {
	Interval          time.Duration // discovery/index pass tick (WORKER_INTERVAL)
	MetaInterval      time.Duration // metadata pass tick (WORKER_META_INTERVAL, default Interval)
	MirrorInterval    time.Duration // mirror pass tick (WORKER_MIRROR_INTERVAL, default Interval)
	Concurrency       int           // ponytail: starting default, to be refined based on facts
	MirrorsDir        string
	HistoryMaxCommits int   // ponytail: starting default, to be refined based on facts
	MaxFileBytes      int64 // ponytail: starting default, to be refined based on facts
	CIWindow          int   // ponytail: starting default, to be refined based on facts
}

// MCP is the MCP server transport (FR-013).
type MCP struct {
	Transport string // "stdio" | "sse"
	Addr      string // address for sse
}

// ConfigError is a configuration error listing the variable names that must be
// set (or the conflict that must be resolved).
type ConfigError struct {
	Problem string   // short problem description
	Missing []string // environment variable names
}

func (e *ConfigError) Error() string {
	if len(e.Missing) == 0 {
		return "configuration: " + e.Problem
	}
	return "configuration: " + e.Problem + ": " + strings.Join(e.Missing, ", ")
}

// Load reads environment variables and returns a validated configuration.
func Load() (*Config, error) {
	cfg := &Config{}

	// --- VCS provider ---
	gitlabGroups := splitList(os.Getenv("GITLAB_GROUP_ID"))
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	githubOrgs := splitList(os.Getenv("GITHUB_ORG"))
	githubToken := os.Getenv("GITHUB_TOKEN")

	hasGitLab := len(gitlabGroups) > 0 || gitlabToken != ""
	hasGitHub := len(githubOrgs) > 0 || githubToken != ""

	switch {
	case hasGitLab && hasGitHub:
		return nil, &ConfigError{Problem: "both providers are set (GitLab and GitHub), keep exactly one"}
	case hasGitLab:
		if len(gitlabGroups) == 0 {
			return nil, &ConfigError{Problem: "GitLab group is not set", Missing: []string{"GITLAB_GROUP_ID"}}
		}
		if gitlabToken == "" {
			return nil, &ConfigError{Problem: "GitLab token is not set", Missing: []string{"GITLAB_TOKEN"}}
		}
		cfg.Provider.Type = "gitlab"
		cfg.Provider.GitLabGroups = gitlabGroups
		cfg.Provider.GitLabToken = gitlabToken
		cfg.Provider.GitLabBaseURL = envDefault("GITLAB_BASE_URL", "https://gitlab.com")
	case hasGitHub:
		if len(githubOrgs) == 0 {
			return nil, &ConfigError{Problem: "GitHub organization is not set", Missing: []string{"GITHUB_ORG"}}
		}
		if githubToken == "" {
			return nil, &ConfigError{Problem: "GitHub token is not set", Missing: []string{"GITHUB_TOKEN"}}
		}
		cfg.Provider.Type = "github"
		cfg.Provider.GitHubOrgs = githubOrgs
		cfg.Provider.GitHubToken = githubToken
		cfg.Provider.GitHubBaseURL = envDefault("GITHUB_BASE_URL", "https://api.github.com")
	default:
		return nil, &ConfigError{
			Problem: "VCS provider is not set, configure GitLab or GitHub",
			Missing: []string{"GITLAB_GROUP_ID", "GITLAB_TOKEN", "GITHUB_ORG", "GITHUB_TOKEN"},
		}
	}

	// --- Embeddings ---
	embProvider := os.Getenv("EMBEDDINGS_PROVIDER")
	switch embProvider {
	case "ollama", "lm-studio":
	case "":
		return nil, &ConfigError{
			Problem: "embeddings provider is not set (ollama|lm-studio)",
			Missing: []string{"EMBEDDINGS_PROVIDER"},
		}
	default:
		return nil, &ConfigError{Problem: "EMBEDDINGS_PROVIDER: invalid value (ollama|lm-studio)"}
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
	// the dedicated lanes default to the common tick (T080).
	metaInterval, err := envDuration("WORKER_META_INTERVAL", interval)
	if err != nil {
		return nil, err
	}
	mirrorInterval, err := envDuration("WORKER_MIRROR_INTERVAL", interval)
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
		MetaInterval:      metaInterval,
		MirrorInterval:    mirrorInterval,
		Concurrency:       concurrency,
		MirrorsDir:        envDefault("MIRRORS_DIR", "./mirrors"),
		HistoryMaxCommits: historyMax,
		MaxFileBytes:      int64(maxFileBytes),
		CIWindow:          ciWindow,
	}

	// --- NATS (T108/T117): empty URL — the refresh mode is off; the instance
	// id is validated only when the mode is on (behavior stays as before).
	natsURL := os.Getenv("NATS_URL")
	instanceID := NATSDefaultInstance
	if natsURL != "" {
		if v := os.Getenv("NATS_INSTANCE_ID"); v != "" {
			if !instanceIDRe.MatchString(v) {
				return nil, &ConfigError{Problem: "NATS_INSTANCE_ID: invalid value (want " + instanceIDRe.String() + ")"}
			}
			instanceID = v
		}
	}
	cfg.NATS = NATS{URL: natsURL, InstanceID: instanceID}

	// --- MCP ---
	transport := envDefault("MCP_TRANSPORT", "stdio")
	if transport != "stdio" && transport != "sse" {
		return nil, &ConfigError{Problem: "MCP_TRANSPORT: invalid value (stdio|sse)"}
	}
	cfg.MCP = MCP{
		Transport: transport,
		Addr:      envDefault("MCP_ADDR", ":8080"),
	}

	return cfg, nil
}

// envDefault returns the environment variable value or the default.
func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envInt reads a positive integer; the error names the variable, not its value.
func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, &ConfigError{Problem: name + ": positive integer expected"}
	}
	return n, nil
}

// envDuration reads time.ParseDuration; the error names the variable, not its value.
func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, &ConfigError{Problem: name + ": positive duration expected (e.g. 60s)"}
	}
	return d, nil
}

// splitList splits a comma-separated list, trimming spaces and skipping empty items.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
