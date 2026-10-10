# Процедура reindex: смена модели/размерности эмбеддингов

Смена `EMBEDDINGS_MODEL`/`EMBEDDINGS_DIM` на модель другой размерности требует
пересоздания коллекции и полной переиндексации. До процедуры воркер стартует с
детерминированным отказом (FR-009): «коллекция "vcs_rag" имеет размерность N,
ожидается M — требуется переиндексация…» — записи при этом **не создаются**
(0 смешанных векторов).

## Шаги

1. **Остановить воркер** (Ctrl-C/SIGTERM) — линия index завершает текущий проход,
   state сохраняется только после успеха (FR-014).

2. **Пересоздать коллекцию** (REST-команда проверена на Qdrant v1.19.2):

   ```bash
   curl -s -X DELETE http://localhost:6333/collections/vcs_rag
   # → {"result":true,"status":"ok",...}
   # коллекция будет создана заново при старте воркера (EnsureCollection, FR-009)
   ```

3. **Удалить sidecar-состояния** (`*.state.json` рядом с зеркалами) — иначе проход
   посчитает историю уже проиндексированной и не перезапишет точки:

   ```bash
   find <MIRRORS_DIR> -name '*.state.json' -delete
   ```

4. **Запустить воркер** с новой конфигурацией:

   ```bash
   docker run --rm --network host \
     -e GITLAB_GROUP_ID=... -e GITLAB_TOKEN=... \
     -e EMBEDDINGS_PROVIDER=ollama -e EMBEDDINGS_MODEL=<новая-модель> \
     -e EMBEDDINGS_DIM=<новая-размерность> -e QDRANT_URL=http://localhost:6334 \
     vcs-rag:dev -mode=worker
   ```

   На старте: создание коллекции под новую размерность + payload-индексы; линии
   discovery/mirror/index/metadata наполнят её заново (зеркала на диске не
   трогаются — код читается из локальных git-объектов).

5. **Проверки**:

   ```bash
   curl -s http://localhost:6333/collections/vcs_rag | grep -A3 vectors   # size == EMBEDDINGS_DIM
   curl -s -X POST http://localhost:6333/collections/vcs_rag/points/count \
     -H 'Content-Type: application/json' -d '{"exact":true}'               # count растёт
   ```

## Примечания

- Размерность проверяется и на каждом upsert (embed-валидация, FR-009) — запись
  «чужих» векторов невозможна.
- Процедура проверена e2e-тестом `TestE2E_Reindex`: отказ при 768 против 1024 —
  count не изменился; после drop + очистки sidecar — count>0 и 100% векторов
  новой конфигурации (см. `tests/integration/final-validation.md`, SC-009).
