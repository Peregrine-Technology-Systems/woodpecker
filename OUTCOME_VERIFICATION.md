# Outcome Verification on Step Kill (Peregrine #235)

When a step is **killed mid-execution** (an agent scaled down or preempted, a
disconnect, a server-side cancel), the work it was doing may nonetheless have
*landed* — most commonly a deploy that reached the target before the agent
died. Outcome verification lets such a step declare a **read-only proof-query**
the server runs on kill; if the query confirms the work landed, the killed step
is **reconciled to success** instead of failing the pipeline.

This is a *recovery for a genuine kill*, distinct from the spurious-cancel fix
(#230/#232): there, no step was actually killed and the workflow-success
invariant reconciles it with no probe at all. The two compose — when
verification flips the only killed step to success, the workflow succeeds.

## The probe is a proof query, never a re-run

The server issues a **single HTTP `GET`** from the Woodpecker server itself.
It does **not** re-run the step and does **not** run arbitrary commands. The
only question it answers is "did the work land?" — e.g. "does the deploy
target's `/version` report the commit we just shipped?"

Side-effecting verification is an anti-pattern. If you find yourself wanting the
probe to *do* something, it does not belong in `verify`.

## Two forms

### `kind: deploy` shorthand (recommended for deploys)

Matches the CLAUDE.md deploy `/version` smoke contract.

```yaml
steps:
  - name: deploy
    image: bash
    commands:
      - ssh "$TARGET" 'bash -s' < deploy.sh
    kind: deploy
    verify_url: https://${TARGET}/version
    verify_expect_commit: ${CI_COMMIT_SHA}
```

On kill the server `GET`s `verify_url`, expects HTTP 200, parses the JSON body,
and compares its `commit` field to `verify_expect_commit` by 7-char prefix. A
match reconciles the step to success.

### Explicit `verify:` block

```yaml
steps:
  - name: deploy
    image: bash
    commands: [ ./deploy.sh ]
    verify:
      when_killed: true                  # the only trigger currently supported
      url: https://${TARGET}/version
      expect_commit: ${CI_COMMIT_SHA}    # optional
      expect_status: 200                 # optional, default 200
```

If `expect_commit` is omitted, a matching status code alone is sufficient proof.

`${...}` variables in `verify_url` / `verify_expect_commit` / `url` /
`expect_commit` are substituted from the pipeline environment at compile time,
exactly like `commands`.

## When to use it

| Use it for | Don't use it for |
|---|---|
| Deploys with a `/version`-style endpoint that reports the running commit | Test / lint / build steps (no canonical "did it pass" URL — reconciling a killed test to success would be a false green) |
| rsync / systemd deploys whose target exposes an HTTP health/version probe | Steps whose success can't be proven by a read-only HTTP GET |
| Any step where "the work landed" is independently observable over HTTP | Anything that would require *doing* work to check |

## Validation

The YAML schema enforces:

- `kind` may only be `deploy`.
- `kind: deploy` requires `verify_url`.
- The `kind: deploy` / `verify_url` shorthand and an explicit `verify:` block
  are mutually exclusive.
- A `verify:` block must set `when_killed: true` and a `url`.
- Probe URLs must be `http://` or `https://` (the server issues the GET
  directly, so the URL must be reachable from the Woodpecker server).

A misconfigured verify fails safe: the step simply isn't verified and stays
killed, exactly as before this feature.

## Out of scope (follow-up)

Arbitrary-command probes (`ssh`, `gcloud`, `sha256sum` on the target) that can
only run on a fresh agent are **not** implemented here — that is Option B in
#235, tracked as a separate follow-up issue. This feature (Option A) is the
server-side HTTP proof-query only.
