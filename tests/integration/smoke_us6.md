# Smoke US6 — release (T061–T063)

Run date: 2026-10-10. The repository is public, release v0.0.1 is allowed.

## T061. Image versioning

```bash
docker build --build-arg VERSION=v0.0.1-test -t vcs-rag:ver .
docker run --rm vcs-rag:ver -version
```

Observed: `vcs-rag v0.0.1-test`.
`release.yml` passes `build-args: VERSION=${{ github.ref_name }}` — the version
comes from the tag name; README updated (release image flags and tags).

## T062. Release pipeline

### v0.0.1 (stable)

- `git tag v0.0.1 && git push origin v0.0.1` → release run **38065841149 —
  success** (1 m 17 s).
- Image tags (ghcr API): `0.0.1, 0.0, 0, latest` — **without the `v` prefix**
  (metadata-action strips it; `:v0.0.1` does not exist).
- `docker pull ghcr.io/gorizond/vcs-rag:0.0.1` → `-version` = **`vcs-rag v0.0.1`**.
- Public visibility: an anonymous token `ghcr.io/token?...:pull` → GET of the
  `0.0.1` manifest → **HTTP 200** (the image is public; `PATCH visibility=public`
  was not needed).
- Release: `gh release view v0.0.1` → `prerel=false draft=false`, auto-generated notes.

### v0.0.2-rc1 (pre-release)

- The first run produced `isPrerelease=false` — `softprops/action-gh-release@v3`
  does not mark rc automatically. Fix: `prerelease: ${{ contains(github.ref_name, '-') }}`
  was added to `release.yml` (commit `a1bd281`).
- Nuance: the workflow for a tag is taken **from the tag's commit**, so the rc tag
  was moved to `a1bd281` (with the fix); the repeated run **38066260938 — success**.
- Image tags: `0.0.2-rc1` (major/minor tags are not created for rc),
  `docker pull …:0.0.2-rc1` → `vcs-rag v0.0.2-rc1`.
- Release: `isPrerelease=true`; **`latest` was not moved** — the digest
  `sha256:0aca538f…ff027` matched before/after rc.
- Cleanup: `gh release delete v0.0.2-rc1 -y --cleanup-tag` → the release and the
  git tag were deleted (verified by `gh release view` → not found, `git ls-remote --tags` without rc).
  ⚠️ The image version `0.0.2-rc1` remains in ghcr (release cleanup does not remove it).

### Negative control

- `git tag test-1 && git push origin test-1` → **no** new release runs
  (the `databaseId` set matched before/after), the `test-1` tag was deleted from origin and locally.

## T063. Self-sufficiency of the release image (`0.0.1`)

```bash
docker run -i --rm ghcr.io/gorizond/vcs-rag:0.0.1 -mode=mcp </dev/null        # exit 2 (no env)
docker run -i --rm -e GITLAB_GROUP_ID=1 -e GITLAB_TOKEN=dummy \
  -e EMBEDDINGS_PROVIDER=ollama -e QDRANT_URL=http://127.0.0.1:19999 \
  ghcr.io/gorizond/vcs-rag:0.0.1 -mode=mcp </dev/null                          # exit 0
docker run --rm ghcr.io/gorizond/vcs-rag:0.0.1 -mode=worker                    # exit 2 (no env)
docker run --rm --entrypoint git ghcr.io/gorizond/vcs-rag:0.0.1 --version      # git 2.47.3
```

Observed: without env — config error exit 2 (tokens via env only);
with dummy env and an unavailable Qdrant the server starts, EOF → exit 0 (no panic);
git is present in the runtime image. (The brief for the first item states exit 0 —
that is achieved with a valid env; without env the correct config error 2 is returned.)

## What was left on the machine

- Local images: `vcs-rag:ver`, `ghcr.io/gorizond/vcs-rag:0.0.1`, `:0.0.2-rc1`.
- ghcr: versions `0.0.1` and `0.0.2-rc1` (the rc version was not deleted).
- Git: tag `v0.0.1` (stable), branch main; rc/test tags deleted.
