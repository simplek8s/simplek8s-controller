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

1. **Detect + recover fast (runbook).** Bootloader-brick alert recipe
   (no new controller alert path) and a console recovery runbook
   (typed-`LABEL` boot, proven twice live).
2. **Decide the real fallback (spike, not a build commitment).** A
   time-boxed kexec-chooser spike reports whether a try-and-fall-back
   boot is viable on this distro; its report gates a future M5. M4
   builds no chooser.

Non-goal: making unbootable kernels bootable. The residual threat is
precisely kernels that are byte-correct yet don't boot on a given
node (missing driver, early panic, incompatibility): signature,
checksums and any header check all pass on them by construction. A
kernel that loads but panics mid-boot is indistinguishable from
garbage at the bootloader level; both land in the same
`completed`+mismatch state and the same no-retry discipline.

## 2. Constraints

- **Stdlib only** (PLAN.md §2 unchanged): nothing new to depend on —
  M4 ships no code.
- **No new annotation, no new state, no new mechanism.** M4 is docs
  plus a spike — the claim, eligibility and bootloader machinery are
  untouched.
- **UTC/clock discipline unchanged.** No timing involved.
- **KISS.** Runbook + recipe + spike report. The rejected validation
  design is recorded in §3.1 so it is not re-proposed without new
  evidence.

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

### 3.2 Brick alert recipe (docs, no code)

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

### 3.3 Console recovery runbook (docs, proven twice in W13)

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

### 3.4 kexec-chooser spike (time-boxed, gates M5)

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

### 3.5 AGENTS.md bootloader path fix (done)

Ground truth 2026-09-11 (mounted cp1): kernels at
`/mnt/boot/simplek8s/`, config at `/mnt/boot/syslinux/syslinux.cfg`
(matching `syslinuxConfigRel` in code). `AGENTS.md` says
`/mnt/boot/syslinux.conf` — wrong. Fixed in the M4 commit.

## 4. Decision log (M4, new)

| # | Decision | Rationale |
|---|---|---|
| 1 | No content validation in M4 (header check considered and rejected) | It defends against filesystem failures and operator sabotage (`rm -rf /` class) — outside the controller's contract. Transport is sha256-verified, writes are O_SYNC; torn-write micro-fix (atomic rename) noted for field evidence only. |
| 2 | No automatic fallback in M4 | Syslinux cannot do it (verified); the chooser needs the spike. M4 is detect + recover. |
| 3 | Alert as recipe/docs, no new alert path | No metrics endpoint exists to hang automation on (TODO 7); a kubectl recipe works today. |
| 4 | kexec spike time-boxed (3 days) gating M5 | The chooser is the only viable automatic-fallback shape found; committing to build it before validating PE extraction + distro touchpoints would be a blank check. |
| 5 | Recovery-backup sidecar dir not blessed | Ad-hoc, outside purge ownership, confuses inventory. Delete-then-restage is the blessed recovery. |

## 5. Behavior changes & migration

None: M4 ships no controller change (docs + spike only). No annotation,
no RBAC, no ConfigMap, no event, no migration.

## 6. Implementation

### 6.1 Modules

| Module | Change |
|---|---|
| (none — no code) | M4 is runbook + alert recipe docs plus the lab spike. |
| README ops + TODO 14 | runbook + alert recipe; TODO 14 → resolved-or-spiked per §6.3. |

### 6.2 Unit test matrix

None (no code). The PROD header values captured 2026-09-11 (`MZ` @0,
`PE\0\0` @0x40, machine `0x8664`, PE32+ `0x20b`) are recorded in §3.1
for the day content validation is ever reconsidered.

### 6.3 Phases

| Phase | Content |
|---|---|
| 1 | Runbook + alert recipe docs; live V1 (§7); TODO 14 updated. |
| 2 | kexec spike (parallelizable from day one; 3-day box) → M5 go/no-go. |

## 7. E2E (V-cases, live)

| # | Case | Trigger | Expect |
|---|---|---|---|
| V1 | Alert recipe + runbook rehearsal | tabletop against the W13 evidence (timestamps from the campaign) + live query-shape check; console `LABEL` boot already proven twice (W13) — cited, not re-bricked | recipe detects the `rebooting`+NotReady shape; runbook steps execute as written. |

## 8. Deferred

- The chooser build itself (M5 candidate on spike go).
- UEFI/systemd-boot migration (distro decision, not this repo).
- Per-pool/canary windows (fleet batches contain bricks but fix no
  boot failure — separate future item if wanted).

## 9. Risks & safety notes

- M4 ships no code, so it adds no failure modes; the spike is
  read-only lab work (no boot-partition writes beyond read-only
  mounts on scratch VMs).
- The spike must not touch the test cluster's boot partitions beyond
  read-only mounts.
