# PLAN-M3 — simplek8s-controller: maintenance windows, the update reboot loop & boot-partition hygiene

> **Planning index**
> - [PLAN-M1.md](PLAN-M1.md) — node reboots (shipped)
> - [PLAN-M2.md](PLAN-M2.md) — distro updates (implemented; E2E campaign in progress)
> - [PLAN-M3.md](PLAN-M3.md) — maintenance windows + update reboot loop + boot-partition hygiene (this document; in planning)
> - [E2E.md](E2E.md) — reboots E2E campaign (28/28 PASS)
> - [E2E-UPDATE.md](E2E-UPDATE.md) — updates E2E campaign (redrawn by this plan)
> - [E2E-WINDOWS.md](E2E-WINDOWS.md) — windows E2E campaign (created in M5)

Phase: **PLAN** (design & planning). Status: v2 — all design decisions
closed with the maintainer (2026-09-09; scope extended with TODO 6/8 and
the TODO 13 build half), pending implementation.

Terminology: the node's boot partition (vfat, `vda1`) is mounted by the
controller pod at a scratch mountpoint, used, and unmounted — SimpleK8s
does not mount `/boot` at runtime. This document says **boot partition**
and refers to the release directory at its root as `simplek8s/`.

## 1. Purpose

The third feature, built on the shipped M1 reboot machinery and the M2
update engine. It closes five TODO items plus the build half of a sixth:

- **TODO 1 — reboot maintenance windows.** Cron-style time windows,
  configured in the flat ConfigMap, that gate **when reboots may
  start**. Two independent window lists: `reboots.windows` gates
  operator-driven (M1 API) non-forced reboots; `updates.windows` gates
  the whole update feature (checks, staging, and the update-driven
  reboots).
- **TODO 11 — the update-driven reboot loop.** The leader waits for the
  updates window to be open and, while it is, sends every reboot-eligible
  node into the M1 reboot queue. The M2 plan layer (the
  `simplek8s-update-plans` ConfigMap, per-version all-or-nothing plans,
  plan cancel) is **abolished**: pending update state lives only in the
  node's `reboot-eligible` annotation, execution is the M1 queue itself,
  and verification is per-node.
- **TODO 9 — operator-pinned `next-kernel` is honored.** When the
  operator points `next-kernel` at a valid version that already exists on
  the boot partition, the local pod re-points the bootloader `DEFAULT`
  and marks the node `reboot-eligible` — the change-triggered bootloader
  reconciliation designed in PLAN-M2 §3.5 but not yet implemented.
- **TODO 6 — syslinux stale-entry cleanup.** The bootloader entry writer
  only ever appends, so `syslinux.cfg` on the boot partition grows one
  entry per staged kernel. Entries for kernels the purge has already
  deleted are pruned in the same mounted session (§3.9).
- **TODO 8 — `next-kernel` validation & safe-state recovery.** The
  annotation is the node's boot goal and it is **file-first**: no
  controller writer sets a value whose file is not on the boot partition
  (audited, §3.10). The residual stuck case — a malformed value, or a
  value the repository can no longer satisfy — is corrected back to a
  **safe state** instead of being left dangling (§3.10).
- **TODO 13 (build half) — multi-platform image build.** The controller
  image is built for **both** `linux/amd64` and `linux/aarch64` by
  `make image` (§3.11); the public `v*` publishing half stays in the
  backlog (no registry, tag scheme undecided, §7).

Together these make the controller a safe unattended mechanism: nothing
reboots (and no update work happens) outside the windows an operator has
explicitly configured, and the boot partition stays bounded and
consistent over time.

## 2. Constraints

- **Stdlib only.** The two dependencies M2 added are the ceiling. A
  third (a cron library, e.g. `robfig/cron`) was explicitly rejected: the
  schedule set is small enough to hand-roll with exhaustive tests
  (§3.3).
- **Flat-key ConfigMap** (PLAN-M2 §3.2): values are strings; a list is a
  JSON array of strings inside one flat key. Last-valid-wins on invalid
  values; unknown keys warned.
- **UTC always.** No timezone key (same rule as PLAN-M2 §2).
- **KISS.** No window ranges/`end` fields, no weekday aliases, no
  maximum-duration guards, no new API endpoints, no new node annotations.
- **M1 invariants untouched.** The window is one more gate on *start*.
  Serialization, workers-before-CP ordering, the CP gate, PDB (for
  non-forced), drain timeout, no-effect grace, and
  `on-reboot-failure` pause/continue all keep their M1 semantics. An
  in-flight reboot is **never** interrupted by a closing window.
- **Stateless windows.** Window openness is computed from the clock and
  the ConfigMap every cycle — nothing is persisted (the
  `update-last-check` claim, §3.4, is per-node check bookkeeping, not
  window state). A leader crash or handover changes nothing: the new
  leader recomputes the same answer.

## 3. Design

### 3.1 Window model

A **window list** is a set of schedules plus a grace period:

- **Schedule** — a recurrence, from the Kubernetes CronJob standard set
  only (§3.3): a 5-field cron expression, a named schedule
  (`@hourly`, `@daily`, …), or `@every <duration>`. Each schedule
  produces occurrences on the UTC timeline.
- **Grace** — `reboots.window-grace` / `updates.window-grace` (duration,
  default `5m`) is how long a window stays open after each occurrence.
- **Openness** — at time `t` (UTC), a window list is **open** iff any of
  its schedules has an occurrence `O` with `O ≤ t < O + grace`. The
  union over schedules makes multiple windows work: any open window
  suffices.

Consequences:

- **Window = start gate.** Only the *start* of a reboot (admission
  `requested → draining`) is gated. A reboot already draining/rebooting
  runs to completion even if the window closes mid-flight.
- **Queue waits, never fails.** A non-forced node that is `requested`
  while the window is closed stays `requested` — no timeout, no error —
  until a window opens. Same for eligible update nodes: the marker
  simply waits on the annotation.
- **Explicit empty list = feature OFF** (supersedes PLAN-M2 §3.2's
  "absent = no restriction" — see decision 6). Key absent → the built-in
  default applies, and the defaults differ per feature (§3.2):
  `reboots.windows` → `[]` (OFF); `updates.windows` → `["@every 12h"]`.
  - `reboots.windows` empty → **no non-forced reboots execute**, and
    non-forced `POST /reboots` is rejected at admission (§3.5). Forced
    reboots always execute immediately.
  - `updates.windows` explicit `[]` → the update feature is **fully
    inert**: no release checks, no downloads, no staging, no window-open
    enqueue — regardless of `updates.update-mode`.
- **Edge case (documented, not validated — KISS):** if the grace is
  longer than the interval between occurrences (e.g. `@hourly` with a
  2h grace), the window is always open. That is an operator error; the
  controller does not refuse it.

### 3.2 Configuration keys

New keys in the flat ConfigMap `simplek8s/simplek8s-controller`
(PLAN-M2 §3.2 form; the existing table is unchanged):

```yaml
data:
  # ... existing engine.*/reboots.*/updates.* keys ...
  reboots.windows: '[]'                  # built-in default (OFF); e.g. '["@daily", "0 7 * * 3"]'
  reboots.window-grace: 5m
  updates.windows: '["@every 12h"]'      # built-in default; '[]' turns updates fully off
  updates.window-grace: 5m
```

| Key | Type | Default | Description |
|---|---|---|---|
| `reboots.windows` | JSON array of schedule strings | `[]` (empty) | Windows that gate non-forced reboots (M1 API and any leader enqueue). Empty ⇒ non-forced reboots never execute; forced reboots always do. |
| `reboots.window-grace` | duration | `5m` | How long a `reboots.windows` occurrence stays open. |
| `updates.windows` | JSON array of schedule strings | `'["@every 12h"]'` | Windows for the update feature: the per-node check/staging loop runs only while open, and the leader enqueues eligible nodes only while open. An explicit `[]` ⇒ the whole feature is inert. The default preserves M2's 12h check cadence, so an M3 rollout does not silently stop updating. |
| `updates.window-grace` | duration | `5m` | How long an `updates.windows` occurrence stays open. |

Parse rules (one rule for all keys, PLAN-M2 §3.2):

- The value must parse as a JSON array of strings, and **every** string
  must parse as a schedule (§3.3). Any failure → the whole key is
  invalid → the last valid value is kept (or the default) + one warning.
  A partially valid list is not accepted.
- `[]` is valid and means OFF for both features. Key absent → the
  built-in default: `reboots.windows` → `[]` (OFF); `updates.windows` →
  `'["@every 12h"]'`.
- The `*-window-grace` keys use the existing duration parser (must be > 0).

### 3.3 Cron parser (`internal/cron`, hand-rolled, stdlib)

A small new package with two responsibilities — parsing and window
evaluation — and no state:

```go
type Schedule struct{ /* parsed form */ }
func Parse(spec string) (Schedule, error)
func (s Schedule) lastOccurrenceAtOrBefore(t time.Time) time.Time
func WindowsOpen(specs []Schedule, grace, now time.Time) bool
```

Accepted set — **the Kubernetes CronJob standard set, nothing else**:

| Form | Examples | Meaning |
|---|---|---|
| 5-field cron | `0 7 * * 3`, `*/10 * * * *`, `0 0 1-15 * 1-5` | minute hour dom month dow, UTC. Standard field syntax: `*`, values, lists `a,b`, ranges `a-b`, steps `*/n` and `a-b/n`. |
| Named | `@hourly`, `@daily`/`@midnight`, `@weekly`, `@monthly`, `@yearly`/`@annually` | The fixed CronJob expressions: `0 * * * *`, `0 0 * * *`, `0 0 * * 0`, `0 0 1 * *`, `0 0 1 1 *`. |
| `@every <duration>` | `@every 30m`, `@every 2h` | Occurrences every duration. |

Semantics pinned (all tested):

- **dow**: `0` and `7` are both Sunday; `1`=Mon … `6`=Sat.
- **dom/dow OR rule** (vixie-cron/K8s): when *both* dom and dow are
  restricted (neither is `*`), a day matches if **either** matches —
  `0 0 13 * 5` fires on every 13th **and** on every Friday.
- **`@every` reference point**: occurrences are `00:00 UTC (today) +
  k·duration`. Openness = `(now − todayMidnightUTC) mod duration <
  grace`. A fixed reference makes openness identical on every pod and
  leader across handovers (a "since process start" reference would
  differ per pod and is rejected).
- **5-field last occurrence**: bounded backward search from `now` (at
  most ~366 days for yearly schedules); the window is open iff
  `now − lastOccurrence < grace`. Cheap at engine cadence; memoization
  per minute is an implementation detail, not a requirement.
- **Rejected**: 6-field (seconds) expressions, weekday aliases
  (`@monday` … `@sunday` are **not** in the K8s set → invalid),
  `@reboot` (not a K8s CronJob schedule; "always open" is the operator
  expressing via grace, not a schedule), unknown `@` names, empty
  strings, out-of-range field values.

The window forms proposed before this plan (time-of-day ranges
`22:00-06:00`, bare intervals `30m`) are **not** supported: duration
comes from the grace key, periodicity from `@every`/cron. One mechanism
instead of four.

### 3.4 Updates: master switch, window-open enqueue, per-node verification

This section replaces PLAN-M2 §3.8/§3.9/§3.10 (plan, plan durability,
plan verification).

**Master switch (local pod).** The per-node check/staging loop
(`update.RunLocal`) runs a window check **first**: `updates.windows`
empty or no window open at `now` → skip the cycle (no HTTP fetch, no
partition mount, rate-limited log).
`updates.update-mode: off` still wins over everything (no update
behavior at all). So the effective dial is:

| `update-mode` | `updates.windows` | Behavior |
|---|---|---|
| `off` | any | no update behavior. |
| `stage` / `full` | explicit `[]` | **fully inert** — not even checks. |
| `stage` / `full` | non-empty (default `["@every 12h"]`) | check + stage only while a window is open; staging itself (mount, download, verify, extract, bootloader, purge) runs as one unit inside the window. |

**One check per occurrence** (retires `updates.check-interval` —
decision 20), **persisted across pod restarts** (decision 21). The
newest checked occurrence is the node annotation
`simplek8s.org/update-last-check` (RFC3339 UTC occurrence start) — not
in-memory state: a pod crash loop or rolling restart must not re-fetch
the release repository once per restart. The local pod **claims** the
occurrence with one conditional RMW **before** fetching (write only if
the stored value is absent or older than `O`); a new check starts only
while the window is open and some schedule has a newer occurrence than
the stored one — at most one check per occurrence per node regardless
of pod lifetime (schedules with coincident occurrences share the claim).
An unparseable stored value is treated as absent (one extra check,
logged). Consequences: a pod restart does **not** re-check a claimed
occurrence; a pod down for a whole occurrence simply misses it; a check
interrupted mid-staging is not retried within the occurrence — the next
occurrence re-detects and re-stages (no regression vs M2's interval;
the defensive re-staging covers the `next-kernel`-missing case).
**No retries within an occurrence**: a failed check (network, GPG,
parse) is logged + evented (rate-limited) and consumes the occurrence;
the next check is the next occurrence. The occurrence schedule *is* the
check schedule: the operator expresses the desired check cadence
directly in `updates.windows` (`["@every 30m"]` = every 30 minutes;
`["@daily"]` = once a day).

**Eligibility — who sets `reboot-eligible`** (the marker is a valid
version string; the node it is on must be non-quiescent to mean
anything):

1. **Fresh full-mode staging** (existing M2 behavior, generalized):
   when staging completes a version `V` **new to this node's boot
   partition** and `next-kernel == V` (whether the staging anchor just
   wrote it or the operator had pinned it), the local pod sets
   `reboot-eligible := V` (conditional RMW). In `stage` mode, fresh
   staging anchors but does **not** set the marker (the operator
   reboots via the M1 API when desired).
2. **Operator pin** (TODO 9; §3.7): the operator changes
   `next-kernel` to a valid version already present on this node's boot
   partition → the local pod sets `reboot-eligible := V`, in `stage` and
   `full` modes alike (a pin is an explicit reboot intent).

**Who clears the marker:**

3. **Leader, on send**: the window-open enqueue (§below) deletes the
   marker **in the same RMW** that puts the node in the M1 queue —
   the marker is consumed, the node is never re-enqueued, and a later
   `DELETE /reboots/<node>` cannot be undone by a stale marker
   (no resurrection).
4. **Local pod, stale-marker sweep**: if the node is quiescent
   (`running == next-kernel`) while carrying a marker, the marker is
   stale (its intent was achieved by other means, e.g. an operator
   reboot) → delete it (conditional RMW; write only on the anomaly).
   Extended in §3.10: a marker whose version file is **absent** from the
   partition is stale as well.

**Window-open enqueue (leader, replaces `maybeStartPlan`).** Every
engine cycle, if `updates.windows` is non-empty and a window is open:
for each node with a valid `reboot-eligible` marker,
`next-kernel != running` (non-quiescent), and a **resting** M1 state
(absent or `completed` — never `failed`, which stays an operator-alarm
state, and never an in-flight state), a single conditional RMW writes
the same four-annotation state the M1 API writes — `reboot-state :=
requested` with a controller-issued request (`requestedBy:
"controller"`, fresh req id, `force: false`) — **and deletes the
marker**. From that moment the node is fully M1-managed: visible in
`GET /reboots`, governed by serialization/PDB/CP-ordering, and
cancellable with `DELETE /reboots/<node>` (which is now the **only**
cancel — there is no plan cancel). While the window is open, newly
eligible nodes are picked up on subsequent cycles; when it closes, the
remaining markers simply wait (rate-limited `UpdateHeldWindow` event).
Precondition failure on the fresh read (marker gone, state changed,
operator re-pin) → skip the node, no clobbering.

**Per-node verification (leader, replaces plan verification).** Every
cycle, for each node with a valid `next-kernel`:

- `running == next-kernel` → **quiescent**; nothing to do. The
  transition into quiescence after a reboot emits
  `UpdateApplied` (rate-limited, keyed per node+version).
- `running != next-kernel` and the node's M1 state is `completed` (it
  came back and rested) → `UpdateMismatch` event + warn log (keyed per
  node, rate-limited). **No automatic retry** — consistent with
  PLAN-M2's accepted consequence "a failed version is never
  auto-retried". The operator reboots via the M1 API (non-forced, so it
  waits for `reboots.windows`; or force), or re-pins.

**What disappears** (recorded, decision 11):

- The `simplek8s-update-plans` ConfigMap and all plan state
  (`planstate.go`); the leader performs a **best-effort one-shot
  delete** of a leftover `simplek8s-update-plans` ConfigMap on its first
  cycle (logged; ignored if absent or not deletable). No RBAC change is
  expected — the leader already manages a ConfigMap in namespace
  `simplek8s` (verify at implementation).
- Per-version all-or-nothing plans, two-phase cancel/reset,
  "one active plan at a time", and the plan-level verify.
- The BUG 12 fix (`resetStaleRebootState`) lived in the plan layer; with
  no plan object there is no plan to poison. The pending BUG 12 E2E
  re-run is **superseded** by the E2E-WINDOWS successive-auto-update
  case (W5).

### 3.5 M1 reboots: window gate, admission rejection, force

`reboots.windows` touches exactly two M1 sites:

- **API admission** (`POST /api/v1/reboots`, `api.admit`): a
  **non-forced** request for a node, when `reboots.windows` is empty,
  is rejected per node with `422, reason: "no reboot windows configured
  (reboots.windows is empty)"` (code `NoWindowsConfigured`) — the
  request could never be honored, so it is not queued (no zombie
  queue). With windows configured, non-forced requests are admitted as
  today (`requested`, queued). **Forced** requests bypass the check and
  always queue (and, as today, bypass PDB and execute immediately at
  admission).
- **Orchestrator admission** (`orchestrator.runOrchestrator`
  phase 2): the existing gates keep their order and semantics
  (pause-on-failure → queued-NotReady hold → slots → per-node PDB).
  One more gate joins the per-candidate loop: a **non-forced** candidate
  is admitted (`requested → draining`) only while a reboot window is
  open; otherwise it stays `requested` and the loop emits the
  rate-limited `QueueHeldWindow` event (the `QueueHeldNotReady`
  analogue) and does not admit further non-forced candidates this cycle.
  A **forced** candidate is admitted regardless of window state. In-flight
  nodes (`draining`/`rebooting`) are managed in phase 1 exactly as today
  — the window never touches them.

### 3.6 Writer discipline (updated)

The annotation has three writer classes; every controller write remains
a conditional RMW on the M1 nodestate primitives (read → precondition →
patch carrying the read `resourceVersion`). PLAN-M2 §3.5's table, as
affected by M3:

| Writer | Action | Precondition |
|---|---|---|
| Local pod | Bootstrap `next-kernel` (unchanged, M2) | annotation absent. |
| Local pod | Fresh-stage anchor `next-kernel := V` (unchanged, M2) | value at write time `== running` or absent. |
| Local pod | **Fresh full-mode staging → `reboot-eligible := V`** (M3) | staging of `V` completed new-to-partition; `next-kernel == V` at write time. |
| Local pod | **Operator pin → `reboot-eligible := V`** (M3, §3.7) | the pin value `V` is valid, present on the partition, and still `== next-kernel` at write time; modes `stage`/`full`. |
| Local pod | **Stale-marker sweep → delete `reboot-eligible`** (M3) | node quiescent (`running == next-kernel`) with a marker present, **or** the marker's version file is absent from the partition (§3.10). |
| Local pod | **Goal correction → `next-kernel := <safe state>` or delete** (M3, §3.10) | value malformed (node state, ungated), or file absent and the verified index does not contain the value (window occurrence); re-checked on the fresh read. |
| Local pod | **Check claim → `update-last-check := O`** (M3, §3.4) | window open; stored value absent or older than the claimed occurrence `O`; re-checked on the fresh read. |
| Leader | **Window-open enqueue: `reboot-state := requested` + delete marker, one RMW** (M3, §3.4) | marker present and valid; non-quiescent; M1 state resting (absent/`completed`); re-checked on the fresh read. |
| ~~Leader~~ | ~~Two-phase plan cancel/reset~~ (M2, **abolished**) | — |
| Operator | Any `next-kernel` edit (unchanged) | none — the operator wins by definition. |

### 3.7 Change-triggered bootloader reconciliation (TODO 9)

Implements PLAN-M2 §3.5's designed-but-unimplemented discipline: the
local pod ensures the bootloader `DEFAULT` equals `next-kernel`,
**whatever changed the annotation**, but change-triggered — the boot
partition is vfat and must not be mounted every 2 s. Mount + write
happen only:

1. **Pod start** — the initial applied value is unknown; one mount,
   compare, re-point if needed.
2. **Value change** — `next-kernel` differs from the last value the pod
   applied (tracked in memory). On change to a **valid** version:
   - version **present** on the partition → re-point `DEFAULT` to it,
     and set `reboot-eligible := V` (the operator-pin path, §3.4).
   - version **not** present → do not re-point (it cannot boot); the
     existing defensive re-staging path (`maybeStage`) fetches it when
     the window allows, and the fresh-staging rule (item 1, §3.4) sets
     the marker once it is local. No marker is set while the file is
     absent.
3. **Folded into staging** — staging already has the partition mounted;
   the default is set in the same session (no extra mount). This is how
   a fresh stage and a re-stage converge.

Steady state: zero disk I/O (the value comes from the listed Node
object). Hand-edits of the bootloader files remain out of contract —
the annotation is the source of truth. This is what makes manual
rollback (TODO item 4) and operator pins actually honored without
touching the boot partition by hand.

### 3.8 Observability

Events (namespace `default`, rate-limited per key as in M1/M2):

| Event | Source | Meaning |
|---|---|---|
| `QueueHeldWindow` | leader (reboot) | non-forced queued nodes waiting for a `reboots.windows` window. |
| `UpdateHeldWindow` | leader (update) | eligible node(s) waiting for an `updates.windows` window (keyed per node). |
| `UpdateApplied` | leader (update) | node quiescent on `next-kernel` after a reboot (keyed per node+version). |
| `UpdateMismatch` | leader (update) | node came back `running != next-kernel`; no auto-retry (keyed per node). |
| `UpdateGoalCorrected` | local pod (update) | `next-kernel` corrected to its safe state (malformed, or unreachable — §3.10) (keyed per node). |

Existing events are kept (`QueueHeldNotReady`, `PDBBlocked`,
`RebootCompleted`, `UpdateStagingSkipped`, …). Structured logs:
`Info` on window open→closed transitions (rate-limited, per feature), on
each enqueue, and on each pin re-point; `Warn` on invalid config
(last-valid-wins), on mismatch, on the one-shot stale-ConfigMap delete.
The `NoWindowsConfigured` rejection is returned in the API response and
logged at `Info` (an expected operator-visible condition).

### 3.9 Syslinux stale-entry prune (TODO 6)

The syslinux writer (M2 staging step 5, the target of §3.7's
reconciliation) only ever appends a `LABEL` block and moves `DEFAULT`;
`syslinux.cfg` therefore grows one entry per staged kernel. The prune
bounds it:

- **Prune candidate** — an entry block whose `KERNEL` line references
  one of *our* kernel files (matching the stored-kernel pattern
  `simplek8s.<ts>.<arch>.efi` under the release directory) **and** whose
  file no longer exists on the partition. Blocks whose `KERNEL` path
  does not match our pattern (foreign entries) are **never touched**,
  file present or not. Global lines (`DEFAULT`, `TIMEOUT`, `PROMPT`, …)
  are preserved verbatim.
- **Block** — a block runs from its `LABEL` line to (not including) the
  next `LABEL` line or EOF — the exact mirror of the writer's block
  form, so the prune parser and the writer can never disagree about
  block boundaries.
- **Protection** — the block named by the current `DEFAULT` is never
  pruned. In practice it can never be a candidate: the purge's
  `protectedSet` includes the bootloader default, so that file is never
  deleted. The guard is belt-and-braces.
- **Trigger** — the prune runs **only when a purge deleted at least one
  kernel** (capacity pre-check, staging step 4, or retention, step 7),
  in the same mounted session, after the staging work. No purge
  deletion → no rewrite: rewriting `syslinux.cfg` is a vfat write and is
  not done gratuitously. The rewrite uses the writer's existing
  discipline (temp file + synchronous `copyOver`).
- **Scope** — syslinux only. The rpi config (`config.txt`) carries a
  single `kernel=` line; there is nothing to prune.
- **The distro's initial entry** (confirmed to reference one of our
  kernels) is a normal candidate: when the purge deletes that kernel,
  the entry goes with it. Accepted behavior change — the entry cannot
  boot once its file is gone, so nothing usable is lost.

Real sample (captured 2026-09-10 from the `sk8s-cp1` test node's boot
partition: the distro's initial entry plus one staged by the
controller) — the reference fixture for the prune unit tests. Newer
distro images ship a **documentation header** (a block of `#`
comments at the top of the file) instead of the per-entry `#APPEND`
hint shown here; existing nodes keep the sample's shape.

```cfg
DEFAULT simplek8s.202609061935.x86-64

LABEL simplek8s.202608291203.x86-64
 KERNEL /simplek8s/simplek8s.202608291203.x86-64.efi
 #APPEND log_buf_len=5M printk.devkmsg=on systemd.debug-shell=1 debug


LABEL simplek8s.202609061935.x86-64
 KERNEL /simplek8s/simplek8s.202609061935.x86-64.efi

```

Parser/fixture notes from the sample:

- The parser must tolerate **extra per-block lines** — the sample's
  commented-out `#APPEND` hint, and the `INITRD` lines the writer
  appends when a microcode file is present. Comments inside a pruned
  block are dropped with the block (maintainer-confirmed: a hint for a
  kernel that no longer exists is useless); the distro's top-of-file
  DOC header is a *global* line and survives every rewrite and prune
  verbatim.
- `DEFAULT`/`LABEL` names drop the `.efi` suffix; `KERNEL` carries the
  full `/simplek8s/...` path. Ownership is decided on the `KERNEL`
  path only — the label name is irrelevant.
- This sample has no `TIMEOUT`/`PROMPT`, but the rewrite must preserve
  any global line verbatim; the file ends with a blank line (`\n\n`),
  and the blank lines after a block belong to that block (pruning
  block 1 above leaves a clean file).

### 3.10 `next-kernel` validation & safe-state recovery (TODO 8)

`next-kernel` is the node's boot **goal**, and it is **file-first**: no
controller writer sets a value whose file is not on the boot partition.
Audited — all four existing writers satisfy it:

| Writer | Why the file is present |
|---|---|
| `bootstrap` (annotation absent) | the target comes from a local partition scan (running-if-local, else newest local). |
| Fresh-stage `anchor` | runs only after `Store.Stage` succeeded — the file was just written. |
| Plan-layer resets (×2, M2) | write `running` — the kernel the node is executing, which the purge protects. (Abolished by this plan anyway.) |

The only ways a node can hold a value whose file is absent are an
**operator pin ahead of staging** (by design — the annotation leads
staging; the system converges via defensive re-staging, or corrects
below) and **manual file deletion**. Validation restores a stuck goal to
a **safe state**:

**Safe state** — (a) the running version, if its file is present on the
partition; else (b) the newest version present on the partition; else
(c) delete the annotation. In (a)/(b) the bootloader `DEFAULT` is
re-pointed to the corrected value; in (c) the bootloader is left alone
(there is nothing local to point at).

**Correction paths** (local pod; both fire the rate-limited
`UpdateGoalCorrected` event, keyed per node):

1. **Malformed value** (present but failing the version grammar) —
   node state, evaluated in the same ungated path as `bootstrap` (no
   repository involved): correct to the safe state. The correction is
   an annotation RMW only; the bootloader re-point follows via the
   change-triggered reconciliation (§3.7), which is already an
   ungated, change-triggered partition-write path.
2. **Well-formed value, file absent** — evaluated in the
   window-occurrence update loop (the defensive re-stage target, §3.4),
   against the verified index of the node's effective repository:
   - the check **failed** this occurrence (no verified index) → **no
     correction**; retried at the next occurrence;
   - the value **is in the verified index** → the staging failure
     (download, GPG, checksum — a mismatch included) is **transient** →
     **no correction**; the defensive re-stage retries at the next
     occurrence;
   - the value is **not in the verified index** → positive knowledge of
     absence (removed from the repo, or a ts that never existed) →
     **correct** to the safe state, folded into the same mounted
     session (re-point in the session).

The rule is **stateless** — no failure counters: correction happens
only on (a) local malformation or (b) positive knowledge of absence,
never on a transient failure (correcting on a transient failure would
silently destroy the operator's intent), and a transient failure costs
at most one occurrence — the same cost as any failed check (§3.4).

**Marker** — the stale sweep (§3.4, item 4) is extended: a node
carrying `reboot-eligible` whose version file is **absent** from the
partition loses the marker (conditional RMW, same discipline as the
quiescent sweep). This covers a marker left stale by a correction and a
manually deleted file alike.

**Leader** — no new leader logic: after a correction, the value is
bootable by construction and the existing per-node verification (§3.4)
converges (`UpdateApplied` on the next quiescence). An operator pin to
a version that *is* in the repository is never corrected — it is
staged when the window allows.

### 3.11 Multi-platform image build (TODO 13, build half)

SimpleK8s nodes can be `x86-64` **or** `aarch64`; the release kernels
are per-arch, but the controller **image** was built single-arch (host
architecture), so an `aarch64` node could not run it.

- **`make image` builds for both platforms**: buildx against
  `linux/amd64,linux/aarch64` — the target **fails if either platform
  fails**, which is the point (cross-arch surprises surface at build
  time, not at the first aarch64 deployment). The **host-architecture
  image is then loaded** into the local docker store as
  `$(IMAGE):$(TAG)` (a classic store is single-arch; only one platform
  can be loaded). The deploy flow is unchanged (`docker save` +
  `scp -O` + `docker load` + `kubectl apply`).
- **The Dockerfile needs no change** — verified arch-agnostic: static
  Go binary (`CGO_ENABLED=0`), `alpine:3.20` + `util-linux` (multi-arch
  package), embedded keyring (a plain file).
- **Prerequisite (documented)**: on Linux, the foreign-arch `apk add`
  layer runs a foreign-arch shell, which needs qemu/binfmt_misc
  registered **once** (`docker run --privileged --rm
  tonistiigi/binfmt --install arm64`). The Go cross-compile itself
  needs no emulation. **`make image` preflights this**: before invoking
  `buildx build` it checks the foreign platform is actually emulable
  (on Linux, `qemu-aarch64` present under
  `/proc/sys/fs/binfmt_misc/`; a host that is itself aarch64 only needs
  the amd64 half) and, on failure, prints
  the `docker run --privileged --rm tonistiigi/binfmt --install arm64`
  remedy — buildx's own failure without binfmt is cryptic (a
  foreign-arch shell error deep in the build), so the message must come
  from us.
- **Out of scope** (decisions 27/28): publishing a public multi-arch
  `v*` image (no registry exists yet; tag scheme undecided) and CI
  (explicitly not in M3). Notes for when the CI is built:
  `actions/checkout` needs `lfs: true` (the keyring is a Git LFS file —
  otherwise the LFS pointer text is embedded *as the keyring*, a
  build-successful / runtime-GPG-failing silent failure) and
  `fetch-depth: 0` (for the `git describe`-based `VERSION`); GitHub
  runners already have binfmt registered; a release push should use
  `--provenance=false`.
- **E2E**: no arm64 test node exists yet — verification is that both
  platforms build; once an arm64 node is stood up, deploy the aarch64
  image and run one full auto-update (W4-shaped).

## 4. Decision log (closed)

| # | Decision | Rationale |
|---|---|---|
| 1 | Windows = JSON **array of schedule strings** in flat ConfigMap keys | ConfigMap values are strings (M2 §3.2); a list needs a serialization; JSON is stdlib and unambiguous. |
| 2 | K8s CronJob schedule set only (5-field cron, named, `@every`) | What every K8s operator already knows; no new syntax to teach or document; small enough to hand-roll. |
| 3 | No window ranges / `end` field; duration = the grace key | Replaces TODO 1's four window forms (range, interval, cron, shortcut) with one mechanism; less surface, fewer edge cases. |
| 4 | No weekday aliases (`@monday`…`@friday` invalid) | Not in the K8s CronJob set; `0 7 * * 3` already expresses it. |
| 5 | Grace default `5m`, one key per feature (`reboots`/`updates`) | Short default so a misconfigured schedule cannot hold the queue silently for hours; per-feature so the two cadences can differ. |
| 6 | **Explicit empty windows = feature OFF**; built-in defaults: `reboots.windows` = `[]`, `updates.windows` = `'["@every 12h"]'` (supersedes M2's "absent = no restriction") | Reboots stay opt-in (safety); updates keep M2's de-facto 12h cadence as the default, so an M3 rollout does not silently halt updates — a full stop is an explicit `'[]'`. |
| 7 | No maximum-duration guard | KISS; the always-open edge (grace > period) is documented as an operator error. |
| 8 | Gate start only; in-flight never interrupted; queue waits, never fails | M1 invariant; windows are scheduling, not a kill switch. |
| 9 | `force` bypasses windows (as it bypasses PDB) | The escape hatch stays immediate; one `force` semantics everywhere. |
| 10 | Non-forced `POST /reboots` with empty `reboots.windows` rejected at admission (`NoWindowsConfigured`, 422) | No zombie queue for a request that can never be honored; immediate operator feedback. |
| 11 | **The M2 plan layer is abolished** — plan ConfigMap, per-version all-or-nothing, plan cancel, "one active plan at a time" (supersedes M2 §3.8/§3.9 and PLAN.FIXME #7) | Pending state in node annotations + the M1 queue is strictly less machinery; the BUG 12 class (plan state vs M1 state disagreement) disappears with the plan object; serialization is already M1's job. |
| 12 | Marker consumed on send (deleted in the enqueue RMW) | No reprocessing, no resurrection after `DELETE`, single writer per transition. |
| 13 | Operator pin (valid, locally present) → re-point `DEFAULT` + set marker, in `stage` and `full` modes | A pin is explicit reboot intent; makes rollback/pins honored without hand-editing the bootloader (TODO 9). |
| 14 | Stale-marker sweep (local pod clears a marker on a quiescent node) | Markers set while the node was in flight (and never consumed by the leader) must not accumulate. |
| 15 | One-shot best-effort delete of a leftover `simplek8s-update-plans` ConfigMap | Self-cleaning migration; no operator step, no code path left reading it. |
| 16 | Per-node verification with events; **no auto-retry** on mismatch | Consistent with M2's "a failed version is never auto-retried"; the M1 API is the retry path. |
| 17 | Cron parser hand-rolled in `internal/cron` (stdlib) | Third dependency rejected; the accepted set is small and exhaustively testable. |
| 18 | UTC, no timezone key | M2 §2 rule. |
| 19 | Invalid windows value → whole key invalid → last-valid-wins + warn | One validation rule for all flat keys (M2 §3.2). |
| 20 | **`updates.check-interval` retired**; at most one check per window occurrence, no retries within an occurrence | With windows, the interval was a second dial on the same thing (it could only reduce or delay the frequency); per-occurrence checks also bound an always-open window by the occurrence period; the operator expresses the check cadence directly in `updates.windows`. |
| 21 | The last-checked occurrence is a **node annotation** (`simplek8s.org/update-last-check`, RFC3339 UTC), claimed by the local pod with an RMW **before** the fetch | An in-memory tracker would re-fetch the PROD release repo once per pod restart; a crash loop or rolling restart would amplify into fleet-wide repo traffic. The annotation is the project's established persistence primitive (M1/M2): one small RMW per occurrence, never cleared, and claim-on-start bounds repo traffic even when staging is interrupted (the next occurrence self-heals). Rejected alternatives: shared ConfigMap (write contention, wrong primitive), pod-local volume (stateless pods, none in SimpleK8s), hard-coded minimum interval (a new unconfigurable constant that only dampens, does not bound). |
| 22 | Prune rule: remove only entry blocks whose `KERNEL` references one of our kernels (`simplek8s.<ts>.<arch>.efi`) **and** whose file no longer exists; foreign entries never touched, file state irrelevant | The writer's naming pattern is the ownership boundary; foreign entries (recovery, non-managed paths) are out of contract like hand-edited files; the existence check is stateless — no "what did we delete" bookkeeping. |
| 23 | Prune trigger: only when a purge deleted ≥1 kernel, in the same mounted session; no rewrite otherwise | The only situation the rule can apply to by our own doing; no gratuitous vfat writes; the distro's initial entry (one of our kernels, confirmed) is pruned when its file is purged — accepted. |
| 24 | Goal correction only on (a) a malformed value or (b) a verified index that does not contain the value; fetch failure, or value-in-index with staging failed (including checksum/GPG mismatch) → no correction, retry next occurrence; no failure counters | Correcting on a transient failure would silently destroy operator intent (a pin, an update target); positive knowledge of absence is the only safe trigger; stateless, and a transient failure costs at most one occurrence — the same as any failed check (§3.4). |
| 25 | Safe state: running (if local) → newest local → delete the annotation; written by the local pod, re-pointing `DEFAULT` in the same session (case (c) leaves the bootloader untouched); `UpdateGoalCorrected` event | Generalizes bootstrap's fallback (M2 §3.5) from the annotation-absent case to the present-but-unreachable case; the corrected value is bootable by construction, so the leader's verification converges with no new logic. |
| 26 | Marker-sweep extension: `reboot-eligible` whose version file is absent from the partition → cleared | A marker pointing at a version that cannot boot is dead intent; the same conditional-RMW discipline as the quiescent sweep; covers post-correction stale markers and manual file deletion. |
| 27 | `make image` = buildx for `linux/amd64,linux/aarch64` (fails if either fails) + host-arch load into the local store; deploy flow unchanged | The Dockerfile is verified arch-agnostic (static Go, `alpine` + `util-linux`); the maintainer wants cross-arch failure caught at build time; a classic docker store is single-arch, so only the host platform is loadable. |
| 28 | CI and public release out of M3 (no workflow); TODO 13 reduced to the public `v*` publishing half (registry + tag scheme TBD); qemu/binfmt prerequisite documented | Maintainer decision (2026-09-09); the publishing target does not exist yet; the local multi-arch build check suffices until an arm64 test node is stood up. |

## 5. Behavior changes & migration

- **Updates keep working after an M3 rollout**: absent
  `updates.windows` → the built-in default `["@every 12h"]` (M2's 12h
  check cadence, preserved). To stop the update feature entirely (not
  even checks), set `updates.windows: '[]'` explicitly (or `update-mode:
  off`).
- **Non-forced reboots are OFF by default.** The built-in default for
  `reboots.windows` is empty: after an M3 rollout, `POST /reboots`
  without `force` is rejected until the operator configures
  `reboots.windows`. Forced reboots are unaffected. This is the
  intended safety default, not a regression.
- **`updates.check-interval` is retired.** It is no longer parsed: a
  ConfigMap that still carries it gets the existing unknown-key
  warn+ignore (the migration signal). Remove it from the ConfigMap and
  from `deploy/configmap.yaml`. The check cadence is now fully
  determined by `updates.windows` (one check per occurrence): with a
  weekly window, a new release is detected at most weekly.
- The `reboot-eligible` annotation widens from "transient plan trigger
  (fresh full-mode stage only)" to "pending reboot intent" (also set by
  operator pins, consumed by the window-open enqueue, swept when
  stale).
- New node annotation `simplek8s.org/update-last-check` (newest checked
  occurrence, RFC3339 UTC), written once per occurrence by the local
  pod and never cleared — a stale value only ever delays a check.
- **`syslinux.cfg` can now shrink.** After a purge that deletes
  kernels, the stale entries for those kernels are pruned in the same
  session — including the distro's initial entry once its kernel file is
  purged (accepted: the entry cannot boot once its file is gone).
  `DEFAULT` and foreign entries are never touched.
- **The controller can now correct or delete `next-kernel`.** Before
  M3 it only set the annotation when absent (bootstrap) or after a
  successful stage (anchor). A malformed value, or a value the verified
  repository index does not contain, is corrected to the safe state
  (running → newest local → delete) with an `UpdateGoalCorrected`
  event. Operator pins ahead of staging are unaffected: a value that is
  in the repository index is never corrected — it is staged when the
  window allows.
- **`reboot-eligible` is also cleared when its version file is absent
  from the partition** (stale-sweep extension, §3.10).
- **`make image` now builds for `linux/amd64` and `linux/aarch64`**
  (fails if either fails) and needs qemu/binfmt_misc registered once on
  Linux; the deploy flow is unchanged.
- The `simplek8s-update-plans` ConfigMap is gone; the leader deletes a
  leftover on first cycle.
- `E2E-UPDATE.md` is **redrawn** by this plan: the plan-layer campaign
  (U-cases around plan create/cancel/verify) is rewritten as window
  cases in the new `E2E-WINDOWS.md`; the pending BUG 12 re-run is
  superseded by case W5.
- `TODO.md` items 1, 6, 8, 9, 11 are closed by this plan and leave the
  backlog; item 13 is reduced to its public `v*` publishing half.
- `PLAN-M2.md`'s `/boot/simplek8s/` shorthand is obsolete (SimpleK8s
  does not mount `/boot`); left as-is, corrected here and going forward.
- `deploy/configmap.yaml` gains the four keys, shipped **present with
  the built-in default values** (`reboots.windows: '[]'`,
  `updates.windows: '["@every 12h"]'`, graces `5m`) plus a commented
  example (`'["@daily"]'`) — the reboots opt-in point is explicit.

## 6. Implementation

### 6.1 Modules

| Module | Change |
|---|---|
| `internal/cron` (new) | `Parse`, last-occurrence, `WindowsOpen`; exhaustive table tests. |
| `internal/config` | Four new keys + `Config` fields (parsed `[]Schedule` + two `time.Duration`); validation per §3.2; `updates.check-interval` no longer parsed (decision 20). |
| `internal/features/reboot` | Orchestrator: window gate in the per-candidate admission loop, forced bypass, `QueueHeldWindow` event. |
| `internal/api` | `admit`: `NoWindowsConfigured` (422) for non-forced requests when `reboots.windows` is empty. |
| `internal/features/update` | `update.go`: window master switch in `RunLocal` + persisted check claim (§3.4, decision 21); operator-pin handler + stale-marker sweep (§3.4/§3.7); goal validation & safe-state correction (§3.10); prune hook after purge (§3.9). New `window.go` (leader): window-open enqueue + per-node verification + one-shot stale-ConfigMap delete. **Delete** `plan.go`, `planstate.go` and their tests. |
| `internal/features/update` (`bootloader.go`) | `pruneSyslinuxEntries` (pure block parse + rewrite, mirror of the writer) + temp-dir unit tests (§3.9). |
| `Makefile` | `image` target → buildx for `linux/amd64,linux/aarch64` + host-arch load; binfmt prerequisite documented (§3.11). |
| `internal/nodestate` | New conditional-RMW builds: enqueue (state→`requested` + delete marker, one patch), marker-clear, and check-claim (`update-last-check`, write only if absent/older). |
| `deploy/configmap.yaml` | The four new keys with their built-in defaults (`reboots.windows: '[]'`, `updates.windows: '["@every 12h"]'`, graces `5m`), commented example; remove `updates.check-interval`. |

### 6.2 Unit test matrix

- **`internal/cron`**: every field syntax (`*`, values, lists, ranges,
  steps, combined); dom/dow OR rule; `7`/`0` = Sunday; all named
  schedules; `@every` across midnight and across days; window-open
  boundaries (`t == O`, `t == O+grace` excluded); union of schedules;
  the always-open edge (grace > period); invalid inputs (6-field,
  `@monday`, `@reboot`, bad values, empty).
- **`config`**: valid arrays; `[]`; invalid JSON; one bad entry in a
  good list (whole key rejected, previous kept, warn); grace
  parsing/bounds; defaults when absent (`reboots` → `[]`, `updates` →
  `'["@every 12h"]'`); retired `updates.check-interval` → unknown-key warn.
- **reboot orchestrator**: window closed → non-forced held
  (`QueueHeldWindow`), forced admitted, in-flight untouched; window open
  → normal admission; mixed queue (forced + non-forced) with window
  closed; pause/NotReady gates still win in order.
- **api**: empty `reboots.windows` → non-forced 422
  `NoWindowsConfigured` (per node, partial-batch semantics preserved),
  forced 202; windows set → 202 as today.
- **update**: master switch (empty/closed window → no check at all);
  one check per occurrence (second cycle in the same occurrence → no
  second check; newer occurrence → new check; coincident schedules share
  a claim; failed check consumes the occurrence, no retry); check claim
  via `update-last-check` (written before the fetch; restart with
  occurrence claimed → no re-check; restart with a newer occurrence →
  claim; corrupt stored value → treated as absent; precondition race →
  no clobber); fresh full-mode stage sets marker (anchor-written and
  operator-pre-pinned variants); stage mode does not; pin handler
  (present file → marker + re-point; absent file → no marker; corrupt
  value; mid-reboot node; same-value no-op); stale sweep; enqueue
  preconditions (resting vs in-flight vs `failed`; marker consumed;
  raced operator edits skipped); verification events (applied/mismatch,
  keyed, no auto-retry); one-shot stale-ConfigMap delete (present /
  absent / error); goal validation (malformed → safe-state correction
  without a repo, ungated; file absent + failed check → no correction;
  file absent + value in verified index + staging failed → no
  correction, next occurrence; file absent + value not in verified
  index → correction; safe-state precedence: running present / running
  absent → newest local / empty partition → annotation deleted,
  bootloader untouched); marker-sweep extension (marker file absent →
  cleared, present → kept).
- **bootloader (prune, §3.9)**: entry for our kernel with a missing
  file removed (including the distro's initial-entry shape and its
  in-block comment); top-of-file DOC header preserved verbatim;
  foreign entries (non-matching `KERNEL` path) untouched, file present
  or not; global lines preserved verbatim; the `DEFAULT` block never
  pruned; no purge deletion → no rewrite; rpi `config.txt` untouched.
- **nodestate**: enqueue build writes all four annotations + deletes the
  marker in one patch; precondition failures abort; check-claim build
  (absent → write, older → write, newer → skip, race → abort).

### 6.3 E2E-WINDOWS campaign (new `E2E-WINDOWS.md`, E2E.md conventions)

| # | Case | Trigger | Expect |
|---|---|---|---|
| W1 | Empty `reboots.windows` | non-forced `POST /reboots` | 422 `NoWindowsConfigured` per node; `{"force":true}` executes immediately. |
| W2 | `reboots.windows: '["@every 2m"]'` | POST while closed → open | stays `requested` + `QueueHeldWindow` while closed; admitted on open; an in-flight reboot is not interrupted by close. |
| W3 | `updates.windows` master switch + per-occurrence check | explicit `[]` vs default (absent) vs short campaign window; several occurrences; delete the local pod mid-opening | explicit `[]` → no check activity (no index fetches); absent → the default `@every 12h` applies (verified via config snapshot/log); short window → exactly one index fetch per occurrence (next occurrence → next fetch); pod restart mid-opening → **no** re-fetch, `update-last-check` holds; nothing while closed. |
| W4 | Full auto-update inside windows | new release in repo; `full` mode; windows open | check → stage → anchor → marker → enqueue (marker gone) → M1 lifecycle → `UpdateApplied`; node running the new kernel. |
| W5 | Successive auto-updates (BUG 12 regression) | two releases back-to-back, both applied | both apply; no stale `reboot-state` interference (supersedes the pending BUG 12 re-run). |
| W6 | Operator pin / rollback | set `next-kernel` to a preserved older version | bootloader `DEFAULT` re-pointed, marker set; window open → enqueue → node back on the pinned version (`UpdateApplied`). |
| W7 | Cancel does not resurrect | DELETE a window-enqueued node while `requested` | cleared; **not** re-enqueued at the next window open. |
| W8 | Marker waits for the window | node made eligible while the updates window is closed | marker persists + `UpdateHeldWindow`; enqueued at the next open window. |
| W9 | Leader handover mid-window | delete the leader pod while nodes are queued/eligible | standby recomputes the window from the clock; queued nodes continue, markers are honored; nothing lost. |
| W10 | M1 regression | quick pass: B1 happy path, C1 (PDB), D9 (pause) | M1 semantics unchanged with windows configured. |
| W11 | Syslinux prune | stage releases until the purge deletes a kernel (small `updates.preserve` / usage cap) | entries for the purged kernels (and the distro's initial entry, once purged) are gone from `syslinux.cfg`; `DEFAULT` and foreign lines intact; the node still boots. |
| W12 | Goal validation / safe state | hand-set `next-kernel` to (a) a malformed value, (b) a well-formed ts absent from the repo | (a) corrected promptly to the safe state without waiting for a window; (b) corrected at the next window occurrence; in both: `DEFAULT` re-pointed, `UpdateGoalCorrected` event, and a `reboot-eligible` pointing at an absent version is cleared. |

### 6.4 Milestones

| M | Content |
|---|---|
| M1 | `internal/cron` + the four window config keys + unit tests (no behavior change). |
| M2 | Reboot window gate + API rejection + unit tests. |
| M3 | Update rework: master switch (one check per occurrence, claim persisted in `update-last-check`), pin handler, enqueue, per-node verification, stale-marker sweep (+ §3.10 extension), goal validation & safe-state correction, syslinux prune, stale-ConfigMap delete, `updates.check-interval` retired; plan layer removed. |
| M4 | E2E-WINDOWS campaign on the 3-node test VMs (W1–W12). |
| M5 | Docs: `E2E-WINDOWS.md` results, `E2E-UPDATE.md` redrawn, `TODO.md` (items 1/6/8/9/11 out, 13 reduced), README, `deploy/configmap.yaml` example; PLAN-M3 marked shipped. |
| M6 | Multi-platform image build: `make image` via buildx for `linux/amd64,linux/aarch64` + host-arch load, binfmt prerequisite documented (§3.11). No Go code; independent of M1–M5. |

## 7. Deferred

- TODO item 10 (leader-centralized check + distribution) is untouched by
  this plan and stays deferred — the per-node check stays per-node; M3
  only gates it.
- The pod split (TODO item 3) remains the home of any future hardening
  of the boot-partition mount/write path.
- TODO item 12 (BUG 12) is closed by this plan: the plan layer it lived
  in is removed; W5 is its end-to-end regression.
- **TODO 13 remainder — public `v*` image publishing + CI**: a registry
  (none exists yet), the tag scheme (timestamp / incremental / semver —
  undecided), the build-check workflow, and the release push. Explicitly
  out of M3 (maintainer decision, 2026-09-09); §3.11 carries the notes
  for when it is built (LFS checkout, binfmt, provenance).
- **arm64 E2E**: no arm64 test node exists yet; when one is stood up,
  deploy the aarch64 image and run one full auto-update (W4-shaped).
