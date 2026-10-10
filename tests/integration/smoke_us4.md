# Smoke US4 — метаданные: issues/MR-PR/CI-фейлы (T055)

Дата прогона: 2026-10-10. Изолированный контур Qdrant **:16334** / Ollama **:11435**
(порты 6334/11434 заняты чужими сервисами; подъём — `smoke_us2.md`).

## 0. Контур

```bash
docker run -d --name vcs-rag-qdrant -p 127.0.0.1:16333:6333 -p 127.0.0.1:16334:6334 qdrant/qdrant:v1.19.2
docker run -d --name vcs-rag-ollama -p 127.0.0.1:11435:11434 -v vcs-rag-ollama:/root/.ollama ollama/ollama
```

## 1. E2E-тест

```bash
docker run --rm --network host -v "$PWD":/src -v vcs-rag-gomod:/go/pkg/mod -w /src \
  -e QDRANT_URL=http://localhost:16334 -e EMBEDDINGS_BASE_URL=http://localhost:11435 \
  golang:1.27 go test -tags e2e ./tests/integration -run TestE2E_US4 -v -count=1
```

Fake-провайдер: 1 issue, 1 PR, 1 failed pipeline с 100-строчным логом.

Наблюдено (PASS 7.08 s):
- первый проход: `issue=1 pull_request=1 ci_failure=1` (всего 3);
- повторный проход: точек **3** (не выросло — идемпотентность по детерминированным ID);
- «пайплайн перезапущен в success» → `issue=1 pull_request=1 ci_failure=0`
  (prune ci-точки по паре `pipeline_id, job_id`);
- payload'ы полны (поля data-model §4–6), `text` = заголовок + тело (issue/PR);
  CI `text` = заголовок + хвост лога: строк ≤ 51 (заголовок + 50), оканчивается
  `line 100`, строки вне последних 50 отсутствуют.

## 2. Read-back (REST, после прогона)

```bash
curl -s -X POST http://localhost:16333/collections/vcs_rag_e2e4/points/count \
  -H 'Content-Type: application/json' -d '{"exact":true}'
# {"count":2}  (issue + PR; ci удалён prune'ом)

curl -s -X POST http://localhost:16333/collections/vcs_rag_e2e4/points/scroll \
  -H 'Content-Type: application/json' \
  -d '{"filter":{"must":[{"key":"type","match":{"value":"issue"}}]},"limit":1,"with_payload":true}'
# Наблюдено: {'state':'opened','group_org':'e2e/grp','vcs_type':'gitlab','issue_id':1,
#   'text':'Issue one\n\nissue body','type':'issue','repo':'fixture'}
```

## 3. Что осталось на машине

- Коллекция `vcs_rag_e2e4` (2 точки) в изолированном Qdrant — оставлена для ручных запросов.
- Контейнеры `vcs-rag-qdrant` / `vcs-rag-ollama` остановлены и удалены после прогона;
  volume `vcs-rag-ollama` (bge-m3) оставлен.
