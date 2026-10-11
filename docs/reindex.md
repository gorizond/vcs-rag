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

## Graph schema version bump (`WORKER_GRAPH_SCHEMA_VER`)

The graph schema version is folded into the uuid5 ID namespace of the
payload-only graph collection ([graph](graph.md)): bumping it — e.g. the `1`→`2`
canonical key-space migration (edges moved to `from_key`/`to_key` endpoint
refs), or the `2`→`3` Orbit-parity node/edge catalog (structure, review and
security families, `docs/graph.md`) — **re-keys every graph point id**. The
same revision indexed under the
new version gets fresh ids; the old ids stay behind as stale points, and the
query tools do not filter by `schema_ver`, so both sets would surface until the
old space is purged. A bump therefore requires a one-time **full graph
re-scan**:

1. **Stop the worker** (a canceled/in-flight pass never sweeps).
2. **Set the new version** — `WORKER_GRAPH_SCHEMA_VER=<new>` ([env](env.md)).
3. **Purge the old id space** — either delete the graph collection
   (`QDRANT_GRAPH_COLLECTION`; it is recreated vector-less at lane start,
   `EnsureGraphCollection`) or let the normal generation sweep delete the old
   points, which needs `WORKER_GC_DRY_RUN=false`. With the dry-run default the
   stale old-id points are only journalled as `gc would-delete` and keep
   answering queries alongside the new ones.
4. **Start the worker** with the new configuration. Each completed repo pass
   writes the new id space and upserts its coverage marker.
5. **Checks**:
   - `graph_schema` reports `query_dsl.version` == the new
     `WORKER_GRAPH_SCHEMA_VER`, and `coverage_per_repo` lists the repositories;
   - no points carry the old `schema_ver` once every repository has been
     re-scanned.

Coverage markers are **stable across a bump**: they live in a dedicated fixed
namespace (`schema:coverage`), not the versioned one, so a bump does not
re-key them — after a collection drop they are re-upserted in place at each
repo's next completed scan (`indexed_commit` is empty until then). Only
node/edge/entity ids change. The lane rotates `WORKER_GRAPH_MAX_REPOS_PER_PASS`
(default **5**) repositories per pass — raise it temporarily to re-scan the
whole fleet faster.

## Notes

- The dimension is also checked on every upsert (embed validation, FR-009) —
  writing "foreign" vectors is impossible.
- The procedure is verified by the e2e test `TestE2E_Reindex`: a failure at 768
  against 1024 — count did not change; after drop + sidecar cleanup — count>0 and
  100% of vectors of the new configuration (see `tests/integration/final-validation.md`, SC-009).
