# Smoke US3 — MCP search (T049)

Run date: 2026-10-10. Isolated environment Qdrant **:16334** / Ollama **:11435**
(ports 6334/11434 are occupied by foreign services; setup — in `smoke_us2.md`).

## 1. MCP client config (stdio)

Build the image locally: `docker build -t vcs-rag:dev .`

Cursor / Claude (`mcpServers`):

```json
{
  "mcpServers": {
    "vcs-rag": {
      "command": "docker",
      "args": ["run", "-i", "--rm", "--network", "host",
        "-e", "GITLAB_GROUP_ID=1", "-e", "GITLAB_TOKEN=dummy",
        "-e", "EMBEDDINGS_PROVIDER=ollama",
        "-e", "EMBEDDINGS_BASE_URL=http://localhost:11435",
        "-e", "QDRANT_URL=http://localhost:16334",
        "-e", "QDRANT_COLLECTION=vcs_rag_e2e3",
        "vcs-rag:dev", "-mode=mcp"]
    }
  }
}
```

Network mode (streamable HTTP):

```bash
docker run -d --name vcs-rag-mcp -p 8080:8080 --network host \
  -e MCP_TRANSPORT=sse -e MCP_ADDR=:8080 \
  -e GITLAB_GROUP_ID=1 -e GITLAB_TOKEN=dummy \
  -e EMBEDDINGS_PROVIDER=ollama -e EMBEDDINGS_BASE_URL=http://localhost:11435 \
  -e QDRANT_URL=http://localhost:16334 -e QDRANT_COLLECTION=vcs_rag_e2e3 \
  vcs-rag:dev -mode=mcp
# endpoint: http://localhost:8080/mcp
```

## 2. Example requests for all tools

- `search_code`: `{"query": "where is main?", "repo": "fixture", "top_k": 5}` →
  an array of `{repo, group_org, branch, file_path, commit_sha, score, snippet}`;
- `search_pull_requests`: `{"query": "parser fix", "state": "opened"}` →
  `{vcs_type, repo, pr_id, source_branch, target_branch, state, score, snippet}`;
- `search_issues`: `{"query": "crash during indexing", "repo": "fixture"}` →
  `{vcs_type, repo, issue_id, state, score, snippet}`;
- `search_ci_failures`: `{"query": "timeout in tests", "branch": "main"}` →
  `{vcs_type, repo, branch, commit_sha, pipeline_id, job_name, score, snippet}`.

`top_k`: default 5, maximum 20; `snippet` ≤ 400 characters.
Errors: `{"error": {"code": "...", "message": "..."}}` — the server does not crash.

## 3. Observed (e2e `TestE2E_US3`, PASS 17.09 s)

```bash
QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
go test -tags e2e ./tests/integration -run TestE2E_US3 -v -count=1
```

- indexed points = **3** (collection `vcs_rag_e2e3`);
- `tools/list` → all 4 tools;
- `tools/call search_code {"query":"main"}` → 3 chunks: `feature.txt`
  (score 0.649), `main.go` of branch `main` (0.586), `main.go` of branch `feature`
  (0.586) — `fixture`, `main.go`, and the full payload are present;
- negative: `QDRANT_URL=http://127.0.0.1:19999` → structured response
  `{"code":"qdrant_unavailable","message":"qdrant: search: ... connection refused"}`,
  the process keeps responding (the next `tools/list` — OK);
- CLI: empty stdin with an unavailable Qdrant → clean shutdown (exit 0, no panic).
- REST control: `count({exact:true})` = 3.

## 4. What was left on the machine

- Collection `vcs_rag_e2e3` (3 points) in the isolated Qdrant — kept for manual queries.
- Containers `vcs-rag-qdrant` / `vcs-rag-ollama` stopped and removed after the run;
  the `vcs-rag-ollama` volume (bge-m3) was kept.
