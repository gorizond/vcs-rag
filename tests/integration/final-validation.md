# Final validation SC-001…SC-009 (T068)

Date: 2026-10-10. Full e2e run in a clean isolated environment
(Qdrant v1.19.2 :16334, Ollama 0.40.2 :11435, bge-m3): **5 tests PASS, 90.97 s**
(`TestE2E_Perf`, `TestE2E_Reindex`, `TestE2E_US2`, `TestE2E_US3`, `TestE2E_US4`).

```bash
QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
go test -tags e2e ./tests/integration -run 'TestE2E_' -v -count=1
```

| SC | Status | Evidence |
|----|--------|----------|
| SC-001 | ✓ | `tests/contract`: single contract suite `RunProviderContract` (fake) + `TestFactorySelectsProvider` (gitlab/github, no network); both providers behind one interface; switching the source is env-only, 0 core changes |
| SC-002 | ✓ | e2e US2: count 3 → 3 → 2; `TestE2E_Perf`: unchanged pass **17–29 ms** (tick 60 s); no duplicates |
| SC-003 | ✓* | `TestE2E_Perf`: min **50–107 ms**; repo/branch/type filters — tools tests + e2e; *on a loaded host p50 > 2 s — environment noise (A/B control), see `perf-baseline.md` |
| SC-004 | ✓ | e2e US2 negative control: only git objects + `*.state.json` in the mirror, no working code files |
| SC-005 | ✓ | `TestProcessSafelyIsolation` (on 1 repo failure N−1 are processed), the panic is isolated; FR-014 in the pipelines: log + continue |
| SC-006 | ✓ | e2e US2/US3/US4: payloads of all types (`code`/`issue`/`pull_request`/`ci_failure`) are complete; `EnsureIndexes` — 7 keyword fields |
| SC-007 | ✓ | `secret-scan.md`: strict secret patterns — **0** across the entire history; artifacts contain only env names and dummy values |
| SC-008 | ✓ | US6: v0.0.1 → release run 38065841149 success; tags `0.0.1/0.0/0/latest`; anonymous pull — HTTP 200; rc → Pre-release, `latest` not moved; the image in both modes + git (smoke_us6.md) |
| SC-009 | ✓ | `TestE2E_Reindex`: rejection of dim=768 against 1024 (0 records, count=3 unchanged); after drop+sidecar cleanup — count=3, vectors **dim=1024: 3/3 (100%)** |

**Result: 9/9** (SC-003 — with an environment caveat: on a dedicated CPU < 2 s,
on a shared host with load>14 the degradation is confirmed by the control
measurement).

Also in this run: US3 negative (`qdrant_unavailable` with a live process),
US4 prune (ci_failure 1 → 0 when the pipeline is rerun to success),
US2 prune (file deleted → count 3 → 2).
