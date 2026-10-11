# Environment reference

Every variable read by `internal/config/config.go` (the service refuses to
start on an invalid value; error messages name the variable, never print the
value). Optional variables may stay unset. Secrets are marked; keep them in a
local `.env` or the orchestrator's secret store — never in the repository.

The quickstart uses [`.env.example`](../.env.example); the compose stacks read
the same names from `.env`. See also: [refresh](refresh.md) ·
[GC](gc.md) · [reindex](reindex.md).

## VCS provider (exactly one flavor)

| Variable | Default | Purpose |
|----------|---------|---------|
| `GITLAB_GROUP_ID` | — | GitLab group (or comma-separated list), subgroups included. Required for the GitLab flavor |
| `GITLAB_TOKEN` | — | GitLab token (`read_api`). **Secret**; required for GitLab |
| `GITLAB_BASE_URL` | `https://gitlab.com` | Self-hosted GitLab endpoint |
| `GITHUB_ORG` | — | GitHub organization (or comma-separated list). Required for the GitHub flavor |
| `GITHUB_TOKEN` | *(empty)* | Optional for public organizations (anonymous limit: 60 requests/hour per IP); recommended for the metadata lane (issues/MRs). **Secret** |
| `GITHUB_BASE_URL` | `https://api.github.com` | GitHub Enterprise (GHES) endpoint |

Setting both flavors at once is a configuration error — keep exactly one.

## Embeddings

| Variable | Default | Purpose |
|----------|---------|---------|
| `EMBEDDINGS_PROVIDER` | — | `ollama` or `lm-studio`. Required |
| `EMBEDDINGS_BASE_URL` | `http://localhost:11434` (ollama) / `http://localhost:1234` (lm-studio) | Embeddings endpoint |
| `EMBEDDINGS_MODEL` | `bge-m3` | Model identifier (Ollama tag or LM Studio name) |
| `EMBEDDINGS_DIM` | `1024` | Vector dimension; a different value requires the [reindex](reindex.md) procedure |
| `EMBEDDINGS_BATCH` | `32` | Texts per embedding call |

## Qdrant

| Variable | Default | Purpose |
|----------|---------|---------|
| `QDRANT_URL` | `http://localhost:6334` | Qdrant gRPC endpoint |
| `QDRANT_API_KEY` | *(empty)* | API key for Qdrant Cloud / secured instances. **Secret** |
| `QDRANT_COLLECTION` | `vcs_rag` | Collection name; one collection per instance (see multi-instance notes in the README) |

## Worker

| Variable | Default | Purpose |
|----------|---------|---------|
| `WORKER_INTERVAL` | `60s` | Discovery/index pass tick — a ceiling: the lanes pace themselves (see Adaptive tuning) |
| `WORKER_META_INTERVAL` | `WORKER_INTERVAL` | Metadata pass (issues/MRs/CI) ceiling |
| `WORKER_MIRROR_INTERVAL` | `WORKER_INTERVAL` | Mirror (git fetch) pass ceiling |
| `WORKER_GC_INTERVAL` | `15m` | Repo/entity GC pass ceiling |
| `WORKER_GC_DRY_RUN` | `true` | GC kill-switch: `true` — observe-only (`gc would-delete` journal), `false` — real deletions |
| `WORKER_DISCOVERY_MIN_INTERVAL` | `0` | Reuse a younger discovery listing without an API call; `0` — list on every pass |
| `WORKER_CONCURRENCY` | `2` | Ceiling for the AIMD embeddings limiter (the limiter itself scales 1…this) |
| `MIRRORS_DIR` | `./mirrors` | Bare mirrors + `*.state.json` sidecars + `.gc_registry.json` |
| `HISTORY_MAX_COMMITS` | `2000` | Commits walked back per branch on the first indexing pass |
| `MAX_FILE_BYTES` | `1048576` | Skip larger files while chunking |
| `CI_WINDOW` | `50` | Pipelines inspected per repository for failed jobs |

## NATS (refresh / auto-freshness / status)

| Variable | Default | Purpose |
|----------|---------|---------|
| `NATS_URL` | *(empty)* | Local JetStream. Empty — the refresh mode is off (no `refresh_repo`, no auto-refresh, no `index_status` data) |
| `NATS_INSTANCE_ID` | `default` | Scopes the consumer, subject and the `refresh_status_<id>` / `worker_status_<id>` KV buckets; several deployments can share one NATS server |

## MCP server

| Variable | Default | Purpose |
|----------|---------|---------|
| `MCP_TRANSPORT` | `stdio` | `stdio` or `sse` (streamable HTTP) |
| `MCP_ADDR` | `:8080` | Listen address for `sse` (the compose stacks publish it on `127.0.0.1`) |
