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
  pts-build.yaml         compile the fork on an ephemeral pts-build-vm
  pts-ci.yaml            Peregrine-specific CI
  pts-build-compile.yaml build step
  pts-build-cleanup.yaml cleanup step
```

## ⚠️ This repo is PUBLIC — it is a fork of an upstream OSS project

`gh repo view --json visibility` returns **`PUBLIC`**. Everything written here is world-readable: issue and PR bodies, comments, commit messages, release notes, and code comments. Upstream Woodpecker contributors and anyone else can read all of it. This is the one property of this repo most likely to be missed, because every sibling repo in the estate is private and the estate's conventions were written for those.

**Sign comments, issues and PRs WITHOUT the seat chip image.** The global convention's sign-off is `— Claude <model> · <seat> ![<CHIP>](<chip-url>)`, but the chip is explicitly optional (global-claude#650) and its stated reason covers this case directly: the chip "was never validated and is branding, not attribution", and a seat that must not put a Peregrine mark on an outward-facing repo can sign without one. Use:

```
— Claude <model> · woodpecker
```

Reference incident: 2026-09-25, eight artifacts (#364 ×2, #365, #366, #367, #368, #369 ×2) were signed with the full chip before anyone noticed the repo was public. The chip URL also publishes an internal asset-bucket name and the estate's seat taxonomy, so it is a small disclosure on top of being unwanted branding. Stripped retroactively; the sign-off line itself was kept.

**Keep internal detail out of anything written here.** Hostnames, GCP project and service-account names already appear in committed files (`scripts/woodpecker/pts-*.sh`, `.woodpecker/*.yaml`, this file), so those are already public and repeating them costs nothing new. What must NOT be added: object-store paths for exported production data, absolute paths on anyone's workstation, credential or secret names, and anything about another seat's internals that they have not published themselves. When a cross-repo thread needs that kind of detail, put it in the private sibling repo's issue and reference it by number here.

**Sibling-repo issue references are fine but opaque to outside readers.** `infra#6594` means nothing to an upstream contributor and cannot be followed. Keep a one-line plain-English summary alongside any such reference so a public reader is not left with a dead pointer.

## Standards

- All Peregrine-specific changes must be clearly marked — prefix commits with `[pts]` and reference a `pts-` issue
- Never rebase onto upstream without verifying Peregrine patches survive
- No `backend: local` in any pipeline step (global ban — see global CLAUDE.md)
- Go standards: `go vet`, `gofmt`, `go test ./...`
- Cross-repo: bugs surfacing in `peregrine-ci-scaler` or `ci-infrastructure` that originate here → fix here, file issues in consumers describing the impact
- Do NOT drive-by PR upstream Woodpecker without explicit intent — changes here are fork-local unless deliberately upstreamed
- When upstreaming, open a PR against `woodpecker-ci/woodpecker` from a branch that isolates just the upstream-ready change (no Peregrine-specific context)
