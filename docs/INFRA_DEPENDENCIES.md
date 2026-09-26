# This fork's dependencies on infrastructure it does not own — pointers, not copies

## What this is

A map of the places **this repository** depends on infrastructure it does not own, and
the cheapest way to re-establish each fact when you need it.

## What this deliberately is NOT

**It is not a copy of the other team's facts, and it records no statuses.** Two reasons,
and both have cost this fork real time:

1. **A copy makes the copier responsible for its currency.** If this file restated
   another team's configuration, then whoever reads it is trusting *us* for something
   only *they* can know is still true — and we cannot discharge that. The fact moves;
   the copy does not; the copy is the one being read.
2. **A status goes stale silently.** Two changes in this repository were reverted after
   being built on a fact that was true when it was relayed and false when it was used.
   A third was planned around a step that could never happen. In each case the fault was
   not carelessness — it was that a remembered fact and a checked fact feel identical.

So: this file records **structure and method**. Structure changes rarely. Method does not
go stale. Where a current value is needed, the command to obtain it is here instead of the
value.

The rule is borrowed, with credit, from the infrastructure team's own cutover document:
**restate structure, point at state, never copy a status.**

## ⚠️ This file is deliberately thin, because this repository is PUBLIC

This is a fork of an upstream open-source project, so everything here is world-readable.
Specific identity names, role definitions, permission sets, secret names, the internal
reasoning behind another team's security decisions, and anything about their unapplied or
in-flight work are **deliberately omitted** — not unknown.

**Do not read the omissions as gaps to be filled.** A future session that "helpfully" adds
the missing specifics would be publishing another team's internals on the open internet.
If you need them, the pointers below say whose they are; go and read the source.

## Where this repository touches infrastructure it does not own

These are our own lines, so they belong here. Each one is a place where a change in
somebody else's project can break a build in ours.

| our code | depends on | tracked by |
|---|---|---|
| `scripts/woodpecker/pts-wake.sh` — the build-target switch | which project the ephemeral build VM is created in, an image family in that project, a network and per-region subnets, and an identity permitted to create instances there | `#353` |
| `scripts/woodpecker/pts-wake.sh` — the token mint | a mint script placed on the CI host by **the other team's deploy**, not by ours. Our copy of the repo does not contain it and cannot | `#353` |
| `scripts/woodpecker/pts-build.sh` — the mint-script fetch | an object-store prefix holding three scripts, in whichever project is current | `#345` |
| `scripts/woodpecker/pts-build.sh` — the image-builder wake | a long-lived VM in the legacy project, and the job-file bucket beside it | untracked; the quietest of the four, because it degrades to a warning |
| `scripts/woodpecker/pts-test.sh`, `pts-lint.sh`, `pts-build.sh` — toolchain resolution | the Go toolchain being **discoverable on `PATH`** rather than at a fixed install path | `#369`, fixed |

**The pattern across all five:** every one broke, or nearly broke, because an *install path
or project name* was written down here instead of being **resolved at run time**. That is
the single most useful generalisation in this file. Prefer discovery over a literal, and
when a literal is unavoidable, make its absence fail loudly rather than fall back.

## The questions this seat needs answered, and who owns each

Deliberately the questions, not the answers.

| question | owner | where the answer lives |
|---|---|---|
| Is the legacy project still current for builds, and what is the runway before it goes? | infrastructure | their decommission tracker, and rulings recorded on it |
| Which image should an ephemeral build VM boot from? | infrastructure | their fleet terraform, and the image list in the live project |
| Which identity may create instances, and how does a caller become it? | infrastructure | their terraform for that identity, plus the mint script their deploy ships |
| Is the object-store prefix we fetch mint scripts from current *and* fresh? | infrastructure | the prefix itself; freshness lags their applies |
| Can this seat be reached by another seat unprompted? | infrastructure / courier | our own courier health; see the caveat below |

**A caveat learned the hard way.** Asking "does identity X have permission Y?" can return a
true *no* while the real answer to "is there a path for this to work?" is *yes, already
built*. That happened here: a correct answer about the wrong identity read as a blocker for
most of a day. **Ask about the capability, not about the identity you assumed would carry it.**

## How to re-establish a fact rather than recall one

No values, just the shape of the check. All of these are read-only.

- **Live project state** (identities, roles and bindings, images, image families, networks
  and subnets): the infrastructure repo publishes a token-mint script with a read-only
  capability for exactly this. Mint, then query the relevant Google API. **Pin the client
  identity explicitly when minting** — an unpinned mint on a workstation holding several
  seat credentials can authenticate as a different seat and succeed, which is worse than
  failing.
- **Our own CI server's view** (pipelines, steps, step logs): the same repo publishes a
  wrapper that mints an API token for it. Reading a failing step's log is almost always
  faster than inferring the cause from a status.
- **Their intent, as opposed to their state**: read their repository. It is checked out
  alongside this one. Reading another team's source is explicitly permitted — what is
  protected is their *queue*, not their knowledge — and it is the only thing that reliably
  distinguishes "they have not done it" from "they did it and I asked the wrong question".
- **Host state** (what is actually installed on the CI host, which identity is ambient
  there): requires shell access to the host, which this seat does not hold by default.
  Ask; do not infer it from their deploy script's intent.

### Two traps in the checks themselves

Both of these produced a wrong conclusion in this repository, so they are worth naming:

- **`$?` after a pipeline reports the last command, not the one you care about.** Capture
  the status of the command under test, not of the `tail` you piped it into.
- **A listing that returns nothing does not prove you were authorised.** At least one
  Google CLI listing returns *empty with exit 0* on an invalid credential. Use a
  discriminating query — one whose result differs between the identity you intend and the
  one you might accidentally be — and check that it *differs*, rather than checking that
  the command succeeded.

## What this seat cannot verify, by construction

Recorded so that nobody mistakes an untested thing for a tested one:

- **That the ephemeral build VM can actually be created.** Creating one is a write and
  belongs in a pipeline. Every step leading up to it can be checked; the create cannot.
- **Anything about the build VM's own environment.** It is ephemeral and built from an image
  we do not own. Only a real build exercises it — which is why a change to the build path
  should be landed inert and proven by a deliberate run, never by a merge.
- **That the CI host is in the state its deploy intends.** Their deploy script says what it
  places there; only the host says what is there.

## Related

- `docs/ARCHITECTURE.md` — this fork's own operating contracts.
- `CLAUDE.md` — the public-repository rules that constrain what may be written here.
