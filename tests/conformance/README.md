# Orbit parity conformance harness (T005)

Runnable, read-only verification of the graph surface (SC-001/SC-002/SC-003/SC-006,
`contracts/conformance-harness.md`). It drives a **live** `vcs-rag` MCP server through
`tools/call` only — nothing is indexed or written server-side — and emits
`parity-report.json` + `parity-report.md`.

## Transport

The server registers the MCP **streamable-HTTP** handler (`internal/mcp/server.go`,
`runHTTP`; `MCP_TRANSPORT=sse`, `MCP_ADDR`), so the harness speaks a minimal stdlib
SSE + JSON-RPC 2.0 client: `POST /mcp` with `Accept: application/json, text/event-stream`,
responses arrive as `data:` frames, `Mcp-Session-Id` is echoed on every request.

## Run

```bash
GRAPH_E2E_MCP_ADDR=127.0.0.1:18083 PARITY_REPORT_DIR=tests/conformance \
  go test -tags e2e ./tests/conformance -run TestOrbitParityConformance -v -count=1
```

Compile-only checks:

```bash
go build -tags e2e ./...
go vet -tags e2e ./tests/conformance/...
```

With no reachable server the test **skips** cleanly and writes **no report**.

## Env

| Env | Meaning | Default |
|---|---|---|
| `GRAPH_E2E_MCP_ADDR` | host:port of the live MCP server | `127.0.0.1:18083` |
| `PARITY_REPORT_DIR` | where the reports are written | repo root |
| `GRAPH_E2E_REPO` | explicit repo fixture, tried before discovery | auto-discovery |
| `GRAPH_E2E_SYMBOL` | symbol for the `search_definitions` mode=name discovery | auto |

## Rows (62)

1. **schema** (4) — `node_types`/`edge_types`/`entity_types` ⊇ required sets;
   `query_dsl` with the four modes and a non-empty version (not pinned to a literal:
   the schema version is raised alongside the parity families).
2. **Orbit domain table** (27) — Core, code, review, CI/CD, planning, security mapped
   item by item to the declared Orbit-named families; legacy aliases (`mr_file`,
   `vuln`) are accepted as alternatives.
3. **data families** (23) — `query_graph` mode=aggregation, op=count, group_by=repo
   for every family: `group, project, branch, directory, file, imported_symbol,
   merge_request, merge_request_diff, merge_request_diff_file (alias mr_file),
   vulnerability (alias vuln), finding, vulnerability_occurrence,
   vulnerability_identifier, security_scan, vulnerability_scanner` plus the
   already-ingested `definition, ci, runner, user, note, work_item, milestone, label`.
   The canonical entity is tried first; a legacy alias is probed when the canonical
   kind is rejected (`invalid_input`, compile-time, repo-independent). Per row:
   PASS = `count>0` (canonical or alias, evidence names the entity and the repo),
   SKIP = every answered name reports `count=0` on the probed repos, GAP =
   unsupported/error.
4. **query modes** (4) — aggregation (count by package), path_finding (two discovered
   definition node keys), neighbors (1-hop), traversal (definition→file, file→imports).
5. **tool surface** (2) — `get_imports`, `get_dependents` on discovered fixtures.
6. **negative** (1) — a `query_graph` without a selective predicate returns
   `non_selective_query`.
7. **cross-domain** (1) — traversal `ci --executed_by--> runner`.

## Report format

`parity-report.json`:

```json
{
  "generated_at": "2026-10-11T18:00:00Z",
  "mcp_addr": "127.0.0.1:18083",
  "dsl_version": "2",
  "totals": {"pass": 0, "skip": 0, "gap": 0, "total": 0},
  "rows": [
    {"area": "...", "orbit_item": "...", "vcs_rag": "...", "status": "PASS|SKIP|GAP", "evidence": "..."}
  ]
}
```

`parity-report.md` renders the same rows as `| Area | Orbit | vcs-rag | Status | Evidence |`
plus a summary line `X/Y PASS, S SKIP, G GAP` and a **Gaps** section. Every evidence
string carries counts.

Status semantics:

- **PASS** — declared in the schema and/or returned live data (non-empty result on an
  instance with data).
- **SKIP** — no data on this instance (`count=0`); not a defect.
- **GAP** — missing from the schema or the request fails as unsupported.

Baseline (0.0.19, gorizond, pre-feature): 37/62 PASS, 3 SKIP, 22 GAP — the 22 GAPs
are the 10 domain-table items and the 12 data families not ingested yet; they flip
to PASS (or SKIP without data) as the parity-gap phases land. The previous 49-row
layout recorded 36/49 PASS, 3 SKIP, 10 GAP.
