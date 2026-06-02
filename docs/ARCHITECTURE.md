# Woodpecker CI Server — Architecture (Peregrine Fork)

This document describes the Peregrine-specific architecture decisions and operational contracts for the woodpecker-server fork. Upstream Woodpecker documentation covers the generic CI semantics; this file covers only what differs or is specific to our deployment.

## d3ci42 Operating Environment

### Port Map

| Port | Listener | Purpose |
|------|----------|---------|
| 80 | Caddy | HTTP → HTTPS redirect |
| 443 | Caddy | TLS termination — routes to 8000/9000 by content type |
| 8000 | woodpecker-server | HTTP API, web UI, WebSocket agent transport (`/ws/agent`) |
| 9000 | woodpecker-server | gRPC agent transport (direct, Caddy proxies `application/grpc*`) |
| 9001 | woodpecker-server | Prometheus metrics endpoint (scraped by monitoring droplet) |
| 8081 | scaler | Prometheus metrics endpoint (scraped via `metrics.d3ci42.peregrinetechsys.net/scaler/`) |

Caddy routes (`/etc/caddy/Caddyfile`):
1. `Content-Type: application/grpc*` → h2c `127.0.0.1:9000` (gRPC agents)
2. `/ws/agent*` → `127.0.0.1:8000` with `flush_interval -1` and `keepalive off` (WS agents, long-lived, no buffering)
3. Everything else → `127.0.0.1:8000` with `lb_try_duration 30s` (web UI, retried during restarts)

### Service Units

| Service | Binary | EnvironmentFile(s) | Restart |
|---------|--------|-------------------|---------|
| `woodpecker-server.service` | `/opt/woodpecker/server/current/woodpecker-server` | `/etc/woodpecker/server.env`, `/etc/woodpecker/secrets.env` | `on-failure` |
| `woodpecker-agent.service` | `/opt/woodpecker/woodpecker-agent` | `/etc/woodpecker/agent.env`, `/etc/woodpecker/secrets.env` | `on-failure` |
| `scaler.service` | `/opt/woodpecker/scaler/current/scaler` | `/etc/woodpecker/secrets.env` | `on-failure` |
| `caddy.service` | `/usr/bin/caddy` | — | `no` (managed by OS package) |

WorkingDirectory for all Woodpecker services: `/var/lib/woodpecker`  
WorkingDirectory for scaler: `/opt/woodpecker`

Restart ordering: `woodpecker-agent.service` declares `Wants=woodpecker-server.service` (not `Requires=` — agent does not cascade-stop the server).

### Config Files

**`/etc/woodpecker/server.env`** (non-secret, mode 644):
```
WOODPECKER_HOST=https://d3ci42.peregrinetechsys.net
WOODPECKER_OPEN=false
WOODPECKER_ADMIN=amalc
WOODPECKER_GITHUB=true
WOODPECKER_DATABASE_DRIVER=sqlite3
WOODPECKER_DATABASE_DATASOURCE=file:/var/lib/woodpecker/woodpecker.sqlite?...
WOODPECKER_METRICS_SERVER_ADDR=:9001
WOODPECKER_TIMEOUT=15m
WOODPECKER_WEBHOOK_ENDPOINT=http://webhook-sidecar:8080/webhook
WOODPECKER_PLUGINS=gcppubsub
WOODPECKER_PLUGIN_GCPPUBSUB_PROJECT=ci-runners-de
WOODPECKER_PLUGIN_GCPPUBSUB_TOPIC=ci-events
GOOGLE_APPLICATION_CREDENTIALS=/etc/gcp/server-sa-key.json
WOODPECKER_MAX_WORKFLOWS=2
```

**`/etc/woodpecker/agent.env`** (non-secret, mode 644):
```
WOODPECKER_SERVER=localhost:9000   # gRPC direct — bypasses Caddy
WOODPECKER_BACKEND=local
WOODPECKER_MAX_WORKFLOWS=2
WOODPECKER_AGENT_LABELS=backend=local   # NEVER add platform=linux — causes d3ci42 to steal GCP fleet tasks
WOODPECKER_HOSTNAME=d3ci42-local
CLOUDSDK_CONFIG=/root/.config/gcloud
```

**`/etc/woodpecker/secrets.env`** (GCP SM–sourced, mode 600) — key names:
```
WOODPECKER_GITHUB_CLIENT, WOODPECKER_GITHUB_SECRET   # OAuth app
WOODPECKER_AGENT_SECRET                              # shared gRPC auth
WOODPECKER_JWT_SECRET                                # agent JWT signing (rotated on deploy #92)
WOODPECKER_ENCRYPTION_KEY                            # pipeline secret encryption
WOODPECKER_API_TOKEN                                 # external API clients (scaler, Grafana)
SLACK_WEBHOOK_URL
HEALTHCHECKS_PING_URL
GRAFANA_ADMIN_PASSWORD
GRAFANA_LIVE_TOKEN
GITHUB_TOKEN
```

**`/etc/gcp/server-sa-key.json`**: `ci-monitoring@ci-runners-de.iam.gserviceaccount.com` — roles: Compute Viewer (for GCP metrics), Pub/Sub Publisher (ci-events topic)  
**`/etc/gcp/scaler-sa-key.json`**: scaler SA — roles: Compute Admin (MIG resize), GCS read/write (state bucket)

**`/opt/woodpecker/scaler.yaml`**: source-of-truth scaler config. Deployed via `scripts/woodpecker/deploy.sh` scp on every main push.

### cron (d3ci42 crontab, `/opt/woodpecker/crontab`)

| Schedule | Script | Purpose |
|----------|--------|---------|
| `*/2 * * * *` | `healthcheck.sh` | 5-check deep health (healthz, queue API, orphan tasks, orphan VMs, ghost pipelines) |
| `*/2 * * * *` | `mig-watchdog.sh` | Force MIG→0 when queue empty + no agents + VMs running |
| `*/5 * * * *` | `watchdog-monitoring.sh` | Power-cycle monitoring droplet if Grafana unreachable |
| `*/5 * * * *` | `wp-cron-ping.sh` | WordPress wp-cron ping for scheduled posts |
| `*/10 * * * *` | `ttl-reaper-droplets.sh` ×4 | Reap TTL-expired DO droplets (identity, backend, orchestrator, sight) |
| `3,8,…,58 * * * *` | `stale-flock-reaper.sh` | Release kernel-leaked FLOCK holds from killed pipelines |
| `17 * * * *` | `snapshot-cron.sh` | Hourly d3ci42 droplet snapshot (content-hash-guarded) |
| `42 * * * *` | `zombie-sweeper.sh` | Cancel pipelines stuck at status=created/pending (fork#31 residue) |
| `30 */2 * * *` | `sync-posts-cron.sh` | Drift-sync blog posts via peregrine-publisher REST |
| `0 2 * * *` | `backup.sh` | Daily SQLite backup |
| `30 2 * * *` | `purge-logs.sh` | Daily log_entries purge (keeps SQLite < 1 GB) |
| `0 3 * * *` | `droplet-sweeper.sh` | Daily journal vacuum, apt clean, tmp sweep |
| `5 0 * * *` | `cron-wrapper.sh daily-cost-report.sh` | Daily GCP cost report → Slack |
| `0 7 * * *` | `cron-wrapper.sh audit-repos.sh` | Daily repo audit |
| `0 6 * * *` | `peregrine-websites-daily-check.sh` | Daily staging+production smoke check |
| `15 6 * * *` | `auxscan-security-probe.sh` | Daily auxscan-nginx security probe |
| `0 */6 * * *` | `cron-wrapper.sh ct-monitor.sh` | Certificate transparency monitor |
| `0 6 * * 1` | `ci-agent-image-reaper.sh` | Reap old ci-agent/ci-agent-base images (keep 3/2) |
| `0 5 * * 1` | `image-reaper.sh` | Reap old GCE worker-class images |
| `0 8 * * 1` | `cron-wrapper.sh dependency-drift-check.sh` | Weekly dep drift |
| `0 8 * * 1` | `cron-wrapper.sh packer-audit.sh` | Weekly Packer audit |
| `30 4 * * 0` | `vm-init/install-fleet.sh` | Weekly vm-init.sh fleet pass |
| `30 4 * * 0` | `cron-wrapper.sh audit-secret-allowlists.sh` | Weekly Woodpecker secret allowlist audit |
| `0 3 * * 0` | `backup-secrets.sh` | Weekly secrets backup to GCP SM |
| `30 3 * * *` | `rotate-worker-publisher-keys.sh` | Daily SA key rotation check (90-day lifecycle) |
| `0 1 * * 1` | `cron-wrapper.sh audit-export.sh` | Weekly SOC 2 audit export |
| `0 4 * * 1` | `cron-wrapper.sh image-scan.sh` | Weekly image vulnerability scan |
| `0 10 1 * *` | `cron-wrapper.sh monthly-compliance-report.sh` | Monthly compliance report |
| `0 9 1 1,4,7,10 *` | `cron-wrapper.sh access-audit.sh` | Quarterly access audit |

**`cron-wrapper.sh`** fetches secrets from GCP SM into env vars, checks out the ci-infrastructure repo into a temp dir, and execs the named script. Used for all repo-level audits that need a git checkout but don't justify a full CI agent warm-up (Global CLAUDE.md rule #11).

**`healthcheck.sh` five checks:**
1. `/healthz` reachability (HTTP 204)
2. Queue API returns 200 (catches 500 from orphaned agent registrations)
3. Orphaned task detection — queue says "running" but no agents connected for 5+ min → `systemctl restart woodpecker-server` + Slack
4. Orphaned VM detection — queue empty but MIG target > 0 for 5+ min → force resize to 0 + Slack
5. Ghost pipeline detection — pipelines stuck "running" with `started_at=null` for 10+ min → auto-cancel + Slack

Healthcheck pings `HEALTHCHECKS_PING_URL` on success (dead-man's-switch alert if cron stops).

---

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

### Agent label taxonomy — three orthogonal dimensions (#255/#261)

Pipelines route to agents via labels: every non-empty task label must be present on an agent (`server/rpc/filter.go`), matched by **exact string** (or `*`). Three dimensions, each with exactly one job:

| Label | Owns | Values |
|---|---|---|
| `platform` | OS / arch | `linux/amd64`, … (heading toward Linux-variant granularity) |
| `backend` | execution **environment** (free-form) | `local`, `docker`; flavors (`local-stripped`); **host pins** via `local-<host>` |
| `tier` | scaling/scheduling class **only** | `spot`, `ondemand`, `n2`, `integration-test` |

**Host pinning.** Post peregrine-infrastructure#1357 the whole fleet honestly advertises `backend: local` (Docker-less native execution), so `backend: local` no longer isolates the co-located server box. Pin a specific host on the **`backend`** axis via the `local-<host>` convention: the box advertises `backend: local-d3ci42`. Exact-match means generic `backend: local` work can't land on the box, and `backend: local-d3ci42` targets *only* it. A host is **not** a `tier` — `tier: local` is rejected.

**The validator (`pipeline.ValidateLabelCombination`, [pts] #255/#261)** runs on every workflow in `server/pipeline.Create` before enqueue and governs **only `tier`**: it must be a known scaling class (`pipeline.KnownTiers` — keep in lockstep with the scaler). `backend` and `platform` are free-form constraints and are never gated — `backend` is deliberately extensible (engine → flavor → host pin). `backend: local + tier: spot` ("native step on a spot VM") is the common case, not an error. The only rejection is an unknown/dead `tier` value (notably `tier: local`, which conflated host-identity with scaling class). It converts a silent forever-pending stall into a loud submit-time error (2026-05-31: 14 pending / 0 running on d3ci42 from an unsatisfiable combo).

### Submit-time tier auto-routing (#266)

`tier` is also **rewritten** at submit, in the same `server/pipeline.Create` path, immediately **before** validation (`rewritePipelineTier` → `pipeline.ShouldForceOndemand`). Deploy-class workflows are forced to `tier: ondemand` so no repo needs a per-pipeline `tier:` knob — and none can drift onto spot and lose a release pipeline to a mid-flight preemption (the killed-workflow re-queue, `server/rpc/rpc.go`, deliberately will **not** restart a workflow that already did observable work, so a spot preemption *during* a promote/deploy is fatal).

A workflow is deploy-class when:
- the pipeline is **tag-triggered** (`event: tag` — production releases are never recoverable on preemption), or
- its **name contains a deploy pattern** (case-insensitive substring, matching the scaler's `IsDeployPipeline`).

The pattern list is `WOODPECKER_TIER_DEPLOY_PATTERNS` (comma-separated), defaulting to **`deploy,promote,version-bump`** (`pipeline.DefaultDeployPatterns`). `sync-back` is deliberately excluded — idempotent RELEASE_NOTES housekeeping stays in the spot class. The rewrite **always wins** over an explicit label (a `tier: spot` on a `promote` workflow is overridden — the design goal is that a repo cannot route a release pipeline to spot, even by mistake).

This is the precise, central mechanism the spot-default guardrail policy (scaler#1175, global CLAUDE.md tier table) always assumed: ondemand for *exactly* the deploy class, applied automatically, instead of the per-repo manual `tier: ondemand` knob that drifted (repos that forgot it died on preemption) or over-applied (blanket ondemand over-provisioned on-demand VMs). The global-rule table that lists `promote`/`version-bump` as spot describes the pre-classifier stopgap; reconciliation tracked in `global-claude`.

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

Workflow 3 — pts-build-cleanup.yaml (d3ci42-local, always runs):
  pts-cleanup.sh:
   13. gcloud instances stop pts-build-vm
  (Slack notify removed #246 — Slack deprecated org-wide. Pipeline terminal
   status is published to the bus by the server's GCP Pub/Sub plugin, #234.)

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

**woodpecker-deploy.sh is owned by peregrine-infrastructure, NOT this fork.**  
The live script is `peregrine-infrastructure/woodpecker-server/woodpecker-deploy.sh`, installed onto d3ci42 by `deploy-woodpecker-server.sh` (which auto-enables the systemd units). The fork does **not** own or deploy it — there is no copy of it in this repo (a stale duplicate that drifted and never ran was removed; see #257/#258 and the infra ownership issue). The fork's only responsibility is to **honor the deploy contract**: pts-build uploads `<version>/woodpecker-server{,.sha256}` to `gs://ci-runners-de-build-cache/woodpecker-deploy/` and writes `pending` = `version\ncommit\npipeline`; the server serves `/healthz` → HTTP 204 with an `X-Woodpecker-Version` header. Contract changes go to infra as an issue.

**woodpecker-deploy.sh operational notes (infra-side, for reference):**  
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

### `ci-events` Pub/Sub contract (#264)

The server publishes pipeline lifecycle events to the GCP Pub/Sub topic `ci-events` via the `gcppubsub` plugin. The **authoritative wire contract** — flat payload, `data.pipeline` as an integer, full event-type list with the `pipeline.success`-not-`.completed` gotcha — lives next to the code at [`server/plugin/gcppubsub/CONTRACT.md`](../server/plugin/gcppubsub/CONTRACT.md). Subscribers (scaler, worker-registry, monitoring) adapt to that document; changing the shape requires bumping `schema_version` and updating CONTRACT.md in the same PR. The publisher fail-closes on an empty event type (`ErrEmptyEventType`), so empty-type messages on the topic originate from a different publisher.

The plugin also exports `woodpecker_pubsub_publish_failures_total` and `woodpecker_pubsub_published_total` (#259): the bus is the Slack replacement for CI status, so a *sustained* publish failure silently starves consumers while every pipeline goes green. The publish path is best-effort/async (a failure never affects the pipeline) and was logged-only; the counters make it alert-able — `rate(woodpecker_pubsub_publish_failures_total[15m]) > 0`, with the published total as the ratio denominator.

### Pipeline status rollup invariant (#270)

`MergeStatusValues` (`server/pipeline/status.go`) rolls workflow states up to the pipeline status by priority, with `skipped` deliberately the **lowest** priority. The `partial` status (a workflow where some steps succeeded and some were killed — woodpecker-server#28) must absorb any lighter sibling: a `partial` workflow means real work happened, and a `skipped` sibling can never override it. The load-bearing case is `[ci:skipped, deploy:partial, promote:skipped]` (a deploy that mutated production while its CI/promote siblings were skipped) — it must roll up to `partial`, never `skipped`. A top-level `skipped` is reserved for "nothing ran," so reporting it over a successful deploy step is a silent-OK that reads as "nothing happened" on the dashboard.

### Orphaned-workflow observability gauges (#243/#245/#248)

The dispatcher samples `woodpecker_orphaned_workflows{state=...}` every ~100ms tick (alert on sustained `> 0`). States:

| State | Meaning | Drives a reclaim? |
|---|---|---|
| `running_dead_owner` | running task whose owner agent was POSITIVELY observed disconnected (WS reconnect grace expired) | yes — reclaimed within a tick (#243/#246) |
| `pending_dispatchable` | task in `q.pending`, `ShouldRun()` true, aged past `orphanAgeThreshold` (no matching worker, or a dispatch stall) | no — observe-only |
| `running_owner_stale` (#248) | running task whose owner's `LastContact` aged past `AgentStaleThreshold`. **Transport-agnostic** (both gRPC and WS stamp `LastContact`), so it makes the gRPC/local "stranded running task" manifestation visible — the WS-only `running_dead_owner` path never saw it | **no — observe-only.** Measure-first: a reclaim on mere `LastContact` aging would re-introduce the t+0s kill #246 fixed |
| `waiting_on_deps_aged` (#245) | task parked in `q.waitingOnDeps` aged past `orphanAgeThreshold` | no — observe-only |

`running_owner_stale` + `waiting_on_deps_aged` are the measure-first instrumentation for #248/#245: they make the next recurrence of the infra#5265 stuck-workflow pattern self-diagnosing (which bucket was the orphan in?) before any transport-agnostic reclaim is built. The stale oracle is injected observe-only via `Queue.SetAgentStaleFn` (mirrors `SetAgentReclaimFn`) and fails safe — an unknown agent, store error, or `LastContact==0` never reads as stale.

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

Woodpecker signs agent sessions with a JWT secret (`WOODPECKER_JWT_SECRET`) stored in `secrets.env` and GCP SM (`woodpecker--jwt-secret`).

**Current state (post #92, 2026-05-08):** `woodpecker-deploy.sh` rotates `WOODPECKER_JWT_SECRET` on every deploy:
1. Generates a new 32-byte hex secret via `openssl rand -hex 32`
2. Writes to `/etc/woodpecker/secrets.env` and pushes to GCP SM `woodpecker--jwt-secret`
3. Restarts server; waits for `/healthz` to confirm new version
4. Calls `/api/user/token` with the (still-valid) old API token to generate a fresh one
5. Stores new API token in GCP SM `woodpecker-api-token`

After rotation, all agent JWT tokens are invalidated. Agents self-heal via the stale-conf fix (#77): they detect `Unauthenticated` / `AgentID not found` errors, delete `agent.conf`, and re-register fresh within ~5 minutes. External clients (scaler, Grafana) fetch the new token from GCP SM on their next cycle.

**Note:** The API token is signed with `user.Hash` (stored in SQLite), NOT with `WOODPECKER_JWT_SECRET`. Rotating the JWT secret does not invalidate existing API tokens — step 4 refreshes the token as a best-practice rotation, not a necessity.

---

## Reference Incidents

| Date | Symptom | Root cause | Fix |
|---|---|---|---|
| 2026-05-08 ~18:55 UTC | CI UNHEALTHY + QUEUE UNHEALTHY, agent disconnections, pts-build failures | Nil panic in scaler's `CancelOrphanedRunning` — `p["repo"].(map[string]interface{})` panicked on null repo field; crash loop every ~5s for 25 min | Safe type assertion via ok-idiom (peregrine-ci-scaler#941) |
| 2026-05-05 ~19:08 UTC | All agents disconnected, Grafana red, teams reported WP down | `WOODPECKER_JWT_SECRET` not set — random key generated on each startup invalidates all sessions | Agents self-healed via #77. Permanent fix: automated JWT rotation in woodpecker-deploy.sh (#92) |
| 2026-05-05 | `database table is locked` under burst webhook load | Write queue (#88) serializes goroutines but xorm pool had 100 SQLite connections — file-level lock still raced | `MaxOpenConns=1` for SQLite (#88) |
| 2026-05-04 ~20:17 UTC | Server freeze, Slack up/down cascade | `database table is locked` under concurrent writes | Write queue (#55) |
| 2026-05-04 | pts-build deploy failed (`docker daemon not running`) | Docker masked on d3ci42 post #1403 | Native rsync deploy (#57) |
| 2026-05-04 | pts-build agent disconnect mid Docker build | Long Docker image build > agent keepalive | Native go build (#57) — shorter, retryable steps |
