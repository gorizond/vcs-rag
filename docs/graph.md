# SDLC graph — payload-only collection, GC and query DSL

The graph layer extracts a structural view of the code and SDLC metadata into a
**separate, payload-only Qdrant collection**: [`QDRANT_GRAPH_COLLECTION`](env.md)
(default `vcs_rag_graph`, instance-scoped, must differ from `QDRANT_COLLECTION`).
Points carry no vectors — the collection is created as `vectors: {}` (there is
no dummy-vector fallback, and the worker disables the graph lane with an ERROR
if the server rejects it) — so the graph costs no embedding calls. It is written
by the graph lane and read by the graph MCP tools (`search_definitions`,
`get_imports`, `get_dependents`, `graph_schema`, `query_graph`).

## Collection schema

Every point is a flat payload (strings/bools/ints) discriminated by `kind`:

| `kind` | Contents |
|--------|----------|
| `node` + `node_type=definition` | one symbol definition of a file: `symbol`, `symbol_text` (full-text), `symbol_kind`, `package`, `file_path`, `signature`, `commit_sha` |
| `edge` + `edge_type=imports` | one package-level import of a file: `from_file`, `from_package`, `to_package`, `external` |
| `edge` + `edge_type=defines` | one file→symbol declaration: `from_file`, `to_symbol` |
| `mr_file` | one changed file of an MR/PR: `mr_id`, `file_path`, `status`, `author`, `state`, `patch` (≤64 KiB), `patch_truncated` |
| `ci` | one pipeline job, deployment or environment (`ci_level`): `pipeline_id`, `job_name`, `stage`, `status`, `duration_ms`, `ref`, `env`, `commit_sha` |
| `vuln` | one security finding: `severity`, `scanner`, `state`, `location_file` |
| `coverage` | the per-repo coverage marker (below) — rewritten in place, never swept |

Every point also carries the instance/tree scope: `repo` (`namespace/path`),
`group_org`, `vcs_type`, `branch`, `schema_ver`, `generation` and `indexed_at`.
IDs are deterministic uuid5 — `WORKER_GRAPH_SCHEMA_VER` selects the ID
namespace, the generation is deliberately not part of the ID — so re-indexing
the same revision is idempotent. Only Go files are scanned today
(`language=go`); edge endpoints are repo-namespaced package keys, so same-named
packages (`main`, `utils`) never collide across repositories.

## Nodes, edges, entities

- **Node**: a definition — the only extracted node type. A file has no
  standalone point: it is represented by its definition nodes and its edges.
- **Edges**: `imports` (file→package, one edge per imported package) and
  `defines` (file→symbol). Edges are **package-level**: symbol references are
  not indexed, so the dependents of a file are the importers of its package(s),
  never a symbol-level referencer set.
- **Entity families** (`query_graph` start kinds): `definition` (and `file`,
  which shares the discriminator), `mr_file`, `ci`, `vuln`. `ci` and `vuln`
  extraction depends on the provider: CI details come from the pipeline-detail
  seam, vulnerabilities behind the security API (an unavailable 403/404 scope
  latches the domain off for the lane).

## Scan lane and generations

The graph lane is a CPU-only adaptive-paced loop (ceiling
`WORKER_GRAPH_INTERVAL`, default **30m**; floor and jitter shared with the other
lanes). A pass scans up to `WORKER_GRAPH_MAX_REPOS_PER_PASS` (default **5**)
repositories from the discovery snapshot, rotating the window start so every
repository is covered over time, under the per-repo lock; a failed repository
never fails the pass. One **generation** stamp (the pass start, Unix seconds) is
shared by the whole pass: it is stamped on every point written, and the sweep
compares against exactly the generation of the repository's scan.

## Mark-and-sweep GC

Re-indexing writes the new generation; the previous one is removed by a
**sweep that runs only after the repository's scan completed successfully** —
a failed or canceled pass never sweeps (it may still be writing the previous
generation). The sweep deletes points with `generation < current` of the same
`repo`+`branch`, one kind at a time (nodes, edges, mr_file, ci, vuln), by
filter. It honors the global GC kill-switch `WORKER_GC_DRY_RUN` (default
`true`): in dry-run it only journals `gc would-delete` with the kind and point
count, and nothing is deleted. Coverage markers are excluded — they are never
removed by generation.

## Coverage markers

Every completed scan upserts one `kind=coverage` point per repository+branch
(deterministic ID, last writer wins): `indexed_commit`, `generation`,
`indexed_at` and `languages_with_resolvers` (today `["go"]`). Graph responses
carry a best-effort coverage notice (`repo`, `indexed_commit`, `truncated`) and
`graph_schema` reports the full marker per repository. A repository without a
marker keeps `indexed_commit` empty — unknown, not "an empty tree was scanned".

## Query DSL (`query_graph`)

`query_graph` is the unified structured traversal: start entity
(`definition`/`file`/`mr_file`/`ci`/`vuln`) plus filters, an optional edge
selector (`type`: `imports` | `defines` | `*`; `direction`: `outgoing` |
`incoming`), `hops` 1..3 (default 1) and `limit` (default 30, maximum 1000).
`start.filters` scope the start points; `filters` apply to the start and are
carried into the expansion scope (repo/branch bounds cannot be escaped by a
hop). Filter values compile to Qdrant conditions: string = exact match, bool =
boolean match, number = integer match, list = match-any (in), `null` =
is-null, `{gt,gte,lt,lte}` = numeric range.

**Selectivity requirement**: at least one selective indexed predicate is
required — `repo`, `file_path`, `from_file`, `to_package`, `from_package`,
`symbol` or `mr_id` (as a typed field or a raw condition in a must chain;
`should`/`must_not` do not count, and `kind`/`branch` alone is not enough). A
query without one is rejected **before any request** with error code
`non_selective_query` instead of walking the whole collection.

Caps: results ≤ `limit`; at most **10 000** frontier keys per hop and
**20 000** edge points scanned per hop. A cap cut never fails the query — the
answer carries `truncated: true`. Every answer also carries the coverage
notice; the result object is
`{"results": [{...payload, id}], "coverage": [...], "truncated": bool}`.

## Environment

| Variable | Default | Meaning |
|----------|---------|---------|
| `QDRANT_GRAPH_COLLECTION` | `vcs_rag_graph` | payload-only graph collection; instance-scoped, must differ from `QDRANT_COLLECTION` |
| `WORKER_GRAPH_INTERVAL` | `30m` | graph lane tick ceiling (payload-only pass) |
| `WORKER_GRAPH_SCHEMA_VER` | `1` | graph schema version; part of the ID (uuid5) namespace |
| `WORKER_GRAPH_MAX_REPOS_PER_PASS` | `5` | repositories processed per graph pass |

See also: [environment](env.md) · [GC of vanished repos and entities](gc.md) ·
[refresh & auto-freshness](refresh.md).
