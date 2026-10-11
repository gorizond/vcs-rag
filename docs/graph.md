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
| `mr_file` | one changed file of an MR/PR: `mr_id`, `file_path`, `status`, `author`, `state`, `patch` (≤64 KiB), `patch_truncated` — legacy alias of `merge_request_diff_file` |
| `ci` | one pipeline job, deployment or environment (`ci_level`): `pipeline_id`, `job_name`, `stage`, `status`, `duration_ms`, `ref`, `env`, `commit_sha` — the flattened CI family (semantic deltas below) |
| `vuln` | one security finding: `severity`, `scanner`, `state`, `location_file` — legacy alias of `vulnerability` |
| `user` | one VCS account seen on an authored/commented/assigned link: `node_key` (`user:<host>#<id>`), `vcs_user_id`, `vcs_host`, `login`, `name`, `state` |
| `note` | one issue/change-request comment: `node_key` (`repo\|note#<id>`), `note_id`, `parent_key`, `author_key`, `body` (≤2 KiB, unindexed), `system` |
| `work_item` | one issue/task work item: `node_key` (`repo\|wi:<type>#<iid>`), `item_id`, `item_type`, `state`, `title` |
| `milestone` | one milestone: `node_key` (`repo\|ms#<id>`), `ms_id`, `title`, `state`, `due_date` |
| `label` | one normalized label: `node_key`/`label_key` (`lab:<scope>\|<name>`), `name`, `scope` |
| `runner` | one CI runner: `node_key` (`runner:<host>#<id>`), `runner_id`, `vcs_host`, `description`, `state`, `runner_type`, `tags` |
| `group` | one namespace group: `node_key` (`group:<full_path>`), `group_id`, `full_path`, `path`, `name`, `parent_key` |
| `project` | one project: `node_key` (`<namespace/path>`), `project_id`, `full_path`, `path`, `name`, `namespace_kind`, `creator_key` |
| `branch` | one branch of the provider listing: `node_key` (`repo\|branch:<name>`), `name`, `default`, `protected` |
| `directory` | one tree directory (repository root `""`): `node_key` (`repo\|dir:<path>`), `dir_key`, `path`, `name`, `root` |
| `file` | one scanned file: `node_key` (`repo\|file:<path>`), `file_path`, `name`, `dir_key`, `language` |
| `imported_symbol` | one imported symbol: `node_key` (`repo\|imsym:<file>\|<name>`), `name`, `file_path`, `resolved`, `target_definition_key` |
| `merge_request` | one change request: `node_key` (`repo\|mr:<iid>`), `mr_key`, `iid`, `title`, `state`, `source_branch`, `target_branch`, `merged`, `merge_commit_sha` |
| `merge_request_diff` | one diff version: `node_key` (`repo\|mr:<iid>\|diff:<rev>`), `mr_key`, `version`, base/start/head SHAs, `state` |
| `merge_request_diff_file` | one changed file of a diff (canonical `mr_file`): `node_key`, `mr_key`, `mr_id`, `file_path`, `status`, `patch`, `patch_truncated` |
| `vulnerability` | one finding-derived vulnerability (canonical `vuln`): `node_key` (`repo\|vuln:<id>`), `severity`, `state`, `scan_id`, `scanner_key`, `primary_identifier_key`, `location_path` |
| `security_scan` | one security scan: `node_key` (`repo\|scan:<id>`), `scan_type`, `status`, `pipeline_id` |
| `finding` | one raw security finding: `node_key` (`repo\|finding:<uuid>`), `severity`, `scan_id`, `scanner_key`, `primary_identifier_key`, `location_path` |
| `vulnerability_occurrence` | the occurrence half of a finding: `node_key` (`repo\|occurrence:<uuid>`), `uuid`, `vulnerability_id` |
| `vulnerability_identifier` | one external vulnerability identifier: `node_key` (`<host>\|vid:<external_id>`), `external_type`, `name`, `url` |
| `vulnerability_scanner` | one security scanner: `node_key` (`<host>\|scanner:<id>`), `scanner_id`, `external_id`, `name`, `vendor` |
| `coverage` | the per-repo coverage marker (below) — rewritten in place, never swept |

Every point also carries the instance/tree scope: `repo` (`namespace/path`),
`group_org`, `vcs_type`, `branch`, `schema_ver`, `generation` and `indexed_at`.
IDs are deterministic uuid5 — `WORKER_GRAPH_SCHEMA_VER` selects the ID
namespace, the generation is deliberately not part of the ID — so re-indexing
the same revision is idempotent. Only Go files are scanned today
(`language=go`); edge endpoints are repo-namespaced package keys, so same-named
packages (`main`, `utils`) never collide across repositories.

**Canonical-key migration and schema v3**: edges carry the canonical
`from_key`/`to_key` endpoint refs (plus `from_type`/`to_type`) next to the legacy
kind-specific fields (`from_file`/`to_package`/`to_symbol`, kept for backward
compatibility), and nodes carry `node_key`. The key-space change bumped
`WORKER_GRAPH_SCHEMA_VER` `1`→`2`; the Orbit-parity node/edge catalog (structure,
review and security families) bumped `2`→`3`. The version is folded into the ID
namespace, so every graph id was re-keyed and a one-time **full graph re-scan**
is required — see the
[schema-version bump procedure](reindex.md#graph-schema-version-bump-worker_graph_schema_ver).
Coverage markers live in a dedicated version-stable namespace and are **not**
re-keyed by a bump.

## Nodes, edges, entities

- **Node kinds**: `definition` is the only code node — a file has no standalone
  point, it is represented by its definition nodes and its edges; the schema-v3
  `file` node carries the same `node_key` (`repo|file:<path>`) as the
  definition nodes of that file, and a ref resolution that sees both picks the
  definition deterministically. The Orbit-parity families are `user`, `note`,
  `work_item`, `milestone`, `label` and `runner`; the schema-v3 structure
  family is `group`, `project`, `branch`, `directory`, `file`,
  `imported_symbol`; the review family is `merge_request`,
  `merge_request_diff`, `merge_request_diff_file` (legacy `mr_file`); the
  security family is `vulnerability` (legacy `vuln`), `security_scan`, `finding`,
  `vulnerability_occurrence`, `vulnerability_identifier` and
  `vulnerability_scanner`. `ci` is a flattened entity family, not a node/edge
  kind.
- **Edges**: `imports` (file→package) and `defines` (file→symbol), both
  **package-level** — symbol references are not indexed, so the dependents of a
  file are the importers of its package(s), never a symbol-level referencer
  set. The Orbit relations are `authored` (work_item→user), `commented`
  (note→user), `assigned_to` (work_item→user), `labelled` (work_item→label),
  `in_milestone` (work_item→milestone) and `executed_by` (ci→runner). The
  schema-v3 relations are `contains` (group→group/project, project→branch,
  branch→directory, directory→file), `on_branch` (directory/file→branch),
  `has_diff`/`has_latest_diff` (merge_request→merge_request_diff), `has_file`
  (merge_request_diff→merge_request_diff_file), `merged`/`reviewer`
  (user→merge_request), `in_project` (review/security kinds→project),
  `in_pipeline` (security_scan→ci), `has_finding`, `occurrence_of`,
  `detected_by`, `has_identifier`, plus the `fixes`, `scans`, `resolved_by`,
  `confirmed_by`, `dismissed_by` and `merged_at_commit` joins the review and
  security families declare.
- **Semantic deltas vs the GitLab Orbit ontology (FR-027)** — "parity" here
  means literal name compatibility plus functional availability, not an
  identical data model:
  - `ci` is a **flattening** of Orbit's separate Pipeline/Stage/Job/
    Deployment/Environment entities into one kind discriminated by `ci_level`
    (`job` | `deployment` | `environment`): there is no separate pipeline/stage
    node.
  - `vulnerability` is **derived from `vulnerability_findings`** (the finding's
    `vulnerability_id`, else its id, keys the node): the link is **INFERRED**,
    not an authoritative Orbit object.
  - **Legacy spellings stay accepted**: `mr_file` ↔ `merge_request_diff_file`
    and `vuln` ↔ `vulnerability` (the canonical spelling resolves to both
    kinds; the legacy one keeps its exact pre-v3 match), and `ci` as the
    flattened family's name (Orbit has no `ci` entity). The Orbit `Packages`
    domain (`Package`/`DECLARES_DEPENDENCY`/`HAS_VULNERABILITY`) is deliberately
    out of scope.
- **Canonical keys** identify an endpoint: `repo|file:<path>`,
  `repo|pkg:<pkg>`, `user:<host>#<id>`, `repo|note#<id>`,
  `repo|wi:<type>#<iid>`, `repo|ms#<id>`, `lab:<scope>|<name>`,
  `runner:<host>#<id>`, plus the schema-v3 refs `group:<full_path>`,
  `repo|branch:<name>`, `repo|dir:<path>`, `repo|imsym:<file>|<name>`,
  `repo|mr:<iid>`, `repo|mr:<iid>|diff:<rev>`, `repo|vuln:<id>`,
  `repo|scan:<id>`, `repo|finding:<uuid>`, `repo|occurrence:<uuid>`,
  `<host>|vid:<external_id>` and `<host>|scanner:<id>`. A node's `node_key` and
  the matching edge endpoint `from_key`/`to_key` are the same string by
  construction, so path finding and neighbours join them without a translation
  table.
- **Entity families** (`query_graph` start kinds): `definition` (and `file`,
  which shares the discriminator), `mr_file`, `ci`, `vuln`, `user`, `note`,
  `work_item`, `milestone`, `label`, `runner`, plus the schema-v3 `group`,
  `project`, `branch`, `directory`, `imported_symbol`, `merge_request`,
  `merge_request_diff`, `merge_request_diff_file`, `vulnerability`,
  `security_scan`, `finding`, `vulnerability_occurrence`,
  `vulnerability_identifier` and `vulnerability_scanner`. The SDLC domains are
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
`repo`+`branch`, one kind at a time, by filter. **Gated-sweep contract**: only a
kind whose pass listing completed may be generation-swept, so a partial or
failed listing — or a domain latched off as unavailable — can never delete
live points the pass simply did not re-see. The core kinds are swept
unconditionally: `node`, `edge` and `mr_file` (their writer fails the pass on a
listing error) plus the code-graph-derived `file`, `directory` and
`imported_symbol`. Every best-effort family rides its own gate: `user`,
`work_item`, `milestone`, `label` and `runner` (orbit), `group`/`project`
(ownership), `branch` (structure), `merge_request`/`merge_request_diff`/
`merge_request_diff_file` (review), the six security kinds, the flattened `ci`
family and the legacy `vuln` points. Notes are **never generation-swept**: they
are cascade-deleted only when their parent work item or change request is
removed; the schema-v3 sub-objects (diff files under their diff, occurrences
under their vulnerability) cascade with a stale parent, and a gated parent
cascades nothing while its own sweep is withheld. It honors the global GC
kill-switch `WORKER_GC_DRY_RUN` (default `true`): in dry-run it only journals
`gc would-delete` with the kind and point count, and nothing is deleted.
Coverage markers are excluded — they are never removed by generation.

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
`milestone`/`label`/`runner`/`group`/`project`/`branch`/`directory`/
`imported_symbol`/`merge_request`/`merge_request_diff`/
`merge_request_diff_file`/`vulnerability`/`security_scan`/`finding`/
`vulnerability_occurrence`/`vulnerability_identifier`/`vulnerability_scanner`)
plus filters, an optional edge selector (`type`:
`imports` | `defines` | `authored` | `commented` | `assigned_to` | `labelled` |
`in_milestone` | `executed_by` | `contains` | `on_branch` | `has_diff` |
`has_latest_diff` | `has_file` | `merged` | `reviewer` | `in_project` |
`in_pipeline` | `has_finding` | `occurrence_of` | `detected_by` |
`has_identifier` | `fixes` | `scans` | `resolved_by` | `confirmed_by` |
`dismissed_by` | `merged_at_commit` | `*`; `direction`: `outgoing` |
`incoming`), `hops` 1..3 (default 1) and `limit` (default 30, maximum 1000).
The other query families — `aggregation`, `path_finding` (canonical
`from`/`to` keys, `rel_types` required) and `neighbors` (`node_key`) — are
described in the [README](../README.md#graph-query-modes-query_graph).
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
**20 000** edge points scanned per hop; `neighbors` keeps at most
`MCP_GRAPH_MAX_EDGES_PER_NODE` (default **10 000**) edge points of the centre.
A cap cut never fails the query — the answer carries `truncated: true`. Every
answer also carries the coverage notice; the result object is
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
| `WORKER_GRAPH_SCHEMA_VER` | `3` | graph schema version; part of the ID (uuid5) namespace. Bumped `1`→`2` for the canonical key-space migration and `2`→`3` for the Orbit-parity node/edge catalog (structure, review and security families) — a bump re-keys every id and requires a full graph re-scan ([reindex](reindex.md#graph-schema-version-bump-worker_graph_schema_ver)) |
| `WORKER_GRAPH_MAX_REPOS_PER_PASS` | `5` | repositories processed per graph pass |
| `MCP_AUTHZ_ENABLED` | `false` | reserved **no-op**: per-user authz is punted (no filtering behind the flag) |

See also: [environment](env.md) · [GC of vanished repos and entities](gc.md) ·
[refresh & auto-freshness](refresh.md).
