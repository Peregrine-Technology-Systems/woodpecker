# Release Notes

## Unreleased

- Add plugin architecture core framework: EventHook, StatusHook, DispatchHook interfaces with registry and channel-based event bus (#642)
- Add pipeline lifecycle event emission at all lifecycle points: created, pending, started, completed, failed, killed, step.completed (#643)
- Add GCP Pub/Sub plugin: publishes pipeline events to ci-events topic with sidecar-compatible schema_version "1.0" format (#644)
