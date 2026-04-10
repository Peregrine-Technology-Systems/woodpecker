# Release Notes

## Unreleased

- Feat: Full WebSocket agent transport — replaces gRPC for agent↔server communication. All 11 RPC methods (Next, Wait, Init, Done, Update, Extend, Log, RegisterAgent, UnregisterAgent, ReportHealth, Version) over single bidirectional WebSocket at /ws/agent. Fixes deploy workflows killed by gRPC disconnect (#3496, #3497, #3360, #857). Feature flag: WOODPECKER_AGENT_TRANSPORT=ws (default grpc for upstream compat) (ci-infrastructure#474)
- Fix: shared RPC peer between gRPC and WS transports — duplicate prometheus metric registration caused panic on startup
- Fix: WS agent registration creates agent via store directly — RPC.RegisterAgent requires pre-existing agent_id from gRPC auth flow
- Fix: gRPC Wait() no longer cancels running workflows on connection errors — retries instead of returning error that triggers cancelWorkflowCtx (backend#3496, #3497)
- Fix: WS server write mutex — gorilla/websocket concurrent write protection, prevents connection drops after agent registration
- Fix: WS client reconnection — close pending channels on disconnect so Next() unblocks and reconnects
- Fix: WS agent registration sets OrgID/OwnerID to IDNotSet — agents could not match tasks (org-id filter mismatch)
- Fix: WS handler uses connection-scoped context — gin request context canceled queue.Poll goroutines
- Fix: TaskTimeout increased to 15min to match WOODPECKER_TIMEOUT — 5min was too short, deploy workflows killed mid-flight when gRPC Extend calls failed through Caddy (backend#3360)
- Fix: findIndependentWorkflows uses persistent DependsOn field on Workflow model instead of transient TaskList — running deploy workflows were invisible after agent pickup and got killed on cancel (ci-infrastructure#853)
- Fix: workflow independence — Cancel preserves independent workflows (depends_on: []) when superseded by new push, preventing deploy kills on healthy agents (ci-infrastructure#822)
- Fix: TaskTimeout increased to 5 minutes — safety net only, WebSocket hub handles orphan detection at 20s (ci-infrastructure#162)
- Fix: deploy hold window — spot agents wait 30s for on-demand agents to boot before accepting deploy jobs
- Feat: CI + auto-deploy pipeline — build on merge to main, push to AR, deploy to d3ci42 (ci-infrastructure#648)
- Revert: TaskTimeout back to 60s — 15s caused task expiry under CPU load — orphaned tasks requeue 4x faster after agent death (ci-infrastructure#774)
- Feat: auto-route deploy workflows to on-demand agents — score-based tier preference (on-demand +20, n2 +15, spot default) with configurable patterns via WOODPECKER_DEPLOY_PATTERNS (ci-infrastructure#798)
- Fix: merge agent CustomLabels into filter — tier label was never checked during assignment
- Fix: canceled/skipped pipelines update GitHub status to failure instead of stuck pending (#159)
- Add plugin architecture core framework: EventHook, StatusHook, DispatchHook interfaces with registry and channel-based event bus (#642)
- Add pipeline lifecycle event emission at all lifecycle points: created, pending, started, completed, failed, killed, step.completed (#643)
- Add GCP Pub/Sub plugin: publishes pipeline events to ci-events topic with sidecar-compatible schema_version "1.0" format (#644)
- Add Status API plugin: REST endpoints for external agents to report workflow init/update/done with bearer token auth (#645)
- Add External Dispatch plugin: intercepts queue task assignment, publishes task.available events for external agent dispatch via WebSocket (#646)
- Add multi-stage Dockerfile: Node frontend build + Go server build with plugins, Alpine runtime with static-linked CGO for SQLite support (#650)
- Add CI pipeline: pts-ci (test + lint on feature branches), pts-build (Docker build + push to Artifact Registry on main) (#651)
- Build agent binary from fork source alongside server — both at gRPC version 15 (#658)
- Fix gcppubsub publish "context canceled" — use background context for async Pub/Sub delivery (#667)
- Fix queue API 500 on missing agent — use placeholder name instead of erroring, prevents cascade cancellation of healthy pipelines (ci-infrastructure#678)

