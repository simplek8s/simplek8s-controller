# TODO — deferred backlog

Cross-cutting items deliberately left out of the current plans
(PLAN-M1 shipped, PLAN-M2 in planning). Each item is ready to be picked
up as its own plan; nothing here is blocking.

## 1. Reboot maintenance windows (`reboots.windows`) — future PLAN-M3

Optional time windows that gate **only the start** of reboots **and of the
update-driven reboot plan** (item 11). Semantics already agreed with the
maintainer:

- **Window forms** (each window is one of): a **time-of-day range**
  `[start, end)` (may span midnight, e.g. `22:00-06:00`); a **periodic
  interval** (e.g. `30m` — the intended default for the update plan); a
  **cron-style** expression; or a **shortcut** (`hourly`/`daily`/`weekly`/
  `monthly`). A window repeats on its cadence (daily/weekly to study).
- **Gate start only**: a reboot already in flight (draining/rebooting)
  is **never** interrupted when a window closes.
- **Queue waits, never fails**: nodes `requested` outside a window stay
  queued (`requested`) until a window opens; no timeout, no error.
- **Multiple windows**: a list; the node may start if any window is
  open.
- **UTC always** (no timezone key, same rule as PLAN-M2 §2).
- **To study**: whether a window needs a maximum-duration guard (a
  misconfigured tiny window silently holding the queue for days).

Config: a `reboots.windows` key in the controller **ConfigMap** (flat-key
rule from PLAN-M2 §3.2) — a compact list mixing forms (e.g. `"30m"`,
`"hourly"`, `"Mo-Fr 22:00-06:00"`, a cron string); absent = no
restriction (today's behavior).

## 2. `simplek8s-update` CLI

Successor of the legacy `simplek8s-update` project:

- Reuses `internal/features/update` from this repo (k8s-agnostic core,
  PLAN-M2 §3.16) — check, GPG/sha verification, staging, purge,
  bootloader writers.
- Owns the power-user knobs the controller deliberately lacks: `--url`,
  `--keyring`, `--checksign` (skip verification, explicitly).
- Updates a single node without a cluster (or a fleet via a loop),
  e.g. for pre-cluster installs and for out-of-band maintenance.
- Once it exists, the keyring can leave the distro (item 5).

## 3. Pod split (security hardening)

Split the single privileged DaemonSet into:

- an **unprivileged controller pod** (engine, API, orchestration), and
- a **privileged host daemon** (nsenter reboot, boot partition mount,
  bootloader writes),

communicating via an annotation protocol (the controller requests a host
action in a node annotation, the daemon executes and reports back).
This removes most privileged capabilities from the long-running
orchestration code path. The current single privileged DS is the MVP;
the split is a future hardening milestone, not a correctness issue.

## 4. Rollback automation/ergonomics

Manual rollback is already one annotation edit
(`next-kernel :=` the older preserved version, PLAN-M2 §3.5) — no
feature to build. Remaining:

- document it in the operations guide (shipped with PLAN-M2 M5);
- (optional, later) a one-shot fleet-wide rollback helper (API or CLI)
  that re-anchors a set of nodes to a given version and reboots them via
  the M1 API.

## 5. Keyring leaves the distro

Today the SimpleK8s signing keyring lives in the distro
(`/usr/lib/systemd/import-pubring.gpg`) for the legacy
`simplek8s-update` tool. Once the CLI (item 2) exists, the keyring is
carried by the controller image (LFS, PLAN-M2 §3.14) and the CLI
defaulting to it — the distro file can be removed. Distro-side change,
tracked here for visibility.

## 6. Syslinux stale-entry cleanup (bootloader)

Each update's staging writes a new syslinux `MENU LABEL`/`KERNEL` entry
but never prunes the old ones, so `syslinux.conf` in the (vfat) boot
partition grows unbounded across many updates. Harmless today (the default
and the fallback keep booting), but should be bounded: remove syslinux
entries for kernels already deleted by the purge step. Distro/bootloader
concern — the controller's syslinux writer currently only adds the new
entry and moves the default.

## 7. Reboot orchestration success observability

The reboot orchestrator logs the failure path (`Error`) but emits no
`Info` line on the happy path: a successful cordon → drain → issue →
verify sequence is invisible in the controller log (it only shows up in
the node annotations and the `RebootCompleted` event). Add `Info`-level
log lines for the completed transitions so "it worked" is observable
without inspecting each node's annotations.

## 8. `next-kernel` is the source of truth — validate & safe-state recovery

The `next-kernel` annotation is the node's boot **goal** (the kernel it
should run after its next reboot). The controller should treat it as the
source of truth: validate that the goal is reachable, and when it is not,
return the annotation to a **safe state** rather than leaving it stuck.

Current behavior:

- File missing but **downloadable** → `maybeStage` target (1)
  (`update/update.go:229`) defensively re-stages it (re-download); the
  goal is achieved.
- File missing and **unachievable** (no longer in the repo, checksum
  mismatch, or a malformed ts) → `stageOne` fails → `UpdateStagingSkipped`
  event → the annotation is **left unchanged** and retried every check
  cycle (the node points forever at a kernel it can never boot).
- `bootstrap` (`update/update.go:149`) already implements the safe
  fallback (running → newest local → leave absent), but **only when the
  annotation is absent** — not when it is present-but-unachievable.

Desired: generalize that fallback to the present-but-unachievable case.
When `next-kernel`'s file is missing and cannot be (re)staged, correct the
annotation to: (a) the running kernel if its file exists, else (b) the
newest version present on the boot partition, else (c) delete the
annotation. Same for a malformed value.

## 9. Valid `next-kernel` change → re-point the bootloader + mark reboot-eligible

When `next-kernel` changes to a **valid** value whose kernel file already
exists on the boot partition, the controller should (a) re-point the
bootloader `DEFAULT` at it, and (b) mark the node **reboot-eligible** for
the next plan (item 11). Today neither happens for an operator-driven
change:

- The bootloader `DEFAULT` is re-pointed **only** as a side-effect of
  staging a not-yet-present kernel (`update/staging.go:183`, the sole
  caller of `SetBootloaderDefault`). Pointing `next-kernel` at an
  already-present kernel (e.g. a manual downgrade) leaves the `DEFAULT`
  stale — the node would boot the old default and ignore its
  `next-kernel`. (Had to be done by hand, editing `syslinux.cfg`, in the
  2026-09-07 HA downgrade.)
- The reboot-eligible trigger is set **only** by `anchor` on a fresh
  full-mode stage (`update/update.go:240,283`); an operator-set
  `next-kernel` sets no trigger, so it is never planned.

Making a validated `next-kernel` change drive both is what makes manual
rollback and operator-pinned kernels actually honored without touching the
boot partition by hand.

## 10. Leader-centralized update check + distribution

Today the release check, download and staging are **per-node and
autonomous**: every pod's `RunLocal` (`update/update.go:111`, throttled to
`updates.check-interval`) independently fetches the index, downloads the
artifact, extracts and stages it. The leader only runs the plan logic
(`update/plan.go`).

Desired: the **leader** performs the check once; when an update exists the
leader downloads it and **distributes it to the nodes** (channel + form to
study — node-pull via annotation/API, an in-cluster push, or object
storage) so each deploys it onto its own boot partition. A node, once it
has validated its local copy, sets `next-kernel`; once validated, it marks
itself reboot-eligible (item 11). Study the trade-offs: a single download
at the leader vs N; the leader as a check-time SPOF; transfer reliability
and resumability; and how this composes with the per-node `preserve`/purge
and the pod split (item 3).

## 11. Leader schedules the update reboot plan on a window

The leader, once it finds nodes **eligible** for a reboot (item 9), should
**wait for the next available window** (item 1; default a 30 m recurring
period) and, when the window opens, create the reboot plan with the
eligible nodes.

Current behavior: `maybeStartPlan` (`update/plan.go:75`) starts the plan
**immediately** on the same leader cycle (the 2 s engine interval) as soon
as eligible nodes exist and no plan is active — there is no window gating
and no delay. This adds the wait-for-window step before a plan is created,
reusing the `reboots.windows` mechanism (item 1).

## 12. BUG: stale `reboot-state` poisons a fresh update plan (`verifyPlan` false cancel)

**Reproduced 2026-09-08 in the 3-CP auto-update test.** A prior reboot —
a manual M1 reboot or a previous auto-update — leaves `reboot-state=completed`
on each node. When the auto-updater then stages a newer kernel and the leader
starts a plan, `verifyPlan` (`update/plan.go:154`) reads each member's
`reboot-state` **before** the plan's own enqueue overwrites it: `managePlan`
verifies (`plan.go:116`) before it admits/enqueues (`plan.go:127`). Seeing
`completed` + `running != plan-version` it concludes "member came up on the
wrong kernel" and **cancels the plan in the same cycle it was created**. The
members are then reset to `running` (`resetMembers`/`resetToRunning`,
`plan.go:200,230`) and their eligible triggers cleared (`clearSettled`) — so
the update never applies. Worse: the kernel is now **local on the boot
partition**, so on the next cycle `maybeStage` does not re-anchor it (it is
not a "fresh stage",
`update/update.go:224`) and the eligible trigger is never re-set → the cluster
is left **staged-but-never-rebooted** (deadlock until an operator intervenes).

This is not limited to manual-then-auto: it also breaks **successive
auto-updates** (update N's `completed` reboot-state poisons update N+1's
plan).

**Fix (implemented 2026-09-08):** clear the stale terminal state when a plan
starts. In `maybeStartPlan` (`update/plan.go`), after the plan is persisted the
leader runs `resetStaleRebootState`: for each member it conditionally clears a
present, uncorrupt terminal `reboot-state` (`completed`/`failed`) back to the
resting state — `nodestate.ClearStaleRebootStateBuild` only touches a terminal
state, so an absent/queued/in-flight node is never clobbered — guarded by a
`VerifyOwnership` check (consistent with `admitMember`/`clearSettled`). The
in-memory view is updated on success so the **same-cycle** `managePlan` verify
(which reads the snapshot, not a re-read) sees the cleared state and does not
cancel. Chosen over scoping verify to the plan's own reboots (option 1) because
that leaves a misleading `completed` visible to operators and does not address
the in-memory same-cycle read.

Status: implemented + unit/integration tested
(`TestPlanStaleCompletedAtStartDoesNotCancel`,
`TestPlanStaleFailedAtStartDoesNotCancel`, `TestClearStaleRebootStateBuild`).
Pending E2E re-run on the 3-CP test VMs (successive auto-update) to confirm the
deadlock is gone end-to-end.

(Workaround previously used in the test: clear the stale
`reboot-state`/`reboot-exec`/`reboot-request` annotations and remove the new
kernel from the boot partition to force a fresh re-stage.)
