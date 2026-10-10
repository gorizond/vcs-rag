# vcs-rag

Self-hosted, VCS-agnostic RAG-сервис по исходному коду и метаданным репозиториев.
Альтернатива GitLab Orbit: зеркалирует репозитории (GitLab/GitHub), чанкует код,
строит embeddings и отдаёт поиск через MCP-сервер.

Один бинарник — два режима:

| Режим | Назначение |
|-------|------------|
| `-mode=worker` | зеркалирование репозиториев, чанкинг, эмбеддинги, запись в Qdrant |
| `-mode=mcp` | MCP-сервер поиска по коду/PR/issues/CI-фейлам |

## Ключевые переменные окружения

| Переменная | Описание |
|------------|----------|
| `GITLAB_GROUP_ID` / `GITLAB_TOKEN` | группа GitLab (список через запятую) и токен доступа |
| `GITHUB_ORG` / `GITHUB_TOKEN` | организация GitHub (список) и токен доступа |
| `GITLAB_BASE_URL` / `GITHUB_BASE_URL` | self-hosted/GHES endpoint (опционально) |
| `EMBEDDINGS_PROVIDER` | `ollama` или `lm-studio` |
| `EMBEDDINGS_BASE_URL` / `EMBEDDINGS_MODEL` | endpoint и модель эмбеддингов |
| `EMBEDDINGS_DIM` | размерность векторов (default 1024) |
| `QDRANT_URL` / `QDRANT_COLLECTION` | gRPC-адрес Qdrant и имя коллекции |
| `MCP_TRANSPORT` / `MCP_ADDR` | `stdio` (default) или `sse`; адрес для sse |
| `WORKER_INTERVAL` / `WORKER_CONCURRENCY` | тик проходов и параллелизм |
| `MIRRORS_DIR` | каталог bare-зеркал (по умолчанию `./mirrors`) |

Смена GitLab ↔ GitHub — только env: ядро не меняется (оба провайдера за одним
интерфейсом, проходят один контрактный набор).

## Запуск

```bash
docker build -t vcs-rag:dev .
docker run --rm vcs-rag:dev            # → vcs-rag dev
docker run --rm vcs-rag:dev -version   # → vcs-rag <version>
```

Воркер (нужны доступные Qdrant и Ollama/LM Studio; токены — только env):

```bash
# GitLab: группа или список групп (подгруппы включены, дедуп по ID)
docker run --rm --network host \
  -e GITLAB_GROUP_ID=<group-или-список> -e GITLAB_TOKEN=<read_api-токен> \
  -e EMBEDDINGS_PROVIDER=ollama -e QDRANT_URL=http://localhost:6334 \
  -v "$PWD/mirrors":/mirrors vcs-rag:dev -mode=worker

# GitHub: организация или список
docker run --rm --network host \
  -e GITHUB_ORG=<org-или-список> -e GITHUB_TOKEN=<repo-read-токен> \
  -e EMBEDDINGS_PROVIDER=ollama -e QDRANT_URL=http://localhost:6334 \
  -v "$PWD/mirrors":/mirrors vcs-rag:dev -mode=worker
```

## MCP: Cursor / Claude

stdio (в конфиг клиента, `mcpServers`):

```json
{"mcpServers": {"vcs-rag": {"command": "docker", "args": [
  "run", "-i", "--rm", "--network", "host",
  "-e", "GITLAB_GROUP_ID=1", "-e", "GITLAB_TOKEN=dummy",
  "-e", "EMBEDDINGS_PROVIDER=ollama", "-e", "QDRANT_URL=http://localhost:6334",
  "vcs-rag:dev", "-mode=mcp"]}}}
```

Для поиска VCS-доступ не нужен: провайдерные env требуются только валидацией
конфига (можно dummy). Сетевой режим: `MCP_TRANSPORT=sse MCP_ADDR=:8080` →
endpoint `http://localhost:8080/mcp`.

Инструменты: `search_code`, `search_pull_requests`, `search_issues`,
`search_ci_failures`; ошибки бэкендов возвращаются структурированно
(`{"error": {...}}`), сервер не падает.

## Релиз

Образы — `ghcr.io/gorizond/vcs-rag:<tag>` (semver-теги без префикса `v`, напр.
`0.0.1`/`0.0`/`0`, и `latest`; rc-теги — только версия + Pre-release, `latest`
не двигают); версия вшивается на сборке (`--build-arg VERSION=...` из имени
git-тега, release-workflow):

```bash
docker pull ghcr.io/gorizond/vcs-rag:0.0.1
docker run --rm ghcr.io/gorizond/vcs-rag:0.0.1 -version   # → vcs-rag v0.0.1
```

## Обслуживание

- Смена модели/размерности эмбеддингов — процедура reindex: [`docs/reindex.md`](docs/reindex.md).
- Валидация SC-001…SC-009 и перф-база — `tests/integration/final-validation.md`,
  `tests/integration/perf-baseline.md`.

Полные контракты (CLI, MCP-инструменты, схема Qdrant) — в spec-kit проекта
(репозиторий 0_WORK), см. `contracts/cli.md` в спецификации.
