# Smoke US3 — MCP-поиск (T049)

Дата прогона: 2026-10-10. Изолированный контур Qdrant **:16334** / Ollama **:11435**
(порты 6334/11434 заняты чужими сервисами; подъём — в `smoke_us2.md`).

## 1. Конфиг MCP-клиента (stdio)

Собрать образ локально: `docker build -t vcs-rag:dev .`

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

Сетевой режим (streamable HTTP):

```bash
docker run -d --name vcs-rag-mcp -p 8080:8080 --network host \
  -e MCP_TRANSPORT=sse -e MCP_ADDR=:8080 \
  -e GITLAB_GROUP_ID=1 -e GITLAB_TOKEN=dummy \
  -e EMBEDDINGS_PROVIDER=ollama -e EMBEDDINGS_BASE_URL=http://localhost:11435 \
  -e QDRANT_URL=http://localhost:16334 -e QDRANT_COLLECTION=vcs_rag_e2e3 \
  vcs-rag:dev -mode=mcp
# endpoint: http://localhost:8080/mcp
```

## 2. Примеры запросов ко всем инструментам

- `search_code`: `{"query": "где main?", "repo": "fixture", "top_k": 5}` →
  массив `{repo, group_org, branch, file_path, commit_sha, score, snippet}`;
- `search_pull_requests`: `{"query": "фикс парсера", "state": "opened"}` →
  `{vcs_type, repo, pr_id, source_branch, target_branch, state, score, snippet}`;
- `search_issues`: `{"query": "падение при индексации", "repo": "fixture"}` →
  `{vcs_type, repo, issue_id, state, score, snippet}`;
- `search_ci_failures`: `{"query": "timeout в тестах", "branch": "main"}` →
  `{vcs_type, repo, branch, commit_sha, pipeline_id, job_name, score, snippet}`.

`top_k`: по умолчанию 5, максимум 20; `snippet` ≤ 400 символов.
Ошибки: `{"error": {"code": "...", "message": "..."}}` — сервер не падает.

## 3. Наблюдено (e2e `TestE2E_US3`, PASS 17.09 s)

```bash
QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
go test -tags e2e ./tests/integration -run TestE2E_US3 -v -count=1
```

- проиндексировано точек = **3** (коллекция `vcs_rag_e2e3`);
- `tools/list` → все 4 инструмента;
- `tools/call search_code {"query":"main"}` → 3 чанка: `feature.txt`
  (score 0.649), `main.go` ветки `main` (0.586), `main.go` ветки `feature`
  (0.586) — присутствуют `fixture`, `main.go`, полный payload;
- негатив: `QDRANT_URL=http://127.0.0.1:19999` → структурированный ответ
  `{"code":"qdrant_unavailable","message":"qdrant: поиск: ... connection refused"}`,
  процесс продолжает отвечать (следующий `tools/list` — OK);
- CLI: пустой stdin при недоступном Qdrant → штатное завершение (exit 0, без паники).
- REST-контроль: `count({exact:true})` = 3.

## 4. Что осталось на машине

- Коллекция `vcs_rag_e2e3` (3 точки) в изолированном Qdrant — оставлена для ручных запросов.
- Контейнеры `vcs-rag-qdrant` / `vcs-rag-ollama` остановлены и удалены после прогона;
  volume `vcs-rag-ollama` (bge-m3) оставлен.
