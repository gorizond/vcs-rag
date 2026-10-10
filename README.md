# vcs-rag

Self-hosted, VCS-agnostic RAG service over source code and repository metadata.
An alternative to GitLab Orbit: mirrors repositories (GitLab/GitHub), chunks
code, builds embeddings, and serves search via an MCP server.

One binary — two modes:

| Mode | Purpose |
|------|---------|
| `-mode=worker` | repository mirroring, chunking, embeddings, writes to Qdrant |
| `-mode=mcp` | MCP server for search over code/PRs/issues/CI failures |

## Key environment variables

| Variable | Description |
|----------|-------------|
| `GITLAB_GROUP_ID` / `GITLAB_TOKEN` | GitLab group (comma-separated list) and access token |
| `GITHUB_ORG` / `GITHUB_TOKEN` | GitHub organization (list) and access token |
| `GITLAB_BASE_URL` / `GITHUB_BASE_URL` | self-hosted/GHES endpoint (optional) |
| `EMBEDDINGS_PROVIDER` | `ollama` or `lm-studio` |
| `EMBEDDINGS_BASE_URL` / `EMBEDDINGS_MODEL` | embeddings endpoint and model |
| `EMBEDDINGS_DIM` | vector dimension (default 1024) |
| `QDRANT_URL` / `QDRANT_COLLECTION` | Qdrant gRPC address and collection name |
| `MCP_TRANSPORT` / `MCP_ADDR` | `stdio` (default) or `sse`; address for sse |
| `WORKER_INTERVAL` / `WORKER_CONCURRENCY` | discovery/index pass tick and concurrency |
| `WORKER_META_INTERVAL` / `WORKER_MIRROR_INTERVAL` | metadata / mirror pass tick (default — `WORKER_INTERVAL`) |
| `MIRRORS_DIR` | bare mirror directory (default `./mirrors`) |
| `NATS_URL` | local JetStream for on-demand refresh (`refresh_repo`); empty — the feature is off |
| `NATS_INSTANCE_ID` | instance id scoping consumers/subjects/KV on a shared NATS (default `default`) |

Switching GitLab ↔ GitHub is env-only: the core does not change (both providers
share one interface and pass one contract suite).

Metadata (issues, MR/PR, CI failures) is indexed incrementally: a point is
re-embedded only when its source `updated_at` changed; a CI job is indexed once
(a rerun gets new `(pipeline, job)` ids); the first pass over a repository is
full. Deleting `*.state.json` resets this cache ([`docs/reindex.md`](docs/reindex.md)).

`git fetch` runs on the mirror lane only (`WORKER_MIRROR_INTERVAL`); the index
lane reads the local mirror — network access only for the initial clone.

With `NATS_URL` set, the MCP adds the `refresh_repo` tool: the LLM can pull one
repository's fresh commits and metadata before searching (the worker executes
the task from the local JetStream). `NATS_INSTANCE_ID` scopes the consumer,
subject and KV bucket, so several deployments can share one NATS server — see
[`docs/refresh.md`](docs/refresh.md) for the full flow, semantics and limits.

Embeddings — the target model is **`bge-m3`** (dim 1024, COSINE): in Ollama
(`ollama pull bge-m3`) or in LM Studio (GGUF build, e.g. `lm-kit/bge-m3-gguf`;
the local server is enabled in the UI). The providers are interchangeable —
switching is env-only; the Qdrant collection name is `QDRANT_COLLECTION`
(default `vcs_rag`). A different model/dimension only via the reindex procedure
([`docs/reindex.md`](docs/reindex.md)). For LM Studio:
`EMBEDDINGS_PROVIDER=lm-studio`,
`EMBEDDINGS_BASE_URL=http://localhost:1234`,
`EMBEDDINGS_MODEL=<identifier from LM Studio>`, `EMBEDDINGS_DIM=1024`.

## Running

```bash
docker build -t vcs-rag:dev .
docker run --rm vcs-rag:dev            # → vcs-rag dev
docker run --rm vcs-rag:dev -version   # → vcs-rag <version>
```

Worker (requires reachable Qdrant and Ollama/LM Studio; tokens via env only):

```bash
# GitLab: group or list of groups (subgroups included, dedup by ID)
docker run --rm --network host \
  -e GITLAB_GROUP_ID=<group-or-list> -e GITLAB_TOKEN=<read_api-token> \
  -e EMBEDDINGS_PROVIDER=ollama -e QDRANT_URL=http://localhost:6334 \
  -v "$PWD/mirrors":/mirrors vcs-rag:dev -mode=worker

# GitHub: organization or list
docker run --rm --network host \
  -e GITHUB_ORG=<org-or-list> -e GITHUB_TOKEN=<repo-read-token> \
  -e EMBEDDINGS_PROVIDER=ollama -e QDRANT_URL=http://localhost:6334 \
  -v "$PWD/mirrors":/mirrors vcs-rag:dev -mode=worker
```

Private repositories: the token is passed to git processes through the
environment (`http.extraHeader`, Basic: GitLab `oauth2:<token>`, GitHub
`x-access-token:<token>`) — it is not stored in the clone URL or the mirror's
`.git/config`; local git operations run without the token.

## Deploy example

`docker-compose.yml` (repository root) brings up the whole stack — worker, MCP
search, Qdrant and a local NATS JetStream:

```bash
GITLAB_GROUP_ID=<your-group-id> GITLAB_TOKEN=<your-read-api-token> docker compose up -d
# or put the same variables into a local .env (never commit real values)
```

- The embeddings endpoint points at your Ollama/LM Studio
  (`EMBEDDINGS_BASE_URL`, default `http://host.docker.internal:11434`; on Linux
  the compose file adds `host.docker.internal:host-gateway`).
- MCP is exposed on `http://127.0.0.1:8080/mcp` (`MCP_TRANSPORT=sse`).
- `NATS_URL` enables `refresh_repo`; `NATS_INSTANCE_ID` allows several
  deployments on one NATS server.
- All values in the compose file are `${ENV}` placeholders or example values —
  no real tokens, groups or private endpoints belong in the repository.

## MCP: Cursor / Claude

stdio (into the client config, `mcpServers`):

```json
{"mcpServers": {"vcs-rag": {"command": "docker", "args": [
  "run", "-i", "--rm", "--network", "host",
  "-e", "GITLAB_GROUP_ID=1", "-e", "GITLAB_TOKEN=dummy",
  "-e", "EMBEDDINGS_PROVIDER=ollama", "-e", "QDRANT_URL=http://localhost:6334",
  "vcs-rag:dev", "-mode=mcp"]}}}
```

Search needs no VCS access: provider envs are required only for config
validation (dummy is fine). Network mode: `MCP_TRANSPORT=sse MCP_ADDR=:8080` →
endpoint `http://localhost:8080/mcp`.

Tools: `search_code`, `search_pull_requests`, `search_issues`,
`search_ci_failures`; backend errors are returned in a structured form
(`{"error": {...}}`), the server does not crash.

## Release

Images — `ghcr.io/gorizond/vcs-rag:<tag>` (semver tags without the `v` prefix,
e.g. `0.0.1`/`0.0`/`0`, and `latest`; rc tags — version only + Pre-release, they
do not move `latest`); the version is baked in at build time (`--build-arg
VERSION=...` from the git tag name, release workflow):

```bash
docker pull ghcr.io/gorizond/vcs-rag:0.0.1
docker run --rm ghcr.io/gorizond/vcs-rag:0.0.1 -version   # → vcs-rag v0.0.1
```

## Maintenance

- Changing the embeddings model/dimension — reindex procedure: [`docs/reindex.md`](docs/reindex.md).
- Validation SC-001…SC-009 and the performance baseline — `tests/integration/final-validation.md`,
  `tests/integration/perf-baseline.md`.

Full contracts (CLI, MCP tools, Qdrant schema) live in the project spec-kit
(0_WORK repository), see `contracts/cli.md` in the specification.
