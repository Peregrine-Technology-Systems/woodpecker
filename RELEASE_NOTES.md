# Release Notes

## Unreleased

- Feat: CI + auto-deploy pipeline — build on merge to main, push to AR, deploy to d3ci42 (ci-infrastructure#648)
- Fix: TaskTimeout reduced from 60s to 15s — orphaned tasks requeue 4x faster after agent death (ci-infrastructure#774)
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

