# Garbage collection — vanished repositories and entities (v0.0.10)

**Discovery is the source of truth**: the GC lane removes from the index what
has disappeared from the VCS provider — whole repositories (repo GC) and
individual issue/MR points (entity GC). The default `WORKER_GC_DRY_RUN=true` is
the permanent kill-switch: until it is disabled explicitly, the lane only
observes and journals — nothing is ever deleted.

The lane runs in the worker (started with the other lanes, even with an empty
`NATS_URL`) and is the single writer of its registry; it needs no extra env
besides the two below.

## Environment

| Variable | Default | Meaning |
|----------|---------|---------|
| `WORKER_GC_INTERVAL` | `15m` | the GC pass ceiling (the lane paces adaptively: min 5 s, jitter, idle backoff) |
| `WORKER_GC_DRY_RUN` | `true` | `true` — observe-only (journal `would-delete`), `false` — real deletions |

## Repo GC

Registry: `<MIRRORS_DIR>/.gc_registry.json` (atomic tmp+rename), entries
`{"namespace", "path", "last_seen_at", "first_missing_at", "miss_streak"}`.

- a repository is a candidate when it is absent from **2** consecutive
  discovery listings **and** at least **10 min** have passed since
  `first_missing_at`; a returned repository resets its streak;
- at most **10** deletions per pass, oldest disappearance first; an in-flight
  repository (refresh/mirror/index holds its lock) is skipped and retried on a
  later pass; a store error keeps the entry;
- deletion order: count points (`CountRepoPoints`) → delete points
  (`DeleteRepoPoints`, `Wait=true`) → best-effort mirror + `*.state.json{,.tmp}`
  removal → registry entry removal. Points-delete failure keeps the entry;
- **anomaly guard**: an empty snapshot (while the registry is non-empty), a
  snapshot dropped by more than **30 %** (with ≥10 known repositories) or a
  stale listing (older than max(3×`WORKER_INTERVAL`, 10m)) freezes the streaks
  for up to **3** consecutive passes; afterwards the streaks resume. A vanished
  whole namespace is logged as a WARN;
- seeding on the first non-anomalous pass: mirrors uniquely matched to a known
  namespace become entries (they get the normal grace); a mirror of an unknown
  namespace is an **orphan mirror** — WARN only, with no entry and no deletion.

## Entity GC (in the metadata pass)

Reuses the full issue/MR listings already fetched by the pass — no extra API
calls. A `Meta` key `issue:<id>` / `mr:<id>` absent from the listing gets
`meta_miss[key]++`; a returned key resets it. At the second consecutive miss the
point is deleted by ID (`IssueID`/`PullRequestID`, one batch per kind) and the
key is dropped from `Meta`/`MetaMiss`; a failed batch keeps the keys for the
next pass. Caps: **32** candidates per repository and **128** deletions per
pass (shared by all repositories). CI keys (`ci:...`) belong to the existing CI
prune and are never counted or touched here. Dry-run journals the candidates and
keeps both the keys and the counters.

## Journal and metrics

Dry-run (default):

```
gc would-delete repo=<namespace/path> points=<N>
reconcile would-delete repo=<namespace/path> issues=<a> mrs=<b>
```

Real mode:

```
gc deleted repo=<namespace/path> points=<N>
```

Pass summary (one line per pass):

- GC: `gc_known`, `gc_vanished`, `gc_deleted_repos`, `gc_deleted_points`,
  `gc_suppressed_grace`, `gc_suppressed_busy`, `gc_suppressed_cap`,
  `gc_anomaly` (1 — the pass was frozen), `gc_dry_run` (would-delete count);
- metadata: `reconciled` (candidates found), `gone_keys` (dropped `Meta` keys),
  `deleted_entities` (points deleted by ID), `suppressed_cap` (candidates
  skipped because of a cap).

## Recommended rollout

1. Run with the default dry-run for at least **48 h**; check `gc_anomaly=0` and
   sample the `would-delete` candidates manually against the provider.
2. Switch the instances to real mode one by one; the kill-switch stays: setting
   `WORKER_GC_DRY_RUN=true` (restart) fully stops deletions.
3. Entity GC follows the same switch; a test issue deleted in the VCS is
   removed from the index after two metadata passes.
