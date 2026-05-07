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
    v3.13.0-pts.182/woodpecker-server
  current → releases/v3.13.0-pts.182  ← symlink, updated on each deploy
```

Systemd unit: `/etc/systemd/system/woodpecker-server.service`  
Environment: `/etc/woodpecker/secrets.env` (loaded by EnvironmentFile=)  
Health endpoint: `http://localhost:8000/healthz` (returns 204 when healthy)

### Local Agent (`woodpecker-agent.service`)

d3ci42 runs a local `woodpecker-agent` process for `backend: local` steps (internal tooling only — NOT pts-build as of #140).

```
/etc/systemd/system/woodpecker-agent.service
/etc/woodpecker/agent.env        (non-secret config — mode 600)
/opt/woodpecker/woodpecker-agent-<VERSION>  (binary, same release as server)
```

Key agent config (`agent.env`):
```
WOODPECKER_SERVER=localhost:9000   # gRPC direct — bypasses Caddy
WOODPECKER_BACKEND=local
WOODPECKER_MAX_WORKFLOWS=2
WOODPECKER_AGENT_LABELS=backend=local  # NEVER add platform=linux — causes d3ci42 to steal GCP fleet tasks
WOODPECKER_HOSTNAME=d3ci42-local
```

`WOODPECKER_AGENT_SECRET` is injected from `/etc/woodpecker/secrets.env` via a separate `EnvironmentFile=` line. The service declares `Wants=woodpecker-server.service` (not `Requires=` — agent should not cascade-stop the server on failure).

**Stale agent.conf self-heal (#77)**: after a woodpecker-server restart that changes the JWT signing key or resets the agents DB, the agent's saved ID (`/etc/woodpecker/agent.conf`) becomes invalid. The agent detects `Unauthenticated` / `AgentID not found` / `sql: no rows` errors and calls `log.Fatal()` — the process exits with status 1, `Restart=on-failure` triggers, systemd restarts with no conf, and the agent re-registers fresh.

### Build + Deploy Pipeline (three workflows, #74/#80/#140)

Triggered on every push to `main`. Three decoupled Woodpecker workflows — wake/compile on pts-build-vm (GCP), cleanup on any GCP agent, deploy via standalone systemd timer on d3ci42.

**Why three workflows / decoupled deploy:** pts-build.sh previously ran `systemctl restart woodpecker-server` inside the pipeline step. This restarted the server mid-pipeline, killing the gRPC connection and marking the pipeline killed before the health check could run (self-kill loop). The deploy now happens entirely outside any Woodpecker pipeline.

```
Workflow 1 — pts-build.yaml (GCP agent, labels: platform:linux, tier:ondemand):
  pts-wake.sh:
    1. gcloud instances start pts-build-vm (project: ci-runners-de)
    2. Set ttl-override-min=45 + ttl-expire-epoch labels (reaper coordination)
    3. Poll /api/agents until pts-build-vm registers (up to 180s)

Workflow 2 — pts-build-compile.yaml (pts-build-vm agent, label: agent=pts-build):
  pts-build.sh:
    4. GCS build cache restore (GOCACHE + GOMODCACHE from gs://ci-runners-de-build-cache/)
    5. pnpm build (web UI)
    6. CGO_ENABLED=1 go build woodpecker-server
    7. CGO_ENABLED=0 go build woodpecker-agent
    8. GCS build cache save
    9. Upload binary + SHA256 to gs://ci-runners-de-build-cache/woodpecker-deploy/${VERSION}/
   10. Write pending-deploy marker to gs://.../woodpecker-deploy/pending
   11. GitHub Release + binary assets (woodpecker-server-linux-amd64, woodpecker-agent-linux-amd64)
   12. Write job to gs://ci-runners-de-image-builder-state/jobs/ + start ci-image-builder

Workflow 3 — pts-build-cleanup.yaml (GCP agent, always runs):
  pts-cleanup.sh:
   13. gcloud instances stop pts-build-vm
  pts-notify.sh:
   14. Slack notify (success/failure of compile workflow)

Separately on d3ci42 — woodpecker-deploy.service (systemd timer, every 30s):
  woodpecker-deploy.sh:
   15. Poll GCS for pending-deploy marker; exit 0 if absent
   16. Download binary, verify SHA256
   17. Stage release: mkdir, cp, chmod
   18. ln -sfn releases/${VERSION} current  (atomic symlink)
   19. systemctl restart woodpecker-server
   20. 90s health check: poll /healthz until version matches or timeout
   21. On failure: rollback symlink + systemctl restart + Slack alert
   22. On success: prune old releases (keep 3), remove pending marker, Slack success
```

**pts-build-vm** (`ci-runners-de`, zone `us-central1-a`):  
Dedicated GCE VM with the `pts-build` image family baked by ci-image-builder. Contains Go + gcc + pnpm toolchain. Stopped when idle; started by pts-wake.sh on each build. woodpecker-agent.service auto-starts on boot with label `agent=pts-build`.

**GCS build cache** (`gs://ci-runners-de-build-cache`):  
Restored at step 4, saved after step 8. No persistent disk on pts-build-vm — cache lives entirely in GCS. Warm cache reduces compile from ~20 min to ~2 min.

**GCS deploy path** (`gs://ci-runners-de-build-cache/woodpecker-deploy/`):  
Binary and SHA256 at `${VERSION}/woodpecker-server{,.sha256}`. Pending marker at `pending` (content: `${VERSION}\n${COMMIT_SHA}\n${PIPELINE_NUM}`). Deleted after successful or failed deploy.

**TTL coordination:** pts-wake.sh sets `ttl-override-min=45` on pts-build-vm. The `ttl-reaper-dev-vm.sh` timer on d3ci42 reads this label and keeps the VM alive for 45 min — enough for boot + cache restore + compile + cleanup.

**woodpecker-deploy.sh operational notes:**  
- Runs as `woodpecker-deploy.service` (oneshot) fired by `woodpecker-deploy.timer` (every 30s)  
- Exclusive flock at `/tmp/woodpecker-deploy.lock` prevents concurrent runs  
- Sources `/etc/woodpecker/secrets.env` for `SLACK_WEBHOOK_URL`  
- Rollback target: `readlink -f /opt/woodpecker/server/current` before symlink swap

---

### pts-build-vm Image Rebuild Runbook

The `pts-build` Packer image bakes in the Go/gcc/pnpm toolchain and a woodpecker-agent binary. **Routine woodpecker server builds do NOT require an image rebuild** — the agent on pts-build-vm just picks up pipeline steps regardless of which server version is being compiled.

Rebuild the pts-build image when:
- `libsqlite3-dev`, `pnpm`, or Go version need updating
- The woodpecker-agent binary has breaking gRPC protocol changes incompatible with the current pts-build-vm agent

**Trigger a rebuild:**
```bash
WP_AGENT_VERSION="v3.13.0-pts.NNN"   # agent version to bake in
REQUEST_ID=$(python3 -c "import uuid; print(str(uuid.uuid4()))")
JOB_JSON="{\"request_id\":\"${REQUEST_ID}\",\"build_type\":\"pts-build\",\"wp_agent_version\":\"${WP_AGENT_VERSION}\",\"triggered_by\":\"manual\"}"

echo "$JOB_JSON" | gsutil -q cp - "gs://ci-runners-de-image-builder-state/jobs/${REQUEST_ID}.json"
gcloud compute instances start ci-image-builder \
  --zone=us-central1-a --project=ci-runners-de --quiet

echo "Job submitted: ${REQUEST_ID}"
```

ci-image-builder's `bootstrap.sh` downloads `woodpecker-agent-linux-amd64` from the fork's GitHub Release for `$WP_AGENT_VERSION`, runs `packer build pts-build.pkr.hcl`, and the new image lands in the `pts-build` GCE family. The builder stops itself on completion.

**Check build progress:**
```bash
# Watch build logs
gsutil ls "gs://ci-runners-de-image-builder-state/logs/" | sort | tail -5
gsutil cat "gs://ci-runners-de-image-builder-state/logs/LATEST_LOG_FILE"

# Confirm new image in pts-build family
gcloud compute images list \
  --project=ci-runners-de \
  --filter="family=pts-build" \
  --sort-by=~creationTimestamp --limit=3 \
  --format="table(name,status,creationTimestamp)"
```

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
systemctl restart woodpecker-server
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

## JWT Secret and API Token Lifecycle

Woodpecker signs all user tokens and agent sessions with a JWT secret (`WOODPECKER_JWT_SECRET`). If this is not set, a random key is generated on each startup — all tokens and agent sessions are immediately invalidated on every restart.

**Current state (2026-05-05):** `WOODPECKER_JWT_SECRET` is NOT set in `secrets.env`. Each restart invalidates all sessions. Agents self-heal via the stale-conf fix (#77) and reconnect within ~5 minutes. External API token (`woodpecker-api-token` in GCP SM) is also invalidated.

**Target state (tracked in #92):** woodpecker-deploy.sh rotates the JWT secret on every deploy:
1. Generates `WOODPECKER_JWT_SECRET` → writes to `secrets.env` + GCP SM
2. Restarts server with new secret
3. After health check: self-signs a new API token with the new key
4. Updates `woodpecker-api-token` in GCP SM

This makes rotation automatic (once per deploy) and eliminates manual intervention after restarts.

**Operator note:** Until #92 lands, any restart will require agents to reconnect (automatic via #77, ~5 min) and may require monitoring tools to wait for the next scrape cycle to clear alerts.

---

## Reference Incidents

| Date | Symptom | Root cause | Fix |
|---|---|---|---|
| 2026-05-05 ~19:08 UTC | All agents disconnected, Grafana red, teams reported WP down | `WOODPECKER_JWT_SECRET` not set — random key generated on each startup invalidates all sessions | Agents self-healed via #77. Permanent fix: automated JWT rotation in woodpecker-deploy.sh (#92) |
| 2026-05-05 | `database table is locked` under burst webhook load | Write queue (#88) serializes goroutines but xorm pool had 100 SQLite connections — file-level lock still raced | `MaxOpenConns=1` for SQLite (#88) |
| 2026-05-04 ~20:17 UTC | Server freeze, Slack up/down cascade | `database table is locked` under concurrent writes | Write queue (#55) |
| 2026-05-04 | pts-build deploy failed (`docker daemon not running`) | Docker masked on d3ci42 post #1403 | Native rsync deploy (#57) |
| 2026-05-04 | pts-build agent disconnect mid Docker build | Long Docker image build > agent keepalive | Native go build (#57) — shorter, retryable steps |
