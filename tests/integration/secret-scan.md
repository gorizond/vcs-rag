# Secret-scan (T069)

Date: 2026-10-10. Repository: `github.com/gorizond/vcs-rag` (public), 11 commits.

## Method

```bash
git grep -nE 'glpat-|ghp_|gho_|AKIA|-----BEGIN' $(git rev-list --all)   # entire history
git grep -nE '...' HEAD                                                  # working tree
```

Result: 20 matches (2 unique lines) — **both** in
`internal/config/config_test.go`, these are knowingly invalid test placeholders:

- `"GITLAB_TOKEN": "glpat-token"` — a config test fixture;
- `const gitlabSecret = "glpat-SUPERSECRET-123"` — a fake for the test
  "the secret does not leak into the error text" (`TestSecretNotLeaked`).

Strict check for realistic formats (entire history):

```bash
git grep -nE 'glpat-[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{36}|gho_[A-Za-z0-9]{36}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY' $(git rev-list --all)
```

Result: **0**.

## Artifacts

- `README.md`, `docs/`, `tests/integration/smoke_us*.md`: only env names and
  placeholders (`<group>`, `<token>`, `dummy`) — no real token values.
- e2e/smoke runs used only `dummy` tokens; config error messages contain only
  variable NAMES (verified by a test).
- No secrets were set in the run environment (`GITLAB_TOKEN=dummy`); real values
  never left the owner's env.

## Conclusion

There are no real secrets in the history or the working tree; the only
pattern-scan matches are intentional test placeholders (0 by the strict patterns).
