# On-demand refresh (`refresh_repo`) — v0.0.7

Hybrid freshness: the background lanes keep the index approximately up to date
(`WORKER_MIRROR_INTERVAL` / `WORKER_META_INTERVAL`, the safety net), while the
LLM can pull one repository **on demand** before searching. The feature is off
by default: an empty `NATS_URL` keeps the previous behavior bit for bit (the
`refresh_repo` tool is not registered and no NATS connection is opened).

## Flow

```
MCP refresh_repo ──publish──▶ JetStream stream ──▶ worker consumer
       ▲                                              │
       │                                          RefreshOnce
       │                                    mirror fetch → index new commits
       │                                    (per-commit checkpoints) → metadata
       └──────poll KV status──────────────────────────┘
```

1. The MCP `refresh_repo {repo, timeout_ms?}` tool checks the status KV first:
   a fresh `done` (<30 s) is returned as `cached: true` **without publishing**;
   a live `in_progress` is polled without a new publication. Concurrent calls
   for one repository share a single in-flight run.
2. Otherwise the tool publishes `{repo, request_id, ts}` to
   `vcsrag.<id>.refresh` (header `Nats-Msg-Id=request_id`) and polls the status
   KV every 500 ms until `done` / `failed` or the timeout
   (default **20 s**, capped at **120 s**).
3. The worker consumer resolves the repository against the discovery snapshot
   (exact `namespace/path`, otherwise a unique suffix; an ambiguous suffix is an
   error listing the candidates), then runs `RefreshOnce` under the per-repo
   lock: fetch (mirror-lane semantics) → index new commits → metadata. Phases
   are best-effort: an error does not cancel the next one and is collected in
   `errors[]`.
4. The KV status is written strictly before the ack; the ack happens only after
   all Qdrant upserts, state checkpoints and the KV write.

## Responses

| Result | Meaning |
|--------|---------|
| `{status:"done", repo, commits_indexed, meta_updated, errors[], duration_ms, cached}` | the repository is fresh (or a fresh result was cached) |
| `{status:"in_progress", repo, started_at, elapsed_ms, worker_seen}` | the task continues in the background; retry once or proceed with the current data. `worker_seen=false` means no worker status was observed for the key |
| structured error `refresh_unavailable` ("continue searching the current index") | NATS is unavailable, publishing failed, or the task terminally failed |

The index stays consistent under redelivery: commits are checkpointed per
commit, so a retry indexes only the remainder (duplicates are impossible).

Repository forms:

- the search tools filter by the `repo` payload **as shown in search results**
  (e.g. `nats`, `secrets/runner`) — the full `namespace/path` matches nothing
  there;
- `refresh_repo` accepts `namespace/path` or a unique suffix (resolved against
  the discovery snapshot).

## Delivery semantics

- Consumer `worker-refresh-<id>`: durable **pull**, `MaxAckPending=1`
  (serialization — the refresh is gentle to the embeddings backend),
  `MaxDeliver=3`, backoff `[30s, 2m]`.
- **Effective ack wait is 30 s**: nats-server ties `AckWait` to the retry
  schedule — when `BackOff` is set it overrides `AckWait` with `BackOff[0]`.
- Coalesce guard: a duplicate request for a fresh run (<30 s) or a fresh result
  (<30 s) is acked as a no-op. A redelivery after a crash is older than the
  window and proceeds.
- A partial/transient failure is `Nak`-ed (retry with the backoff) and the KV
  status is kept as `failed` with the errors; after `MaxDeliver` the task is
  `Term`-ed (terminal) with the KV status `failed`.

## Instance scoping (`NATS_INSTANCE_ID`)

Several deployments can share one NATS server: the stream is common, everything
else is scoped by the instance id (default `default`).

| Object | Name |
|--------|------|
| stream | `VCSRAG_REFRESH` (WorkQueue, File, MaxAge 24 h), subjects shared: `vcsrag.*.refresh` |
| subject | `vcsrag.<id>.refresh` |
| consumer | `worker-refresh-<id>` (filter = the exact instance subject) |
| KV | `refresh_status_<id>` (TTL 24 h): `{state, request_id, started_at, finished_at, commits_indexed, meta_updated, errors[], duration_ms}` |
| connection name | `vcs-rag-<id>` (`-mcp` for the search server) |

Consumer filters must not overlap — a WorkQueue stream rejects a second
consumer with an overlapping filter, and the provisioning validates the exact
instance names up front. The topology (stream/consumer/KV) is provisioned
idempotently at worker startup and recreated if deleted manually; the
parameters are visible in the NATS monitoring `/jsz?streams=1&consumers=1`.

## Deployment

- `docker-compose.yml` (repository root) — worker + MCP + Qdrant + NATS
  JetStream with `${ENV}` placeholders.
- Environment variables — the `Key environment variables` table in
  [`../README.md`](../README.md): `NATS_URL`, `NATS_INSTANCE_ID`,
  `WORKER_INTERVAL`, `WORKER_META_INTERVAL`, `WORKER_MIRROR_INTERVAL`.
