# TODO — deferred backlog

Cross-cutting items deliberately left out of the current plans
(PLAN-M1 shipped, PLAN-M2 in planning). Each item is ready to be picked
up as its own plan; nothing here is blocking.

## 1. Reboot maintenance windows (`reboots.windows`) — future PLAN-M3

Optional time windows that gate **only the start** of reboots. Semantics
already agreed with the maintainer:

- **Open windows**: a window `[start, end)` during which reboots may be
  *started*. A window may span midnight (e.g. `22:00-06:00`); windows
  repeat on a cadence (daily/weekly to study).
- **Gate start only**: a reboot already in flight (draining/rebooting)
  is **never** interrupted when a window closes.
- **Queue waits, never fails**: nodes `requested` outside a window stay
  queued (`requested`) until a window opens; no timeout, no error.
- **Multiple windows**: a list; the node may start if any window is
  open.
- **UTC always** (no timezone key, same rule as PLAN-M2 §2).
- **To study**: whether a window needs a maximum-duration guard (a
  misconfigured tiny window silently holding the queue for days).

Config sketch (syntax TBD in PLAN-M3, flat-key rule from PLAN-M2 §3.2):
a `reboots.windows` key, e.g. a compact
`"Mo-Fr 22:00-06:00"`-style string, absent = no restriction (today's
behavior).

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
