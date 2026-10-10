# Secret-scan (T069, refreshed T121)

Date: 2026-10-10. Repository: `github.com/gorizond/vcs-rag` (**public**).

## Method

```bash
# working tree
git grep -nE '<pattern>' -- .
# entire history (all commits)
git log -p --all | grep -v EXAMPLE | grep -nE '<pattern>'
```

Weak pattern: `glpat-|ghp_|gho_|AKIA|-----BEGIN`.
Strict pattern: `glpat-[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{36}|gho_[A-Za-z0-9]{36}|AKIA[0-9A-Z]{16}|BEGIN [A-Z ]*PRIVATE KEY`.

The `EXAMPLE` exclusion is required: test fixtures use explicit markers
(`glpat-EXAMPLE-NOT-A-TOKEN`, `ghp-EXAMPLE-NOT-A-TOKEN`), which are visibly
invalid and must never be mistaken for credentials.

## Results (T121)

- **Working tree, strict: 0 matches.**
- **Entire history, strict (EXAMPLE excluded): 0 matches.**
- Working tree, weak: only the pattern expressions documented in this file.
- History, weak (EXAMPLE excluded): 18 matches — all are the *pre-T121*
  intentional test placeholders in `internal/config/config_test.go` and
  `internal/gitengine/auth_test.go` (`glpat-token`, `glpat-SUPERSECRET-123`,
  `glpat-secret`) plus the earlier revisions of this document. None of them is
  a real credential, and none matches the strict patterns; history is immutable.
- README / docs / docker-compose example: only `${ENV}` placeholders
  (`${GITLAB_TOKEN:-<your-read-api-token>}`), example values (`dummy` for the
  MCP validation-only provider env) and loopback addresses — no real tokens,
  groups, third-party IPs, private paths or repo data.

## Artifacts

- README, docs, compose example: placeholders only; `docker compose config -q`
  passes.
- e2e/smoke runs used only `dummy` tokens; config error messages contain only
  variable NAMES (verified by `TestSecretNotLeaked`).
- No secrets were set in the run environment (`GITLAB_TOKEN=dummy`); real values
  never left the owner's env.

## Conclusion

There are no real secrets in the working tree or the history; the only
weak-pattern matches are documented, intentionally invalid examples (0 by the
strict patterns), and the fixtures are now clearly marked as `EXAMPLE`.
