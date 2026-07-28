# `ci-events` pipeline-event contract (Peregrine fork)

This is the **authoritative** wire contract for the pipeline lifecycle events this
fork's `gcppubsub` publisher emits on the GCP Pub/Sub topic `ci-events`. The fork
is the publisher, so this document — co-located with the code that produces it
(`format.go`, `publisher.go`) — is the source of truth. Downstream subscribers
(`peregrine-ci-scaler`, `peregrine-platform-worker-registry`, monitoring) adapt to
this shape; if you change the shape, bump `schema_version` and update this file in
the same PR.

Reference: woodpecker#264 (scaler subscriber rejected every event because it
expected a different shape that was never what the fork emitted).

## Envelope

```json
{
  "schema_version": "1.0",
  "type": "pipeline.started",
  "source": "woodpecker-server",
  "timestamp": "2026-06-02T00:39:19.123456789Z",
  "data": { ... }
}
```

| field            | type   | notes |
|------------------|--------|-------|
| `schema_version` | string | currently `"1.0"`. The versioning seam — bump on any breaking shape change. |
| `type`           | string | the event type — see the table below. Also mirrored as the `event_type` Pub/Sub message **attribute** (the subscription-filter key). |
| `source`         | string | always `"woodpecker-server"`. |
| `timestamp`      | string | RFC3339Nano, UTC. |
| `data`           | object | **flat** — see below. |

## `data` — FLAT, `pipeline` is an integer

```json
"data": {
  "repo":     "owner/name",
  "pipeline": 42,
  "status":   "running",
  "branch":   "main",
  "commit":   "a1b2c3d4",
  "author":   "amalc",
  "message":  "fix: …",
  "event":    "push",
  "kill_reason": "agent_shutdown"
}
```

| field      | type   | notes |
|------------|--------|-------|
| `repo`     | string | repository **full name** (`owner/name`), not a numeric id. |
| `pipeline` | int64  | the pipeline **number** (the per-repo sequence shown in the UI), not a struct and not the internal pipeline id. |
| `status`   | string | Woodpecker status string (`pending`, `running`, `success`, `failure`, `killed`, …). |
| `branch`   | string | |
| `commit`   | string | short SHA, truncated to 8 chars. |
| `author`   | string | |
| `message`  | string | first line of the commit message, truncated to 80 chars. |
| `event`    | string | forge event (`push`, `pull_request`, `manual`, `deployment`, …). |
| `kill_reason` | string | **optional** — the #202 kill attribution. Present **only on terminal-kill states** (`status` is `killed`/`canceled`/`superseded`); **omitted** otherwise. Values: `agent_disconnect` (hard agent WS drop), `agent_shutdown` (agent self-reported receiving SIGTERM, #275 — **renamed from `agent_preempted` in #339**: SIGTERM alone doesn't confirm a GCP spot preemption, so the value no longer claims one — three ondemand/non-preemptible VMs hit the old label from a plain agent restart, confirmed via GCP audit log showing zero preemption/stop/delete events), `agent_done_kill` (any other agent-reported kill), `user_initiated`, `superseded_by_newer_push`, `pending_only_canceled`, `reconciler_orphan`, `external_status_api`. **Additive, non-breaking** (added without a `schema_version` bump per the breaking-change-only rule); subscribers on `"1.0"` that ignore unknown fields are unaffected, and consumers that want the attribution read it when present. **Consumers matching on the literal string `agent_preempted` must update to `agent_shutdown`.** |

**Do not** expect a nested `pipeline: { id, status, event, branch, repo_id }`
object — that shape has never been emitted by this fork. `status`, `branch`, and
`event` are flat siblings of `pipeline`.

## Message attributes

Alongside the JSON payload, each Pub/Sub message carries:

| attribute    | value |
|--------------|-------|
| `event_type` | same string as the envelope `type` (subscription-filter key). |
| `source`     | `"woodpecker-server"`. |
| `severity`   | per the severity column below; defaults to `info`. |

## Event types

The complete set this fork publishes (internal `EventType` → wire `type`):

| wire `type`           | severity   | when |
|-----------------------|------------|------|
| `pipeline.created`    | info       | pipeline row created |
| `pipeline.pending`    | info       | enqueued / awaiting an agent — the demand-wake trigger |
| `pipeline.started`    | info       | an agent picked it up |
| `pipeline.success`    | info       | completed successfully — **note: `pipeline.success`, NOT `pipeline.completed`** |
| `pipeline.failed`     | critical   | completed with failure |
| `pipeline.killed`     | warning    | killed (cancel / agent disconnect / preemption) |
| `pipeline.superseded` | info       | superseded by a newer pipeline on the same ref |
| `step.completed`      | info       | a single step finished |
| `pipeline.tier_bypass`| warning    | **telemetry, not a lifecycle event** — a `spot`-tier agent claimed a deploy-class task, i.e. the #266 submit-time tier rewrite was bypassed. With #266 working this never fires (deploy-class tasks carry `tier=ondemand` and never match a spot agent). The pipeline still runs; alert on a non-zero rate. The flat payload identifies the pipeline (`repo`/`pipeline`/`branch`/`commit`/`event`); the offending workflow is in the server log line, resolvable via the pipeline number. The event type itself encodes "agent tier was spot." (#268) |

**Gotchas for subscribers:**
- The terminal-success event is **`pipeline.success`**, not `pipeline.completed`.
  The internal constant is `EventPipelineCompleted` but it maps to the wire string
  `pipeline.success` (see `eventTypeMap` in `format.go`).
- There is **no `pipeline.running`** event. If a subscriber waits on one, that's
  its own assumption — not part of this contract.

## Not emitted by this fork

If you observe these on `ci-events`, they come from a **different publisher** on
the shared topic (e.g. the infra deploy script), not from this fork:

- `pipeline.deploy_completed` — not in the `EventType` enum; route to
  `peregrine-infrastructure`.
- Empty `type=` — the publisher **fail-closes** on an empty resolved type
  (`ErrEmptyEventType` in `publisher.go`); a typeless message can never originate
  here. Route empty-type messages to whatever else publishes to `ci-events`.
