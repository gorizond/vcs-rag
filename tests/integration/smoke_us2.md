# Smoke US2 — code indexing (T044)

Run date: 2026-10-10. Real Qdrant v1.19.2 + Ollama (bge-m3).
⚠️ Ports 6333/6334 (a foreign `qdrant` container, publishes REST only) and 11434
(native Ollama) are occupied on the machine — an **isolated environment** on
16334/11435 was used. Foreign services were not affected.

## 0. Stack (isolated environment)

```bash
docker pull qdrant/qdrant:v1.19.2
docker run -d --name vcs-rag-qdrant -p 127.0.0.1:16333:6333 -p 127.0.0.1:16334:6334 qdrant/qdrant:v1.19.2
docker pull ollama/ollama
docker run -d --name vcs-rag-ollama -p 127.0.0.1:11435:11434 -v vcs-rag-ollama:/root/.ollama ollama/ollama
docker exec vcs-rag-ollama ollama pull bge-m3   # 1.2 GB
```

## 1. E2E test

```bash
docker run --rm --network host -v "$PWD":/src -w /src \
  -e QDRANT_URL=http://localhost:16334 -e EMBEDDINGS_BASE_URL=http://localhost:11435 \
  golang:1.27 go test -tags e2e ./tests/integration -run TestE2E_US2 -v -count=1
```

Observed (10.10.2026): **PASS in 6.25 s**.
- count after the first pass = **3**;
- count after a repeated pass = **3** (idempotency, FR-011);
- count after deleting `feature.txt` + commit = **2** (prune);
- negative control (SC-004): no working code files in the mirror — only
  git objects and the sidecar `fixture.git.state.json`.

## 2. Data read-back (REST)

```bash
curl -s http://localhost:16333/collections/vcs_rag_e2e | python3 -m json.tool
# vectors: {'size': 1024, 'distance': 'Cosine'}  (observed)

curl -s -X POST http://localhost:16333/collections/vcs_rag_e2e/points/count \
  -H 'Content-Type: application/json' -d '{"exact":true}'
# {"count":2}  (after prune)

curl -s -X POST http://localhost:16333/collections/vcs_rag_e2e/points/scroll \
  -H 'Content-Type: application/json' \
  -d '{"filter":{"must":[{"key":"type","match":{"value":"code"}}]},"limit":2,"with_payload":true}'
# Observed: 2 points — main.go in branches main and feature; payload is complete:
# vcs_type=gitlab, group_org=e2e/grp, repo=fixture, type=code, branch, commit_sha,
# file_path=main.go, text="func main() { println(1) }", chunk_index=0.
```

## 3. What was left on the machine

- Images: `qdrant/qdrant:v1.19.2`, `ollama/ollama` (new).
- Volume `vcs-rag-ollama` (bge-m3 model, 1.2 GB) — **kept** for repeated runs.
- Collection `vcs_rag_e2e` in the isolated Qdrant (2 points) — kept for read-back.
- Containers `vcs-rag-qdrant` / `vcs-rag-ollama` — stopped and removed (`docker rm -f`).
