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

	GitLabGroups []string // GITLAB_GROUP_ID, comma-separated
	// GitLabProjects restricts discovery to explicit projects — a numeric ID
	// or a full path (group/sub/project); additive to GitLabGroups.
	GitLabProjects []string // GITLAB_PROJECT_ID, comma-separated
	GitLabToken    string   // GITLAB_TOKEN (secret)
	GitLabBaseURL  string   // GITLAB_BASE_URL

	GitHubOrgs []string // GITHUB_ORG, comma-separated
	// GitHubRepos restricts discovery to explicit owner/repo references;
	// additive to GitHubOrgs.
	GitHubRepos   []string // GITHUB_REPO, comma-separated
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
	// GraphCollection is the payload-only graph collection; it is part of the
	// instance identity (unique per deployment): several contours may share
	// one Qdrant, so it must differ from Collection (FR-021/SC-007).
	GraphCollection string
}

// Worker holds background loop parameters (FR-005/006/018).
type Worker struct {
	Interval       time.Duration // discovery/index pass tick (WORKER_INTERVAL)
	MetaInterval   time.Duration // metadata pass tick (WORKER_META_INTERVAL, default Interval)
	MirrorInterval time.Duration // mirror pass tick (WORKER_MIRROR_INTERVAL, default Interval)
	GCInterval     time.Duration // repo-GC pass tick (WORKER_GC_INTERVAL, default 15m)
	GCDryRun       bool          // WORKER_GC_DRY_RUN (default true — the kill-switch)
	GraphInterval  time.Duration // graph lane tick (WORKER_GRAPH_INTERVAL, default 30m)
	// GraphSchemaVer is part of the graph uuid5 namespace
	// (WORKER_GRAPH_SCHEMA_VER, default "4"). Bumped 1→2 for the edge
	// key-space feature, 2→3 for the Orbit-parity gap catalog (structure,
	// review, security families) and 3→4 for the literal-parity completion
	// (commit node, 9 Orbit relations): adding node and edge kinds changes
	// the graph schema, and fresh deterministic ids must not collide with
	// the previous namespace. A bump re-keys every graph id and therefore
	// requires a full graph reindex (re-scan) — see docs/reindex.md.
	GraphSchemaVer string
	// GraphMaxReposPerPass caps repositories processed per graph pass
	// (WORKER_GRAPH_MAX_REPOS_PER_PASS, default 5).
	GraphMaxReposPerPass int
	// DiscoveryMinInterval reuses a younger discovery snapshot without an API
	// call (T143); 0 (default) refreshes on every pass as before.
	DiscoveryMinInterval time.Duration
	Concurrency          int // ponytail: starting default, to be refined based on facts
	MirrorsDir           string
	HistoryMaxCommits    int   // ponytail: starting default, to be refined based on facts
	MaxFileBytes         int64 // ponytail: starting default, to be refined based on facts
	CIWindow             int   // ponytail: starting default, to be refined based on facts

	// Upstream request budget (FR-002/FR-003/FR-007/FR-010/FR-014).
	// UpstreamMaxUnits == 0 means unlimited (the only field where 0 is valid).
	UpstreamConcurrency int           // WORKER_UPSTREAM_CONCURRENCY (default 2)
	UpstreamMaxUnits    int           // WORKER_UPSTREAM_MAX_UNITS (default 500)
	RetryFloor          time.Duration // WORKER_UPSTREAM_RETRY_FLOOR (default 60s)
	RetryCap            time.Duration // WORKER_UPSTREAM_RETRY_CAP (default 15m)
	MaxRetries          int           // WORKER_UPSTREAM_MAX_RETRIES (default 5)
	UpstreamQueueLimit  int           // WORKER_UPSTREAM_QUEUE_LIMIT (default 1000)
	FreshThreshold      time.Duration // WORKER_FRESH_THRESHOLD (default 24h)
}

// MCP is the MCP server transport (FR-013) and query-mode caps.
type MCP struct {
	Transport string // "stdio" | "sse"
	Addr      string // address for sse
	// GraphAggMaxScanned caps payload points scanned per aggregation request
	// (MCP_GRAPH_AGG_MAX_SCANNED, default 50000).
	GraphAggMaxScanned int
	// GraphAggMaxGroups caps distinct groups returned by an aggregation
	// (MCP_GRAPH_AGG_MAX_GROUPS, default 10000).
	GraphAggMaxGroups int
	// GraphPathMaxDepth caps path traversal depth
	// (MCP_GRAPH_PATH_MAX_DEPTH, default 3, clamped to ≤3).
	GraphPathMaxDepth int
	// GraphMaxEdgesPerNode caps the edge points kept per node in a graph scan
	// (MCP_GRAPH_MAX_EDGES_PER_NODE, default 10000 — FR-025). A cut marks the
	// response truncated, never silently partial.
	GraphMaxEdgesPerNode int
	// AuthzEnabled is an explicit no-op punt (T041): per-user authorization
	// is NOT implemented — the graph has no ACL and nothing reads this flag.
	// It exists so the reserved name parses and the punt stays visible; the
	// graph is served to a trusted loopback consumer, with instance isolation
	// as the only boundary (docs/graph.md). MCP_AUTHZ_ENABLED, default false.
	AuthzEnabled bool
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
	// The discovery set = (all projects of every configured group) ∪
	// (every explicitly listed project/repo), deduplicated. If only the
	// allowlist is set (no group/org), only those are indexed.
	gitlabGroups := splitList(os.Getenv("GITLAB_GROUP_ID"))
	gitlabProjects := splitList(os.Getenv("GITLAB_PROJECT_ID"))
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	githubOrgs := splitList(os.Getenv("GITHUB_ORG"))
	githubRepos := splitList(os.Getenv("GITHUB_REPO"))
	githubToken := os.Getenv("GITHUB_TOKEN")

	hasGitLab := len(gitlabGroups) > 0 || len(gitlabProjects) > 0 || gitlabToken != ""
	hasGitHub := len(githubOrgs) > 0 || len(githubRepos) > 0 || githubToken != ""

	switch {
	case hasGitLab && hasGitHub:
		return nil, &ConfigError{Problem: "both providers are set (GitLab and GitHub), keep exactly one"}
	case hasGitLab:
		if len(gitlabGroups) == 0 && len(gitlabProjects) == 0 {
			return nil, &ConfigError{
				Problem: "GitLab group or project is not set",
				Missing: []string{"GITLAB_GROUP_ID", "GITLAB_PROJECT_ID"},
			}
		}
		if gitlabToken == "" {
			return nil, &ConfigError{Problem: "GitLab token is not set", Missing: []string{"GITLAB_TOKEN"}}
		}
		cfg.Provider.Type = "gitlab"
		cfg.Provider.GitLabGroups = gitlabGroups
		cfg.Provider.GitLabProjects = gitlabProjects
		cfg.Provider.GitLabToken = gitlabToken
		cfg.Provider.GitLabBaseURL = envDefault("GITLAB_BASE_URL", "https://gitlab.com")
	case hasGitHub:
		if len(githubOrgs) == 0 && len(githubRepos) == 0 {
			return nil, &ConfigError{
				Problem: "GitHub organization or repository is not set",
				Missing: []string{"GITHUB_ORG", "GITHUB_REPO"},
			}
		}
		// T143: the token is optional for public organizations — go-github
		// sends anonymous requests without it (60 req/h per IP).
		cfg.Provider.Type = "github"
		cfg.Provider.GitHubOrgs = githubOrgs
		cfg.Provider.GitHubRepos = githubRepos
		cfg.Provider.GitHubToken = githubToken
		cfg.Provider.GitHubBaseURL = envDefault("GITHUB_BASE_URL", "https://api.github.com")
	default:
		return nil, &ConfigError{
			Problem: "VCS provider is not set, configure GitLab or GitHub",
			Missing: []string{"GITLAB_GROUP_ID", "GITLAB_PROJECT_ID", "GITLAB_TOKEN", "GITHUB_ORG", "GITHUB_REPO", "GITHUB_TOKEN"},
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
		// FR-021/SC-007: the graph collection is instance-scoped (unique per
		// deployment) because several contours may share one Qdrant.
		GraphCollection: envDefault("QDRANT_GRAPH_COLLECTION", "vcs_rag_graph"),
	}
	if cfg.Qdrant.GraphCollection == cfg.Qdrant.Collection {
		return nil, &ConfigError{Problem: "QDRANT_GRAPH_COLLECTION must differ from QDRANT_COLLECTION (instance isolation)"}
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
	// T128: the embeddings limiter ceiling — 2 concurrent calls are gentle to
	// a single local model (the AIMD limiter scales within this ceiling).
	concurrency, err := envInt("WORKER_CONCURRENCY", 2)
	if err != nil {
		return nil, err
	}
	// T140: the repo-GC lane runs rarely; the dry-run default is the permanent
	// kill-switch (nothing is deleted until it is disabled explicitly).
	gcInterval, err := envDuration("WORKER_GC_INTERVAL", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	gcDryRun, err := envBool("WORKER_GC_DRY_RUN", true)
	if err != nil {
		return nil, err
	}
	// T143: an optional discovery cache window; 0 (the default) keeps the
	// previous behavior — a fresh listing on every pass.
	discoveryMin, err := envDuration("WORKER_DISCOVERY_MIN_INTERVAL", 0)
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
	// T001/FR-021: the graph lane cadence and per-pass cap.
	graphInterval, err := envDuration("WORKER_GRAPH_INTERVAL", 30*time.Minute)
	if err != nil {
		return nil, err
	}
	graphMaxRepos, err := envInt("WORKER_GRAPH_MAX_REPOS_PER_PASS", 5)
	if err != nil {
		return nil, err
	}
	// FR-002/FR-003/FR-007/FR-010/FR-014: the upstream budget. Only
	// WORKER_UPSTREAM_MAX_UNITS accepts 0 (unlimited); every other value must
	// be positive — 0 is rejected by envInt/envDuration.
	upstreamConcurrency, err := envInt("WORKER_UPSTREAM_CONCURRENCY", 2)
	if err != nil {
		return nil, err
	}
	upstreamMaxUnits, err := envNonNegativeInt("WORKER_UPSTREAM_MAX_UNITS", 500)
	if err != nil {
		return nil, err
	}
	retryFloor, err := envDuration("WORKER_UPSTREAM_RETRY_FLOOR", 60*time.Second)
	if err != nil {
		return nil, err
	}
	retryCap, err := envDuration("WORKER_UPSTREAM_RETRY_CAP", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	if retryFloor > retryCap {
		return nil, &ConfigError{Problem: "WORKER_UPSTREAM_RETRY_FLOOR must not exceed WORKER_UPSTREAM_RETRY_CAP"}
	}
	maxRetries, err := envInt("WORKER_UPSTREAM_MAX_RETRIES", 5)
	if err != nil {
		return nil, err
	}
	upstreamQueueLimit, err := envInt("WORKER_UPSTREAM_QUEUE_LIMIT", 1000)
	if err != nil {
		return nil, err
	}
	freshThreshold, err := envDuration("WORKER_FRESH_THRESHOLD", 24*time.Hour)
	if err != nil {
		return nil, err
	}
	cfg.Worker = Worker{
		Interval:             interval,
		MetaInterval:         metaInterval,
		MirrorInterval:       mirrorInterval,
		GCInterval:           gcInterval,
		GCDryRun:             gcDryRun,
		GraphInterval:        graphInterval,
		GraphSchemaVer:       envDefault("WORKER_GRAPH_SCHEMA_VER", "4"),
		GraphMaxReposPerPass: graphMaxRepos,
		DiscoveryMinInterval: discoveryMin,
		Concurrency:          concurrency,
		MirrorsDir:           envDefault("MIRRORS_DIR", "./mirrors"),
		HistoryMaxCommits:    historyMax,
		MaxFileBytes:         int64(maxFileBytes),
		CIWindow:             ciWindow,

		UpstreamConcurrency: upstreamConcurrency,
		UpstreamMaxUnits:    upstreamMaxUnits,
		RetryFloor:          retryFloor,
		RetryCap:            retryCap,
		MaxRetries:          maxRetries,
		UpstreamQueueLimit:  upstreamQueueLimit,
		FreshThreshold:      freshThreshold,
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
	// T001: query-mode caps — agg scan/group ceilings and the path depth bound.
	aggMaxScanned, err := envInt("MCP_GRAPH_AGG_MAX_SCANNED", 50000)
	if err != nil {
		return nil, err
	}
	aggMaxGroups, err := envInt("MCP_GRAPH_AGG_MAX_GROUPS", 10000)
	if err != nil {
		return nil, err
	}
	pathMaxDepth, err := envInt("MCP_GRAPH_PATH_MAX_DEPTH", 3)
	if err != nil {
		return nil, err
	}
	// The contract caps shortest-path depth at 3; larger values are clamped.
	if pathMaxDepth > 3 {
		pathMaxDepth = 3
	}
	// T010/FR-025: the per-node edge ceiling of the new families.
	maxEdgesPerNode, err := envInt("MCP_GRAPH_MAX_EDGES_PER_NODE", 10000)
	if err != nil {
		return nil, err
	}
	// T041: explicit no-op punt — parsed so the reserved name is visible in
	// config errors, but no filtering is implemented behind it.
	authzEnabled, err := envBool("MCP_AUTHZ_ENABLED", false)
	if err != nil {
		return nil, err
	}
	cfg.MCP = MCP{
		Transport:            transport,
		Addr:                 envDefault("MCP_ADDR", ":8080"),
		GraphAggMaxScanned:   aggMaxScanned,
		GraphAggMaxGroups:    aggMaxGroups,
		GraphPathMaxDepth:    pathMaxDepth,
		GraphMaxEdgesPerNode: maxEdgesPerNode,
		AuthzEnabled:         authzEnabled,
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

// envNonNegativeInt reads a non-negative integer: 0 is meaningful for
// WORKER_UPSTREAM_MAX_UNITS (unlimited); a negative value is an error.
func envNonNegativeInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, &ConfigError{Problem: name + ": non-negative integer expected"}
	}
	return n, nil
}

// envBool reads a boolean (true/false/1/0); the error names the variable.
func envBool(name string, def bool) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, &ConfigError{Problem: name + ": boolean expected (true|false)"}
	}
	return b, nil
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
