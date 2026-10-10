# Smoke US2 — индексация кода (T044)

Дата прогона: 2026-10-10. Реальные Qdrant v1.19.2 + Ollama (bge-m3).
⚠️ Порты 6333/6334 (чужой контейнер `qdrant`, публикует только REST) и 11434
(нативный Ollama) на машине заняты — использован **изолированный контур** на
16334/11435. Чужие сервисы не затронуты.

## 0. Стек (изолированный контур)

```bash
docker pull qdrant/qdrant:v1.19.2
docker run -d --name vcs-rag-qdrant -p 127.0.0.1:16333:6333 -p 127.0.0.1:16334:6334 qdrant/qdrant:v1.19.2
docker pull ollama/ollama
docker run -d --name vcs-rag-ollama -p 127.0.0.1:11435:11434 -v vcs-rag-ollama:/root/.ollama ollama/ollama
docker exec vcs-rag-ollama ollama pull bge-m3   # 1.2 GB
```

## 1. E2E-тест

```bash
docker run --rm --network host -v "$PWD":/src -w /src \
  -e QDRANT_URL=http://localhost:16334 -e EMBEDDINGS_BASE_URL=http://localhost:11435 \
  golang:1.27 go test -tags e2e ./tests/integration -run TestE2E_US2 -v -count=1
```

Наблюдено (10.10.2026): **PASS за 6.25 s**.
- count после первого прохода = **3**;
- count после повторного прохода = **3** (идемпотентность, FR-011);
- count после удаления `feature.txt` + коммита = **2** (prune);
- негативный контроль (SC-004): рабочих файлов кода в зеркале нет — только
  git-объекты и sidecar `fixture.git.state.json`.

## 2. Read-back данных (REST)

```bash
curl -s http://localhost:16333/collections/vcs_rag_e2e | python3 -m json.tool
# vectors: {'size': 1024, 'distance': 'Cosine'}  (наблюдено)

curl -s -X POST http://localhost:16333/collections/vcs_rag_e2e/points/count \
  -H 'Content-Type: application/json' -d '{"exact":true}'
# {"count":2}  (после prune)

curl -s -X POST http://localhost:16333/collections/vcs_rag_e2e/points/scroll \
  -H 'Content-Type: application/json' \
  -d '{"filter":{"must":[{"key":"type","match":{"value":"code"}}]},"limit":2,"with_payload":true}'
# Наблюдено: 2 точки — main.go в ветках main и feature; payload полон:
# vcs_type=gitlab, group_org=e2e/grp, repo=fixture, type=code, branch, commit_sha,
# file_path=main.go, text="func main() { println(1) }", chunk_index=0.
```

## 3. Что осталось на машине

- Образы: `qdrant/qdrant:v1.19.2`, `ollama/ollama` (новые).
- Volume `vcs-rag-ollama` (модель bge-m3, 1.2 GB) — **оставлен** для повторных прогонов.
- Коллекция `vcs_rag_e2e` в изолированном Qdrant (2 точки) — оставлена для read-back.
- Контейнеры `vcs-rag-qdrant` / `vcs-rag-ollama` — остановлены и удалены (`docker rm -f`).
