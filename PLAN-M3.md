# PLAN-M3 — simplek8s-controller: maintenance windows & the update reboot loop

> **Planning index**
> - [PLAN-M1.md](PLAN-M1.md) — node reboots (shipped)
> - [PLAN-M2.md](PLAN-M2.md) — distro updates (implemented; E2E campaign in progress)
> - [PLAN-M3.md](PLAN-M3.md) — maintenance windows + update reboot loop (this document; in planning)
> - [E2E.md](E2E.md) — reboots E2E campaign (28/28 PASS)
> - [E2E-UPDATE.md](E2E-UPDATE.md) — updates E2E campaign (redrawn by this plan)
> - [E2E-WINDOWS.md](E2E-WINDOWS.md) — windows E2E campaign (created in M5)

Phase: **PLAN** (design & planning). Status: v1 — all design decisions
closed with the maintainer (2026-09-09), pending implementation.

Terminology: the node's boot partition (vfat, `vda1`) is mounted by the
controller pod at a scratch mountpoint, used, and unmounted — SimpleK8s
does not mount `/boot` at runtime. This document says **boot partition**
and refers to the release directory at its root as `simplek8s/`.

## 1. Purpose

The third feature, built on the shipped M1 reboot machinery and the M2
update engine. It closes three TODO items that interlock:

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

Together these make the controller a safe unattended mechanism: nothing
reboots (and no update work happens) outside the windows an operator has
explicitly configured.

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
| Local pod | **Stale-marker sweep → delete `reboot-eligible`** (M3) | node quiescent (`running == next-kernel`) with a marker present. |
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

Existing events are kept (`QueueHeldNotReady`, `PDBBlocked`,
`RebootCompleted`, `UpdateStagingSkipped`, …). Structured logs:
`Info` on window open→closed transitions (rate-limited, per feature), on
each enqueue, and on each pin re-point; `Warn` on invalid config
(last-valid-wins), on mismatch, on the one-shot stale-ConfigMap delete.
The `NoWindowsConfigured` rejection is returned in the API response and
logged at `Info` (an expected operator-visible condition).

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
- The `simplek8s-update-plans` ConfigMap is gone; the leader deletes a
  leftover on first cycle.
- `E2E-UPDATE.md` is **redrawn** by this plan: the plan-layer campaign
  (U-cases around plan create/cancel/verify) is rewritten as window
  cases in the new `E2E-WINDOWS.md`; the pending BUG 12 re-run is
  superseded by case W5.
- `TODO.md` items 1, 9, 11 are closed by this plan and leave the
  backlog.
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
| `internal/features/update` | `update.go`: window master switch in `RunLocal` + persisted check claim (§3.4, decision 21); operator-pin handler + stale-marker sweep (§3.4/§3.7). New `window.go` (leader): window-open enqueue + per-node verification + one-shot stale-ConfigMap delete. **Delete** `plan.go`, `planstate.go` and their tests. |
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
  absent / error).
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

### 6.4 Milestones

| M | Content |
|---|---|
| M1 | `internal/cron` + the four window config keys + unit tests (no behavior change). |
| M2 | Reboot window gate + API rejection + unit tests. |
| M3 | Update rework: master switch (one check per occurrence, claim persisted in `update-last-check`), pin handler, enqueue, per-node verification, stale-marker sweep, stale-ConfigMap delete, `updates.check-interval` retired; plan layer removed. |
| M4 | E2E-WINDOWS campaign on the 3-node test VMs (W1–W10). |
| M5 | Docs: `E2E-WINDOWS.md` results, `E2E-UPDATE.md` redrawn, `TODO.md` (items 1/9/11 out), README, `deploy/configmap.yaml` example; PLAN-M3 marked shipped. |

## 7. Deferred

- TODO item 10 (leader-centralized check + distribution) is untouched by
  this plan and stays deferred — the per-node check stays per-node; M3
  only gates it.
- The pod split (TODO item 3) remains the home of any future hardening
  of the boot-partition mount/write path.
- TODO item 12 (BUG 12) is closed by this plan: the plan layer it lived
  in is removed; W5 is its end-to-end regression.
