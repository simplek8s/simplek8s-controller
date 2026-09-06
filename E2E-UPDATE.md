# E2E-UPDATE campaign (PLAN-M2 M6)

Live validation of the distro-update feature on the real test cluster
(cp1/wk1/wk2, x86-64). Order: cheap → disruptive. Conventions from
E2E.md apply (`$API`, `$AUTH`, events in namespace `default`).

## Prerequisites

- **Mock release server**: a small HTTP server on one of the cluster
  nodes serving a `dev/`-style layout:
  `SHA256SUMS`, `SHA256SUMS.gpg`, `simplek8s.<ts>.<arch>.kernel.zst`.
  The access log doubles as the "did the controller talk to the repo?"
  evidence (U0).
- **Test keyring**: a generated keypair signs the mock `SHA256SUMS`; the
  public keyring is embedded in a **variant controller image** at the
  same fixed path (`/etc/simplek8s/pubring.gpg`) — the D10/D11 variant
  pattern from E2E.md.
- **Mock "new version"**: for staging tests the `.kernel.zst` content
  can be dummy files — staging verifies the container's sha256 + GPG,
  not the kernel's bootability. The filename's `ts` must be newer than
  the running one.
- **Real new release** (U5 only): the maintainer publishes a dev release
  newer than the running `ts` on the test cluster.
- **ConfigMap**: `updates.url` pointed at the mock; `updates.update-mode`
  set per case; `updates.check-interval` shortened (e.g. `30s`) for the
  campaign and restored afterwards.
- **Reboots regression subset**: after PLAN-M2 milestone M1 (flags →
  ConfigMap), re-run E2E cases A1, B1, C1 before starting this campaign
  (proves the config migration caused no regression).

## Progress

| Case | Result | Date | Notes |
|---|---|---|---|
|    |        |      |       |

## U0 — mode `off` is inert

| # | Case | Trigger | Expect |
|---|---|---|---|
| U0 | no update activity | `updates.update-mode: "off"`, mock reachable with a newer version | no HTTP traffic to the mock (access log empty), no annotations created, no update events, engine + reboot API healthy |

## U1 — staging, `stage` mode (mock)

| # | Case | Trigger | Expect |
|---|---|---|---|
| U1 | full staging happy path | `updates.update-mode: stage`; mock has a signed newer version | every node: `UpdateAvailable` then `UpdateStaged`; `/boot/simplek8s/` contains the new version + bootloader entry; `next-kernel := V` on all nodes; bootloader default points at V; **nodes keep running the old version** (no reboots); purge respected: running version NOT deleted, old versions pruned per `preserve` |
| U1b | idempotent re-check | wait for the next check interval | no re-download (V already in `/boot`), no annotation change, no plan — "V in /boot → nothing" rule |

## U2 — GPG rejection

| # | Case | Trigger | Expect |
|---|---|---|---|
| U2 | bad signature | mock `SHA256SUMS.gpg` signed with an unknown/wrong key | no staging, no annotation change, `UpdateCheckError` event (rate-limited), nodes untouched, next check retries |

## U3 — sha256 rejection

| # | Case | Trigger | Expect |
|---|---|---|---|
| U3 | corrupted payload | valid signature, but the served `.kernel.zst` does not match the index hash | download rejected, **no partial file** left in `/boot/simplek8s/`, `UpdateCheckError` event, next check retries |

## U4 — per-node URL override

| # | Case | Trigger | Expect |
|---|---|---|---|
| U4 | `update-url` annotation | one node annotated with a second mock URL serving a *different* version | only that node stages its own version (different V); the other nodes stage the cluster `updates.url` version; both `next-kernel` values correct per node |

## U5 — `full` mode, real release (the big one)

| # | Case | Trigger | Expect |
|---|---|---|---|
| U5 | full cluster update | `updates.update-mode: full`; maintainer publishes a new dev release | all nodes stage → `UpdatePlanStarted` → reboots serialize per `max-concurrent-reboots` (M1 queue: cordon/drain/issue/verify) → each node returns on the new version (`running == next-kernel`, quiescent) → plan marker cleared → cluster fully on the new version; no `failed` |

## U6 — plan failure → all-or-nothing cancel

| # | Case | Trigger | Expect |
|---|---|---|---|
| U6 | drain timeout mid-plan | `full` mode with a new release; make one node's drain fail (stuck pod / PDB `maxUnavailable:0`) | that node `failed` (M1) → `UpdatePlanCanceled`; in-flight reboots (if any) complete; **uniform reset**: updated nodes keep the new version (`next-kernel == running == V`, no-op); cancelled nodes reset to the old version (`next-kernel := running`, bootloader default back to the old one, old kernel still in `/boot`); marker cleared; next check does **not** auto-retry (V in `/boot` → nothing) |

## U7 — operator re-launch

| # | Case | Trigger | Expect |
|---|---|---|---|
| U7 | re-anchor + M1 reboot | after U6: set `next-kernel := V` on the cancelled nodes, then reboot them via the M1 API | bootloader followed the annotation before the reboot; nodes come back on V; `running == next-kernel` → quiescent; no update events (the updater did nothing — operator-driven) |

## U8 — rollback

| # | Case | Trigger | Expect |
|---|---|---|---|
| U8 | operator rollback | on an updated node (e.g. after U5): `next-kernel :=` the older preserved version, reboot via the M1 API | node comes back on the old version; `next-kernel == running` → quiescent; the check does **not** re-stage or re-update automatically (newer V is in `/boot` → nothing); the operator decides the next move |
