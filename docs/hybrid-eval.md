# Hybrid Search Evaluation (T040)

Status: evaluation spike — document only. No production code or schema changes.

## Rationale

Purely dense retrieval (`bge-m3`, 1024-dim, COSINE) can miss exact lexical
matches — identifiers, file paths, error strings — where sparse/lexical
scoring is stronger. Qdrant supports sparse vectors since v1.7 and query
fusion (RRF / DBSF) since v1.10. The Go client (`github.com/qdrant/go-client
v1.19.4`, already pinned in `go.mod`) exposes the building blocks:
`NewSparseVectorsConfig`, `NewVectorInputSparse`, and
`NewQueryFusion(qdrant.Fusion_RRF)` / `qdrant.Fusion_DBSF`.

Before committing to hybrid search, we need offline evidence that fusion
beats dense-only on our corpus, measured on a shadow collection that leaves
production untouched.

## Non-goals (not in scope)

- No change to the production `vcs_rag` collection schema.
- No reindex of production this feature.
- No production Go code changes; this document plus throwaway eval scripts
  (run outside the repo or in a scratch branch) are the only artifacts.
- No tuning of the embedding model itself.

## Version gates

- Qdrant server ≥ 1.10 (query fusion API).
- Go client pinned at `v1.19.4` (already the case). Do not float the version
  during the experiment — record exact versions in the eval report.
- Sparse vectors require the collection to be created with a named sparse
  vector config at creation time.

## Method

### 1. Shadow collection

Create a **separate, throwaway collection** (e.g. `vcs_rag_hybrid_eval`) with
two NAMED vectors:

- `dense`: 1024-dim, COSINE (same as production).
- `sparse`: a sparse vector config (no modifier needed for pure lexical
  scoring in the first pass).

Why shadow: adding a named sparse vector to the existing `vcs_rag`
collection **in-place is not documented** and must not be assumed. Creating a
new collection guarantees no production schema change and an easy rollback
(delete the collection).

### 2. Population

Two acceptable approaches; pick one and record which:

- **Scroll copy** (preferred): scroll the source `vcs_rag` (or a snapshot of
  it) with payload included, re-use the existing dense vectors, and compute
  sparse vectors on the fly from the stored text. No re-embedding cost.
- **Sample re-embedding** (fallback if scrolling with vectors is
  impractical): take a fixed random sample (e.g. 10k–50k points), re-embed
  with `bge-m3`, build sparse vectors from the same text.

Sparse vector generation should be simple and reproducible (e.g. term
frequency over tokenized text); the experiment is about fusion behavior, not
about the best sparse encoder.

### 3. Query set

- Fix a query set of **N = 100–200** queries, drawn from realistic IDE/MCP
  usage (symbol names, file paths, error fragments, natural-language
  questions). Store as a checked-in JSON/CSV fixture used by all runs.
- Where feasible, annotate relevance (which point IDs are correct) for
  recall/MRR; otherwise use dense-only top-k as a weak pseudo-gold and treat
  results as directional only.

### 4. Runs

For each query, execute four retrievals at top-k (k = 10; report also k = 50):

1. dense-only (`using: dense`)
2. sparse-only (`using: sparse`)
3. RRF fusion (`NewQueryFusion(qdrant.Fusion_RRF)`)
4. DBSF fusion (`qdrant.Fusion_DBSF`)

### 5. Metrics

- recall@k (against annotations, or pseudo-gold)
- MRR@k
- latency: p50 / p95 per query per mode (client-side, same warm cache state)
- overlap between modes (how much fused results differ from dense-only)

Record environment: Qdrant version, client version, host resources, sample
size, sparse builder version.

## Decision criteria

Recommend hybrid (RRF preferred unless DBSF clearly wins) **only if**:

- fusion recall@10 (and MRR@10) exceeds dense-only by **≥ 10% relative** on
  the annotated set, AND
- p95 latency stays within **1.5×** of dense-only, AND
- gain is consistent across at least two query categories (e.g. lexical vs
  natural-language), not driven by one category.

Otherwise, do not adopt. If adopted later, the production flip is a
collection/alias pointer change only (build the real hybrid collection,
switch the alias), executed as a separate, planned change — never a mutation
of the live schema.

## Recommendation

Run this evaluation before any implementation work. If the margin above is
met, open a follow-up feature spec for a hybrid `vcs_rag` collection plus
alias switch and reindex; if not, keep dense-only and close the spike. The
shadow-collection approach keeps the cost of either outcome low.
