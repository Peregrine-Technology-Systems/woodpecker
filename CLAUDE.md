# woodpecker (fork)

Peregrine fork of the Woodpecker CI/CD engine. Go 1.25 multi-package project: agent, CLI, server, web UI, RPC. Active fork — upstream patches cherry-picked selectively; Peregrine-specific fixes tracked with `pts-` prefix in branch names and issue references.

## Fork relationship

- Upstream: `woodpecker-ci/woodpecker`
- This fork: `Peregrine-Technology-Systems/woodpecker`
- Deployed to: `d3ci42.peregrinetechsys.net` (Woodpecker server) + GCP agent VMs
- Active Peregrine issues tracked in this repo: `#27` (unknown /api paths return 200+HTML), `#39` (zombie defense), `#74` (pts-build wake pattern), `#77` (local agent stale agent.conf)

## Structure

```
agent/       Woodpecker agent (connects to server, runs pipelines)
cli/         `woodpecker-cli` command
cmd/         entrypoints (agent, cli, server)
server/      pipeline server + API + scheduler
web/         React/TypeScript web UI
rpc/         gRPC + protobuf definitions
pipeline/    pipeline execution engine
shared/      common types, config, utils

.woodpecker/
  pts-build.yaml         compile the fork on pentest-dev-vm
  pts-ci.yaml            Peregrine-specific CI
  pts-build-compile.yaml build step
  pts-build-cleanup.yaml cleanup step
```

## Standards

- All Peregrine-specific changes must be clearly marked — prefix commits with `[pts]` and reference a `pts-` issue
- Never rebase onto upstream without verifying Peregrine patches survive
- No `backend: local` in any pipeline step (global ban — see global CLAUDE.md)
- Go standards: `go vet`, `gofmt`, `go test ./...`
- Cross-repo: bugs surfacing in `peregrine-ci-scaler` or `ci-infrastructure` that originate here → fix here, file issues in consumers describing the impact
- Do NOT drive-by PR upstream Woodpecker without explicit intent — changes here are fork-local unless deliberately upstreamed
- When upstreaming, open a PR against `woodpecker-ci/woodpecker` from a branch that isolates just the upstream-ready change (no Peregrine-specific context)
