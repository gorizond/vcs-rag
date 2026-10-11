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
| `node` + `node_type=definition` | one symbol definition of a file: `symbol`, `symbol_text` (full-text), `symbol_kind`, `package`, `file_path`, `signature`, `commit_sha`, `node_key` (canonical ref) |
| `edge` | one relation (types below): `edge_type`, canonical `from_key`/`to_key` + `from_type`/`to_type`, plus the legacy kind-specific fields (`from_file`, `from_package`, `to_package`, `to_package_name`, `to_symbol`, `external`) |
| `mr_file` | one changed file of an MR/PR: `mr_id`, `file_path`, `status`, `author`, `state`, `patch` (≤64 KiB), `patch_truncated` |
| `ci` | one pipeline job, deployment or environment (`ci_level`): `pipeline_id`, `job_name`, `stage`, `status`, `duration_ms`, `ref`, `env`, `commit_sha` |
| `vuln` | one security finding: `severity`, `scanner`, `state`, `location_file` |
| `user` | one VCS account seen on an authored/commented/assigned link: `node_key` (`user:<host>#<id>`), `vcs_user_id`, `vcs_host`, `login`, `name`, `state` |
| `note` | one issue/change-request comment: `node_key` (`repo\|note#<id>`), `note_id`, `parent_key`, `author_key`, `body` (≤2 KiB, unindexed), `system` |
| `work_item` | one issue/task work item: `node_key` (`repo\|wi:<type>#<iid>`), `item_id`, `item_type`, `state`, `title` |
| `milestone` | one milestone: `node_key` (`repo\|ms#<id>`), `ms_id`, `title`, `state`, `due_date` |
| `label` | one normalized label: `node_key`/`label_key` (`lab:<scope>\|<name>`), `name`, `scope` |
| `runner` | one CI runner: `node_key` (`runner:<host>#<id>`), `runner_id`, `vcs_host`, `description`, `state`, `runner_type`, `tags` |
| `coverage` | the per-repo coverage marker (below) — rewritten in place, never swept |

Every point also carries the instance/tree scope: `repo` (`namespace/path`),
`group_org`, `vcs_type`, `branch`, `schema_ver`, `generation` and `indexed_at`.
IDs are deterministic uuid5 — `WORKER_GRAPH_SCHEMA_VER` selects the ID
namespace, the generation is deliberately not part of the ID — so re-indexing
the same revision is idempotent. Only Go files are scanned today
(`language=go`); edge endpoints are repo-namespaced package keys, so same-named
packages (`main`, `utils`) never collide across repositories.

**Canonical-key migration**: edges carry the canonical `from_key`/`to_key`
endpoint refs (plus `from_type`/`to_type`) next to the legacy kind-specific
fields (`from_file`/`to_package`/`to_symbol`, kept for backward compatibility),
and nodes carry `node_key`. The key-space change bumped
`WORKER_GRAPH_SCHEMA_VER` `1`→`2`: the version is folded into the ID namespace,
so every graph id was re-keyed and a one-time **full graph re-scan** is
required — see the [schema-version bump procedure](reindex.md#graph-schema-version-bump-worker_graph_schema_ver).
Coverage markers live in a dedicated version-stable namespace and are **not**
re-keyed by a bump.

## Nodes, edges, entities

- **Node kinds**: `definition` is the only code node — a file has no standalone
  point, it is represented by its definition nodes and its edges. The
  Orbit-parity families are `user`, `note`, `work_item`, `milestone`, `label`
  and `runner`; `mr_file`, `ci` and `vuln` are entity points the DSL can start
  from (not nodes in the node/edge sense).
- **Edges**: `imports` (file→package) and `defines` (file→symbol), both
  **package-level** — symbol references are not indexed, so the dependents of a
  file are the importers of its package(s), never a symbol-level referencer
  set. The parity relations are `authored` (work_item→user), `commented`
  (note→user), `assigned_to` (work_item→user), `labelled` (work_item→label),
  `in_milestone` (work_item→milestone) and `executed_by` (ci→runner).
- **Canonical keys** identify an endpoint: `repo|file:<path>`,
  `repo|pkg:<pkg>`, `user:<host>#<id>`, `repo|note#<id>`,
  `repo|wi:<type>#<iid>`, `repo|ms#<id>`, `lab:<scope>|<name>`,
  `runner:<host>#<id>`. A node's `node_key` and the matching edge endpoint
  `from_key`/`to_key` are the same string by construction, so path finding and
  neighbours join them without a translation table.
- **Entity families** (`query_graph` start kinds): `definition` (and `file`,
  which shares the discriminator), `mr_file`, `ci`, `vuln`, `user`, `note`,
  `work_item`, `milestone`, `label`, `runner`. The SDLC domains are
  **best-effort and gated**: extraction depends on the provider's API/role, an
  unavailable 403/404 scope latches the domain off for the lane and only
  withholds its sweep — it never fails the pass.

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
`repo`+`branch`, one kind at a time (nodes, edges, mr_file, ci, vuln and — only
when that domain's listing completed — user, work_item, milestone, label,
runner), by filter. Notes are **never generation-swept**: they are
cascade-deleted only when their parent work item or change request is removed.
It honors the global GC kill-switch `WORKER_GC_DRY_RUN` (default
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
(`definition`/`file`/`mr_file`/`ci`/`vuln`/`user`/`note`/`work_item`/
`milestone`/`label`/`runner`) plus filters, an optional edge selector (`type`:
`imports` | `defines` | `authored` | `commented` | `assigned_to` | `labelled` |
`in_milestone` | `executed_by` | `*`; `direction`: `outgoing` | `incoming`),
`hops` 1..3 (default 1) and `limit` (default 30, maximum 1000). The other
query families — `aggregation`, `path_finding` (canonical `from`/`to` keys,
`rel_types` required) and `neighbors` (`node_key`) — are described in the
[README](../README.md#graph-query-modes-query_graph).
`start.filters` scope the start points; `filters` apply to the start and are
carried into the expansion scope (repo/branch bounds cannot be escaped by a
hop). Filter values compile to Qdrant conditions: string = exact match, bool =
boolean match, number = integer match, list = match-any (in), `null` =
is-null, `{gt,gte,lt,lte}` = numeric range.

**Selectivity requirement**: at least one selective indexed predicate is
required — `repo`, `file_path`, `from_file`, `to_package`, `from_package`,
`symbol`, `mr_id`, `node_key`, `from_key` or `to_key` (as a typed field or a
raw condition in a must chain;
`should`/`must_not` do not count, and `kind`/`branch` alone is not enough). A
query without one is rejected **before any request** with error code
`non_selective_query` instead of walking the whole collection.

Caps: results ≤ `limit`; at most **10 000** frontier keys per hop and
**20 000** edge points scanned per hop. A cap cut never fails the query — the
answer carries `truncated: true`. Every answer also carries the coverage
notice; the result object is
`{"results": [{...payload, id}], "coverage": [...], "truncated": bool}`.

## Authz and freshness (scope)

Two deliberate scope cuts keep this layer bounded:

- **Per-user authorization is punted.** The graph has no ACL: no point carries
  per-user ownership, none of the query tools filter results by the calling
  user, and `MCP_AUTHZ_ENABLED` is a reserved **no-op** (see
  [environment](env.md)). The graph is served to a trusted loopback consumer;
  the isolation boundary is the instance itself — `QDRANT_GRAPH_COLLECTION`
  plus the repo/branch scope every point and hop filter is pinned to
  ([scaling and multiple instances](../README.md#scaling-and-multiple-instances)).
  Expose the MCP endpoint only behind TLS + auth if it must leave loopback.
- **CDC freshness is a non-goal.** Real-time change data capture (provider
  webhooks, a streaming bus, an outbox) needs a stack outside this project's
  bounds. Freshness comes from the **periodic lanes** instead: the graph lane
  ceiling (`WORKER_GRAPH_INTERVAL`, default 30m) plus the metadata/mirror
  lanes, and with NATS the on-demand `refresh_repo`/auto-freshness path
  ([refresh](refresh.md)). The graph is eventually fresh, never real-time.

## Environment

| Variable | Default | Meaning |
|----------|---------|---------|
| `QDRANT_GRAPH_COLLECTION` | `vcs_rag_graph` | payload-only graph collection; instance-scoped, must differ from `QDRANT_COLLECTION` |
| `WORKER_GRAPH_INTERVAL` | `30m` | graph lane tick ceiling (payload-only pass) |
| `WORKER_GRAPH_SCHEMA_VER` | `2` | graph schema version; part of the ID (uuid5) namespace. Bumped `1`→`2` for the canonical key-space migration — a bump re-keys every id and requires a full graph re-scan ([reindex](reindex.md#graph-schema-version-bump-worker_graph_schema_ver)) |
| `WORKER_GRAPH_MAX_REPOS_PER_PASS` | `5` | repositories processed per graph pass |
| `MCP_AUTHZ_ENABLED` | `false` | reserved **no-op**: per-user authz is punted (no filtering behind the flag) |

See also: [environment](env.md) · [GC of vanished repos and entities](gc.md) ·
[refresh & auto-freshness](refresh.md).
