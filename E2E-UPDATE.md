# E2E-UPDATE campaign (PLAN-M2 M6)

Live validation of the distro-update feature on the real test cluster
(cp1/wk1/wk2, x86-64). Order: cheap → disruptive. Conventions from
E2E.md apply (`$API`, `$AUTH`, events in namespace `default`).

## Prerequisites

- **Mock release server**: a small HTTP server on one of the cluster
   nodes serving a `dev/`-style layout:
   `SHA256SUMS`, `SHA256SUMS.gpg`, `simplek8s.<ts>.<arch>.efi.zst`.
  The access log doubles as the "did the controller talk to the repo?"
  evidence (U0).
- **Test keyring**: a generated keypair signs the mock `SHA256SUMS`; the
  public keyring is embedded in a **variant controller image** at the
  same fixed path (`/etc/simplek8s/pubring.gpg`) — the D10/D11 variant
  pattern from E2E.md.
- **Mock "new version"**: for staging tests the `.efi.zst` content can
  be dummy files — staging verifies the container's sha256 + GPG, not
  the kernel's bootability. The filename's `ts` must be newer than the
  running one.
- **Boot layout**: kernels live in the `simplek8s/` dir at the root of
  the (temporarily mounted) boot partition; `/boot/simplek8s/` in the
  cases below is shorthand for that dir. See PLAN-M2.md §3.7
  (ground-truth layout).
- **Real new release** (U5 only): the maintainer publishes a dev release
  newer than the running `ts` on the test cluster.
- **ConfigMap**: `updates.url` pointed at the mock; `updates.update-mode`
  set per case; `updates.check-interval` shortened (e.g. `30s`) for the
  campaign and restored afterwards.
- **Reboots regression subset**: after PLAN-M2 milestone M1 (flags →
  ConfigMap), re-run E2E cases A1, B1, C1 before starting this campaign
  (proves the config migration caused no regression). M1 also rewrites
  the flag references in E2E.md (C4/D15, D10, D14) and the README flags
  section to ConfigMap keys.

## Progress

| Case | Result | Date | Notes |
|---|---|---|---|
| U1 (stage mode) | PASS | 2026-09-07 | Defensive re-stage on all 3 nodes (cp1/wk1/wk2); syslinux entries written; bootloader default at the new version; nodes stayed on the old kernel (no reboots). Found + fixed here: staging was a silent no-op until the boot mount root was created (`6684e07`). |
| U5 (full mode, real release) | PASS | 2026-09-07 | Real dev release `6.18.49-simplek8s-202609061935`; all 3 nodes staged → plan started → reboots serialized workers-first, CP-last (wk1→wk2→cp1) → each returned on the new kernel (`running == next-kernel`) → plan settled, cluster quiescent; no `failed`. |
| U9 (full mode, 3-CP quorum) | PASS (found BUG 12) | 2026-09-08 | Re-ran U5 on the HA 3-CP cluster (cp1/2/3 + wk1/2). Phase A manual downgrade then Phase B `full` re-update: all 5 back on the latest, **API never dropped** (2/3 etcd quorum through each single-CP reboot; leader handover `xb4bh`→`p4vrz`). First Phase-B attempt hit **BUG 12** (stale `reboot-state` → `verifyPlan` false-cancel); unblocked by clearing stale M1 annotations + re-staging fresh. See §U9 + TODO item 12. |

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
| U5 | full cluster update | `updates.update-mode: full`; maintainer publishes a new dev release | all nodes stage → `UpdatePlanStarted` → reboots serialize per `max-concurrent-reboots` (M1 queue: cordon/drain/issue/verify) → each node returns on the new version (`running == next-kernel`, quiescent) → plan-state ConfigMap entry cleared → cluster fully on the new version; no `failed` |

## U6 — plan failure → all-or-nothing cancel

| # | Case | Trigger | Expect |
|---|---|---|---|
| U6 | drain timeout mid-plan | `full` mode with a new release; make one node's drain fail (stuck pod / PDB `maxUnavailable:0`) | that node `failed` (M1) → `UpdatePlanCanceled`; in-flight reboots (if any) complete; **two-phase reset**: non-in-flight members reset immediately to `next-kernel := running`; in-flight members settle when their M1 state lands — the failing node comes back on the old kernel (mismatch) → reset, any completed+verified node keeps V (`next-kernel == running == V`, no-op); bootloader defaults back to the old version where reset applied; the plan-state ConfigMap entry is cleared only when **all** members have settled; next check does **not** auto-retry (V in `/boot` → nothing) |

## U7 — operator re-launch

| # | Case | Trigger | Expect |
|---|---|---|---|
| U7 | re-anchor + M1 reboot | after U6: set `next-kernel := V` on the cancelled nodes, then reboot them via the M1 API | bootloader followed the annotation before the reboot; nodes come back on V; `running == next-kernel` → quiescent; no update events (the updater did nothing — operator-driven) |

## U8 — rollback

| # | Case | Trigger | Expect |
|---|---|---|---|
| U8 | operator rollback | on an updated node (e.g. after U5): `next-kernel :=` the older preserved version, reboot via the M1 API | node comes back on the old version; `next-kernel == running` → quiescent; the check does **not** re-stage or re-update automatically (newer V is in `/boot` → nothing); the operator decides the next move |

## U9 — `full` mode on an HA 3-CP cluster (quorum) + BUG 12

Re-validates U5 on the **HA** cluster (cp1/cp2/cp3 + wk1/wk2). The new
aspect: the update reboot plan must bring all 3 control planes back (one at
a time, `max-concurrent-reboots: 1`) **without the API losing quorum**.

### Phase A — manual downgrade
`next-kernel` + the boot-partition `DEFAULT` flipped to the previous kernel
(`202608291203`) on all 5, then a full M1 reboot (workers→CP, serialized).
All 5 came back quiescent on the old kernel (`running == next-kernel`); the
API stayed up; then the new `.efi` was removed from `/boot` so the
auto-updater would have to re-download it.

### Phase B — auto re-update
`updates.update-mode: full`. The auto-updater re-downloaded, re-staged
(`DEFAULT`→new), anchored (eligible), and the leader drove the plan through
all 5 reboots (workers-first, CP-last, serialized). **The API never
dropped**: etcd kept 3 members — each CP's etcd/apiserver restarted only on
its own single-node reboot and 2/3 quorum always held — and the leader
handed over (`xb4bh`→`p4vrz`) cleanly when cp1 rebooted. Final state: all 5
quiescent on the new kernel, plan ConfigMap empty (`plans: '{}'`), no
`eligible` left.

### BUG 12 (found here)
The **first** Phase-B attempt failed. The `reboot-state=completed` left by
Phase A's manual reboot poisoned `verifyPlan` (`update/plan.go:154`), which
cancels a plan in the very cycle it is created when a member reads
`completed` + `running != plan-version` (`managePlan` verifies at
`plan.go:116` before it enqueues at `plan.go:127`). The plan was cancelled,
members reset to `running`, and the eligible triggers cleared — and because
the new kernel was now local in `/boot`, it was not re-anchored (not a
"fresh stage", `update/update.go:224`), leaving the cluster
staged-but-never-rebooted. Unblocked by clearing the stale M1 annotations
(`reboot-state`/`reboot-exec`/`reboot-request`) and removing the new kernel
from `/boot` to force a fresh re-stage. Documented in TODO.md item 12; also
affects successive auto-updates (not only manual-then-auto).
