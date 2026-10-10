# Smoke US6 — релиз (T061–T063)

Дата прогона: 2026-10-10. Репозиторий public, релиз v0.0.1 разрешён.

## T061. Версионирование образа

```bash
docker build --build-arg VERSION=v0.0.1-test -t vcs-rag:ver .
docker run --rm vcs-rag:ver -version
```

Наблюдено: `vcs-rag v0.0.1-test`.
`release.yml` передаёт `build-args: VERSION=${{ github.ref_name }}` — версия
берётся из имени тега; README дополнен (флаги и теги релизного образа).

## T062. Релиз-пайплайн

### v0.0.1 (стабильный)

- `git tag v0.0.1 && git push origin v0.0.1` → release-ран **38065841149 —
  success** (1 m 17 s).
- Теги образа (ghcr API): `0.0.1, 0.0, 0, latest` — **без префикса `v`**
  (metadata-action снимает его; `:v0.0.1` не существует).
- `docker pull ghcr.io/gorizond/vcs-rag:0.0.1` → `-version` = **`vcs-rag v0.0.1`**.
- Публичность: анонимный токен `ghcr.io/token?...:pull` → GET манифеста
  `0.0.1` → **HTTP 200** (образ публичный; `PATCH visibility=public` не потребовался).
- Release: `gh release view v0.0.1` → `prerel=false draft=false`, авто-ноты.

### v0.0.2-rc1 (pre-release)

- Первый прогон дал `isPrerelease=false` — `softprops/action-gh-release@v3`
  не помечает rc автоматически. Исправление: в `release.yml` добавлен
  `prerelease: ${{ contains(github.ref_name, '-') }}` (коммит `a1bd281`).
- Нюанс: workflow для тега берётся **из коммита тега**, поэтому rc-тег перенесён
  на `a1bd281` (с фиксом); повторный ран **38066260938 — success**.
- Теги образа: `0.0.2-rc1` (major/minor-теги для rc не создаются),
  `docker pull …:0.0.2-rc1` → `vcs-rag v0.0.2-rc1`.
- Release: `isPrerelease=true`; **`latest` не сдвинут** — digest
  `sha256:0aca538f…ff027` совпал до/после rc.
- Cleanup: `gh release delete v0.0.2-rc1 -y --cleanup-tag` → релиз и git-тег
  удалены (проверено `gh release view` → not found, `git ls-remote --tags` без rc).
  ⚠️ Версия образа `0.0.2-rc1` в ghcr остаётся (cleanup релиза её не удаляет).

### Негатив

- `git tag test-1 && git push origin test-1` → новых release-ранов **нет**
  (набор `databaseId` до/после совпал), тег `test-1` удалён с origin и локально.

## T063. Самодостаточность релизного образа (`0.0.1`)

```bash
docker run -i --rm ghcr.io/gorizond/vcs-rag:0.0.1 -mode=mcp </dev/null        # exit 2 (нет env)
docker run -i --rm -e GITLAB_GROUP_ID=1 -e GITLAB_TOKEN=dummy \
  -e EMBEDDINGS_PROVIDER=ollama -e QDRANT_URL=http://127.0.0.1:19999 \
  ghcr.io/gorizond/vcs-rag:0.0.1 -mode=mcp </dev/null                          # exit 0
docker run --rm ghcr.io/gorizond/vcs-rag:0.0.1 -mode=worker                    # exit 2 (нет env)
docker run --rm --entrypoint git ghcr.io/gorizond/vcs-rag:0.0.1 --version      # git 2.47.3
```

Наблюдено: без env — config-ошибка exit 2 (токены только через env);
с dummy env и недоступным Qdrant сервер стартует, EOF → exit 0 (без паники);
git в runtime-образе есть. (В брифе для первого пункта указан exit 0 — он
достигается при валидном env; без env корректная конфигурационная ошибка 2.)

## Что осталось на машине

- Локальные образы: `vcs-rag:ver`, `ghcr.io/gorizond/vcs-rag:0.0.1`, `:0.0.2-rc1`.
- ghcr: версии `0.0.1` и `0.0.2-rc1` (rc-версия не удалялась).
- Git: тег `v0.0.1` (стабильный), ветка main; rc/test-теги удалены.
