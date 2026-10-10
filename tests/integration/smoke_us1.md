# Smoke US1 — manual check (T023)

There are no tokens in the environment: the commands below are run by the owner
with real tokens. Expectations come from the contracts (`contracts/cli.md`,
`contracts/mcp-tools.md`); actual outputs without tokens are marked "observed".

> ⚠️ Since phase US2 `-mode=worker` requires a reachable Qdrant: on startup the
> collection and its dimension are checked (fail-fast, FR-009). Add
> `-e QDRANT_URL=http://<host>:6334` to the commands below and start Qdrant (see `smoke_us2.md`).

## Build

```bash
docker build -t vcs-rag:dev .
```

## 1. Worker: repository listing

GitLab (group or list of groups, subgroups included, dedup by ID):

```bash
docker run --rm \
  -e GITLAB_GROUP_ID=<group-id-or-list> \
  -e GITLAB_TOKEN=<read_api-token> \
  -e EMBEDDINGS_PROVIDER=ollama \
  vcs-rag:dev -mode=worker
```

Expected: `level=INFO msg="repositories fetched" provider=gitlab count=N` and one
`msg=repo` line per project (`repo=namespace/path`, `namespace=<full path>`);
the loop does not terminate on its own — exit via Ctrl-C/SIGTERM.

GitHub (organization or list):

```bash
docker run --rm \
  -e GITHUB_ORG=<org-or-list> \
  -e GITHUB_TOKEN=<repo-read-token> \
  -e EMBEDDINGS_PROVIDER=ollama \
  vcs-rag:dev -mode=worker
```

Expected: the same with `provider=github`; `namespace` = the organization name.

Negative control (FR-014) — an unavailable API does not crash the loop:

```bash
docker run --rm \
  -e GITLAB_GROUP_ID=1 -e GITLAB_TOKEN=dummy \
  -e EMBEDDINGS_PROVIDER=ollama \
  -e GITLAB_BASE_URL=http://127.0.0.1:9 \
  vcs-rag:dev -mode=worker
```

Observed: after ~30 s (SDK retries) `level=ERROR msg="repository listing failed"`
`err="... connection refused"`; the process keeps running (stopped by Ctrl-C).

## 2. MCP stdio: handshake + tool list

The server responds on stdout, logs go to stderr:

```bash
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
  ; sleep 2; } | docker run -i --rm \
  -e GITLAB_GROUP_ID=1 -e GITLAB_TOKEN=dummy -e EMBEDDINGS_PROVIDER=ollama \
  vcs-rag:dev -mode=mcp
```

Observed: two JSON-RPC responses. `initialize` → `serverInfo {"name":"vcs-rag","version":"dev"}`,
`protocolVersion 2025-06-18`; `tools/list` → 4 tools (`search_code`,
`search_pull_requests`, `search_issues`, `search_ci_failures`) with schemas from
`contracts/mcp-tools.md`. Empty stdin (EOF) — clean shutdown, exit 0.
Flush nuance (observed): without `sleep` after sending EOF the session may be cut
off before the responses are written — the pause is mandatory for a manual check.

## 3. SSE (streamable HTTP)

```bash
docker run --rm -p 8080:8080 \
  -e MCP_TRANSPORT=sse -e MCP_ADDR=:8080 \
  -e GITLAB_GROUP_ID=1 -e GITLAB_TOKEN=dummy -e EMBEDDINGS_PROVIDER=ollama \
  vcs-rag:dev -mode=mcp &

curl -s -X POST http://localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
```

Observed: HTTP 200, `event: message` + JSON-RPC response with `serverInfo vcs-rag`,
header `Mcp-Session-Id`; the handler responds on both `/` and `/mcp`.
