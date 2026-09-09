# TODO — deferred backlog

Cross-cutting items deliberately left out of the current plans
(PLAN-M1 shipped, PLAN-M2 implemented, PLAN-M3 in planning). Each item
is ready to be picked up as its own plan; nothing here is blocking.

**Closed by PLAN-M3** (the numbering is kept stable for
cross-references, hence the gaps): items 1 (reboot maintenance
windows), 9 (operator pin → bootloader re-point + `reboot-eligible`)
and 11 (window-scheduled update reboots) are designed and carried by
PLAN-M3.md. Item 12 (BUG 12) is closed with it — see its entry.

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

## 10. Leader-centralized update check + distribution

Today the release check, download and staging are **per-node and
autonomous**: every pod's `RunLocal` (`update/update.go:111`; cadence:
M2's `updates.check-interval`, M3's one-check-per-window-occurrence)
independently fetches the index, downloads the
artifact, extracts and stages it. The leader only runs the plan logic
(`update/plan.go`).

Desired: the **leader** performs the check once; when an update exists the
leader downloads it and **distributes it to the nodes** (channel + form to
study — node-pull via annotation/API, an in-cluster push, or object
storage) so each deploys it onto its own boot partition. A node, once it
has validated its local copy, sets `next-kernel`; once validated, it marks
itself reboot-eligible (as designed in PLAN-M3 §3.4). Study the
trade-offs: a single download
at the leader vs N; the leader as a check-time SPOF; transfer reliability
and resumability; and how this composes with the per-node `preserve`/purge
and the pod split (item 3).

## 12. BUG: stale `reboot-state` poisons a fresh update plan — CLOSED by PLAN-M3

A prior reboot left `reboot-state=completed`, and the M2 plan layer's
`verifyPlan` read it **before** the plan's own enqueue — concluding the
member "came up on the wrong kernel" and cancelling the plan in the same
cycle it was created (reproduced 2026-09-08 in the 3-CP auto-update test;
also broke successive auto-updates). Fixed 2026-09-08 in the plan layer:
clear the stale terminal `reboot-state` when a plan starts
(`resetStaleRebootState`; unit/integration tested).

**Closed by PLAN-M3:** the plan layer it lived in is abolished (the
`simplek8s-update-plans` ConfigMap and plan start/cancel/verify are
replaced by the window-open enqueue into the M1 queue + per-node
verification, PLAN-M3 §3.4), so the bug class no longer exists. The
pending E2E re-run (successive auto-update) is superseded by the
E2E-WINDOWS campaign, case W5.

## 13. Multi-platform container image (amd64 + aarch64)

SimpleK8s nodes can be `x86-64` **or** `aarch64` (= arm64) — the release
artifacts are per-arch (`simplek8s.<ts>.x86-64.efi.zst` /
`simplek8s.<ts>.aarch64.efi.zst`), and the update engine already maps the
node arch. But the controller **container image** is currently built
single-arch: the `Makefile` `image:` target runs a plain `docker build`
(host architecture only), so an `aarch64` node cannot pull/run it. Publish
the image as a **multi-platform** `linux/amd64,linux/aarch64` image — e.g.
a buildx builder + `docker buildx build --platform
linux/amd64,linux/aarch64` (and push a multi-arch manifest). The `Dockerfile`
itself is arch-agnostic (static Go binary + `util-linux`), so only the
build/publish step needs the multi-arch treatment.
