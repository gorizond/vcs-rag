# Smoke US1 — ручная проверка (T023)

Токенов в среде нет: команды ниже выполняет владелец с реальными токенами.
Ожидания — из контрактов (`contracts/cli.md`, `contracts/mcp-tools.md`);
фактические выводы без токенов помечены «наблюдено».

## Сборка

```bash
docker build -t vcs-rag:dev .
```

## 1. Worker: листинг репозиториев

GitLab (группа или список групп, подгруппы включены, дедуп по ID):

```bash
docker run --rm \
  -e GITLAB_GROUP_ID=<group-id-или-список> \
  -e GITLAB_TOKEN=<read_api-токен> \
  -e EMBEDDINGS_PROVIDER=ollama \
  vcs-rag:dev -mode=worker
```

Ожидание: `level=INFO msg="репозитории получены" provider=gitlab count=N` и по строке
`msg=repo` на каждый проект (`repo=namespace/path`, `namespace=<полный путь>`);
цикл не завершается сам — выход по Ctrl-C/SIGTERM.

GitHub (организация или список):

```bash
docker run --rm \
  -e GITHUB_ORG=<org-или-список> \
  -e GITHUB_TOKEN=<repo-read-токен> \
  -e EMBEDDINGS_PROVIDER=ollama \
  vcs-rag:dev -mode=worker
```

Ожидание: то же с `provider=github`; `namespace` = имя организации.

Негативный контроль (FR-014) — недоступный API не роняет цикл:

```bash
docker run --rm \
  -e GITLAB_GROUP_ID=1 -e GITLAB_TOKEN=dummy \
  -e EMBEDDINGS_PROVIDER=ollama \
  -e GITLAB_BASE_URL=http://127.0.0.1:9 \
  vcs-rag:dev -mode=worker
```

Наблюдено: через ~30 s (ретраи SDK) `level=ERROR msg="листинг репозиториев не удался"`
`err="... connection refused"`; процесс продолжает работу (останавливается по Ctrl-C).

## 2. MCP stdio: handshake + список инструментов

Сервер отвечает в stdout, логи — в stderr:

```bash
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
  ; sleep 2; } | docker run -i --rm \
  -e GITLAB_GROUP_ID=1 -e GITLAB_TOKEN=dummy -e EMBEDDINGS_PROVIDER=ollama \
  vcs-rag:dev -mode=mcp
```

Наблюдено: два JSON-RPC-ответа. `initialize` → `serverInfo {"name":"vcs-rag","version":"dev"}`,
`protocolVersion 2025-06-18`; `tools/list` → 4 инструмента (`search_code`,
`search_pull_requests`, `search_issues`, `search_ci_failures`) со схемами из
`contracts/mcp-tools.md`. Пустой stdin (EOF) — штатное завершение, exit 0.
Флаш-нюанс (наблюдено): без `sleep` после отправки EOF может оборвать сессию
раньше записи ответов — для ручной проверки пауза обязательна.

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

Наблюдено: HTTP 200, `event: message` + JSON-RPC-ответ с `serverInfo vcs-rag`,
заголовок `Mcp-Session-Id`; хендлер отвечает и на `/`, и на `/mcp`.
