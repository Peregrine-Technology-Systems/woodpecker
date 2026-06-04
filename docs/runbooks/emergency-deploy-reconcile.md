# Runbook: out-of-band / emergency server deploy → reconcile

**Scope.** What to do after the woodpecker **server** (or the co-located
`d3ci42-local` agent) on `d3ci42.peregrinetechsys.net` is rolled **out-of-band** —
by an operator, outside the normal `pts-build` → GCS `pending`-marker →
`woodpecker-deploy.timer` flow. Examples: a hand-copied binary, a manual
`systemctl restart` onto a swapped binary, or a forced version swap during an
incident.

This is the **fork-side** companion to infra's
[`docs/runbooks/emergency-release-terraform-reconcile.md`](https://github.com/Peregrine-Technology-Systems/peregrine-infrastructure/blob/main/docs/runbooks/emergency-release-terraform-reconcile.md)
(out-of-band *terraform* applies). Read both: the terraform runbook covers the
GCP fleet / scaler side; this one covers the server binary + the `d3ci42-local`
agent.

> **Why this runbook exists.** During the 2026-05-29 CI jam
> (peregrine-infrastructure#2955/#2957) the server was rolled to `pts.434`/`pts.435`
> out-of-band. It recovered the server but left two **silent** reconcile gaps:
> `d3ci42-local` stayed two versions behind (the local-agent refresh only runs on
> a *normal* deploy), and the version pin / deploy artifacts could lag source.
> Neither was visible until later. The point of reconciling is to make the
> out-of-band state reproducible-from-source again — the falcon "repeatability
> over speed" discipline.

---

## Ownership map (who owns what)

| Artifact | Owner | Where |
|---|---|---|
| Server + agent **binary** | **this fork** | built by `.woodpecker/pts-build*.yaml` + `scripts/woodpecker/pts-build.sh` |
| Deploy **artifacts** (`${DEPLOY_BUCKET}/${VERSION}/woodpecker-{server,agent}` + `.sha256`) and the GCS `pending` marker | **this fork** | written by `pts-build.sh` (`DEPLOY_BUCKET=${BUILD_CACHE_BUCKET}/woodpecker-deploy`) |
| `woodpecker-deploy.sh` / `.timer`, the `/opt/woodpecker/` layout, the `deployed-version` pin, the local-agent refresh, `woodpecker-agent-pin.{service,timer}` | **peregrine-infrastructure** | `d3ci42`, deployed via `deploy-woodpecker-server.sh` |
| GCP agent fleet roll (`apply-agent-image`, Packer image) | **peregrine-infrastructure** | scaler + MIGs |

**Do not SSH-edit `/opt/woodpecker/` to "fix" a reconcile gap** — that is exactly
the out-of-band move that created the gap. File against peregrine-infrastructure
(it owns that tree) or re-run the normal deploy path. See the cross-repo
discipline in the global CLAUDE.md.

---

## Step 0 — log it (always, first)

Capture in the incident channel / issue **before** touching anything else:

- **What** version is now running, **when**, **who** rolled it, **why**.
- The **exact commands** used (so the next person can reproduce or unwind them).

```bash
# Running server version (the X-Woodpecker-Version header is authoritative):
curl -s -D - http://localhost:8000/version | grep -i x-woodpecker-version
# or externally:  curl -s https://d3ci42.peregrinetechsys.net/version
```

## Step 1 — make the deploy artifacts match the running version

The next *normal* deploy and the local-agent refresh reconcile against the GCS
deploy bucket + the on-box version pin. If the emergency roll skipped
`pts-build.sh`, those inputs can be stale.

```bash
RUNNING=$(curl -s -D - http://localhost:8000/version | sed -n 's/.*[Xx]-[Ww]oodpecker-[Vv]ersion: *//p' | tr -d '\r')
DEPLOY_BUCKET="${BUILD_CACHE_BUCKET}/woodpecker-deploy"   # = the value pts-build.sh uses

# (a) version pin on the box must equal the running version:
cat /opt/woodpecker/server/deployed-version          # expect == $RUNNING
# (b) artifacts for $RUNNING must exist in the bucket:
gsutil stat "${DEPLOY_BUCKET}/${RUNNING}/woodpecker-server"        || echo "MISSING server artifact"
gsutil stat "${DEPLOY_BUCKET}/${RUNNING}/woodpecker-server.sha256" || echo "MISSING server sha"
gsutil stat "${DEPLOY_BUCKET}/${RUNNING}/woodpecker-agent"         || echo "MISSING agent artifact"
gsutil stat "${DEPLOY_BUCKET}/${RUNNING}/woodpecker-agent.sha256"  || echo "MISSING agent sha"
```

- **Pin mismatch** (`deployed-version` ≠ running): the box was rolled without
  updating the pin → fix via the **infra** deploy path (re-run a deploy of
  `$RUNNING`, see Step 3), not by hand-editing the file.
- **Missing artifacts**: re-run `pts-build` for the running commit so it
  rebuilds + uploads them (`pts-build.sh` does the `gsutil cp … + pending`).
  Don't hand-`cp` a binary you can't reproduce from source.

## Step 2 — reconcile `d3ci42-local` (the co-located agent)

`d3ci42-local` is the persistent local-backend agent on the box. After an
out-of-band server bump it can lag the server version (silent drift).

```bash
# Compare the local agent's version to the server (look for the d3ci42-local row):
WP_TOKEN=$(gcloud secrets versions access latest --secret=ci-api-token --project=ci-runners-de)
curl -s https://d3ci42.peregrinetechsys.net/api/agents -H "Authorization: Bearer $WP_TOKEN" \
  | jq -r '.[] | select(.name=="d3ci42-local") | "\(.name)\t\(.version)\t\(.last_contact)"'
```

**Current reality (2026-06):** lag is mostly self-correcting now —
1. the GCP fleet **boot-pulls the latest agent binary** at startup (so spot/ondemand
   agents are never stale), and
2. `d3ci42-local` self-heals a wedged registration via the stale-conf removal
   path (woodpecker#77/#101): if its stored `agent.conf` AgentID goes invalid it
   removes the conf and re-registers fresh.

The **durable** fix #251 asked for — a Rule-11 systemd timer that pins
`d3ci42-local` to the server version *independently of any pipeline* — was built
as `woodpecker-agent-pin.{service,timer}` on the box. ⚠️ **As of 2026-06-04 that
timer is failing `203/EXEC` — its `ExecStart` script
`/opt/woodpecker/woodpecker-agent-pin.sh` is absent**, so the pin never actually
runs (tracked: peregrine-infrastructure issue for the missing script). Until that
lands, treat local-agent reconcile as **manual-verify** per the `jq` check above.

If it does lag and you need it pinned now, prefer staging a no-op `pending` for
the running version so the normal `woodpecker-deploy.sh` runs its local-agent
refresh — **noting that re-deploying restarts the prod server** (Step 3).

## Step 3 — prove the automated deploy path still works

An out-of-band roll can leave the *normal* path silently broken. Confirm it:

```bash
# woodpecker-deploy.timer is active and its last run was clean:
ssh root@d3ci42  systemctl status woodpecker-deploy.timer  --no-pager
ssh root@d3ci42  journalctl -u woodpecker-deploy.service -n 50 --no-pager
```

- A no-op `pending` (or the next real `pts-build`) should drive
  `woodpecker-deploy.sh` to completion. **Re-deploying restarts the prod
  server** — do it deliberately, not as a reflex.
- Confirm the **infra** fleet-roll path (`apply-agent-image`) isn't a
  phantom-success: infra#2960 found an agent-image roll reporting green while the
  fleet never moved. Verify the fleet actually advanced (agent versions via the
  `/api/agents` check above), don't trust the pipeline's green alone — the
  Act→Verify rule.

## Step 4 — kill incident suppressors before declaring recovery

Anything spawned to hold the system during the incident — a paused queue, a
forced-state loop (`while true; … resize 0`), a muted alert, a circuit breaker
pinned open — **must be killed before you can answer "is the normal path really
working?"** A green check against a suppressed system only proves the suppression
works.

```bash
# Audit for loops/suppressors started during the incident window:
ssh root@d3ci42  'ps -ef | grep -E "while|sleep [0-9]+|resize 0" | grep -v grep'
```

Write down every suppressor at incident-start (PID, command, stop condition);
kill every one at fix-deploy. See the global CLAUDE.md "Kill emergency watchers
when the structural fix lands" rule.

---

## Acceptance checklist

- [ ] Step 0 logged (version / when / who / why / exact commands).
- [ ] `deployed-version` pin == running `X-Woodpecker-Version`.
- [ ] `${DEPLOY_BUCKET}/${VERSION}/woodpecker-{server,agent}{,.sha256}` all present.
- [ ] `d3ci42-local` version verified against the server (`/api/agents`).
- [ ] `woodpecker-deploy.timer` confirmed healthy on a no-op or next real deploy.
- [ ] Fleet roll verified by actual agent versions, not pipeline-green alone.
- [ ] All incident suppressors killed.

## References

- peregrine-infrastructure#2955 / #2957 — the 2026-05-29 incident.
- peregrine-infrastructure#2960 + PR #2977 — infra reconcile runbook + the
  phantom-success fix.
- peregrine-infrastructure#2945 — the local-agent refresh in `woodpecker-deploy.sh`.
- woodpecker#77 / #101 — `d3ci42-local` stale-conf self-heal.
- woodpecker#251 — this runbook.
