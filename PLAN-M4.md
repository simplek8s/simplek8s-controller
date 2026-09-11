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

M4 closes TODO 14 in two layers, in order:

1. **Stop booting garbage (mechanism, this plan's core).** Validate the
   staged image before enqueue — a PE32+ header check that rejects
   truncated, zeroed, partially written or wrong files. The W13
   hand-corruption stands in for these realistic classes (torn writes
   on power loss, storage corruption on a checksum-less vfat, bad
   payloads that pass transport); adversarial hand edits as such stay
   out of contract, as do manual deletions. Corrupt-but-present never
   enqueues; the operator deletes the file and the existing defensive
   re-stage heals it from the repo.
2. **Detect + recover fast when prevention fails (runbook).**
   Bootloader-brick alert recipe (no new controller alert path) and a
   console recovery runbook (typed-`LABEL` boot, proven twice live).
3. **Decide the real fallback (spike, not a build commitment).** A
   time-boxed kexec-chooser spike reports whether a try-and-fall-back
   boot is viable on this distro; its report gates a future M5. M4
   builds no chooser.

Non-goal: making unbootable kernels bootable. A kernel that loads but
panics mid-boot is indistinguishable from garbage at the bootloader
level; both land in the same `completed`+mismatch state and the same
no-retry discipline.

## 2. Constraints

- **Stdlib only** (PLAN.md §2 unchanged): PE parsing is `encoding/binary`
  over a 64 KiB prefix — no new dependency.
- **No new annotation, no new state.** The header check is stateless
  (reads the file, compares against constants + node arch); the claim
  and eligibility machinery is untouched.
- **UTC/clock discipline unchanged.** No timing involved.
- **KISS.** One pure function + two call sites + one event. The full
  re-hash-vs-stage-time design is explicitly deferred (below).

## 3. Design

### 3.1 Staged-image validation (`validateKernelImage`, pure)

```go
// imagecheck.go (new, update package or bootloader.go):
// validateKernelImage checks the PE32+ boot image header:
// DOS MZ magic, sane e_lfanew, "PE\0\0" signature, COFF machine
// matching the node's release arch (0x8664 x86-64 / 0xAA64 aarch64),
// NumberOfSections > 0. Operates on the first 64 KiB + total size.
func validateKernelImage(header []byte, size int64, arch string) error
```

- Checked against the **real PROD format** (verified 2026-09-11 on
  `simplek8s.202609090435.x86-64.efi`: `MZ` @0, `PE\0\0` @0x40,
  machine `0x8664`, PE32+ magic `0x20b`).
- Call site 1 — **post-copy in `stagePartition`**: the bytes just
  written are page-cache hot. Failure → remove the partial file and
  fail staging through the existing error path (no new semantics:
  staging already fails this way on download/extract errors).
- Call site 2 — **verify step of `EnsureBootGoal`** (physical store
  only): failure → new typed error `ErrGoalInvalid` (distinct from
  `ErrGoalAbsent`: present-but-unusable vs missing).
- The fake store keeps name-level presence (no bytes to check).
- Residual, accepted: a file with intact headers but corrupt body
  (subtle bitrot) still passes. The full design — sha256 of the
  written `.efi` recorded at stage time, re-verified pre-enqueue —
  needs persisted state for the hash and is deferred; the header
  check kills the demonstrated class (garbage, truncation,
  wrong-file) at ~1% of the cost.

### 3.2 Enqueue gate (`ErrGoalInvalid` path)

`maybeEnqueue`: `EnsureBootGoal` → `ErrGoalInvalid` ⇒ **no enqueue**,
Warn log + new Warning event `UpdateBootGoalInvalid` (rate-limited per
node, same edge discipline as `UpdateHeldWindow`; cleared when the
file becomes valid, absent, or the goal moves). No auto-correction,
no auto-delete: a corrupt-but-present file is presumed manual
interference (out of contract, like hand deletion). Operator recovery,
in order of preference:

1. **Delete the corrupt file** → the next occurrence's defensive
   re-stage re-downloads it from the verified index (existing
   mechanics, no new code), then the loop proceeds.
2. **Restore by hand** (copy a good file over it) → the next cycle's
   check passes and the loop proceeds.
3. **Re-pin** to a good version (running or otherwise present).

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

### 3.5 kexec-chooser spike (time-boxed, gates M5)

Questions the spike must close (established facts 2026-09-11):

- Kernel: `CONFIG_KEXEC=y` (classic `kexec_load`), `KEXEC_FILE` **off**,
  `/sys/kernel/kexec_loaded` present; **no kexec-tools on the distro**.
- Payload: PE/UKI `.efi` (EFI stub) — classic kexec cannot load PE
  directly; needs `.linux`/`.initrd` section extraction (objcopy or a
  Go PE split at stage time).
- Bootloader: syslinux 6.04, config at `syslinux/syslinux.cfg`, no
  `TIMEOUT`/`PROMPT`/`ONTIMEOUT` lines (failure ⇒ prompt, waits
  forever). `ldlinux.c32` present. No native failure fallback —
  confirmed by reading the config + live.
- Rejected without spike: UEFI `BootNext` (nodes boot legacy BIOS via
  SeaBIOS/syslinux; per-version EFI entries would be new distro
  machinery), systemd-boot boot counting (distro migration, not this
  repo), watchdog-only (resets into the same broken default — a loop,
  not a fallback), stock COM32 fallback module (none exists).

Spike deliverable: minimal chooser shape (static kexec binary +
initramfs? who builds/ships/updates it — distro touchpoints),
PE-section extraction point (stage time vs boot time), failure modes
of the chooser itself (it must be pinned immutable), and a go/no-go
with cost for M5. Timebox: 3 days. If no-go, the fallback stays
detect+recover (this plan) and TODO 14 records the outcome.

### 3.6 AGENTS.md bootloader path fix

Ground truth 2026-09-11 (mounted cp1): kernels at
`/mnt/boot/simplek8s/`, config at `/mnt/boot/syslinux/syslinux.cfg`
(matching `syslinuxConfigRel` in code). `AGENTS.md` says
`/mnt/boot/syslinux.conf` — wrong. Fixed in the M4 commit.

## 4. Decision log (M4, new)

| # | Decision | Rationale |
|---|---|---|
| 1 | PE-header check, not full-hash state | Stateless (no annotation, no claim mechanics); kills the demonstrated class (garbage/truncation/wrong-file). Subtle bitrot is the accepted residual; the hash design waits until the residual bites. |
| 2 | Validate at stage-post-copy AND pre-enqueue | Post-copy catches bad writes while hot; pre-enqueue catches later corruption (the W13 shape). Same pure function, existing error paths. |
| 3 | `ErrGoalInvalid` ⇒ no enqueue + Warning event, never auto-correct | A corrupt-but-present file smells of manual interference; the controller must not destroy operator evidence. Delete-then-restage heals via existing mechanics. |
| 4 | No automatic fallback in M4 | Syslinux cannot do it (verified); the chooser needs the spike. M4 is detect + recover + prevent-garbage. |
| 5 | Alert as recipe/docs, no new alert path | No metrics endpoint exists to hang automation on (TODO 7); a kubectl recipe works today. |
| 6 | kexec spike time-boxed (3 days) gating M5 | The chooser is the only viable automatic-fallback shape found; committing to build it before validating PE extraction + distro touchpoints would be a blank check. |
| 7 | Recovery-backup sidecar dir not blessed | Ad-hoc, outside purge ownership, confuses inventory. Delete-then-restage is the blessed recovery. |

## 5. Behavior changes & migration

- A staged file failing validation **no longer enqueues** (previously:
  reboot into it). Strictly safer; the only behavior delta in M4.
- New Warning event `UpdateBootGoalInvalid` (keyed per node).
- No migration: no annotation, no RBAC, no ConfigMap change.

## 6. Implementation

### 6.1 Modules

| Module | Change |
|---|---|
| `internal/features/update` (`imagecheck.go` new) | `validateKernelImage` pure + `ErrGoalInvalid`; unit tests with synthetic PE headers (no fixtures). |
| `internal/features/update` (`staging.go`) | post-copy header check (remove partial + fail staging on error). |
| `internal/features/update` (`bootstore.go`) | `EnsureBootGoal` verify step runs the header check (physical only). |
| `internal/features/update` (`enqueue.go`) | `ErrGoalInvalid` → Warn + `UpdateBootGoalInvalid` edge, no enqueue. |
| README ops + TODO 14 | runbook + alert recipe; TODO 14 → resolved-or-spiked per §6.3. |

### 6.2 Unit test matrix

- `validateKernelImage`: real PROD header shape (MZ/e_lfanew/PE/machine/sections — values captured 2026-09-11); garbage (short/zero/random); truncated mid-PE; wrong machine (aarch64 image on amd64 node and vice versa); `e_lfanew` out of bounds; zero sections.
- `stagePartition`: post-copy failure removes the partial and fails the stage (fault-injected writer).
- Enqueue: invalid-but-present goal file → no RMW + `UpdateBootGoalInvalid`; file healed (deleted → re-staged, or restored) → proceeds next cycle.
- Fake store unchanged (name-level presence).

### 6.3 Phases

| Phase | Content |
|---|---|
| 1 | Validation + event + unit tests (no behavior change except the new gate). |
| 2 | Runbook + alert recipe docs; live V1/V2 (§7); TODO 14 updated. |
| 3 | kexec spike (parallelizable from day one; 3-day box) → M5 go/no-go. |

## 7. E2E (V-cases, live)

| # | Case | Trigger | Expect |
|---|---|---|---|
| V1 | Corrupt staged file never enqueues | pin an uncached version → staged → corrupt the file by hand (fault injection for torn writes/storage corruption — not a threat model in itself) with windows open | **no enqueue** (state stays absent) + `UpdateBootGoalInvalid` Warning; then delete the corrupt file → defensive re-stage → enqueue → reboot → applied. Safe: garbage is never booted (contrast W13, which bricked). |
| V2 | Alert recipe + runbook rehearsal | tabletop against the W13 evidence (timestamps from the campaign) + live query-shape check; console `LABEL` boot already proven twice (W13) — cited, not re-bricked | recipe detects the `rebooting`+NotReady shape; runbook steps execute as written. |

## 8. Deferred

- Full-hash state (sha256 of the written `.efi` recorded at stage time,
  re-verified pre-enqueue) — waits until subtle bitrot (not garbage)
  is observed in the field.
- The chooser build itself (M5 candidate on spike go).
- UEFI/systemd-boot migration (distro decision, not this repo).
- Per-pool/canary windows (fleet batches contain bricks but fix no
  boot failure — separate future item if wanted).

## 9. Risks & safety notes

- The header check runs on every enqueue path entry (one 64 KiB read
  per cycle while eligible-and-open — negligible) and once per staging.
- A false-negative header check (valid headers, broken body) still
  bricks exactly like today; the alert recipe + runbook is the net.
- The spike must not touch the test cluster's boot partitions beyond
  read-only mounts.
