# Woodpecker CI Server — Architecture (Peregrine Fork)

This document describes the Peregrine-specific architecture decisions and operational contracts for the woodpecker-server fork. Upstream Woodpecker documentation covers the generic CI semantics; this file covers only what differs or is specific to our deployment.

## Deployment

### Runtime (post peregrine-infrastructure#1403)

The woodpecker-server runs as a **native systemd service** on d3ci42 — no Docker.

```
/opt/woodpecker/server/
  releases/
    v3.13.0-pts.92/woodpecker-server   ← versioned binary
    v3.13.0-pts.93/woodpecker-server
    v3.13.0-pts.write-queue/woodpecker-server
  current → releases/v3.13.0-pts.write-queue  ← symlink, updated on each deploy
```

Systemd unit: `/etc/systemd/system/woodpecker-server.service`  
Environment: `/etc/woodpecker/secrets.env` (loaded by EnvironmentFile=)  
Health endpoint: `http://localhost:8000/healthz` (returns 204 when healthy)

### Local Agent (`woodpecker-agent.service`)

`pts-build.yaml` uses `backend: local` — steps run directly on d3ci42 as subprocesses rather than on a GCP agent VM. This requires a local `woodpecker-agent` process registered to the server.

```
/etc/systemd/system/woodpecker-agent.service
/etc/woodpecker/agent.env        (non-secret config — mode 600)
/opt/woodpecker/woodpecker-agent-<VERSION>  (binary, same release as server)
```

Key agent config (`agent.env`):
```
WOODPECKER_SERVER=localhost:9000        # gRPC direct — bypasses Caddy
WOODPECKER_BACKEND=local
WOODPECKER_MAX_WORKFLOWS=2
WOODPECKER_AGENT_LABELS=backend=local  # only picks up backend:local pipelines
WOODPECKER_HOSTNAME=d3ci42-local
```

`WOODPECKER_AGENT_SECRET` is injected from `/etc/woodpecker/secrets.env` via a separate `EnvironmentFile=` line. The service declares `Requires=woodpecker-server.service` so it starts after and restarts with the server.

**Incident 2026-05-05**: the infra#1403 migration created `woodpecker-server.service` but not `woodpecker-agent.service`. `backend: local` pipelines (pts-build) queued at `status: created` forever — the queue showed 0 workers with matching labels even with 4 VMs and 8 GCP agents registered (those agents have no `backend=local` label). Manually created the service; tracked for codification in peregrine-infrastructure#1465.

**Operator note**: after a woodpecker binary update, `ExecStart=` in `woodpecker-agent.service` must be updated to point to the new agent binary. Not yet automated — tracked in #1465.

### Build + Deploy Pipeline (`pts-build.yaml` / `pts-build.sh`)

Triggered on every push to `main`. Native build — no Docker.

```
1. pnpm install --frozen-lockfile && pnpm build   (web UI assets → web/dist/)
2. CGO_ENABLED=1 go build -o bin/woodpecker-server ./cmd/server
3. CGO_ENABLED=0 go build -o bin/woodpecker-agent  ./cmd/agent
4. rsync bin/woodpecker-server → d3ci42:/opt/woodpecker/server/releases/${VERSION}/
5. sha256 checksum verify
6. ln -sfn releases/${VERSION} /opt/woodpecker/server/current
7. systemctl restart woodpecker-server
8. 60s health check; rollback to previous release on failure
9. Keep 3 releases (older pruned automatically)
10. Phase 3: GitHub Release with both binary assets + ci-image-builder wake
```

**GCS build cache** (`gs://ci-runners-de-build-cache`, #851):  
Restored from GCS at step 2 start, saved back after step 3. Go's content-addressable cache skips recompiling unchanged packages — warm cache reduces compile from ~8 min to ~1 min. 30-day lifecycle policy keeps the bucket bounded. `pentest-scanner` and `ci-agent` SAs have `objectAdmin` access.

**Rollback**: symlink to previous release dir + `systemctl restart`. Instant — no download, no image pull.

### `Dockerfile.archived`

The Dockerfile is archived (not deleted) for reference. It is no longer used in any build or deploy path as of PR #57 (2026-05-04). Do not restore it to the active build.

---

## SQLite Write Queue (#55)

The woodpecker-server uses SQLite (`server/store/datastore/`) with WAL mode. Under concurrent write load — pipeline status updates, webhook ingestion, agent registrations — goroutines raced on the SQLite writer lock and produced `SQLITE_BUSY "database table is locked"` errors that froze the server.

**Fix**: all 33 write methods route through a single drain goroutine via a bounded channel (depth 256). Reads bypass the queue (WAL allows concurrent readers). Non-SQLite drivers: queue is nil, direct call.

```
HTTP handlers → s.wq.serialize(fn) → writeQueue.ops channel → drain goroutine → SQLite
                                                                    ↑ one at a time
```

Implementation: `server/store/datastore/write_queue.go`  
Integration test: `TestWriteQueue_ConcurrentWrites_NoLockErrors` — 50 goroutines × 10 writes on file-based SQLite, all succeed, all readable.

**SQLite DSN defaults** (`server/store/datastore/sqlite_dsn.go`):
```
_busy_timeout=30000   (30s wait before SQLITE_BUSY — belt-and-suspenders)
_journal_mode=WAL     (concurrent readers)
_synchronous=NORMAL   (safe, faster than FULL)
_txlock=immediate     (fail-fast on write lock, not on first write attempt)
```

---

## Behavioral Patches (Peregrine-specific)

### Suppress `errored` forge status on agent disconnect (#44)

When an agent disconnect kills a workflow (`state=killed`, `error="agent disconnected"`), the server no longer posts an `errored` check to GitHub. Errored status blocks branch-protection-gated merges even for `gh pr merge --admin`.

Decision point: `model.Workflow.KilledByAgentDisconnect()` — returns true only for `StatusKilled` + error containing `"agent disconnected"`. Real failures (non-zero exit, config errors, explicit kills) still post.

### Stale-created pipeline detection (#30 / fork#31)

Pipelines stuck at `status=created` are detected after a configurable threshold and cancelled. Root cause: `forge.Status()` in `prepareStart()` was synchronous — slow GitHub calls blocked the pipeline before it reached `pending`. Fix: 5s per-call timeout on `forge.Status()` + defensive `force-pending` guard at end of `Create()`.

### `woodpecker_rpc_update_to_terminal_step_total` counter (#31)

Prometheus counter incremented when the RPC layer rejects a step-update because the step is already in a terminal state. Non-zero steady-state rate indicates server–agent state divergence (typically post-restart desync). Dashboard this metric; spike = alarm.

---

## GitHub Release Assets

Every `pts-build` run publishes two binaries to the GitHub Release for that version tag:

| Asset | Source | Use |
|---|---|---|
| `woodpecker-server-linux-amd64` | `bin/woodpecker-server` | d3ci42 native upgrade path |
| `woodpecker-agent-linux-amd64` | `bin/woodpecker-agent` | Packer image build (ci-image-builder) |

Download path for manual upgrade:
```bash
gh release download v3.13.0-pts.NN \
  --repo Peregrine-Technology-Systems/woodpecker \
  --pattern woodpecker-server-linux-amd64 \
  --output /opt/woodpecker/server/releases/v3.13.0-pts.NN/woodpecker-server
chmod +x /opt/woodpecker/server/releases/v3.13.0-pts.NN/woodpecker-server
ln -sfn /opt/woodpecker/server/releases/v3.13.0-pts.NN /opt/woodpecker/server/current
systemctl reload-or-restart woodpecker-server
```

---

## Healthcheck Integration (d3ci42)

`healthcheck.sh` (runs every 2 min via cron on d3ci42) checks woodpecker-server health. Post peregrine-infrastructure#1403 it uses native systemd:

```bash
# Server restart path:
if systemctl is-active --quiet woodpecker-server; then
    systemctl restart woodpecker-server
else
    docker compose restart woodpecker-server   # pre-migration fallback
fi
```

**SQLite BUSY → server freeze → Slack up/down alerts** (incident 2026-05-04):  
Heavy concurrent write load caused the server to freeze. The write queue (#55) eliminates this. If bounce alerts recur, check `woodpecker_rpc_update_to_terminal_step_total` for state divergence and review recent concurrent write patterns.

---

## Reference Incidents

| Date | Symptom | Root cause | Fix |
|---|---|---|---|
| 2026-05-04 ~20:17 UTC | Server freeze, Slack up/down cascade | `database table is locked` under concurrent writes | Write queue (#55) |
| 2026-05-04 | pts-build deploy failed (`docker daemon not running`) | Docker masked on d3ci42 post #1403 | Native rsync deploy (#57) |
| 2026-05-04 | pts-build agent disconnect mid Docker build | Long Docker image build > agent keepalive | Native go build (#57) — shorter, retryable steps |
