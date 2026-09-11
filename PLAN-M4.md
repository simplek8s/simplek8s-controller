# PLAN-M4 — boot failure fallback (TODO 14)

Era tag: **M4** = boot fallback. Status: **planning v1** (2026-09-11).
Precedent: like PLAN-M3, this file lives separately while the plan is
worked; it folds into `PLAN.md` when shipped (era tags M1/M2/M3 are
defined in `PLAN.md` §1).

## 1. Purpose

W13 (PLAN.md §7.4) proved the negative: a staged kernel that fails to
boot has **no automatic fallback**. Syslinux 6.04 loads the broken
entry (`Loading ... ok`), fails (`Booting kernel failed: Invalid
argument`) and drops to a `boot:` prompt, where the node sits
`rebooting` (no executor evidence → no auto-fail, by design) until an
operator types a good `LABEL` at the console. The `completed`+mismatch
verification and the no-auto-retry guarantee (D16/D30/D34) all held —
only the return path was manual.

M4 closes TODO 14 in two layers, in order. (What M4 does NOT chase:
content integrity — the pipeline already guarantees it end to end:
GPG-signed index → sha256-verified `.zst` download → checksum-verified
extract → O_SYNC write. Bytes that fail anywhere in that chain never
reach the boot partition through the controller. The residual below
is about bytes that are *correct* but don't boot.)

**Correction (2026-09-11, verified in the syslinux wiki): automatic
fallback for load failures DOES exist natively** — `TIMEOUT` +
`ONTIMEOUT`: the timeout timer *resets on return from an unsuccessful
boot attempt*, so a failed `DEFAULT` falls back to the `ONTIMEOUT`
label automatically after the timeout. The earlier "syslinux cannot
do it" claim was wrong (it holds only for panics *after* a successful
load, which never return to the prompt). M4 therefore builds the
native fallback; the kexec chooser is demoted to a deferred
panic-class study.

1. **Native fallback for load failures (mechanism).** The writer
   maintains `TIMEOUT 50` + `ONTIMEOUT <fallback>` in `syslinux.cfg`
   (§3.2); a failed `DEFAULT` boots the fallback automatically after
   5 s. No new artifacts, no distro changes, no state.
2. **Detect + recover fast (runbook).** Bootloader-brick alert recipe
   (no new controller alert path) and a console recovery runbook
   (typed-`LABEL` boot, proven twice live) — now the *backup* path
   (panic class, single-entry edge), not the primary.
3. **Panic-class fallback deferred, not spiked.** A kernel that loads
   but panics never returns to the prompt — only watchdog+chooser
   covers it, which needs distro involvement. Deferred (see §8).

Non-goal: making unbootable kernels bootable, and covering panics.
Split by observability at the prompt: *load failures* (bad image,
incompatible format) return to the prompt and are covered by the
native fallback; *panics after a successful load* never return and
stay manual (watchdog+chooser territory, deferred). Both converge
afterwards in the same `completed`+mismatch state and the same
no-retry discipline.

## 2. Constraints

- **Stdlib only** (PLAN.md §2 unchanged): nothing new to depend on.
- **No new annotation, no new state.** Managed config lines only —
  the claim, eligibility and reboot machinery are untouched.
- **UTC/clock discipline unchanged.** No timing involved.
- **KISS.** Writer lines + guards, runbook + recipe, deferred panic
  class. The rejected validation design is recorded in §3.1 so it is
  not re-proposed without new evidence.

## 3. Design

### 3.1 Staged-image validation — considered and REJECTED

A PE32+ header check (MZ / `PE\0\0` / machine vs node arch over the
first 64 KiB) at stage-post-copy and pre-enqueue would have blocked
the W13 garbage. Rejected: it defends against filesystem failures
(torn writes, bitrot on a checksum-less vfat) and operator sabotage
(`rm -rf /` class) — both outside the controller's contract. And it
could not catch the real threat anyway: a genuinely bad release has
perfect headers by construction (GPG → sha256 → verified extract
already guarantee the bytes). The W13 injection keeps its value as
fault injection proving the alert/no-retry machinery, not as a class
to prevent. (If torn writes are ever observed in the field, the
principled micro-fix is atomic rename instead of in-place truncate —
not content sniffing.)

### 3.2 Native fallback: `TIMEOUT` + `ONTIMEOUT` (mechanism)

Semantics (syslinux wiki, `Config`): `TIMEOUT` waits N/10 s at the
`boot:` prompt before autobooting; any typed key cancels it (manual
recovery unaffected — `TOTALTIMEOUT`, which ignores input, is
deliberately NOT used). The timer **resets on return from an
unsuccessful boot attempt**; on expiry syslinux boots `ONTIMEOUT`
(`DEFAULT` if absent). Chain for a failed `DEFAULT`: load fails →
prompt → timer resets → 5 s → fallback boots. Automatically, no
console, no new artifacts.

Writer policy (`writeNewSyslinuxConfig`, syslinux only):

- `TIMEOUT 50` (5 s) is a **managed line**, always written. Cost: 5 s
  on every normal reboot (negligible next to ~20–30 s M1 cycles);
  benefit: a human can still read/interrupt, and recovery waits at
  most 5 s. Constant, not a ConfigMap key (no new dials).
- `ONTIMEOUT <label>` is managed per re-point: the fallback is the
  **running** kernel's label when its file is present and differs
  from the new `DEFAULT`; else the newest present label differing
  from the new `DEFAULT`; else the line is **omitted**.
- Managed-lines doctrine (like `DEFAULT`): the writer owns these two
  lines and documents it; hand-set values are overwritten. All other
  globals stay verbatim as today.
- Single-entry edge (only one local kernel): no distinct fallback
  exists, `ONTIMEOUT` is omitted, and a load failure retries `DEFAULT`
  every 5 s instead of waiting at the prompt. Bounded harm (reads
  only, alert fires on never-Ready) — accepted and documented, not
  special-cased.

Purge/prune guards (same session, existing discipline):

- `protectedSet` gains the `ONTIMEOUT` label's ts (read from the
  current file like `DEFAULT`): the fallback file is never purged.
  Applies to both the capacity pre-check and the retention purge,
  which each read the file as it stands.
- The prune guard extends from "the `DEFAULT` block" to "the
  `DEFAULT` and `ONTIMEOUT` blocks" (pure-function parameter becomes
  the protected-label set).

Coverage begins at the first controller bootloader write (staging or
re-point): nodes that never staged keep the distro file byte-identical
until then. Every path that sets `DEFAULT` (`stagePartition` step 6,
`EnsureBootGoal` re-point) maintains the lines — one code path in the
writer, no caller changes.

Explicitly NOT covered: panics after a successful load (never return
to the prompt — watchdog+chooser territory, §8); rpi `config.txt`
(single `kernel=` line, no fallback concept — scope syslinux-only,
documented).

### 3.3 Brick alert recipe (docs, no code)

New controller alert paths wait on metrics (TODO 7); until then, the
recipe is a kubectl query reviewers can run and cron:

- Condition: node `reboot-state == rebooting` **and** Node
  `Ready != True` for longer than `reboots.reboot-issue-grace` + margin
  (≈20 min). Rationale: a healthy reboot is Ready-flapping for
  ~1–2 min (M1 E2E); past the grace with no executor evidence, the
  bootloader never handed off.
- Query shape (documented in the runbook with copy-paste):
  nodes whose `simplek8s.org/reboot-state` parses to `rebooting`
  joined against `Ready != True` + `since` age.
- **Negative compra:** `rebooting` + Ready is normal mid-cycle;
  `completed`+mismatch is the *other* failure shape (already evented).

### 3.4 Console recovery runbook (docs, proven twice in W13)

1. Confirm the brick: `virsh screenshot` (or console) shows syslinux
   `Loading <file>... ok` + `Booting kernel failed` + `boot:` prompt.
2. Type a good `LABEL` at the prompt (`virsh send-key … KEY_ENTER`
   works headlessly; label names are `simplek8s.<ts>.<arch>`).
3. After boot: the node lands `completed`+mismatch (goal ≠ running) —
   expected, no retry fires.
4. Repair the bad file (delete → defensive re-stage, §3.2), re-pin to
   running, DELETE the state → quiescent.
5. Record the incident (which file, suspected cause).

The W13 ad-hoc `/mnt/boot/recovery-backup/` sidecar dir is **not**
blessed: it eats partition space outside the purge's ownership and
confuses the file inventory. Delete-then-restage is the blessed path.

### 3.5 Panic-class fallback — deferred (record)

A kernel that loads but panics never returns to the prompt: only a
watchdog reset plus a chooser to land on covers it, which needs distro
involvement (chooser artifact, PE section extraction for the UKI
payload — classic `kexec_load` only, `KEXEC_FILE` off — plus
kexec-tools, absent from the distro). Established 2026-09-11; no spike
in M4. The chooser also remains the only cover if syslinux ever proves
the timer-reset behavior version-dependent — the V2 case below would
catch that on day one.

### 3.6 AGENTS.md bootloader path fix (done)

Ground truth 2026-09-11 (mounted cp1): kernels at
`/mnt/boot/simplek8s/`, config at `/mnt/boot/syslinux/syslinux.cfg`
(matching `syslinuxConfigRel` in code). `AGENTS.md` says
`/mnt/boot/syslinux.conf` — wrong. Fixed in the M4 commit.

## 4. Decision log (M4, new)

| # | Decision | Rationale |
|---|---|---|
| 1 | No content validation in M4 (header check considered and rejected) | It defends against filesystem failures and operator sabotage (`rm -rf /` class) — outside the controller's contract. And it could not catch the real threat anyway (valid-but-unbootable has perfect headers). |
| 2 | Native fallback via `TIMEOUT`+`ONTIMEOUT` (verified in the syslinux wiki, not assumed) | Load failures return to the prompt and the timer resets — the only automatic-fallback shape with no new artifacts, no distro changes, no state. |
| 3 | Fallback target = running-preferred, newest-local fallback, omit if none | Running is the proven-bootable choice; newest-local covers the running-file-absent shape (W12c); omitting (rather than looping deliberately) keeps single-entry behavior closest to today, with the retry loop accepted as documented. |
| 4 | `TIMEOUT 50` constant, managed lines, no new key | 5 s per normal reboot is negligible; a ConfigMap dial for this would be knob proliferation. Hand-set `TIMEOUT`/`ONTIMEOUT` are overwritten like `DEFAULT` (documented). |
| 5 | `protectedSet` + prune guard cover `ONTIMEOUT` | A fallback whose file gets purged — or whose entry gets pruned — is worse than none (silent dangling reference). Both guards read the file as it stands, per purge. |
| 6 | Alert as recipe/docs, no new alert path | No metrics endpoint exists to hang automation on (TODO 7); a kubectl recipe works today. Single-entry retry loop and panic class are both "never Ready" shapes it catches. |
| 7 | Panic class deferred (watchdog+chooser, distro) | Needs distro involvement; the V-cases below would catch syslinux version-dependence of the timer-reset on day one. |
| 8 | Recovery-backup sidecar dir not blessed | Ad-hoc, outside purge ownership, confuses inventory. Delete-then-restage is the blessed recovery. |

## 5. Behavior changes & migration

- `syslinux.cfg` gains two managed lines (`TIMEOUT 50` always;
  `ONTIMEOUT <label>` when a distinct fallback exists), written on the
  next controller bootloader write; nodes that never staged keep the
  distro file byte-identical. Hand-set values are overwritten
  (documented, like `DEFAULT`).
- Normal reboots take +5 s at the prompt (autoboot delay).
- A failed `DEFAULT` boots the fallback automatically (~5 s) instead
  of waiting at the prompt; single-entry files retry `DEFAULT` every
  5 s (accepted, alerted).
- No annotation, no RBAC, no ConfigMap, no event, no migration.

## 6. Implementation

### 6.1 Modules

| Module | Change |
|---|---|
| `internal/features/update` (`bootloader.go`) | `writeNewSyslinuxConfig` manages `TIMEOUT`/`ONTIMEOUT` per §3.2; fixture tests incl. the live cp1 shape (no pre-existing lines), idempotency, hand-set overwrite, single-entry omission. |
| `internal/features/update` (`staging.go` + `purge.go`) | `protectedSet` gains the `ONTIMEOUT` ts (both purges read the file as it stands). |
| `internal/features/update` (`bootloader.go` prune) | prune guard covers the `ONTIMEOUT` block (protected-label set parameter). |
| README ops + TODO 14 | runbook (automatic-first, console as backup) + alert recipe; TODO 14 → shipped per §6.3. |

### 6.2 Unit test matrix

- Writer fixtures: live cp1 shape (no `TIMEOUT`/`ONTIMEOUT` → both
  added); idempotent re-write (byte-identical second pass);
  hand-set values overwritten; single-entry file (no `ONTIMEOUT`
  emitted); pre-existing `ONTIMEOUT` re-pointed.
- `protectedSet`: `ONTIMEOUT` ts protected at both purge call sites.
- Prune: `ONTIMEOUT` block spared even when its file is gone;
  `DEFAULT`+`ONTIMEOUT` same label (degenerate) handled.

### 6.3 Phases

| Phase | Content |
|---|---|
| 1 | Writer + guards + unit tests (no behavior change until a bootloader write happens on a node). |
| 2 | Live V1–V3 (§7) + docs (runbook automatic-first); TODO 14 shipped. |

## 7. E2E (V-cases, live)

| # | Case | Trigger | Expect |
|---|---|---|---|
| V1 | Fallback lines on disk | stage any version (running R) on a worker | `syslinux.cfg` carries `TIMEOUT 50` + `ONTIMEOUT` == R's label (running-preferred); re-pin to another present version → `ONTIMEOUT` follows the previous running... (precisely: the newest present label ≠ new `DEFAULT`, preferring running). |
| V2 | Automatic rescue (the money case) | corrupt the current `DEFAULT`'s file by hand (fault injection — the only on-demand load failure), then M1-reboot the worker with windows open | reboot → load fails → ~5 s → fallback boots **with no console touch** → Ready on the fallback kernel → `completed` + `UpdateMismatch`, no retry. (Contrast W13: same setup without the lines = prompt forever.) |
| V3 | Alert recipe + runbook as backup | tabletop against the W13 evidence + live query-shape check; console `LABEL` boot proven twice (W13) — cited | recipe detects the `rebooting`+NotReady shape (covers the single-entry loop and any panic class); runbook executes as written. |

## 8. Deferred

- Panic-class fallback (watchdog reset + chooser to land on): needs
  distro involvement (chooser artifact, PE section extraction for the
  UKI payload — classic `kexec_load` only, `KEXEC_FILE` off — plus
  kexec-tools, absent from the distro). Record for a future M5 if
  panics demand it.
- UEFI/systemd-boot migration (distro decision, not this repo).
- Per-pool/canary windows (fleet batches contain bricks but fix no
  boot failure — separate future item if wanted).

## 9. Risks & safety notes

- `TIMEOUT` fires on every boot (5 s added to all reboots) and after
  every load failure; a version-dependent syslinux that did NOT reset
  the timer would silently keep prompt-waits — V2 catches that on day
  one (no rescue observed ⇒ investigate, do not assume).
- Single-entry retry loop (accepted): bounded, read-only, alerted.
- Failback-target staleness: writer + guards keep `ONTIMEOUT`
  consistent at every bootloader write; a hand-deleted fallback file
  with no purge/stage afterwards dangles until the next write (same
  class as a hand-deleted `DEFAULT` today — out of contract).
