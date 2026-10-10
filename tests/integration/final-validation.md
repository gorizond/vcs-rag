# Финальная валидация SC-001…SC-009 (T068)

Дата: 2026-10-10. Полный e2e-прогон на чистом изолированном контуре
(Qdrant v1.19.2 :16334, Ollama 0.40.2 :11435, bge-m3): **5 тестов PASS, 90.97 s**
(`TestE2E_Perf`, `TestE2E_Reindex`, `TestE2E_US2`, `TestE2E_US3`, `TestE2E_US4`).

```bash
QDRANT_URL=http://localhost:16334 EMBEDDINGS_BASE_URL=http://localhost:11435 \
go test -tags e2e ./tests/integration -run 'TestE2E_' -v -count=1
```

| SC | Статус | Evidence |
|----|--------|----------|
| SC-001 | ✓ | `tests/contract`: единый контракт-набор `RunProviderContract` (fake) + `TestFactorySelectsProvider` (gitlab/github, без сети); оба провайдера за одним интерфейсом; смена источника — только env, правок ядра 0 |
| SC-002 | ✓ | e2e US2: count 3 → 3 → 2; `TestE2E_Perf`: проход без изменений **17–29 ms** (тик 60 s); дублей нет |
| SC-003 | ✓* | `TestE2E_Perf`: min **50–107 ms**; фильтры repo/branch/type — тесты tools + e2e; *на перегруженном хосте p50 > 2 c — шум среды (A/B-контроль), см. `perf-baseline.md` |
| SC-004 | ✓ | e2e US2 негативный контроль: в зеркале только git-объекты + `*.state.json`, рабочих файлов кода нет |
| SC-005 | ✓ | `TestProcessSafelyIsolation` (при сбое 1 репо обработаны N−1), паника изолирована; FR-014 в линиях: лог + продолжение |
| SC-006 | ✓ | e2e US2/US3/US4: payload всех типов (`code`/`issue`/`pull_request`/`ci_failure`) полон; `EnsureIndexes` — 7 keyword-полей |
| SC-007 | ✓ | `secret-scan.md`: строгие паттерны секретов — **0** по всей истории; в артефактах только env-имена и dummy |
| SC-008 | ✓ | US6: v0.0.1 → release-ран 38065841149 success; теги `0.0.1/0.0/0/latest`; анонимный pull — HTTP 200; rc → Pre-release, `latest` не сдвинут; образ в обоих режимах + git (smoke_us6.md) |
| SC-009 | ✓ | `TestE2E_Reindex`: отказ dim=768 при 1024 (0 записей, count=3 не изменился); после drop+очистки sidecar — count=3, векторов **dim=1024: 3/3 (100%)** |

**Итог: 9/9** (SC-003 — с оговоркой окружения: на выделенном CPU < 2 c, на
shared-хосте с load>14 деградация подтверждена контрольным замером).

Дополнительно в этом прогоне: негатив US3 (`qdrant_unavailable` при живом
процессе), prune US4 (ci_failure 1 → 0 при перезапуске пайплайна в success),
prune US2 (файл удалён → count 3 → 2).
