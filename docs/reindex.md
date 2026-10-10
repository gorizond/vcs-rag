# Reindex procedure: changing the embeddings model/dimension

Changing `EMBEDDINGS_MODEL`/`EMBEDDINGS_DIM` to a model with a different dimension
requires recreating the collection and a full reindex. Before the procedure the
worker starts with a deterministic failure (FR-009): `qdrant: collection "vcs_rag"
has dimension N, expected M — reindex required…` — no records are created in this
case (**0 mixed vectors**).

## Steps

1. **Stop the worker** (Ctrl-C/SIGTERM) — the index pipeline finishes the current
   pass, state is saved only after success (FR-014).

2. **Recreate the collection** (REST command verified on Qdrant v1.19.2):

   ```bash
   curl -s -X DELETE http://localhost:6333/collections/vcs_rag
   # → {"result":true,"status":"ok",...}
   # the collection is recreated on worker startup (EnsureCollection, FR-009)
   ```

3. **Delete sidecar states** (`*.state.json` next to the mirrors) — otherwise the
   pass will consider the history already indexed and will not overwrite the points:

   ```bash
   find <MIRRORS_DIR> -name '*.state.json' -delete
   ```

   The deletion also resets the incremental metadata cache — issues/MR/CI points
   are re-embedded in full.

4. **Start the worker** with the new configuration:

   ```bash
   docker run --rm --network host \
     -e GITLAB_GROUP_ID=... -e GITLAB_TOKEN=... \
     -e EMBEDDINGS_PROVIDER=ollama -e EMBEDDINGS_MODEL=<new-model> \
     -e EMBEDDINGS_DIM=<new-dimension> -e QDRANT_URL=http://localhost:6334 \
     vcs-rag:dev -mode=worker
   ```

   On startup: the collection for the new dimension + payload indexes are
   created; the discovery/mirror/index/metadata pipelines will refill it (the
   mirrors on disk are not touched — code is read from local git objects).

5. **Checks**:

   ```bash
   curl -s http://localhost:6333/collections/vcs_rag | grep -A3 vectors   # size == EMBEDDINGS_DIM
   curl -s -X POST http://localhost:6333/collections/vcs_rag/points/count \
     -H 'Content-Type: application/json' -d '{"exact":true}'               # count grows
   ```

## Notes

- The dimension is also checked on every upsert (embed validation, FR-009) —
  writing "foreign" vectors is impossible.
- The procedure is verified by the e2e test `TestE2E_Reindex`: a failure at 768
  against 1024 — count did not change; after drop + sidecar cleanup — count>0 and
  100% of vectors of the new configuration (see `tests/integration/final-validation.md`, SC-009).
