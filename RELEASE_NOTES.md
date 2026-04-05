# Release Notes

## Unreleased

- Add plugin architecture core framework: EventHook, StatusHook, DispatchHook interfaces with registry and channel-based event bus (#642)
- Add pipeline lifecycle event emission at all lifecycle points: created, pending, started, completed, failed, killed, step.completed (#643)
- Add GCP Pub/Sub plugin: publishes pipeline events to ci-events topic with sidecar-compatible schema_version "1.0" format (#644)
- Add Status API plugin: REST endpoints for external agents to report workflow init/update/done with bearer token auth (#645)
- Add External Dispatch plugin: intercepts queue task assignment, publishes task.available events for external agent dispatch via WebSocket (#646)
- Add multi-stage Dockerfile: Node frontend build + Go server build with plugins, scratch runtime (#650)
