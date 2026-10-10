# Smoke US4 — metadata: issues/MR-PR/CI failures (T055)

Run date: 2026-10-10. Isolated environment Qdrant **:16334** / Ollama **:11435**
(ports 6334/11434 are occupied by foreign services; setup — in `smoke_us2.md`).

## 0. Environment

```bash
docker run -d --name vcs-rag-qdrant -p 127.0.0.1:16333:6333 -p 127.0.0.1:16334:6334 qdrant/qdrant:v1.19.2
docker run -d --name vcs-rag-ollama -p 127.0.0.1:11435:11434 -v vcs-rag-ollama:/root/.ollama ollama/ollama
```

## 1. E2E test

```bash
docker run --rm --network host -v "$PWD":/src -v vcs-rag-gomod:/go/pkg/mod -w /src \
  -e QDRANT_URL=http://localhost:16334 -e EMBEDDINGS_BASE_URL=http://localhost:11435 \
  golang:1.27 go test -tags e2e ./tests/integration -run TestE2E_US4 -v -count=1
```

Fake provider: 1 issue, 1 PR, 1 failed pipeline with a 100-line log.

Observed (PASS 7.08 s):
- first pass: `issue=1 pull_request=1 ci_failure=1` (3 total);
- repeated pass: points **3** (did not grow — idempotency via deterministic IDs);
- "the pipeline is rerun to success" → `issue=1 pull_request=1 ci_failure=0`
  (the ci point is pruned by the `pipeline_id, job_id` pair);
- payloads are complete (data-model §4–6 fields), `text` = title + body (issue/PR);
  CI `text` = header + log tail: ≤ 51 lines (header + 50), ends with
  `line 100`, lines outside the last 50 are absent.

## 2. Read-back (REST, after the run)

```bash
curl -s -X POST http://localhost:16333/collections/vcs_rag_e2e4/points/count \
  -H 'Content-Type: application/json' -d '{"exact":true}'
# {"count":2}  (issue + PR; ci removed by prune)

curl -s -X POST http://localhost:16333/collections/vcs_rag_e2e4/points/scroll \
  -H 'Content-Type: application/json' \
  -d '{"filter":{"must":[{"key":"type","match":{"value":"issue"}}]},"limit":1,"with_payload":true}'
# Observed: {'state':'opened','group_org':'e2e/grp','vcs_type':'gitlab','issue_id':1,
#   'text':'Issue one\n\nissue body','type':'issue','repo':'fixture'}
```

## 3. What was left on the machine

- Collection `vcs_rag_e2e4` (2 points) in the isolated Qdrant — kept for manual queries.
- Containers `vcs-rag-qdrant` / `vcs-rag-ollama` stopped and removed after the run;
  the `vcs-rag-ollama` volume (bge-m3) was kept.
