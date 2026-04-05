# Release Notes

## Unreleased

- Add plugin architecture core framework: EventHook, StatusHook, DispatchHook interfaces with registry and channel-based event bus (#642)
- Add pipeline lifecycle event emission at all lifecycle points: created, pending, started, completed, failed, killed, step.completed (#643)
- Add GCP Pub/Sub plugin: publishes pipeline events to ci-events topic with sidecar-compatible schema_version "1.0" format (#644)
- Add Status API plugin: REST endpoints for external agents to report workflow init/update/done with bearer token auth (#645)
- Add External Dispatch plugin: intercepts queue task assignment, publishes task.available events for external agent dispatch via WebSocket (#646)
- Add multi-stage Dockerfile: Node frontend build + Go server build with plugins, Alpine runtime with static-linked CGO for SQLite support (#650)
- Add CI pipeline: pts-ci (test + lint on feature branches), pts-build (Docker build + push to Artifact Registry on main) (#651)
- Build agent binary from fork source alongside server — both at gRPC version 15 (#658)
- Fix gcppubsub publish "context canceled" — use background context for async Pub/Sub delivery (#667)
