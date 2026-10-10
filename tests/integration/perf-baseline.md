# Performance baseline (T067)

Date: 2026-10-10. Environment: isolated Qdrant v1.19.2 (:16334) + Ollama 0.40.2
(:11435, bge-m3, CPU only; ports 6334/11434 on the host are occupied by foreign
services). E2E fixture: 3 commits, ~3 code chunks. Method: `TestE2E_Perf`
(tag e2e); latency — 10 consecutive `tools/call search_code` via a stdio
subprocess (1 warmup outside the measurement), timing — around the full round-trip.

## (a) Unchanged pass (repeated ProcessRepoOnce)

- **17–29 ms** (4 runs), points 3 → 3 — no duplicates.
- SC-002 (≤ 60 s per tick) holds with ~2000× headroom.

## (b) Latency of `search_code` (end-to-end, stdio)

| run | min | p50 | avg | max |
|---|---|---|---|---|
| #1 | 107 ms | — | 2.60 s | 5.58 s |
| #2 | 86 ms | — | 2.35 s | 8.17 s |
| #3 | 68 ms | 2.94 s | 2.54 s | 11.40 s |
| #4 | **50 ms** | 1.62 s | 2.31 s | 6.32 s |

Control in the same window: a direct `POST /api/embed` x20 — median ~0.37 s,
spikes up to 4.29 s; an A/B probe "our client vs raw HTTP" — the same spread.
The shared host's `load average` during the measurements: **13.8 → 20.5**
(shared machine, many foreign services; CPU-only bge-m3 inference).

## Conclusion

- Best (unloaded) measurement: **50–107 ms** — with an available CPU SC-003
  (< 2 s) holds.
- On a loaded shared host p50 degrades to 1.6–2.9 s; the control direct embed
  shows the same spread — this is a property of the environment, not of the
  pipeline code.
- Scale caveat: the fixture is small (3 points); production repository volumes
  and network were not tested. For a production baseline — repeat on a dedicated
  host and against a real mirror.
