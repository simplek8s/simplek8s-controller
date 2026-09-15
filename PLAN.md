# PLAN — simplek8s-controller

> The single living design doc: shipped behavior + the current plan.
> Consolidated 2026-09-11 from `PLAN-M1.md` (reboots, shipped),
> `PLAN-M2.md` (updates, implemented), `PLAN-M3.md` (windows, in
> planning, v4) and the E2E campaign files (`E2E.md`,
> `E2E-UPDATE.md`). `PLAN.FIXME.md` (implementation deviations found
> against a real cluster) stays in git history only. Full historical
> text: `git log --follow` / `git show <commit>:PLAN-M2.md`.

**How to read this file.** Sections are numbered and the numbers are
stable — jump with `grep -n '^### 3.4' PLAN.md` instead of reading the
whole file. Cross-references use section numbers (§3.4). Era tags used
throughout: **M1** = reboots, **M2** = updates, **M3** = windows. A
reference like "M2 §3.5" points at the historical plan in git, e.g.
`git show HEAD~1:PLAN-M2.md`.

> **Index**
>
> | § | Title |
> | --- | --- |
> | 1 | Purpose & status |
> | 1.1 | This document (status per era) |
> | 1.2 | Shipped baseline (M1 reboots, M2 updates) |
> | 1.3 | Shipped plan (M3: windows, update reboot loop, boot-partition hygiene) |
> | 2 | Constraints |
> | 3 | Design — 3.1 window model · 3.2 config keys · 3.3 cron parser · 3.4 updates rework · 3.5 reboots window gate · 3.6 writer discipline · 3.7 change-triggered reconciliation · 3.8 observability · 3.9 bootloader prune (grub + syslinux legacy) · 3.10 `next-kernel` validation · 3.11 multi-platform build |
> | 4 | Decision log (per era; §4.3 active) — 4.1 M1 · 4.2 M2 · 4.3 M3 (numbers restart per era) |
> | 5 | Behavior changes & migration |
> | 6 | Implementation — 6.1 modules · 6.2 unit test matrix · 6.3 phases |
> | 7 | E2E — 7.1 conventions · 7.2 reboots · 7.3 updates · 7.4 windows (W1–W17, 17/17 PASS) |
> | 8 | Deferred |
> | 9 | Risks & safety notes |

## 1. Purpose & status

### 1.1 This document (status per era)

| Era | Content | Status |
| --- | --- | --- |
| M1 | Node reboots (state machine, drain, orchestrator, API) | Shipped; E2E 28/28 PASS (§7.2) |
| M2 | Distro updates (signed check, staging, `next-kernel`) | Implemented; E2E campaign in progress (§7.3) |
| M3 | Maintenance windows, update reboot loop, boot-partition hygiene | Shipped 2026-09-11 (W1–W17 17/17 PASS, builds `f3b329a`/`b1c6da5`, 34 decisions) |

### 1.2 Shipped baseline

**Reboots (M1).** A privileged DaemonSet plus a single global
orchestrator (leader Lease election) reboots nodes on demand through
the HTTP API (`POST/GET/DELETE /api/v1/reboots[/{node}]`). Per-node
state is a four-annotation JSON state machine on the Node
(`reboot-state`: `requested → draining → rebooting →
completed|failed`), with PDB-aware drain, a global queue (workers
before control planes, at most one CP reboot simultaneously), one
Event per state transition, and a `force` bypass. The local pod is the
only component that reboots the machine (host PID namespace). Full
design in git (`PLAN-M1.md`).

**Updates (M2).** The same pod can check a signed release repo
(GPG + sha256 verification), stage a new kernel on the boot
partition, and — while a `reboots.windows` window is open — reboot
the node into it. Staging runs by default (`updates.windows`
`["@every 12h"]`); automatic reboots are opt-in via `reboots.windows`.
The annotation is `next-kernel` (target) — per-node check throttling
was in-memory
(`update-last-check` is new in M3, §3.4). The leader-side plan layer (per-version all-or-nothing
plans, plan ConfigMap) is **abolished** by the active plan (§3.4).
Full design in git (`PLAN-M2.md`).

### 1.3 Shipped plan (was PLAN-M3)

The third feature, built on the shipped M1 reboot machinery and the M2
update engine. It closes five TODO items plus the build half of a sixth:

- **TODO 1 — reboot maintenance windows.** Cron-style time windows,
  configured in the flat ConfigMap, that gate **when work may start**.
  Two independent window lists, one per concern: `reboots.windows`
  gates **every non-forced reboot** — M1 API reboots and the
  update-driven ones alike (§3.4); `updates.windows` gates the update
  *work* (checks, downloads, staging).
- **TODO 11 — the update-driven reboot loop.** A node is
  reboot-eligible **by derivation** — well-formed `next-kernel`,
  non-quiescent, no completed reboot attempt, goal file locally
  present (no marker annotation, §3.4), open `reboots.windows`. While
  `reboots.windows` is open, the node's **local pod** enqueues it into
  the M1 reboot queue. The M2 plan layer
  (the `simplek8s-update-plans` ConfigMap, per-version all-or-nothing
  plans, plan cancel) is **abolished**: pending update state is derived
  from the node's own annotations, execution is the M1 queue itself,
  and verification is per-node.
- **TODO 9 — operator-pinned `next-kernel` is honored.** When the
  operator points `next-kernel` at a well-formed version, the local pod
  re-points the bootloader `DEFAULT` (ungated — §3.7) and, in `full`
  mode, the node becomes reboot-eligible by derivation and reboots at
  the next `reboots.windows` window (§3.4) — the change-triggered
  bootloader reconciliation designed in M2 §3.5 but not yet
  implemented.
- **TODO 6 — bootloader stale-entry cleanup.** The bootloader entry writer
  only ever appends, so the bootloader config on the boot partition
  (`grub/grub.cfg` on GRUB images, `syslinux.cfg` on legacy syslinux
  images) grows one entry per staged kernel. Entries for kernels the
  purge has already deleted are pruned in the same mounted session (§3.9).
- **TODO 8 — `next-kernel` validation & safe-state recovery.** The
  annotation is the node's boot goal and it is **file-first**: no
  controller writer sets a value whose file is not on the boot partition
  (audited, §3.10). The residual stuck case — a malformed value, or a
  value the repository can no longer satisfy — is corrected back to a
  **safe state** instead of being left dangling (§3.10).
- **TODO 13 (build half) — multi-platform image build.** The controller
  image is built for **both** `linux/amd64` and `linux/aarch64` by
  `make image` (§3.11); the public `v*` publishing half stays in the
  backlog (no registry, tag scheme undecided, §8).

Together these make the controller a safe unattended mechanism: no
reboot starts and no update work happens outside the windows an
operator has explicitly configured, auto-reboots (full mode) are
opt-in via `reboots.windows` (§5), and the boot partition stays
bounded and consistent over time.

## 2. Constraints

- **Stdlib only.** The two dependencies M2 added are the ceiling. A
  third (a cron library, e.g. `robfig/cron`) was explicitly rejected: the
  schedule set is small enough to hand-roll with exhaustive tests
  (§3.3).
- **Flat-key ConfigMap** (M2 §3.2): values are strings; a list is a
  JSON array of strings inside one flat key. Last-valid-wins on invalid
  values; unknown keys warned.
- **UTC always.** No timezone key (same rule as M2 §2).
- **KISS.** No window ranges/`end` fields, no weekday aliases, no
  maximum-duration guards, no new API endpoints. One new node
  annotation (`update-last-check`, the occurrence claim, §3.4) — and the M2
  `reboot-eligible` annotation is **abolished** (eligibility is
  derived, §3.4).
- **M1 invariants untouched.** The window is one more gate on *start*.
  Serialization, workers-before-CP ordering, the CP gate, PDB (for
  non-forced), drain timeout, no-effect grace, and
  `reboots.on-failure` pause/continue all keep their M1 semantics. An
  in-flight reboot is **never** interrupted by a closing window.
- **Stateless windows.** Window openness is computed from the clock and
  the ConfigMap every cycle — nothing is persisted (the
  `update-last-check` claim, §3.4, is per-node check bookkeeping, not
  window state). A pod restart or a leader crash/handover changes
  nothing: each recomputes the same answer from the clock. Clock skew
  and ConfigMap propagation delay can still make the leader and a pod
  straddle a window edge in opposite directions; the designed outcome
  is enqueue-then-hold (the node waits `requested`), never a missed
  gate (§3.4).

## 3. Design

### 3.1 Window model

A **window list** is a set of schedules plus a grace period:

- **Schedule** — a recurrence, from Vixie cron as documented in
  FreeBSD `crontab(5)` plus the controller's `@every` extension
  (§3.3): a 5-field cron expression (names allowed), a named schedule
  (`@hourly`, `@daily`, …), `@every <duration>`, or `@<seconds>`.
  Each schedule produces occurrences on the UTC timeline.
- **Grace** — `reboots.window-grace` / `updates.window-grace` (duration,
  default `5m`) is how long a window stays open after each occurrence.
- **Openness** — at time `t` (UTC), a window list is **open** iff any of
  its schedules has an occurrence `O` with `O ≤ t < O + grace`. The
  union over schedules makes multiple windows work: any open window
  suffices.

Consequences:

- **Window = start gate.** Only the *start* of a reboot (admission
  `requested → draining`) is gated. A reboot already draining/rebooting
  runs to completion even if the window closes mid-flight. Update work
  follows the same principle: a check/stage starts only while
  `updates.windows` is open and then runs to completion (§3.4).
- **Queue waits, never fails.** A non-forced node that is `requested`
  while the window is closed stays `requested` — no timeout, no error —
  until a window opens. Eligible update nodes wait *before* the queue:
  while `reboots.windows` is closed the local pod simply does not
  enqueue its node — the node stays non-quiescent with its M1 state
  absent, and is picked up at the next open window (§3.4). No zombie
  states by construction.
- **Explicit empty list = feature OFF** (supersedes M2 §3.2's
  "absent = no restriction" — see decision 6). Key absent → the built-in
  default applies, and the defaults differ per feature (§3.2):
  `reboots.windows` → `[]` (OFF); `updates.windows` → `["@every 12h"]`.
  - `reboots.windows` empty → **no non-forced reboots execute** —
    operator or update-driven — and non-forced `POST /reboots` is
    rejected at admission (§3.5); no local pod ever enqueues either.
    Forced reboots always execute immediately.
  - `updates.windows` explicit `[]` → the update *work* is **fully
    inert**: no release checks, no downloads, no staging. Per-node verification (purely
    observational) stays on (§3.4); reboots remain governed by
    `reboots.windows` regardless.
- **Edge case (documented, not validated — KISS):** if the grace is
  longer than the interval between occurrences (e.g. `@hourly` with a
  2h grace), the window is always open. That is an operator error; the
  controller does not refuse it.

### 3.2 Configuration keys

New keys in the flat ConfigMap `simplek8s/simplek8s-controller`
(M2 §3.2 form; the existing table is unchanged):

```yaml
data:
  # ... existing engine.*/reboots.*/updates.* keys ...
  reboots.windows: '[]'                  # built-in default (OFF); e.g. '["@daily", "0 7 * * 3"]'
  reboots.window-grace: 5m
  updates.windows: '["@every 12h"]'      # built-in default; '[]' turns updates fully off
  updates.window-grace: 5m
```

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `reboots.windows` | JSON array of schedule strings | `[]` (empty) | Windows that gate **every non-forced reboot**: M1 API reboots and the update-driven (pod-side) enqueue alike. Empty ⇒ non-forced reboots never execute (nodes stage and wait, non-quiescent); forced reboots always do. |
| `reboots.window-grace` | duration | `5m` | How long a `reboots.windows` occurrence stays open. |
| `updates.windows` | JSON array of schedule strings | `'["@every 12h"]'` | Windows for the update *work*: the per-node check/staging loop runs only while open. An explicit `[]` ⇒ no update work at all. The default preserves M2's 12h check cadence; auto-reboots are governed by `reboots.windows` (a new M2-`full` install must set it to keep auto-rebooting — §5). |
| `updates.window-grace` | duration | `5m` | How long an `updates.windows` occurrence stays open. |

Parse rules (one rule for all keys, M2 §3.2):

- The value must parse as a JSON array of strings, and **every** string
  must parse as a schedule (§3.3). Any failure → the whole key is
  invalid → the last valid value is kept (or the default) + one warning.
  A partially valid list is not accepted.
- `[]` is valid and means OFF for both features. Key absent → the
  built-in default: `reboots.windows` → `[]` (OFF); `updates.windows` →
  `'["@every 12h"]'`.
- The `*-window-grace` keys use the existing duration parser (must be > 0); an invalid value keeps the last valid one (or the default) + warn, like any other key.

### 3.3 Cron parser (`internal/cron`, hand-rolled, stdlib)

A small new package with two responsibilities — parsing and window
evaluation — and no state:

```go
type Schedule struct{ /* parsed form */ }
func Parse(spec string) (Schedule, error)
func (s Schedule) lastOccurrenceAtOrBefore(t time.Time) (time.Time, bool)
func WindowsOpen(specs []Schedule, grace time.Duration, now time.Time) bool
```

`lastOccurrenceAtOrBefore` returns `ok == false` when the schedule has
**no occurrence** at or before `t` within the bounded search horizon
(e.g. `0 0 30 2 *` — February 30 — parses fine and never occurs); a
schedule with no occurrence makes its window **closed**. The contract is
explicit: a zero `time.Time` never stands in for an occurrence.

Accepted set — **Vixie cron as documented in FreeBSD `crontab(5)`,
plus the explicit `@every <duration>` extension, nothing else**:

| Form | Examples | Meaning |
| --- | --- | --- |
| 5-field cron | `0 7 * * 3`, `*/10 * * * *`, `0 0 1-15 * 1-5`, `0 22 * * mon-fri`, `5 4 * * sun`, `0 0 1 jan *` | minute hour dom month dow, UTC. Standard field syntax: `*`, values, lists `a,b`, ranges `a-b` (inclusive), steps `*/n` and `a-b/n`; lists and ranges may mix (`1-3,7-9`). Month and dow names: first three letters, case-insensitive (`jan`–`dec`, `sun`–`sat`), usable inside lists and ranges (`mon-fri`, `jan,apr,jul,oct`). |
| Named | `@hourly`, `@daily`/`@midnight`, `@weekly`, `@monthly`, `@yearly`/`@annually`, `@every_minute`, `@every_second` | The fixed Vixie expressions: `0 * * * *`, `0 0 * * *`, `0 0 * * 0`, `0 0 1 * *`, `0 0 1 1 *`; `@every_minute` = `* * * * *`; `@every_second` = `@every 1s` (open for any grace ≥ 1s — always in practice). |
| `@every <duration>` | `@every 30m`, `@every 2h` | Occurrences every duration, midnight-UTC anchored (below). Go duration syntax (`time.ParseDuration`: `ns`/`us`/`ms`/`s`/`m`/`h`; no `d`/`w`). |
| `@<seconds>` | `@300` | The Vixie numeric form ("that many seconds after completion of the previous run"), accepted as an alias for `@every <N>s` (`@300` = `@every 5m`). Documented semantic delta: Vixie anchors at completion and never overlaps; ours anchors at midnight UTC like `@every` and may overlap a slow run. For window openness only occurrence timestamps matter, so the two coincide. |

Semantics pinned (all tested):

- **dow**: `0` and `7` are both Sunday; `1`=Mon … `6`=Sat.
- **dom/dow OR rule** (vixie-cron/K8s): when *both* dom and dow are
  restricted (neither is `*`), a day matches if **either** matches —
  `0 0 13 * 5` fires on every 13th **and** on every Friday.
- **`@every` reference point** (controller-specific semantics — the
  `robfig/cron` `@every` anchors at process start, which we deliberately
  do not replicate): occurrences are `00:00 UTC (today) + k·duration`.
  Openness = `(now − todayMidnightUTC) mod duration < grace`. A fixed
  reference makes openness identical on every pod and leader across
  handovers (a "since process start" reference would differ per pod and
  is rejected).
- **5-field last occurrence**: bounded backward search from `now` (at
  most ~366 days for yearly schedules, extended to 4 years + 1 day for
  schedules that can only occur on Feb 29); the window is open iff
  `now − lastOccurrence < grace`; **no occurrence found ⇒ window
  closed** (the `ok == false` contract above — a syntactically valid
  schedule with impossible combinations, e.g. `0 0 30 2 *`). Cheap at
  engine cadence; memoization per minute is an implementation detail,
  not a requirement.
- **Rejected**: `@reboot` (startup-anchored: it has no stable occurrence
  set, so stateless clock evaluation cannot express it — rejected, not
  mapped; "always open" is the operator expressing via grace, not a
  schedule); 6-field (seconds) expressions; weekday `@` aliases
  (`@monday` … `@sunday` are Quartz-isms, not Vixie → invalid); unknown
  `@` names; empty strings; out-of-range field values; and `?` (not in
  the Vixie field syntax — rejected with a parse error; Kubernetes
  happens to accept it as a `*` alias, which is why this is called out).

The window forms proposed before this plan (time-of-day ranges
`22:00-06:00`, bare intervals `30m`) are **not** supported: duration
comes from the grace key, periodicity from `@every`/cron. One mechanism
instead of four.

### 3.4 Updates: master switch, pod-side enqueue, per-node verification

This section replaces M2 §3.8/§3.9/§3.10 (plan, plan durability,
plan verification).

**Master switch (local pod).** The per-node check/staging loop
(`update.RunLocal`) runs a window check **first**: `updates.windows`
empty or no window open at `now` → skip the cycle (no HTTP fetch, no
partition mount, rate-limited log).
So the effective dial is:

| `updates.windows` | Behavior |
| --- | --- |
| explicit `[]` | **fully inert** — not even checks. |
| non-empty (default `["@every 12h"]`) | check + stage only while a window is open; staging itself (mount, download, verify, extract, bootloader, purge) runs as one unit inside the window. Once started inside an open window, the run completes even if the window closes mid-flight (start-gate only, like reboots — §3.1); what is gated is the *start*, never the in-flight run. |

**Scope of the gate.** `updates.windows` gates the
*update work* only: the periodic check and the staging of new versions
(network + partition). The following are **always ungated** — they
never touch the release repository: the change-triggered bootloader
reconciliation (§3.7, including operator pins and goal corrections),
the M1-state re-arm (below), and per-node verification (leader,
observational, below). So an operator pin re-points `DEFAULT` — and, while a
`reboots.windows` window is open, re-arms a reboot — even with
`updates.windows: []`. With `updates.windows: []` the reconciliation
still re-points (the operator's own edit must be honored) but
check/stage never run.

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
of pod lifetime (schedules with coincident occurrences share the claim; offset
occurrences inside one grace each check — `["@daily","@hourly"]` ≈ 25 checks/day
by construction).
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

**Reboot eligibility — derived, no marker (decisions 12–14, 29–31).**
M2's `reboot-eligible` annotation is **abolished** (leftover cleanup,
below): `next-kernel` is the only source of truth for *which* version,
and *whether a reboot is pending* is derived.

**Three notions of "valid" — used precisely in this plan.**
*Well-formed* — the value matches the version grammar.
*Repository-valid* — the value is in the verified index of the node's
effective repository. *Locally present* — the kernel file exists on the
boot partition. Where a rule says "valid", it means well-formed.

A node is **reboot-eligible** iff, all at once:

1. `next-kernel` is **well-formed**;
2. the node is **non-quiescent** (`running != next-kernel`);
3. its M1 reboot state is **absent** — never `completed`, never
   `failed`, never in flight; and
4. the goal kernel file is **locally present** on the boot partition.

Rule 4 is observable **only by the local pod** — which is why the
enqueue is pod-side (decision 31, below). The M1 state is the
*consumed token* (decision 12): `absent` means "the current
`next-kernel` value has not yet been rebooted into"; a finished attempt
leaves `completed`, which is what keeps a broken version from being
auto-retried (rule 4 + the verification below). Rule 1 keeps `stage`
operator-driven: a staged or pinned node stays non-quiescent and only
reboots when the operator asks (M1 API).

**Re-arming — the local pod clears the M1 state** (decision 14), in
the same ungated, change-triggered path as the bootloader re-point
(§3.7), when either:

1. `next-kernel` **changes to a well-formed version whose file is
   present on the partition** (a fresh-stage anchor, an operator pin to
   a local version, or the pod's own goal correction, §3.10); or
2. **staging completes a version `V` while `next-kernel == V`** (the
   pre-pinned case: the annotation led, the file arrived later).

A pin *ahead of staging* (file not yet present) does **not** re-arm
**and is not enqueue-eligible** (rule 4) — a node is never rebooted
into a kernel that is not on its partition, and never wastes a reboot
waiting for one. Re-arming is what lets successive updates reboot:
without it, the previous update's `completed` state would block the next
one (W5). Re-arm clears `completed` only — on `absent` there is nothing
to clear (the RMW is skipped), and `failed` is never cleared: a failed
node exits only via operator `DELETE` (below, decision 30), which
returns it to `absent` for re-evaluation.

**`failed` and cancel (decision 30).** `failed` stays an
operator-alarm state: no one ever auto-enqueues a `failed` node.
`DELETE /reboots/<node>` clears the M1 state (→ absent) — for an
update-eligible node that means **defer, not abandon**: the node is
re-enqueued at the next open `reboots.windows`. Explicit API contract:
`DELETE` cancels the **currently queued attempt**; it does not suppress
the automatic reboot intent — the intent lives in `next-kernel`, not in
the queue entry. Repeated `DELETE`s while the window is open simply
defer indefinitely (the node is re-enqueued after each one): that is the
operator driving the loop, and the controller provides no suppression
state. To abandon the intent: reboot into the pinned version manually
(the node goes quiescent) or re-pin `next-kernel` to `running`.

**Reboot enqueue (local pod, replaces `maybeStartPlan`).** Every
engine cycle, **only while `reboots.windows` is open**: for **its own
node**, if it is reboot-eligible (all five rules above — including the
file-presence check only the pod can make), a single conditional RMW
writes the same four-annotation state the M1 API writes —
`reboot-state := requested` with a controller-issued request
(`requestedBy: "controller"`, fresh req id, `force: false`). From that
moment the node is fully M1-managed: visible in `GET /reboots`,
governed by serialization/PDB/CP-ordering in the leader's orchestrator
(unchanged), and cancellable with `DELETE /reboots/<node>` (defer,
above — there is no plan cancel). Because the pod enqueues only while
the window is open, the orchestrator's own `reboots.windows` gate
(which applies to this non-forced request like any other — no
controller exemption, decision 29) is open by construction and
admission is immediate; if the window closes in the interim the node
simply waits `requested` (queue waits, never fails). While the window
is closed the pod does **not** enqueue: an eligible node stays
non-quiescent with its state absent (rate-limited `UpdateHeldWindow`
event, keyed per node), and is picked up at the next open window.
Precondition failure on the fresh read (value changed, state changed,
operator edit) → skip, no clobbering.

**Why pod-side (decision 31).** File presence on the boot partition is
observable only by the local pod. A leader-side scan could enqueue a
node whose goal file is absent — an operator pin ahead of staging, or a
`DELETE`-deferral on a not-yet-staged pin — and the node would drain,
reboot into the untouched old `DEFAULT`, come back non-quiescent with
`completed`, and emit a spurious `UpdateMismatch` before the defensive
re-staging healed it. Pod-side, verify-file → re-point `DEFAULT` →
enqueue is one ordered code path in one process: the cross-actor race
cannot exist. The enqueue is one conditional RMW per node — no
cross-node coordination, so centralization bought nothing; M1's
serialization stays in the leader's orchestrator at admission.

**Ordering invariant (implementation, tested).** The local pod
performs, in this order and one code path: (1) verify the goal file is
present on the partition, (2) mount, (3) re-point `DEFAULT`, (4)
durable write (sync) + unmount, (5) **only then** the M1-state RMW
(re-arm or enqueue). No state RMW may land before the bootloader write
it enables. Staging: the re-arm happens after the session's bootloader
write, inside the same mounted session's success path. A crash between
(4) and (5) is closed at pod start: the pod-start reconciliation (§3.7)
re-points if needed (compare); the re-arm re-runs only if the re-point
changed `DEFAULT` or the goal value changed — a no-op observation never
re-arms (decision 34) — both are precondition-checked and idempotent.

**Per-node verification (leader, replaces plan verification).** Every
cycle, for each node with a well-formed `next-kernel`. **Always on** — purely
observational (no network, no disk): it runs even with
`updates.windows: []`, which is *not* part of the "fully inert" scope,
and with `updates.windows: []` (events only):

- `running == next-kernel` → **quiescent**; nothing to do. The
  transition into quiescence after a reboot emits
  `UpdateApplied` (rate-limited, keyed per node+version). The leader
  tracks the last-seen non-quiescent set in memory; after a handover
  the first cycle may re-emit — `UpdateApplied` is idempotent per
  node+version key, so duplicates are bounded and harmless.
- `running != next-kernel` and the node's M1 state is `completed` (it
  came back and rested) → `UpdateMismatch` event + warn log (keyed per
  node, rate-limited). **No automatic retry** — consistent with
  M2's accepted consequence "a failed version is never
  auto-retried". The operator reboots via the M1 API (non-forced, so it
  waits for `reboots.windows`; or force), or re-pins.

**What disappears** (recorded, decision 11):

- The `simplek8s-update-plans` ConfigMap and all plan state
  (`planstate.go`); the leader performs a **best-effort delete** of a
  leftover `simplek8s-update-plans` ConfigMap: `NotFound` → done
  (remembered for the pod's lifetime); any other error (e.g. RBAC
  `Forbidden` while the updated role is still propagating during a
  rollout) → `Warn` log + **retry once per leadership acquisition**
  until it succeeds — a once-per-lifetime attempt could be lost to
  exactly that race and the ConfigMap would linger (inert, but the
  migration should self-clean). The RBAC `configmaps` role **gains the
  `delete` verb** (M2 §3.13 shipped `[get, list, create, update]`;
  without it the delete 403s on every cluster and the "self-cleaning
  migration" would never clean).
- The M2 `reboot-eligible` annotation: never read or written by M3.
  The local pod **deletes any stale leftover at pod start** (one-shot,
  best-effort, same discipline as the ConfigMap delete).
- Per-version all-or-nothing plans, two-phase cancel/reset,
  "one active plan at a time", and the plan-level verify.
- The BUG 12 fix (`resetStaleRebootState`) lived in the plan layer; with
  no plan object there is no plan to poison. The pending BUG 12 E2E
  re-run is **superseded** by the windows campaign's
  successive-auto-update
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
  always queue (and, as today, bypass PDB; they still serialize through
  the orchestrator queue — "immediate" means ungated, not unqueued).
  The `NoWindowsConfigured` check is evaluated **first**, before the
  per-node checks (node exists / `Ready` / controller pod `Running` /
  re-requestable state, M1 §3.9): it is global and cheap, so an empty
  `reboots.windows` wins over per-node outcomes (an unknown node with
  empty windows gets the 422, not a 404 — a deliberate M1 admission-order
change, §5).
- **Orchestrator admission** (`orchestrator.runOrchestrator`
  phase 2): the existing gates keep their order and semantics
  (pause-on-failure → queued-NotReady hold → slots → per-node PDB).
  One more gate joins the per-candidate loop: a **non-forced** candidate
  is admitted (`requested → draining`) only while a reboot window is
  open; otherwise it stays `requested` and the loop emits the
  rate-limited `QueueHeldWindow` event (the `QueueHeldNotReady`
  analogue) and does not admit further non-forced candidates this cycle.
`now` is sampled once per cycle so every candidate sees the same openness.
  A **forced** candidate is admitted regardless of window state. In-flight
  nodes (`draining`/`rebooting`) are managed in phase 1 exactly as today
  — the window never touches them.

### 3.6 Writer discipline (updated)

The annotation has three writer classes; every controller write remains
a conditional RMW on the M1 nodestate primitives (read → precondition →
patch carrying the read `resourceVersion`). M2 §3.5's table, as
affected by M3:

| Writer | Action | Precondition |
| --- | --- | --- |
| Local pod | Bootstrap `next-kernel` (unchanged, M2) | annotation absent. |
| Local pod | Fresh-stage anchor `next-kernel := V` (unchanged, M2) | value at write time `== running` or absent. |
| Local pod | **State re-arm: clear the M1 reboot state** (M3, §3.4) | `next-kernel` changed to a well-formed version whose file is locally present on the partition, or staging completed a version `V` with `next-kernel == V`; state `completed` at write time (`absent` → RMW skipped, nothing to clear; never `failed`); re-checked on the fresh read. |
| Local pod | **Pod start: delete a leftover `reboot-eligible`** (M3, migration) | the M2 marker annotation is present (never written again). |
| Local pod | **Goal correction → `next-kernel := <safe state>` or delete** (M3, §3.10) | value malformed (node state, ungated), or file absent and the verified index does not contain the value (window occurrence); re-checked on the fresh read. |
| Local pod | **Check claim → `update-last-check := O`** (M3, §3.4) | window open; stored value absent or older than the claimed occurrence `O`; re-checked on the fresh read. |
| Local pod | **Reboot enqueue: `reboot-state := requested`** (M3, §3.4) | its node reboot-eligible — well-formed `next-kernel`, non-quiescent, M1 state **absent**, goal file **locally present** (rule 4); `reboots.windows` open; re-checked on the fresh read. |
| ~~Leader~~ | ~~Two-phase plan cancel/reset~~ (M2, **abolished**) | — |
| Operator | Any `next-kernel` edit (unchanged) | none — the operator wins by definition. |

### 3.7 Change-triggered bootloader reconciliation (TODO 9)

Implements M2 §3.5's designed-but-unimplemented discipline: the
local pod ensures the bootloader `DEFAULT` equals `next-kernel`,
**whatever changed the annotation**, but change-triggered — the boot
partition is vfat and must not be mounted every 2 s. It is **always
ungated** — it runs regardless of `updates.windows`: it never touches
the release repository, only the partition, and it exists to honor the
operator's own edits (a manual pin/rollback is always honored).
Mount + write happen only:

1. **Pod start** — the initial applied value is unknown; one mount,
   compare, re-point if needed.
2. **Value change** — `next-kernel` differs from the last value the pod
   applied (tracked in memory). On change to a **well-formed** version:
   - version **locally present** → re-point `DEFAULT` to it and
     **re-arm the M1 state** (§3.4) — the node is reboot-eligible
     and the pod enqueues it at the next open `reboots.windows`
     (ordering invariant, §3.4); with `reboots.windows` closed
     nothing is enqueued (the operator reboots via the M1 API when
     desired).
   - version **not locally present** → do not re-point (it cannot boot)
     and do not re-arm; the node is also not enqueue-eligible (rule 4,
     §3.4) — no wasted reboot while it waits. The existing defensive
     re-staging path (`maybeStage`) fetches it when the window allows,
     and staging completion for a pinned value re-arms once the file is
     local (§3.4). A node is never rebooted into a kernel that is not
     on its partition.
3. **Folded into staging** — staging already has the partition mounted;
   the default is set in the same session (no extra mount). This is how
   a fresh stage and a re-stage converge.

Steady state: zero disk I/O (the value comes from the listed Node
object). Hand-edits of the bootloader files remain out of contract —
the annotation is the source of truth. This is what makes manual
rollback (TODO item 4) and operator pins actually honored without
touching the boot partition by hand.

**Crash between correction and re-point.** A malformed-value correction
(§3.10) lands the annotation RMW first and the bootloader re-point
follows via this path — the reverse of the §3.4 ordering invariant.
Benign: the corrected goal is well-formed, and the enqueue path itself
re-verifies file presence and re-points before any state RMW, so no
reboot can consume the stale `DEFAULT` in between; the next
change-triggered cycle (or pod start) completes the re-point.

### 3.8 Observability

Events (namespace `default`, rate-limited per key as in M1/M2):

| Event | Source | Meaning |
| --- | --- | --- |
| `QueueHeldWindow` | leader (reboot) | non-forced queued nodes waiting for a `reboots.windows` window (keyed per node, rate-limited). |
| `UpdateHeldWindow` | local pod (update) | its node is reboot-eligible and waiting for a `reboots.windows` window (keyed per node). |
| `UpdateApplied` | leader (update) | node quiescent on `next-kernel` after a reboot (keyed per node+version). |
| `UpdateMismatch` | leader (update) | node came back `running != next-kernel`; no auto-retry (keyed per node). |
| `UpdateGoalCorrected` | local pod (update) | `next-kernel` corrected to its safe state (malformed, or unreachable — §3.10) (keyed per node). |

Existing events are kept (`QueueHeldNotReady`, `PDBBlocked`,
`RebootCompleted`, `UpdateStagingSkipped`, …). Structured logs:
`Info` on window open→closed transitions (rate-limited, per feature), on
each enqueue, and on each pin re-point; `Warn` on invalid config
(last-valid-wins), on mismatch, and on each failed stale-ConfigMap
delete attempt.
The `NoWindowsConfigured` rejection is returned in the API response and
logged at `Info` (an expected operator-visible condition).

### 3.9 Bootloader stale-entry prune (TODO 6)

The GRUB writer (M2 staging step 5, the target of §3.7's
reconciliation) only ever appends a `menuentry` block and moves `set
default`; `grub/grub.cfg` therefore grows one entry per staged kernel.
The syslinux-legacy writer behaves the same (`LABEL` block +
`DEFAULT`). The prune bounds both:

- **Prune candidate** — an entry block whose kernel line (`linux` on
  grub, `KERNEL` on syslinux) references
  one of *our* kernel files (matching the stored-kernel pattern
  `simplek8s.<ts>.<arch>.efi` under the release directory) **and** whose
  file no longer exists on the partition. Blocks whose kernel path
  does not match our pattern (foreign entries — including the GRUB
  MOK-enroll `chainloader` entry, which has no `linux` line at all) are
  **never touched**,
  file present or not. Global lines (`set default`/`set timeout`,
  `DEFAULT`, `TIMEOUT`, `PROMPT`, …)
  are preserved verbatim.
- **Block** — on grub, a block runs from its `menuentry` line to its
  closing-brace line (the exact mirror of the writer's block form);
  on syslinux, from its `LABEL` line to (not including) the
  next `LABEL` line or EOF — so each prune parser and its writer can never disagree about
  block boundaries.
- **Protection** — the block named by the current default (`set
  default` / `DEFAULT`) is never
  pruned. In practice it can never be a candidate: the purge's
  `protectedSet` includes the bootloader default, so that file is never
  deleted. The guard is belt-and-braces.
- **Trigger** — the prune runs **only when a purge deleted at least one
  kernel** (capacity pre-check, staging step 4, or retention, step 7),
  in the same mounted session, after the staging work. No purge
  deletion → no rewrite: rewriting the bootloader config is a vfat write and is
  not done gratuitously. The rewrite uses the writer's existing
  discipline (temp file + synchronous `copyOver`). If the rewrite fails
  after staging succeeded, staging is **not** rolled back: the error is
  Warn-logged (purge-triggered sessions are rare, so no dedicated
  rate limit) and the prune retries in the next purge-triggered
  session. A hand-deleted file with no purge deletion
  this session leaves its entry block dangling until a future purge
  triggers a pass (bounded growth, never unbootable — the default is
  guarded).
- **Scope** — grub and syslinux-legacy. The rpi config (`config.txt`) carries a
  single `kernel=` line; there is nothing to prune.
- **The distro's initial entry** (confirmed to reference one of our
  kernels) is a normal candidate: when the purge deletes that kernel,
  the entry goes with it. Accepted behavior change — the entry cannot
  boot once its file is gone, so nothing usable is lost.
- **GRUB default form** — the distro template and the writer use a
  named default (`set default=simplek8s.<ts>.<arch>`, entry `--id`
  equal to the kernel basename without extension), symmetric to
  syslinux `DEFAULT <label>`. Numeric defaults from the pre-`--id`
  template are accepted on read (Nth `menuentry`) and normalized to
  the named form on write. New entries are inserted before the first
  `menuentry` (newest first, oldest last); the MOK conditional is never
  inserted before or into, so the enroll entry stays last. The
  ESP redirect configs (`EFI/BOOT/grub.cfg`, `EFI/debian/grub.cfg`,
  `boot/grub/grub.cfg`) are never touched — only `grub/grub.cfg` is
  managed.

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

The GRUB side has no captured-partition sample (new layout); its
reference fixture lives in the unit tests (`grubSample` in
`bootloader_test.go`: named default, one `menuentry --id` per kernel,
MOK conditional last). Parser rules mirror syslinux: ownership from
the `linux` path only, one trailing blank line belongs to the block,
`--id`-less legacy entries resolve by `linux` basename.

### 3.10 `next-kernel` validation & safe-state recovery (TODO 8)

`next-kernel` is the node's boot **goal**, and it is **file-first**: no
controller writer sets a value whose file is not on the boot partition.
Audited — all four existing writers satisfy it:

| Writer | Why the file is present |
| --- | --- |
| `bootstrap` (annotation absent) | the target comes from a local partition scan (running-if-local, else newest local). |
| Fresh-stage `anchor` | runs only after `Store.Stage` succeeded — the file was just written. |
| Plan-layer resets (×2, M2) | write `running` — the kernel the node is executing, which the purge protects. (Abolished by this plan anyway.) |

The only ways a node can hold a value whose file is absent are an
**operator pin ahead of staging** (by design — the annotation leads
staging; the system converges via defensive re-staging, or corrects
below) and **manual file deletion** — while the file is absent the node
is not enqueue-eligible (rule 4, §3.4), so the gap never costs a reboot.
Validation restores a stuck goal to a **safe state**:

**Safe state** — (a) the running version, if its file is present on the
partition; else (b) the newest version present on the partition; else
(c) delete the annotation. In (a)/(b) the bootloader `DEFAULT` is
re-pointed to the corrected value; in (c) the bootloader is left alone
(there is nothing local to point at). On an empty partition only (c)
applies; the annotation-absent bootstrap path must tolerate finding
nothing and leave the annotation absent (no ping-pong).

**Correction paths** (local pod; both fire the rate-limited
`UpdateGoalCorrected` event, keyed per node):

1. **Malformed value** (present but failing the version grammar) —
   node state, evaluated in the same ungated path as `bootstrap` (no
   repository involved): correct to the safe state. The correction is
   an annotation RMW only; the bootloader re-point follows via the
   change-triggered reconciliation (§3.7), and the state re-arm follows
   the normal rule (§3.4). The two safe states differ in outcome: a
   correction to the **running** version (case a) makes the node
   **quiescent** — no reboot is pending and none is enqueued; a
   correction to **newest local** (case b, necessarily `≠ running`)
   makes the node reboot-eligible — the re-arm fires
   (no-op if the state is already `absent`) and the node reboots into
   the safe state at the next open window; case
   (c) deletes the annotation, leaving nothing to re-arm.
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

## 4. Decision log (per era; §4.3 active)

Numbers restart per era; unqualified references in this document are
to the active era (§4.3), e.g. "decision 31" = §4.3 row 31.

### 4.1 Reboots era (M1)

1. **All nodes are rebootable, control-plane included** — v1 has no
   policy flag to lock CPs out (to be revisited in the future). Safety
   comes from ordering instead: `nodes:["*"]` and queue tie-breaks put
   **workers before control planes**, and a hard limit of one
   simultaneous CP reboot (not configurable).
2. Images are hosted on **GitHub Container Registry**
   (`ghcr.io/simplek8s/simplek8s-controller`).
3. The API is exposed through a **Service** (NodePort,
   `externalTrafficPolicy: Cluster`) instead of per-node host binding —
   so a NodePort hit on a node whose local pod is not ready still
   routes to a ready pod (`Local` would drop such connections,
   contradicting the "routes to a ready pod" semantics).
4. **Events in v1**: one Event per state transition, plus rate-limited
   events on entry to PDB-blocked and on no valid leader (the two stuck
   situations that are not transitions); all low cost, operator-visible
   via `kubectl get events -n <controller-ns>` (events live in the
   controller namespace, not the empty namespace — see 3.11); structured
   logs remain the detailed source.
5. **Global orchestrator** via a single leader Lease
   `simplek8s-controller-leader` (generic name: it leads the controller,
   not the reboot feature). One decision-maker, no per-node leases, no
   queue races. The local pod's only job is issuing the reboot.
6. **No reappearance timeout** (KISS): a node stays `rebooting` until
   it is Ready again. A bricked node is visible through `NotReady` +
   the annotation and keeps its slot (queue naturally stops); the
   operator's escape is `DELETE`. No `lost` state, no give-up timer.
7. **No local-storage gate in the drain** (the `kubectl drain`
   `--delete-emptydir-data` gate is client-side and protects against
   data loss a reboot causes anyway). Documented as a warning instead.
8. **`reboots.on-failure=pause|continue`** (default `pause`): queue
   behavior while any node is `failed`; resumed by the operator clearing
   the `failed` state.
9. **Reboot verification via boot ID** (`reboot-exec`:
   `issuedAt`/`bootId`/`attempt`/`executorPodUID`/`confirmedAt` +
   `reboots.issue-grace`):
   `completed` requires `reboot-exec` plus evidence that the host
   actually rebooted (boot ID change — durable via `confirmedAt` — or a
   `NotReady` the current leader observed, per-leader in-memory);
   a `reboot` that is accepted but has no effect is `failed`, never
   `completed`. Chosen over grace-only heuristics: the boot ID is
   exact, local, and works in the single-CP case.
10. **Cap on concurrent control-plane reboots: 1, not configurable**
    (constant): independent of `reboots.max-concurrent`, so a batch
    can never drop etcd below quorum on a multi-CP cluster. Deliberately
    a constant, not a flag: rebooting two CPs at once is never a valid
    goal, only a quorum hazard.
11. **`NotReady` nodes are not rebootable** (admission 422; in v1 a hung
    node is rebooted manually/out-of-band), and the queue **holds** while
    any queued node is `NotReady` (3.4): a dead node is never drained,
    and its fate is the operator's to decide, not the orchestrator's.
12. **Lease re-validation before every transition** (split-brain
    mitigation): leader election guarantees at most one *self-proclaimed*
    leader, not one *acting* leader; the orchestrator **stops making
    lifecycle decisions when its local renewal deadline expires** and
    re-reads the Lease (verifying `holderIdentity`) immediately before
    each state transition patch, aborting if it no longer owns it. A
    rare one-cycle concurrency overshoot during a handover race is
    accepted and documented (Lease update and Node patch are separate
    API operations — this minimizes, but cannot eliminate, the race).
13. **Node disappearance is not a failure**: a node deleted from the
    cluster at any stage of a reboot plan is not an error and never
    pauses or holds the plan; the state lives on the node, so it
    disappears with it, slots free automatically, and the orchestrator
    derives everything from the current node list every cycle (3.4).
14. **An in-flight drain is not interruptible**: `DELETE` on a
    `draining` node is 409 — the operator does not interfere with an
    ongoing drain; if the drain fails, the node is `failed` and the
    plan pauses per `reboots.on-failure`.
15. **API read semantics**: `GET /reboots` shows the plan — every node
    with a reboot state (in-flight states plus `completed`/`failed`
    history until cleared); nodes without state are never listed, and
    `GET`/`DELETE` `/reboots/{node}` on a node without state is 404.
16. **No `/metrics` in v1**: observability is Events + structured
    logs + `GET /reboots`; a metrics endpoint can be added later without
    API changes.
17. **State lives in four JSON annotations** (`reboot-state`,
    `reboot-request`, `reboot-exec`, `reboot-status`), one per concern
    and writer — replacing the 12-annotation v4 design: `state`+`since`
    is one atomic unit (the request time is `since` while
    `requested`); `reboot-status` is the only shared-value annotation,
    governed by an RMW + conditional-transition discipline (3.3.2);
    corrupt `reboot-state` holds a slot with a loud Event rather than
    being auto-cleared (3.3.3). Chosen over 12 string keys (fewer
    annotations, single-owner fields, atomic 4-key patches) and over one
    monolithic JSON blob (writers stay separated; a leader restart
    cannot clobber the local pod's fields).
18. **`failed` is terminal, by design**: no automatic re-evaluation — if
    a node marked `failed` later turns out to have rebooted (slow boot
    beyond `reboots.issue-grace`, or an operator unbricking it), the
    plan is not resumed or completed; the operator starts a **new** plan
    or clears the history (`DELETE`). The pause-on-failure principle
    outranks self-healing bookkeeping.
19. **A PDB denial mid-drain is retried, not immediately fatal**: a 429
    from the Eviction API during the drain is retried with backoff for
    as long as `reboots.drain-timeout` remains — a PDB's state can
    fluctuate due to activity unrelated to the draining node (the same
    rationale as `kubectl drain`'s retry loop, and consistent with the
    documented pre-check/API divergence, 3.7); only a denial that
    survives until the timeout fails the node.

### 4.2 Updates era (M2)

1. **Windows as the only gates** (KISS) — no mode enum, no separate
   enable/check/stage booleans: `updates.windows: []` stops the work,
   `reboots.windows` gates the automatic reboots.
2. **Flat ConfigMap with feature-prefixed keys** — `data` is
   `map[string]string` (proven on the cluster: `kubectl explain
   configmap.data`, live coredns wire JSON). No nested documents, no
   JSON-in-YAML, no YAML dependency.
3. **Single `updates.url`** (full repo URL) — no channel/base-url split;
   per-node override via the `update-url` annotation (full URL).
4. **No GPG skip in the controller** — verification is always on
   (unattended kernel installation); `--checksign` stays a CLI concern.
5. **Keyring embedded in the image** (LFS in the repo) + optional Secret
   override; fixed paths, not configurable.
6. **No `node-info` annotation** — `Node.status.nodeInfo`
   (`kernelVersion` + `architecture`) is sufficient; running version
   read from the API (no `uname`, no x/sys/unix).
7. **`next-kernel` replaces `update-state`**: permanent, never deleted,
   source of truth; `== running` is quiescent; the bootloader must
   always reflect it (change-triggered reconciliation, §3.5).
8. **The plan is action-based**: triggered only by fresh staging (a
   version not previously in `/boot`), all-or-nothing, cluster-wide. No
   standing "pending → reboot" policy.
9. **Cancel semantics**: in-flight reboot completes;
   `next-kernel := running` on all members in **two phases**
   (immediate for non-in-flight, deferred settle for draining/rebooting
   — §3.8); no branching between updated and cancelled nodes.
10. **Re-launch is two-step operator action** (re-anchor + M1 API); no
    `POST /updates` endpoint; no auto-retry of a failed version.
11. **Rollback = one annotation edit** to the older preserved version
    (documentation, not a feature).
12. **Purge never deletes the running version**; `preserve` and
    `max-percent-usage` apply to the rest.
13. **Always UTC**; no timezone key.
14. **`reboots.windows` deferred** to a future plan (semantics agreed,
    see TODO.md).
15. **Docs and code in English** (maintainer requirement).
16. ~~**Plan durability via the leader Lease annotation**~~ —
   superseded by decision 23 (plan bytes on the heartbeat object).
17. **No new API endpoints** in the MVP.
18. ~~**No new hostPath in the DS**~~ — superseded by decision 24
   (a privileged pod's `/dev` does not contain the host block devices).
19. **All controller writes to `next-kernel` are conditional RMWs**
    (precondition checked at write time, not at decision time); the
    operator's unconditional write always wins a race by design.
20. **No command-line flags for feature configuration** — the
    ConfigMap (over built-in defaults) is the single configuration
    surface; the M1 feature flags are removed at the config
    migration. Deployment wiring (listen, apiserver, creds,
    pod/node/namespace) stays as flags/env.
21. **Bootloader reconciliation is change-triggered** (pod start,
    `next-kernel` value change, folded into the staging mount session)
    — never per-cycle: the boot partition is vfat; mounting it every
    engine cycle would thrash the disk and degrade RPi SD cards.
22. **Cancel reset is two-phase**: immediate for non-in-flight members;
    in-flight (draining/rebooting) members settle individually when
    their M1 state lands (consistent → keep; mismatch → reset) — a node
    mid-reboot still reports the old kernel in nodeInfo, so an immediate
    reset would anchor it to the old version (silent downgrade).
23. **Plan state in a dedicated leader-owned ConfigMap**
    `simplek8s-update-plans` (supersedes 16): the Lease stays a pure
    heartbeat; the operator config ConfigMap stays config-only; written
    only on plan transitions, human-inspectable.
24. **One read-only hostPath in the DS: host `/dev`** (supersedes 18) —
    verified on the test cluster: a privileged pod's `/dev` holds only
    runtime loop nodes (no `vda*`), while `/sys` is visible and
    `mknod` works; the ro bind is the standard pattern and the pod is
    already privileged (no exposure increase).
25. **Removed feature flags are fatal at startup** (Go `flag` package,
    exit 2) — no deprecation shim; an old DS manifest against the new
    binary is a CrashLoop with a clear message, which IS the migration
    signal.
26. **M2 deliberately breaks M1's stdlib-only rule**: exactly two new
    third-party dependencies — `github.com/ProtonMail/go-crypto` (GPG
    verification) and `github.com/klauspost/compress` (zstd) — because
    M2 installs kernels unattended (no human in the loop), which makes
    signature verification mandatory. No YAML library, no x/sys/unix.
27. **Non-plan verify-mismatch resets the single node** (leader, at M1
    `completed`, §3.10): no active plan → `next-kernel := running` for
    that node only, no plan ConfigMap write; a `failed` reboot does not
    reset (node still prepared for the operator's retry). The plan case
    (§3.8) is the same rule at plan scope.

### 4.3 Windows era (M3 — active)

| # | Decision | Rationale |
| --- | --- | --- |
| 1 | Windows = JSON **array of schedule strings** in flat ConfigMap keys | ConfigMap values are strings (M2 §3.2); a list needs a serialization; JSON is stdlib and unambiguous. |
| 2 | Vixie cron as documented in FreeBSD `crontab(5)` (5-field cron with names, the `@` schedules, `@<seconds>`) **+ the explicit `@every <duration>` extension** with controller-specific semantics (midnight-UTC anchor, not process start); nothing else (`?`, `@reboot` rejected) | What every cron operator already knows, pinned to the FreeBSD `crontab(5)` text instead of folklore; `@every <duration>` covers the "every N" cadence; the `@<seconds>` alias absorbs the remaining Vixie schedule form; small enough to hand-roll. |
| 3 | No window ranges / `end` field; duration = the grace key | Replaces TODO 1's four window forms (range, interval, cron, shortcut) with one mechanism; less surface, fewer edge cases. |
| 4 | No weekday aliases (`@monday`…`@friday` invalid) | Not Vixie (Quartz-ism); `0 7 * * 3` or `0 7 * * mon` already expresses it. |
| 5 | Grace default `5m`, one key per feature (`reboots`/`updates`) | Short default so a misconfigured schedule cannot hold the queue silently for hours; per-feature so the two cadences can differ. |
| 6 | **Explicit empty windows = feature OFF**; built-in defaults: `reboots.windows` = `[]`, `updates.windows` = `'["@every 12h"]'` (supersedes M2's "absent = no restriction") | Reboots stay opt-in (safety); updates keep M2's de-facto 12h check cadence, so an M3 rollout does not silently halt check/staging — a full stop is an explicit `'[]'`. **Automatic reboots are opt-in via `reboots.windows`**: an install that relied on auto-reboots must configure it (§5) — the nodes stage and wait non-quiescent, so the state is visible, not silent. |
| 7 | No maximum-duration guard | KISS; the always-open edge (grace > period) is documented as an operator error. |
| 8 | Gate start only; in-flight never interrupted; queue waits, never fails | M1 invariant; windows are scheduling, not a kill switch. |
| 9 | `force` bypasses windows (as it bypasses PDB) | The escape hatch stays immediate; one `force` semantics everywhere. |
| 10 | Non-forced `POST /reboots` with empty `reboots.windows` rejected at admission (`NoWindowsConfigured`, 422) | No zombie queue for a request that can never be honored; immediate operator feedback. |
| 11 | **The M2 plan layer is abolished** — plan ConfigMap, per-version all-or-nothing, plan cancel, "one active plan at a time" (supersedes M2 §3.8/§3.9 and fix-log entry 7) | Pending state **derived** from the node's own annotations (`next-kernel` vs `running` + M1 state) + the M1 queue is strictly less machinery; the BUG 12 class (plan state vs M1 state disagreement) disappears with the plan object; serialization is already M1's job. |
| 12 | **No `reboot-eligible` marker** — the M1 state is the consumed token: `absent` = the current `next-kernel` has not yet been rebooted into; `completed` = attempted (a broken version is never auto-retried); the local pod enqueues only `absent`-state nodes (supersedes the v2 marker design) | One fewer annotation; `next-kernel` stays the single source of truth; the no-retry guarantee falls out of the M1 lifecycle instead of a marker sweep; the enqueue check is idempotent per window (`completed`/`failed`/in-flight nodes are simply skipped); each pod re-evaluates from scratch after any restart — no leader state involved. The v2 marker design was defective: the leader consumed the marker at enqueue time, but the M1 `requested` state was then gated by a different, default-OFF window — a node could be left permanently stuck. |
| 13 | Operator pin (well-formed, locally present) → re-point `DEFAULT` (ungated) + re-arm the M1 state; in `full` the node reboots at the next open `reboots.windows`, in `stage` the operator reboots via the M1 API | A pin is honored without hand-editing the bootloader (TODO 9); `stage` stays operator-driven (a pin is a bootloader re-point, not an implicit reboot); a pin ahead of staging does not re-arm (no reboot into an absent file). |
| 14 | **State re-arm** (local pod, ungated): clear the M1 state when `next-kernel` changes to a well-formed, locally present version, or when staging completes a pre-pinned version | Successive updates must re-arm (W5) — the previous attempt's `completed` would otherwise block the next; the re-arm rides the existing change-triggered path (§3.7) — no new loop, no new annotation. |
| 15 | Best-effort delete of a leftover `simplek8s-update-plans` ConfigMap, **retried once per leadership acquisition** until it succeeds; **RBAC `configmaps` role gains `delete`** | Self-cleaning migration; no operator step, no code path left reading it. M2 shipped the role with `[get, list, create, update]` — without `delete` the cleanup 403s on every cluster and silently never cleans. The retry covers the rollout race in which the new role has not propagated yet: a once-per-lifetime attempt could be lost to exactly that and the ConfigMap would linger. |
| 16 | Per-node verification with events; **no auto-retry** on mismatch | Consistent with M2's "a failed version is never auto-retried"; the M1 API is the retry path. |
| 17 | Cron parser hand-rolled in `internal/cron` (stdlib), Vixie set + `@every` | Third dependency rejected; the accepted set is small and exhaustively testable. |
| 18 | UTC, no timezone key | M2 §2 rule. |
| 19 | Invalid windows value → whole key invalid → last-valid-wins + warn | One validation rule for all flat keys (M2 §3.2). |
| 20 | **`updates.check-interval` retired**; at most one check per window occurrence, no retries within an occurrence | With windows, the interval was a second dial on the same thing (it could only reduce or delay the frequency); per-occurrence checks also bound an always-open window by the occurrence period; the operator expresses the check cadence directly in `updates.windows`. |
| 21 | The last-checked occurrence is a **node annotation** (`simplek8s.org/update-last-check`, RFC3339 UTC), claimed by the local pod with an RMW **before** the fetch | An in-memory tracker would re-fetch the PROD release repo once per pod restart; a crash loop or rolling restart would amplify into fleet-wide repo traffic. The annotation is the project's established persistence primitive (M1/M2): one small RMW per occurrence, never cleared, and claim-on-start bounds repo traffic even when staging is interrupted (the next occurrence self-heals). Rejected alternatives: shared ConfigMap (write contention, wrong primitive), pod-local volume (stateless pods, none in SimpleK8s), hard-coded minimum interval (a new unconfigurable constant that only dampens, does not bound). |
| 22 | Prune rule: remove only entry blocks whose `KERNEL` references one of our kernels (`simplek8s.<ts>.<arch>.efi`) **and** whose file no longer exists; foreign entries never touched, file state irrelevant | The writer's naming pattern is the ownership boundary; foreign entries (recovery, non-managed paths) are out of contract like hand-edited files; the existence check is stateless — no "what did we delete" bookkeeping. |
| 23 | Prune trigger: only when a purge deleted ≥1 kernel, in the same mounted session; no rewrite otherwise | The only situation the rule can apply to by our own doing; no gratuitous vfat writes; the distro's initial entry (one of our kernels, confirmed) is pruned when its file is purged — accepted. |
| 24 | Goal correction only on (a) a malformed value or (b) a verified index that does not contain the value; fetch failure, or value-in-index with staging failed (including checksum/GPG mismatch) → no correction, retry next occurrence; no failure counters | Correcting on a transient failure would silently destroy operator intent (a pin, an update target); positive knowledge of absence is the only safe trigger; stateless, and a transient failure costs at most one occurrence — the same as any failed check (§3.4). |
| 25 | Safe state: running (if local) → newest local → delete the annotation; written by the local pod, re-pointing `DEFAULT` in the same session (case (c) leaves the bootloader untouched); `UpdateGoalCorrected` event | Generalizes bootstrap's fallback (M2 §3.5) from the annotation-absent case to the present-but-unreachable case; the corrected value is bootable by construction, so the leader's verification converges with no new logic. |
| 26 | The M2 `reboot-eligible` annotation is **abolished**: never read or written by M3; the local pod deletes any leftover at pod start (one-shot, best-effort) | Migration cleanliness with no behavior: nothing in M3 derives from it (eligibility is derived, decision 12), so a leftover is dead weight. |
| 27 | `make image` = buildx for `linux/amd64,linux/aarch64` (fails if either fails) + host-arch load into the local store; deploy flow unchanged | The Dockerfile is verified arch-agnostic (static Go, `alpine` + `util-linux`); the maintainer wants cross-arch failure caught at build time; a classic docker store is single-arch, so only the host platform is loadable. |
| 28 | CI and public release out of M3 (no workflow); TODO 13 reduced to the public `v*` publishing half (registry + tag scheme TBD); qemu/binfmt prerequisite documented | Maintainer decision (2026-09-09); the publishing target does not exist yet; the local multi-arch build check suffices until an arm64 test node is stood up. |
| 29 | **One window per concern, no exemptions**: `updates.windows` gates the update *work* (check/download/stage); `reboots.windows` gates **every** non-forced reboot — operator API and update-driven alike, including in the M1 orchestrator (no controller exemption) | Each window means exactly one thing, so the operator's reboots window always means "no reboots outside it"; the local pod enqueues only while it is open, so a node enters `requested` only when admission is possible — no zombie states by construction; with defaults, check/staging keeps running and auto-reboots simply wait for an explicit `reboots.windows` (§5). |
| 30 | `failed` stays an operator alarm (never auto-enqueued); `DELETE /reboots/<node>` on an update-eligible node **defers** (state → absent → re-enqueued at the next open window) | Canceling a pending auto-reboot must not silently drop the update intent — the intent lives in `next-kernel`, not in the queue entry; to abandon, the operator makes the node quiescent (reboot or re-pin to `running`); `failed` visibility is unchanged from M1. |
| 31 | **Enqueue is pod-side** — the local pod writes `reboot-state := requested` for its own node when it is reboot-eligible (all five rules, including goal-file locally present) and `reboots.windows` is open; the leader runs no eligibility scan (supersedes the v3 leader-scan design) | File presence on the boot partition is observable only by the local pod; a leader-side scan could enqueue a node whose goal file is absent (an operator pin ahead of staging, or a `DELETE`-deferral on a not-yet-staged pin) — one wasted drain + reboot into the old `DEFAULT`, a spurious `UpdateMismatch`, then convergence. Pod-side, verify-file → re-point → enqueue is one ordered code path (the ordering invariant, §3.4): the cross-actor race cannot exist. One conditional RMW per node needs no centralization; M1's serialization stays in the leader's orchestrator at admission. The presence check at enqueue time also covers manual deletion of the goal file: the node waits until the defensive re-staging or a goal correction makes it present again (§3.10). |
| 32 | Vixie cron (FreeBSD `crontab(5)`) is the normative syntax reference, superseding the K8s CronJob docs (decision 2) | The K8s docs describe a subset without pinning names, steps, or dom/dow OR semantics; the FreeBSD man page is the complete Vixie text (names, lists+ranges mixing, steps, dom/dow OR, 0/7 Sunday) — one stable external reference instead of folklore; `?`-rejection is principled (not Vixie), not a divergence. |
| 33 | `@<seconds>` accepted as an alias for `@every <N>s`; `@reboot` stays rejected | The numeric form is the last Vixie schedule form without a mapping; the clock-anchored alias is exact for window openness (only occurrence timestamps matter). `@reboot` has no occurrence set at all and cannot be mapped onto stateless evaluation. |
| 34 | Re-arm rides transitions only: a goal-value change observed in-lifetime, a staging-completes event, or a goal correction install a new goal and may re-arm; a no-op observation (pod start rediscovering the applied goal with `DEFAULT` already correct) never re-arms | A completed mismatch resting state must survive pod restarts — otherwise every restart auto-retries broken versions, voiding the no-retry guarantee (found live in the W13 campaign: a pod restart cleared `completed` → re-enqueue → reboot into the broken kernel again). Residual: a crash between the bootloader write and the state RMW, followed by a restart before any new trigger, leaves `completed` stuck; the operator `DELETE` (→ absent → eligible) is the escape. |

## 5. Behavior changes & migration

- **Updates keep working after an M3 rollout** (D6): absent
  `updates.windows` → the built-in default `["@every 12h"]` (M2's 12h
  check cadence, preserved). The shipped `deploy/configmap.yaml`
  carries the keys present, so "absent" only occurs on operator-edited
  ConfigMaps — the built-in is the fallback, not a second source. To stop the update feature entirely (not
  even checks), set `updates.windows: '[]'` explicitly.
- **Non-forced reboots are OFF by default** (D6, D10). The built-in default for
  `reboots.windows` is empty: after an M3 rollout, `POST /reboots`
  without `force` is rejected until the operator configures
  `reboots.windows`. Forced reboots are unaffected. The 422 is
  evaluated before per-node checks (unknown node + empty windows →
  422, a deliberate M1 admission-order change). This is the
  intended safety default, not a regression.
- **Automatic reboots are opt-in via `reboots.windows`** (D6, D29): the
  auto-reboot runs only while `reboots.windows` is open (default `[]` =
  off). Until configured, check/staging keep running and the nodes
  stage and wait, non-quiescent, with no queue entry — visible, not
  silent.
- **`updates.check-interval` is retired** (D20). It is no longer parsed: a
  ConfigMap that still carries it gets the existing unknown-key
  warn+ignore (the migration signal). Remove it from the ConfigMap and
  from `deploy/configmap.yaml`. The check cadence is now fully
  determined by `updates.windows` (one check per occurrence): with a
  weekly window, a new release is detected at most weekly.
- **The `reboot-eligible` annotation is abolished** (D12, D26). It is never read
  or written by M3 (any leftover is deleted by the local pod at pod
  start, best-effort). Reboot eligibility is **derived**: well-formed
  `next-kernel` + non-quiescent + M1 state absent + goal file locally
  present (§3.4), gated by open `reboots.windows`.
- **An operator pin no longer implies an implicit reboot** (D13). A pin
  only re-points the bootloader `DEFAULT` (ungated); the reboot is the
  operator's explicit `POST /reboots`, or the automatic enqueue at the
  next open `reboots.windows`.
- **No plan object: pending update state is derived** (D11). There is
  no "active plan", no plan cancel, no per-version all-or-nothing:
  whether a reboot is pending follows from `next-kernel` vs `running` +
  the M1 state (§3.4). The BUG 12 class (plan state vs M1 state
  disagreement) disappears with the plan object.
- Check-claim annotation `simplek8s.org/update-last-check` (D21;
  new in M3): newest checked occurrence (RFC3339
  UTC), written once per occurrence by the local pod and never cleared
  — a stale value only ever delays a check (a missed occurrence is
  skipped until the next one, §3.4).
- **`syslinux.cfg` can now shrink** (D22, D23). After a purge that deletes
  kernels, the stale entries for those kernels are pruned in the same
  session — including the distro's initial entry once its kernel file is
  purged (accepted: the entry cannot boot once its file is gone).
  `DEFAULT` and foreign entries are never touched.
- **The controller can now correct or delete `next-kernel`** (D24, D25). Before
  M3 it only set the annotation when absent (bootstrap) or after a
  successful stage (anchor). A malformed value, or a value the verified
  repository index does not contain, is corrected to the safe state
  (running → newest local → delete) with an `UpdateGoalCorrected`
  event. Operator pins ahead of staging are unaffected: a value that is
  in the repository index is never corrected — it is staged when the
  window allows.
- **`make image` now builds for `linux/amd64` and `linux/aarch64`** (D27).
  (fails if either fails) and needs qemu/binfmt_misc registered once on
  Linux; the deploy flow is unchanged.
- The `simplek8s-update-plans` ConfigMap is gone (D11, D15); the leader deletes a
  leftover (best-effort, retried once per leadership acquisition until
  it succeeds — the RBAC `configmaps` role gains `delete` for it; the
  retry closes the rollout race in which the new role has not
  propagated yet and a once-per-lifetime attempt would be lost).
- The updates campaign (§7.3) is **redrawn** by this plan (D11): the
  plan-layer U-cases (plan create/cancel/verify) are rewritten as
  windows cases in §7.4; the pending BUG 12 re-run is superseded by
  case W5.
- `TODO.md` items 1, 6, 8, 9, 11 are closed by this plan and leave the
  backlog; item 13 is reduced to its public `v*` publishing half.
- The historical updates plan's shorthand for the boot-partition mount
  point is obsolete (SimpleK8s does not mount the boot partition at a
  fixed path); left as-is in git, corrected here and going forward.
- `deploy/configmap.yaml` gains the four keys (D1, D5, D6:
  `reboots.windows`, `reboots.window-grace`, `updates.windows`,
  `updates.window-grace`), shipped **present with the built-in default
  values** (`reboots.windows: '[]'`, `updates.windows:
  '["@every 12h"]'`, graces `5m`) plus a commented example
  (`reboots.windows: '["@daily"]'`) — the reboots opt-in point is
  explicit.

## 6. Implementation

### 6.1 Modules

| Module | Change |
| --- | --- |
| `internal/cron` (new) | `Parse`, last-occurrence, `WindowsOpen`; Vixie conformance per FreeBSD `crontab(5)` + `@every`; exhaustive table tests. |
| `internal/config` | Four new keys + `Config` fields (parsed `[]Schedule` + two `time.Duration`); validation per §3.2; `updates.check-interval` no longer parsed (decision 20). |
| `internal/features/reboot` | Orchestrator: window gate in the per-candidate admission loop, forced bypass, `QueueHeldWindow` event. |
| `internal/api` | `admit`: `NoWindowsConfigured` (422) for non-forced requests when `reboots.windows` is empty. |
| `internal/features/update` | `update.go` (local pod): window master switch in `RunLocal` + persisted check claim (§3.4, decision 21); **reboot eligibility + state re-arm + `reboots.windows`-gated pod-side enqueue** (§3.4, decisions 12–14, 31, incl. the ordering invariant); operator-pin handler (§3.7); leftover `reboot-eligible` delete at pod start (decision 26); goal validation & safe-state correction (§3.10); prune hook after purge (§3.9). `window.go` (leader): **per-node verification** + stale-ConfigMap delete (retry per leadership acquisition). **Delete** `plan.go`, `planstate.go` and their tests. |
| `internal/features/update` (`bootloader.go`) | `pruneSyslinuxEntries` (pure block parse + rewrite, mirror of the writer) + temp-dir unit tests (§3.9). |
| `Makefile` | `image` target → buildx for `linux/amd64,linux/aarch64` + host-arch load; binfmt prerequisite documented (§3.11). |
| `internal/nodestate` | New conditional-RMW builds: enqueue (state→`requested`, one patch), **M1-state re-arm** (state→absent, precondition-checked), and check-claim (`update-last-check`, write only if absent/older). |
| `deploy/configmap.yaml` | The four new keys with their built-in defaults (`reboots.windows: '[]'`, `updates.windows: '["@every 12h"]'`, graces `5m`), commented example; remove `updates.check-interval`. |
| `deploy/rbac.yaml` | The `configmaps` role gains the `delete` verb (stale-ConfigMap cleanup, decision 15). |

### 6.2 Unit test matrix

- **`internal/cron`**: every field syntax (`*`, values, lists, ranges,
  steps, lists+ranges mixed, `a-b/n`); month/dow names
  (case-insensitive, in lists/ranges: `mon-fri`, `JAN,apr`); dom/dow
  OR rule; `7`/`0` = Sunday; all named schedules incl. `@midnight`,
  `@every_minute`, `@every_second`; `@<seconds>` ≡ `@every <N>s`
  (`@300` ≡ `@every 5m`); `@every` across midnight and across days
  (midnight-UTC anchor, handover-stable openness; non-24h-dividing
  durations documented: `@every 7h` has a short overnight gap);
  Feb-29-only schedules (4-year horizon); window-open boundaries
  (`t == O`, `t == O+grace` excluded); union of schedules; the
  always-open edge (grace > period); **a syntactically valid
  never-occurring schedule (`0 0 30 2 *`) → `ok == false` → window
  closed**; duration grammar (`@every 1d` invalid — Go
  `ParseDuration`, no `d`/`w`); invalid inputs (6-field, `@monday`,
  `@reboot`, `?`, bad values, empty).
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
  forced 202; windows set → 202 as today; `NoWindowsConfigured`
  evaluated **before** per-node checks (unknown node + empty windows →
  422, not 404).
- **update**: master switch (empty/closed window → no check at all);
  one check per occurrence (second cycle in the same occurrence → no
  second check; newer occurrence → new check; coincident schedules share
  a claim; failed check consumes the occurrence, no retry); check claim
  via `update-last-check` (written before the fetch; restart with
  occurrence claimed → no re-check; restart with a newer occurrence →
  claim; corrupt stored value → treated as absent; precondition race →
  no clobber); **eligibility derivation** (well-formed vs malformed
  `next-kernel`; quiescent vs non-quiescent; state absent vs
  `completed` vs `failed` vs in-flight;
  **goal file locally present vs absent (rule 4)** — **non-quiescent +
  `failed` is never touched until the operator clears it**, absent-file
  is never enqueued); **state re-arm** (`next-kernel` changed to a well-formed,
  locally present value → cleared; staging completing a pre-pinned value
  → cleared; pin ahead of staging → **no** re-arm; corrupt value → no
  re-arm; mid-reboot node → no re-arm; same-value no-op); **enqueue**
  (pod-side, decision 31: only while `reboots.windows` is open; only
  eligible nodes incl. **file locally present** — pin ahead of staging
  → no enqueue, no wasted reboot; only absent-state nodes; raced
  operator edits skipped; window closed → no enqueue,
  `UpdateHeldWindow`; **ordering invariant** — re-arm/enqueue never
  lands before the bootloader write); **DELETE defers** (state cleared
  → re-enqueued at the next open window); verification events
  (applied/mismatch, keyed, no auto-retry, **always on even with
  `updates.windows: []`**); stale-ConfigMap delete (present /
  `NotFound` → done / other error → warn + retry at next leadership
  acquisition); leftover `reboot-eligible` deleted at pod start; goal validation (malformed → safe-state correction
  without a repo, ungated; file absent + failed check → no correction;
  file absent + value in verified index + staging failed → no
  correction, next occurrence; file absent + value not in verified
  index → correction; safe-state precedence: running present / running
  absent → newest local / empty partition → annotation deleted,
  bootloader untouched).
- **bootloader (prune, §3.9)**: entry for our kernel with a missing
  file removed (including the distro's initial-entry shape and its
  in-block comment); top-of-file DOC header preserved verbatim;
  foreign entries (non-matching `KERNEL` path) untouched, file present
  or not; global lines preserved verbatim; the `DEFAULT` block never
  pruned; no purge deletion → no rewrite; rpi `config.txt` untouched.
- **nodestate**: enqueue build writes the four M1 state annotations in
  one patch; re-arm build (state→absent) precondition failures abort;
  check-claim build (absent → write, older → write, newer → skip, race →
  abort).

### 6.3 Phases

Phases 1–5 complete 2026-09-11 (W1–W17 17/17 PASS, §7.4); phase 6
(multi-arch build) pending — see TODO 13.

| Phase | Content |
| --- | --- |
| 1 | `internal/cron` + the four window config keys + unit tests (no behavior change). |
| 2 | Reboot window gate + API rejection + unit tests. |
| 3a | Update gating + claim: master switch, one check per occurrence with the persisted `update-last-check` claim; `updates.check-interval` retired (warn+ignore). Plan layer still present — no behavior removed yet. |
| 3b | Pod-side enqueue + re-arm + pin handler + per-node verification + goal validation & safe-state correction + syslinux prune (`internal/nodestate` RMW builders, prune-hook wiring). |
| 3c | Plan-layer deletion (`plan.go`/`planstate.go` out) + migration cleanup: stale-ConfigMap delete (+ RBAC `delete` — the role is applied before the image), leftover `reboot-eligible` cleanup. |
| 4 | Windows E2E campaign on the 3-node test VMs (W1–W17, §7.4) — results recorded in §7.4 as they pass. Prerequisite: phases 1–3c deployed with RBAC propagated. |
| 5 | Docs: finalize §7.4 results, redraw §7.3, `TODO.md` (items 1/6/8/9/11 out, 13 reduced), README, `deploy/configmap.yaml` example; the M3 era marked shipped. |
| 6 | Multi-platform image build: `make image` via buildx for `linux/amd64,linux/aarch64` + host-arch load, binfmt prerequisite documented (§3.11). No Go code; parallel with phases 1–5 (can run first). |

## 7. E2E

Live validation on the real test cluster. Campaigns are recorded here as
they pass; order within each: cheap → disruptive.

### 7.1 Conventions

Live validation on a real cluster. Order: cheap → disruptive. The
happy-path case is automated: `scripts/e2e-reboot.sh <node>` (env:
`BASE_URL`, `TOKEN_FILE`, `KUBEARGS`, `TIMEOUT_S`).

Conventions: `$API` = `http://127.0.0.1:1880` via `kubectl
port-forward` (no Service exists by design — never NodePort),
`-H "Authorization: Bearer $TOKEN"` abbreviated as `-H $AUTH`.
"Worker" = any non-CP node; keep the CP node for the last phases.

Prereq: the cluster needs a CNI with real pod networking (cross-node
pod-to-pod + NodePort). The distro's flat-bridge CNI shares one L2 segment
with no routing, so pod IPs collide and NodePort fails. Calico
(v3.30.1) is installed on the test cluster (cp1/wk1/wk2, podCIDR
10.244.0.0/24, .1.0/24, .2.0/24). Events for nodes live in the
`default` namespace (see PLAN.FIXME entry 5).

### 7.2 Reboots campaign (28/28 PASS)

| Case | Result | Date | Notes |
| --- | --- | --- | --- |
| M1-regression | PASS | 2026-09-06 | config-driven binary `7bf8c43` deployed to all 3 nodes (ctr import, nodeSelector rollout); live ConfigMap checks: invalid value → `WARN feature config ... keeping previous` (last-valid-wins), CM deleted → pods stay Running on built-in defaults, CM restored → 3/3; re-verified A1 (401s + livez 200), B1 (`scripts/e2e-reboot.sh wk1` 20/20), C1 (PDB `maxUnavailable:0` → `requested` + `blockedBy=[default/pdb-target]` + `PDBBlocked` event, DELETE 204) |
| B1 | PASS | 2026-09-06 | `scripts/e2e-reboot.sh wk1`, 20/20 checks, ~20 s reboot. First run had 5 spurious failures: port-forward died with the node (fixed: NodePort on a healthy node), polling through the API (fixed: node annotations), 5 s polling missed the short `draining` window (fixed: 2 s), and the 4 lifecycle events were never created (fixed: events must live in `default`, PLAN.FIXME #5). |
| A1 | PASS | 2026-09-06 | bad token → 401 `{"code":401,"reason":"Unauthorized"}` |
| A2 | PASS | 2026-09-06 | livez 200, readyz 200 |
| A3 | PASS | 2026-09-06 | empty list → `[]` (bare JSON array, see table note) |
| A4 | PASS | 2026-09-06 | unknown node → 202, `rejected:[{node:nope,code:404,reason:node not found}]` |
| A5 | PASS | 2026-09-06 | 2nd POST → 202, `rejected:[{node:wk2,code:409,reason:"state is not re-requestable: requested"}]` |
| A6 | PASS | 2026-09-06 | POST+DELETE in 0.14 s → 204, all four annotations gone, `spec.unschedulable` untouched, node Ready, no events emitted |
| C1 | PASS | 2026-09-06 | pause Deployment (1 rep, pinned wk1) + PDB `maxUnavailable:0`; POST wk1 → stays `requested`, `reboot-status.blockedBy=["default/pdb-target"]`, Warning event `PDBBlocked`; pod untouched |
| C2 | PASS | 2026-09-06 | with wk1 PDB-blocked, POST wk2 → wk2 full lifecycle to `completed`; wk1 still `requested` (skip rule) |
| C3 | PASS | 2026-09-06 | `kubectl delete pdb` → wk1 drain proceeds next cycle, `completed` ~27 s later; `reboot-status` cleared |
| C5 | PASS | 2026-09-06 | unmanaged pod on wk1, `POST {"force":true}` → pod deleted, drain completes, wk1 `completed` |
| C6 | PASS | 2026-09-06 | PDB on wk2, `POST {"force":true}` → eviction rejected by PDB, force-DELETE removes the pod, drain completes, wk2 `completed` (Deployment recreated the pod after the reboot) |
| A7 | PASS | 2026-09-06 | `POST [wk1,wk2]` → strict serialization: wk1 `completed` first, wk2 admitted only after (slot=1) |
| B3 | PASS | 2026-09-06 | re-POST after `completed` → accepted (completed is re-requestable), full lifecycle again |
| C4 | PASS | 2026-09-06 | unmanaged pod on wk1, `force:false` → drain skipped the pod, hit `reboots.drain-timeout` exactly (10m0s) → `failed` + uncordon + `RebootFailed` event; node back to Ready |
| D1 | PASS | 2026-09-06 | leader pod deleted mid-drain → standby took over via lease; drain finished and node reached `completed` |
| D2 | PASS | 2026-09-06 | DS rollout (pod restart) while node `rebooting` → no re-issue: `attempt` stayed 1, no second reboot |
| D3 | PASS | 2026-09-06 | DELETE while `draining` → 409 (in-flight drain not interruptible); `failed`/`completed` DELETE → 204 |
| D4 | PASS | 2026-09-06 | queued node stopped (kubelet down) → `QueueHeldNotReady` event, held while NotReady, admitted + `completed` when it returned |
| D5 | PASS* | 2026-09-06 | `kubectl delete node wk1` while wk1 draining: slot freed, wk2 admitted and `completed` (plan continues). `NodeDisappeared` event MISSED: the leader pod ran on wk2 and died with the host reboot in the same window, dropping the in-memory `prevInflight` (see PLAN.FIXME #6). Escape: kubelet 1.37 on this distro does NOT re-register a runtime-deleted Node object — `systemctl restart kubelet` on wk1 restored it. |
| D6 | PASS | 2026-09-06 | corrupt `reboot-state` (bad JSON) → `parseError` (API 200, no 500), slot held, `CorruptRebootState` event; DELETE cleared it, node re-requestable |
| D7 | PASS | 2026-09-06 | corrupt `cordonedPrev` (wrong type) on `completed` → uncordon skipped (idempotency guard), `UncordonBlocked` event, cordon preserved; fixed value → clean uncordon |
| D8 | PASS | 2026-09-06 | pre-cordoned node (`kubectl cordon`) → `completed` → still cordoned (operator cordon preserved) |
| D9 | PASS | 2026-09-06 | `onRebootFailure=pause`: `failed` node paused the queue; DELETE of the failed node → queue resumed, next node admitted |
| D10 | PASS | 2026-09-06 | variant image with no-op `nsenter` (exit 0) → command "succeeded", boot ID unchanged → `failed` at exactly 5m0s: `reboot did not take effect: host boot ID unchanged 5m0s after issuedAt` |
| D11 | PASS | 2026-09-06 | variant image without `nsenter` → `failed` seconds after issue: `reboot command failed to start: exec: "nsenter": executable file not found in $PATH`, uncordoned, `RebootFailed` event |
| D13 | PASS | 2026-09-06 | `POST *` with wk2 NotReady (kubelet stopped) → 202 partial: Ready nodes accepted, `{node:wk2,code:422,reason:"node not Ready"}` rejected |
| A8 | PASS | 2026-09-06 | `POST [wk1,cp1]` (max-concurrent=1): wk1 `rebooting` while cp1 stayed `requested` (workers-before-CP tiebreak + CP gate); cp1 admitted only after wk1 `completed` |
| D17 | PASS | 2026-09-06 | same campaign: cp1 admitted alone (nothing else in-flight), rebooted, API down ~1 min, node returned → `completed`; everything resumed from annotations |
| D12 | PASS | 2026-09-06 | `virsh destroy` (hard power-off, no reboot) 1 s after issue: node NotReady, stays `rebooting` past the 5 m grace (no executor → no boot-ID check → **no auto-fail**); queued node stayed `requested` (slot held); DELETE cleared annotations on the bricked node; `virsh start` → Ready. First attempt void: the Buildroot guest reboots in ~17 s, completing before a late power-off lands. |
| D16 | PASS | 2026-09-06 | `virsh suspend` (freeze) in the same second as issue, held 6.5 min (past grace): node stayed `rebooting`, **not** `failed` (executor pod frozen with the host); `virsh resume` → node returned 6m26s after issuedAt → `completed` |
| B2 | PASS | 2026-09-06 | same campaign: late return (>30 s after issuedAt) → `completed` via the NotReady-after-issuedAt evidence path (no `confirmedAt` in reboot-exec: the local pod never confirmed, exactly as designed) |
| D14 | PASS | 2026-09-06 | `reboots.max-concurrent: "2"`: `POST [wk1,cp1]` → BOTH `rebooting` concurrently; cp1 reboot took the API down ~1 min (< drain-timeout); on recovery both `completed`, nothing `failed`; leader handover on recovery |
| D15 | PASS | 2026-09-06 | unmanaged pod on wk1 (drain pending); CP apiserver is a static pod on this distro — outage by `mv`-ing its manifest off for 13 min (> drain-timeout); `failed` at 05:46:33, i.e. **on recovery**, not at the 05:43:18 deadline while API was down (wall-clock); `drain timed out after 10m0s`, uncordoned next cycle |

#### Phase A — API & admission (no reboot)

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| A1 | invalid token | `curl -H "Authorization: Bearer wrong" $API/api/v1/reboots` | 401 |
| A2 | liveness/readiness | `curl $API/livez $API/readyz` | 200 / 200 |
| A3 | empty list | `curl -H $AUTH $API/api/v1/reboots` | `[]` (bare JSON array of plan entries) |
| A4 | unknown node | `POST -d '{"nodes":["nope"]}'` | 202, rejected `[{node:nope, code:404}]` |
| A5 | re-request while in flight | POST twice for the same worker | 2nd: 202 rejected 409 (not re-requestable) |
| A6 | cancel `requested` | POST, then `DELETE /reboots/<node>` | 204; all four annotations gone; cordon untouched |
| A7 | batch queueing | `POST -d '{"nodes":["w1","w2"]}'` (max-concurrent=1) | both accepted; w1 admitted first (oldest `since`), w2 stays `requested` until w1 completes |
| A8 | workers before CP | `POST -d '{"nodes":["w1","cp"]}'` | w1 `draining` while cp stays `requested` |

#### Phase B — happy path

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| B1 | full worker reboot | `scripts/e2e-reboot.sh <worker>` | requested→draining(cordon)→rebooting→completed→uncordon; events RebootDraining/RebootIssued/RebootCommandIssued/RebootCompleted; DELETE 204; annotations cleared |
| B2 | late return | B1, but let the node return > 30 s after `issuedAt` | still `completed` (NotReady-after-issuedAt evidence), not `failed` |
| B3 | re-request after `completed` | POST again on the same node | accepted (re-requestable) |

#### Phase C — PDB, unmanaged, force

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| C1 | PDB block | Deployment + PDB (`maxUnavailable:0`) pinned to the worker; POST | stays `requested`; `reboot-status.blockedBy` set; `PDBBlocked` event; queue continues past it |
| C2 | 2-node PDB skip | C1 on w1 + POST w2 | w1 blocked, w2 drains/completes (skip rule) |
| C3 | PDB unblock | delete the PDB (or scale dep to 0) | drain proceeds on next cycle |
| C4 | unmanaged pod, no force | `kubectl run orphan --rm=false ...` on the worker (no owner); POST | drain blocks; at `reboots.drain-timeout` → `failed` + uncordon + `error` set; queue pauses (pause mode) |
| C5 | unmanaged pod, force | same, `POST -d '{"force":true}'` | pod deleted, drain completes, reboot proceeds |
| C6 | force bypasses PDB | C1 setup, `POST -d '{"force":true}'` | drain proceeds despite PDB |

#### Phase D — fault injection (disruptive)

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| D1 | leader takeover mid-drain | while w1 `draining`: `kubectl -n simplek8s delete pod <leader-pod>` | new leader takes over (~30 s); drain continues; no double transitions |
| D2 | duplicate executor | while node `rebooting`: roll the DaemonSet (new pod on the node) | new pod does not re-issue beyond the bound; `attempt` never exceeds the single 1→2 re-issue; one reboot only |
| D3 | DELETE on `draining` | `DELETE /reboots/<draining-node>` | 409 (not interruptible) |
| D4 | queued node NotReady | POST w1,w2; make w2 NotReady before its turn | `QueueHeldNotReady` event; resumes when w2 is Ready again |
| D5 | node deleted mid-plan | while w1 `draining`: `kubectl delete node w1` | `NodeDisappeared` event; slot freed; plan continues with the rest |
| D6 | corrupt `reboot-state` | `kubectl annotate <node> simplek8s.org/reboot-state='{"state":"bogus' --overwrite` (on any state) | GET shows `parseError`; slot held; `CorruptRebootState` event with both escapes + raw value; DELETE clears, no auto-uncordon |
| D7 | corrupt `cordonedPrev` | on a `failed` cordoned node: set `reboot-status='{"cordonedPrev":"yes"}'` (wrong type) | uncordon **skipped** (cordon preserved), `UncordonBlocked` event; `kubectl uncordon` repairs |
| D8 | `cordonedPrev` respected | `kubectl cordon <node>` first, then reboot to `failed`/`completed` | node stays cordoned (operator's cordon) |
| D9 | pause → resume | let a node reach `failed` (e.g. C4); POST another node | `QueuePaused` event, new node stays `requested`; DELETE the failed node → queue resumes |
| D10 | no-effect reboot | image variant whose reboot shim is a no-op (or patch the container command) | boot ID unchanged; `failed` after `reboots.issue-grace` ("reboot did not take effect") |
| D11 | nsenter failure | image without `nsenter` (e.g. scratch-based) | `RebootFailed` command-failure path, node `failed`, uncordoned |
| D12 | bricked node | POST a worker, then power off the VM (do not reboot) | stays `rebooting`; NotReady; queue holds the slot; no auto-assumption; DELETE clears |
| D13 | batch `*` with NotReady | make one node NotReady; `POST -d '{"nodes":["*"]}'` | partial 202: Ready nodes accepted, the NotReady one rejected 422 |
| D14 | API outage mid-drain (short) | `reboots.max-concurrent: "2"`, w1 draining + cp rebooting; stop apiserver < drain-timeout | nothing marked `failed` while down; leader handover on recovery; drain resumes and completes |
| D15 | API outage > drain-timeout | same, outage longer than `reboots.drain-timeout` | draining node `failed` + uncordoned on recovery (wall-clock) |
| D16 | host hang during shutdown | hang the host's shutdown (e.g. qemu pause at reboot) | its pod dies, so the boot-ID check cannot fire: node stays `rebooting` until Ready (does **not** fail after the grace); escape = DELETE |
| D17 | CP reboot last | batch including the CP node | CP admitted only when nothing else in-flight; single-CP: API down until the node returns, then everything resumes from annotations |

#### Notes

- B1 first, always: it is the reference for every later case.
- D14/D15/D16 need host-level access (VM control) — schedule them last.
- VM control on the test host: `sudo virsh` (sk8s-cp1/sk8s-wk1/sk8s-wk2).
  The guests are Buildroot: systemd (kubelet/containerd) + runit (calico
  daemons); the CP components (apiserver, etcd, scheduler, CM) are static
  pods managed by the kubelet — there is no `kube-apiserver` systemd unit.
  To stop the API: move `/etc/kubernetes/manifests/kube-apiserver.yaml`.
  Guests reboot in ~17 s; a power-off for D12 must land within ~10 s of
  the reboot issue.
- Every case must end with: node Ready, annotations cleared or in a
  well-defined state, queue unblocked, and a `kubectl -n simplek8s
  get events` scan for unexpected reasons.

### 7.3 Updates campaign

Live validation of the distro-update feature on the real test cluster
(cp1/wk1/wk2, x86-64), run against the M2 plan layer. Order: cheap →
disruptive. Conventions from §7.1 apply (`$API`, `$AUTH`, events in
namespace `default`).

> **Redrawn by M3.** The plan-layer cases below (U5/U6/U7: plan
> create/cancel/verify) describe the abolished mechanism — they stand
> as the historical record of the 2026-09-07/08 runs. The same
> behaviors under windows live in §7.4 (W4/W5/W7); check cadence is one
> check per window occurrence (no `check-interval`).

#### Prerequisites

- **Release server**: the real **PROD** release repo — *not* a mock —
  `https://dl.simplek8s.org/simplek8s/dev/` (the latest dev releases we
  test against) and `https://dl.simplek8s.org/simplek8s/stable` (also
  PROD, same keyring, but lagging — it does not carry the newest
  releases). Layout: `SHA256SUMS`, `SHA256SUMS.gpg`,
  `simplek8s.<ts>.<arch>.efi.zst`. There is **no mock server**; the
  access-log "did the controller talk to the repo?" evidence (U0) is
  observed on the server side.
- **Keyring**: the standard image embeds the **PROD** public keyring at
  `/etc/simplek8s/pubring.gpg` (from `keys/simplek8s-pubring.gpg`, LFS);
  the same PROD key is on the test machines. The PROD releases are signed
  by that key, so the standard image verifies them directly — no variant
  image and no custom keyring are needed. The optional Secret
  `simplek8s-controller-keyring` is only for a genuinely custom
  (non-PROD) repo/key.
- **A newer release**: staging/`full` cases need a release newer than the
  running `ts` in the repo. After an update, to make the repo's newest
  "newer" again, downgrade the cluster to an older `ts` (the U9
  successive-update setup).
- **Boot layout**: kernels live in the `simplek8s/` dir at the root of
  the (temporarily mounted) boot partition; `/boot/simplek8s/` in the
  cases below is shorthand for that dir. See M2 §3.7 (ground-truth
  layout; historical text in git).
— **ConfigMap**: `updates.url` pointed at the PROD repo (`dev/`);
  `updates.windows` set per case (e.g. `["@every 2m"]` for a fast
  campaign cadence); `reboots.windows` open only where the case
  reboots.
- **Reboots regression subset**: after the updates-era config
  migration (flags → ConfigMap), re-run §7.2 cases A1, B1, C1 before
  starting this campaign (proves the config migration caused no
  regression). That migration also rewrites the flag references in
  §7.2 (C4/D15, D10, D14) and the README flags section to ConfigMap
  keys.

#### Progress

| Case | Result | Date | Notes |
| --- | --- | --- | --- |
| U1 (staging, no auto-reboot) | PASS | 2026-09-07 | Defensive re-stage on all 3 nodes (cp1/wk1/wk2); syslinux entries written; bootloader default at the new version; nodes stayed on the old kernel (no reboots). Found + fixed here: staging was a silent no-op until the boot mount root was created (`6684e07`). |
| U5 (real release, auto-reboot) | PASS | 2026-09-07 | Real dev release `6.18.49-simplek8s-202609061935`; all 3 nodes staged → plan started → reboots serialized workers-first, CP-last (wk1→wk2→cp1) → each returned on the new kernel (`running == next-kernel`) → plan settled, cluster quiescent; no `failed`. |
| U9 (auto-reboot, 3-CP quorum) | PASS (found BUG 12) | 2026-09-08 | Re-ran U5 on the HA 3-CP cluster (cp1/2/3 + wk1/2). Phase A manual downgrade then Phase B re-update (both windows open): all 5 back on the latest, **API never dropped** (2/3 etcd quorum through each single-CP reboot; leader handover `xb4bh`→`p4vrz`). First Phase-B attempt hit **BUG 12** (stale `reboot-state` → `verifyPlan` false-cancel); unblocked by clearing stale M1 annotations + re-staging fresh. See §U9 + TODO item 12. |

#### U0 — windows `[]` are inert

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| U0 | no update activity | `updates.windows: '[]'`, PROD repo reachable with a newer version | no HTTP traffic to the repo (server access log empty), no annotations created, no update events, engine + reboot API healthy |

#### U1 — staging, no auto-reboot

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| U1 | full staging happy path | `updates.windows` open, `reboots.windows` closed; the PROD repo has a newer version | every node: `UpdateAvailable` then `UpdateStaged`; `/boot/simplek8s/` contains the new version + bootloader entry; `next-kernel := V` on all nodes; bootloader default points at V; **nodes keep running the old version** (no reboots); purge respected: running version NOT deleted, old versions pruned per `preserve` |
| U1b | idempotent re-check | wait for the next window occurrence | no re-download (V already in `/boot`), no annotation change, no plan — "V in /boot → nothing" rule |

#### U2 — GPG rejection

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| U2 | bad signature | a `SHA256SUMS.gpg` signed with an unknown/wrong key (tampered or non-PROD key) | no staging, no annotation change, `UpdateCheckError` event (rate-limited), nodes untouched, next occurrence retries |

#### U3 — sha256 rejection

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| U3 | corrupted payload | valid signature, but the served `.efi.zst` does not match the index hash | download rejected, **no partial file** left in the boot-partition `simplek8s/` dir, `UpdateCheckError` event, next occurrence retries |

#### U4 — per-node URL override

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| U4 | `update-url` annotation | one node annotated with a second release URL (e.g. `.../stable`) serving a *different* version | only that node stages its own version (different V); the other nodes stage the cluster `updates.url` version; both `next-kernel` values correct per node |

#### U5 — real release with auto-reboot (the big one)

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| U5 | full cluster update | both windows open; maintainer publishes a new dev release | all nodes stage → `UpdatePlanStarted` → reboots serialize per `reboots.max-concurrent` (M1 queue: cordon/drain/issue/verify) → each node returns on the new version (`running == next-kernel`, quiescent) → plan-state ConfigMap entry cleared → cluster fully on the new version; no `failed` (M2 plan mechanism — abolished by M3; the same outcome via windows is W4) |

#### U6 — plan failure → all-or-nothing cancel

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| U6 | drain timeout mid-plan | both windows open with a new release; make one node's drain fail (stuck pod / PDB `maxUnavailable:0`) | that node `failed` (M1) → `UpdatePlanCanceled`; in-flight reboots (if any) complete; **two-phase reset**: non-in-flight members reset immediately to `next-kernel := running`; in-flight members settle when their M1 state lands — the failing node comes back on the old kernel (mismatch) → reset, any completed+verified node keeps V (`next-kernel == running == V`, no-op); bootloader defaults back to the old version where reset applied; the plan-state ConfigMap entry is cleared only when **all** members have settled; next check does **not** auto-retry (V in the boot-partition dir → nothing) (M2 plan mechanism — abolished by M3; cancel/defer semantics now: W7) |

#### U7 — operator re-launch

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| U7 | re-anchor + M1 reboot | after U6: set `next-kernel := V` on the cancelled nodes, then reboot them via the M1 API | bootloader followed the annotation before the reboot; nodes come back on V; `running == next-kernel` → quiescent; no update events (the updater did nothing — operator-driven) (M2; in M3 re-anchor + M1 reboot still applies — see §7.4) |

#### U8 — rollback

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| U8 | operator rollback | on an updated node (e.g. after U5): `next-kernel :=` the older preserved version, reboot via the M1 API | node comes back on the old version; `next-kernel == running` → quiescent; the check does **not** re-stage or re-update automatically (newer V is in `/boot` → nothing); the operator decides the next move |

#### U9 — auto-reboot on an HA 3-CP cluster (quorum) + BUG 12

Re-validates U5 on the **HA** cluster (cp1/cp2/cp3 + wk1/wk2). The new
aspect: the update reboot plan must bring all 3 control planes back (one at
a time, `reboots.max-concurrent: 1`) **without the API losing quorum**.

### Phase A — manual downgrade

`next-kernel` + the boot-partition `DEFAULT` flipped to the previous kernel
(`202608291203`) on all 5, then a full M1 reboot (workers→CP, serialized).
All 5 came back quiescent on the old kernel (`running == next-kernel`); the
API stayed up; then the new `.efi` was removed from the boot partition so the
auto-updater would have to re-download it.

### Phase B — auto re-update

Both windows open. The auto-updater re-downloaded, re-staged
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
the new kernel was now local on the boot partition, it was not re-anchored
(not a "fresh stage", `update/update.go:224`), leaving the cluster
staged-but-never-rebooted. Unblocked by clearing the stale M1 annotations
(`reboot-state`/`reboot-exec`/`reboot-request`) and removing the new kernel
from the boot partition to force a fresh re-stage. Documented in TODO.md item
12; also
affects successive auto-updates (not only manual-then-auto).

**Fixed (2026-09-08):** a fresh plan start now clears each member's stale
terminal `reboot-state` before its first verify (`resetStaleRebootState`,
`update/plan.go`; `nodestate.ClearStaleRebootStateBuild`), so the same-cycle
verify no longer false-cancels. Unit/integration tested; E2E re-run pending.

### 7.4 Windows campaign (W1–W17, 17/17 PASS)

Live on the 5-node test cluster (cp1, wk1, wk2 driven; cp2/cp3 converged unattended) with builds `f3b329a` (W1–W13) and `b1c6da5` (W13 re-validation post-D34). Cluster left quiescent on a calm ConfigMap (`stage`, `updates=["@every 12h"]`, `reboots=[]`).

| # | Result (2026-09-11 live) | Case | Trigger | Expect |
| --- | --- | --- | --- |
| W1 | PASS 2026-09-11 — default-empty windows: 422 `NoWindowsConfigured` per node (unknown node → 422, not 404); `force:true` admitted and executed (requested→rebooting→completed ~25 s). | Empty `reboots.windows` | non-forced `POST /reboots` | 422 `NoWindowsConfigured` per node; `{"force":true}` executes immediately. |
| W2 | PASS 2026-09-11 — `@every 2m`/30 s: held `requested` + `QueueHeldWindow` while closed; admitted exactly at window open (12:10:01); with 10 s grace the close landed mid-reboot → still `completed` (in-flight never interrupted). | `reboots.windows: '["@every 2m"]'` | POST while closed → open | stays `requested` + `QueueHeldWindow` while closed; admitted on open; an in-flight reboot is not interrupted by close. |
| W3 | PASS 2026-09-11 — explicit `[]`: no claims, no events (2.5 min); `@every 2m`: claims stepped exactly per occurrence (12:16→12:18→12:20→12:22); pod deleted mid-opening → claim holds, no re-fetch; `@daily` closed → nothing for 3 min. Absent→default (`@every 12h`) verified via unit + code (12 h not waited live). | `updates.windows` master switch + per-occurrence check | explicit `[]` vs default (absent) vs short campaign window; several occurrences; delete the local pod mid-opening | explicit `[]` → no check activity (no index fetches); absent → the default `@every 12h` applies (verified via config snapshot/log); short window → exactly one index fetch per occurrence (next occurrence → next fetch); pod restart mid-opening → **no** re-fetch, `update-last-check` holds; nothing while closed. |
| W4 | PASS 2026-09-11 — staged B + both windows open: auto-enqueue → reboot → running 6.18.50-202609090435 + `UpdateApplied` (wk1 first; then the whole fleet converged to B unattended, serialized by the M1 queue). | Full auto-update inside windows | new release in repo; **both** windows open (e.g. `updates.windows` and `reboots.windows` both `'["@every 2m"]'`) | check → stage → anchor → (reboots window open) enqueue → M1 lifecycle → `UpdateApplied`; node running the new kernel. |
| W5 | PASS 2026-09-11 — two successive pin-driven cycles on wk2 (C then D): `completed(C)` → pin D → D staged + `DEFAULT` re-pointed + state re-armed → enqueue → reboot → `UpdateApplied` on D; the interim `UpdateMismatch` left the goal intact (no reset). | Successive auto-updates (BUG 12 regression) | two releases back-to-back, both applied | the full transition is observed, not just the end state: `completed(A)` → `next-kernel = B` → B staged + `DEFAULT` re-pointed + state re-armed → enqueue (window open) → reboot → `UpdateApplied` on B; no stale `reboot-state` interference (supersedes the pending BUG 12 re-run). |
| W6 | PASS 2026-09-11 — `full`: pin B → `DEFAULT` re-pointed ≤20 s (ungated) → reboot at open window → applied. `stage`: pin → re-point only, no enqueue; operator `POST` → reboot → running the pinned version. | Operator pin / rollback | set `next-kernel` to a preserved older version (present on the partition) | bootloader `DEFAULT` re-pointed immediately (ungated); in `full` with a reboots window: state re-armed → enqueued at the next open window → node back on the pinned version (`UpdateApplied`); in `stage`: nothing is enqueued until the operator `POST /reboots`. |
| W7 | PASS 2026-09-11 — `DELETE` while `requested` (closed) → absent for 65 s → re-enqueued exactly at the next open (12:56:01). A quiescent `DELETE` correctly does NOT re-enqueue. | Cancel defers, does not abandon | DELETE a window-enqueued node while `requested` | M1 state cleared → the node is **re-enqueued at the next open `reboots.windows`** (the intent persists in `next-kernel`); re-pinning to `running` (quiescent) stops it. |
| W8 | PASS 2026-09-11 — staged + `reboots.windows=@daily` (closed, non-empty): state absent (not in the M1 queue) + `UpdateHeldWindow`; flipped open → enqueued at the next occurrence → reboot → applied on B. | Eligible node waits for the window | node staged (non-quiescent) while `reboots.windows` is closed (updates window open) | node **not** in the M1 queue + `UpdateHeldWindow`; enqueued at the next open `reboots.windows`. |
| W9 | PASS 2026-09-11 — leader pod deleted with cp1 `requested`-held + wk2 eligible-waiting: new leader admitted at open (cp1 `completed` across the handover; wk2 pod-enqueued 12:58:01, admitted after the slot freed, `completed` 13:00:33). | Leader handover mid-window | delete the leader pod while nodes are queued (one eligible node waiting) | standby takes over the queue (window recomputed from the clock); queued nodes continue; the eligible node is enqueued by its **own local pod** (independent of the leader) and admitted by the new leader; nothing lost. |
| W10 | PASS 2026-09-11 — B1 script 20/20 on wk1; C1 PDB `maxUnavailable:0` → `requested` + `blockedBy` + `PDBBlocked`, pdb deleted → `completed` at the next open; D9 with a hand-crafted `failed` state → queue paused across an open window, `DELETE` → resumed, admitted next open, `completed` (failed-state production via C4 stays M1-covered). | M1 regression | quick pass: B1 happy path, C1 (PDB), D9 (pause) | M1 semantics unchanged with windows configured. |
| W11 | PASS 2026-09-11 — `preserve=1` alone purges nothing (preserve = immunity under pressure, not a target); with `max-percent-usage=1`, staging J purged F1/F2/C2/A (kept newest-1 B + running + goal) and pruned their syslinux blocks (5→3 `LABEL`s, `DEFAULT`→J, per-block `#APPEND` dropped with its block); node booted J + applied. | Syslinux prune | stage releases until the purge deletes a kernel (small `updates.preserve` / usage cap) | entries for the purged kernels (and the distro's initial entry, once purged) are gone from `syslinux.cfg`; `DEFAULT` and foreign lines intact; the node still boots. |
| W12 | PASS 2026-09-11 — (a) malformed with `updates=[]` → corrected to running ≤20 s + event, quiescent, no reboot; (b) never-existed ts untouched 45 s with `[]` → corrected at the next occurrence after opening; (c) running file hand-deleted + malformed → corrected to newest-local A + event → eligible → reboot at open → applied on A. | Goal validation / safe state | hand-set `next-kernel` to (a) a malformed value, (b) a well-formed ts absent from the repo | (a) corrected promptly to the safe state without waiting for a window; (b) corrected at the next window occurrence; in both: `UpdateGoalCorrected` event, `DEFAULT` re-pointed where applicable — and the two safe-state outcomes are distinct: correction to **running** leaves the node quiescent (**no** reboot at any window); correction to **newest local ≠ running** makes it reboot-eligible (it reboots into the safe state at the next open window in `full`). |
| W13 | PASS 2026-09-11 (with the D34 fix) — corrupted staged kernel: NO automatic fallback (syslinux `boot:` prompt, stuck `rebooting`); recovered via `virsh console` typing a good `LABEL`. Post-recovery `completed`+mismatch never re-enqueued across a pod restart + 3 open windows on build `b1c6da5`. Live find: pod-start re-arm auto-retried once (second brick) → fixed by D34 → redeployed → validated. | Broken version is never auto-retried | stage a release whose kernel fails to boot (node falls back); node returns non-quiescent with state `completed` | `UpdateMismatch`; **no re-enqueue** at any subsequent open window; the operator re-pins or reboots it explicitly. |
| W14 | PASS 2026-09-11 — same shape as W5a (pin to an in-index, not-local version with both windows open: ~100 s no enqueue/reboot/mismatch while absent → staged → re-point → enqueue → reboot → applied); cited, not re-run. | Pin ahead of staging never enqueues early | **both** windows open; set `next-kernel` to a repository version not yet on the partition | **no enqueue, no reboot, no `UpdateMismatch` while the file is absent** (rule 4); after the updates window stages it (re-point + re-arm), the node is enqueued at the next open `reboots.windows` and reboots into it (`UpdateApplied`). |
| W15 | PASS 2026-09-11 — `FRI` (uppercase) admitted on Friday immediately; `mon` held + `QueueHeldWindow`; 5-field minute-cron admitted exactly at the minute; `@300` and `@every 5m` held identically off-grid (exactness unit-tested). | Real Vixie schedules on the cluster | `reboots.windows` with a 5-field cron + month/dow names (one minute-matched case, one non-matching), `@300` alongside `@every 5m` | names resolve (case-insensitive); the matching cron admits, the non-matching holds (`QueueHeldWindow`); `@300` opens exactly with `@every 5m` (D32, D33). |
| W16 | PASS 2026-09-11 — `[@every 5m, bogus]` + grace `0s` → both `WARN … keeping previous` (whole-key rejected); `POST` still accepted + held per the kept schedule. | Invalid window config | one bad entry in a good list; bad `*-window-grace` | whole key rejected, last valid kept + warn; behavior unchanged (no spurious opens or closes). |
| W17 | PASS 2026-09-11 — stale `simplek8s-update-plans` CM: new leader attempted delete on first Run → 403 (old RBAC) + Warn (proves the acquisition edge + rollout-race cover); after applying the RBAC + next handover → deleted. `reboot-eligible` marker → deleted at pod restart. (A decoy CM in `default` was the wrong namespace — the real leftover lives in `simplek8s`.) | Migration cleanup | pre-create a stale `simplek8s-update-plans` ConfigMap and a leftover `reboot-eligible` annotation | the leader deletes the ConfigMap (live RBAC `delete`); the pod deletes the leftover at start; no plan behavior remains. |

## 8. Deferred

- TODO item 10 (leader-centralized check + distribution) is untouched by
  this plan and stays deferred — the per-node check stays per-node; M3
  only gates it.
- The pod split (TODO item 3) remains the home of any future hardening
  of the boot-partition mount/write path.
- TODO item 12 (BUG 12) is closed by this plan: the plan layer it lived
  in is removed; W5 is its end-to-end regression.
- **TODO 13 remainder — public `v*` image publishing + CI**: a registry
  (the `ghcr.io/simplek8s/simplek8s-controller` name is reserved by M1
  and the `Makefile`, but no repo exists yet), the tag scheme (timestamp / incremental / semver —
  undecided), the build-check workflow, and the release push. Explicitly
  out of M3 (maintainer decision, 2026-09-09); §3.11 carries the notes
  for when it is built (LFS checkout, binfmt, provenance).
- **arm64 E2E**: no arm64 test node exists yet; when one is stood up,
  deploy the aarch64 image and run one full auto-update (W4-shaped).
- TODO items 2 (CLI), 4 (rollback helper), 5 (keyring), 7 (reboot-log
  observability) are untouched by this plan and stay deferred.

## 9. Risks & safety notes

Accepted, documented (KISS — no mechanism added in M3):

- **Synchronized check bursts (thundering herd).** `@every` occurrences
  are anchored to midnight UTC so that window openness is deterministic
  across pods and leader handovers — but the same anchor makes *all*
  nodes check in the same 5-minute grace, twice a day with the default.
  With many nodes that concentrates `SHA256SUMS` (and, on a new release,
  the same `.efi.zst` download) onto the release repository in a narrow
  window. At the project's current cluster sizes this is negligible;
  the real fix is TODO 10 (leader-centralized check + distribution),
  which remains deferred. A per-node deterministic jitter (hash of the
  node name, inside the grace) is the cheap mitigation if it ever
  matters.
- **Automatic reboots are off by default.** An install
  that relied on auto-reboots loses them at rollout until
  `reboots.windows` is configured — check/staging keep running and the
  nodes wait non-quiescent, so the state is visible, not silent; the
  operator must act (§5). This is the cost of "reboots are opt-in".
- **Always-open edge.** A grace longer than the occurrence period
  (e.g. `@hourly` with a 2h grace) makes a window always open —
  documented operator error, not validated (§3.1).
- **`failed` is a stop, not a retry.** A node that fails to boot the
  updated kernel (falls back) is never auto-retried (W13); it waits for
  the operator (re-pin, force, or the M1 API). Carried over from M2's
  accepted consequence.
- **Manual deletion of the goal file.** An operator who deletes the
  goal kernel file by hand leaves `next-kernel` pointing at an absent
  file. The node is simply not enqueued while it is absent (the pod's
  presence check at enqueue time, §3.4); convergence is via the
  defensive re-staging (value repository-valid) or the goal correction
  (value not in the verified index, §3.10). No wasted reboot and no
  unbootable state on the controller's own paths; a reboot **forced**
  by the operator in that state is out of contract (manual file
  deletion).
- **No catch-up, one write per occurrence.** A pod down for a whole
  occurrence misses it; a failed check consumes the occurrence (§3.4)
  — a behavior change from M2's interval timer. Each occurrence also
  costs one `update-last-check` RMW per node: short windows (the
  W-campaign itself uses `@every 2m`) multiply etcd writes; size
  windows for the fleet, not the test cluster.
- **Safe-state auto-reboot surprise.** A correction to newest-local ≠
  running makes the node reboot-eligible in `full` (W12): an operator
  typo can cause an unattended reboot at the next open window. The
  `UpdateGoalCorrected` event is the signal — watch it.
- **DELETE-defer loop.** Repeated `DELETE`s while the window is open
  re-enqueue indefinitely (§3.4, decision 30): an automation fighting
  the controller burns one RMW per cycle. No suppression state by
  design.
- **RBAC `delete` is permanent.** The widened `configmaps` verb exists
  only for the self-cleaning migration and is never removed afterwards.
  Rollout order matters: the role before the image, or the cleanup
  403-loops until propagation (decision 15).
- **Clock basis.** Openness is UTC wall-clock; skew/NTP behavior across
  pods is unstated — enqueue-then-hold is the designed straddle
  outcome (§3.4), never a missed gate.
- **`force` voids the guarantee by design.** A forced reboot bypasses
  windows like it bypasses PDB (decision 9) — the window promise covers
  non-forced work only.
