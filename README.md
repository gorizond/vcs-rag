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
| `GITLAB_GROUP_ID` / `GITLAB_TOKEN` | группа GitLab и токен доступа |
| `GITHUB_ORG` / `GITHUB_TOKEN` | организация GitHub и токен доступа |
| `EMBEDDINGS_PROVIDER` | `ollama` или `lm-studio` |
| `QDRANT_URL` | адрес Qdrant |
| `MCP_TRANSPORT` | `stdio` или `sse` |

## Запуск

```bash
docker build -t vcs-rag:dev .
docker run --rm vcs-rag:dev            # → vcs-rag dev
docker run --rm vcs-rag:dev -mode=worker
```

Полные контракты (CLI, MCP-инструменты, схема Qdrant) — в spec-kit проекта
(репозиторий 0_WORK), см. `contracts/cli.md` в спецификации.
