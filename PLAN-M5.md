# PLAN-M5 — board flavors for updates (TODO: arm64 updates) — DRAFT v1

Era tag: **M5** = arch flavors. Status: **draft, unapproved**
(2026-09-11). Do not implement from this file until approved; like
PLAN-M3, it folds into `PLAN.md` when shipped.

## 1. Purpose

Live findings on PROD (2026-09-11, read-only + rpi4-node/rpi5-node boot
partitions):

- The repo carries **no `.aarch64` artifacts at all**: flavors are
  `x86-64`, `arm64`, `rpi4`, `rpi5` — and `arm64` is the *legacy*
  pre-rpi5 naming (verified 2026-09-11: recent releases ship only
  `rpi4`/`rpi5`/`x86-64`; the 118 `arm64` files are all old
  releases). A node still carrying `*.arm64.efi` files lives on an
  ended lineage: it updates within `arm64` artifacts only (stale but
  harmless), and newer ts have nothing for it. `MapArch` (`versions.go`) translates node `arm64` →
  `aarch64`, so on **every** arm64 node the check matches nothing and
  updates silently no-op. M2-era bug, never caught (no arm64 update
  E2E ever ran).
- rpi4-node (rpi4, `bcm2711`) stages `*.rpi4.efi`; rpi5-node (rpi5,
  `bcm2712`) stages `*.rpi5.efi`; both keep a `simplek8s.yaml`
  (+`.example`); rpi5-node also carries `simplek8s.latest.rpi5.efi`
  (distro convention — our version regex ignores non-numeric names,
  so it is never touched).
- Nodes expose only `arch=arm64` (no board labels). The flavor is
  discoverable **only** from the boot partition contents (staged
  filenames) or the host device-tree.
- Legacy `arm64` lineage ended with rpi5 (above): old nodes keep
  working within their artifacts; newer ts resolve per §3.6.

M5 makes updates work per board flavor, with rpi4 proven live on
rpi4-node. It does not change the update state machine (§3.4 stands);
it changes *which files* each node considers its own.

## 2. Constraints

- **Stdlib only, no new annotation.** Flavor resolution reads the
  partition scan the pod already does; no override annotation (D3
  rejected — no flavor migration exists to drive it).
- **Pins stay flavor-agnostic.** `next-kernel` carries a bare `ts`;
  the flavor resolves per node at use time. Pinning the same ts on
  x86-64 and rpi4 nodes does the right thing on each.
- **Never cross flavors on write.** A node writes, purges, prunes and
  re-points only its own flavor's files (see §3.4 — today purge/prune
  match any `simplek8s.<ts>.<arch>.efi`).
- **KISS.** One choke point (`MapArch` → flavor resolution); all
  downstream naming is already parameterized by arch string
  (`kernelStoredName`, `kernelArtifactName`, `EnsureBootGoal`).

## 3. Design

### 3.1 Flavor type + resolution order

```go
// Flavor is a release artifact flavor: "x86-64", "arm64", "rpi4", "rpi5".
// ResolveFlavor returns the node's flavor, inferred from staged
// filenames on the boot partition (first arch seen in
// versionFromStoredKernel matches). Unresolvable (empty/foreign-only)
// ⇒ D2 fail-closed.
// NOTE (D3 rejected 2026-09-11): no override annotation — there is no
// flavor migration to drive it (see §3.7).
//  3. CLOSED (D2, 2026-09-11, refined): identify the EXPECTED boot
//     partition among confusing candidates (§3.7); no candidate
//     verifying ⇒ fail closed, touch nothing.
```

- x86-64 nodes: `MapArch` as today (`x86-64`) — no behavior change;
  their partitions only ever hold that flavor.
- arm64 nodes: `MapArch` keeps mapping node arch (used for nothing
  flavor-specific after this plan); the *flavor* comes from §3.1.
- `latest`-style non-numeric files never match the version regex and
  stay foreign, as today.

### 3.2 Check filtering per flavor

`Check` keeps a release file iff `ParseKernelRelease` yields
`(ts, flavor == node flavor)`. `Latest`/`Available`/`Sums` semantics
unchanged, now flavor-scoped. Consequence: an arm64 node with no
matching flavor files in the index sees no update (same silence as
today, but now correct-by-construction per flavor instead of
accidentally-void).

### 3.3 Staging / re-point names (no format change)

`kernelStoredName(ts, flavor)` / `kernelArtifactName(ts, flavor)` —
call sites pass the resolved flavor instead of `MapArch` output. The
rpi bootloader path (`setRPIDefault`, `config.txt` single `kernel=`)
is exercised live for the first time by §7 F2 (no code change
expected, but never proven — that is the point).

### 3.4 Flavor-scoped ownership (purge / prune / defensive)

Today `listKernels`/`planPurge`/`pruneSyslinuxEntries` match any arch
by pattern. Under flavors that is wrong (an rpi4 node must never
purge a stray `x86-64` file, and vice versa):

- `listKernels` gains a flavor filter (empty flavor = unfiltered, for
  the pre-resolution scan in §3.1 only).
- Purge planning/apply, prune candidates, and the defensive
  re-stage target consider **own-flavor files only**. Foreign-flavor
  files are left exactly like foreign entries today: never touched.

### 3.5 Detection details

- Inference scans the already-mounted `Versions()` result — no extra
  mount. First arch observed wins; mixed-flavor partitions resolve to
  the first and log the mix at Warn (pathological, operator-owned).
- Empty partition (no staged files): falls to §3.1 rule 3 (open).
- No override annotation (D3 rejected): every escape hatch below is
  a one-time ssh, which beats permanent API surface on a 7-node fleet
  with working ssh.

### 3.6 Boot device identification and verification (D2)

`findBootDevice` today stops at the first `EFI`/`boot` filesystem
label — a stray USB stick, a second disk's ESP, or a reused `boot`
label elsewhere would be mounted, and on a write path,
written. Other partitions CAN be confused with ours, so discovery
becomes enumerate-then-verify:

1. **Enumerate all candidates**: `PARTLABEL=boot` first (the distro
   convention per AGENTS.md; GPT only — absent on MBR layouts like
   the rpi nodes), then filesystem labels `EFI`/`boot` via by-label
   symlinks and the `blkid` export scan (current sources, kept).
2. **Verify each candidate** (mount, read-only check): it must
   contain the `simplek8s/` directory AND a bootloader config
   (`syslinux/syslinux.cfg` or `config.txt`). First verifying
   candidate wins; a second one Warns loudly (pathological —
   operator-owned ambiguity, first wins deterministically).
3. **No verifying candidate ⇒ fail closed**: Warn + no updates, no
   mounts left behind, nothing written anywhere. A genuinely fresh
   (formatted-but-empty) partition heals with one manual `mkdir
   simplek8s` — then it verifies.
4. **Cache the verified device per pod lifetime** (topology does not
   change under a running pod): re-resolve from scratch on any
   mount/verify failure. Bounds the multi-candidate cost (scans run
   per occurrence, not per mount storm) and survives device renames.

Note the layering: this answers "is this OUR partition" before any
flavor logic runs. An empty partition fails verification (no dir),
which subsumes the old empty-case rule.

### 3.7 Legacy `arm64` lineage + out-of-flavor pins

`arm64` is a first-class flavor (legacy nodes keep working within
their old artifacts), with no live E2E (no such hardware here).
Two rules for the lineage end:

Legacy nodes keep working within their old artifacts; newer ts have
nothing for their flavor, so they (correctly) see no updates. There
is no migration path by design (D3): a pin whose ts exists only under
other flavors is unverifiable for your node and follows the normal
W12 path-2 rules (correct to safe state like a never-existed ts —
the index, scoped to your flavor, does not contain it).

## 4. Decision log (M5, open — numbers restart per era)

| # | Decision | Rationale / status |
|---|---|---|
| 1 | Flavor set `{x86-64, arm64, rpi4, rpi5}`; `aarch64` dropped (repo has none) | Match reality, not Debian naming. `MapArch` keeps existing for node-arch mapping; flavor is separate. |
| 2 | Identify the expected boot partition; fail closed on ambiguity (CLOSED 2026-09-11, refined) | Other `EFI`/`boot`-labeled partitions can confuse first-match discovery and would be mounted (then written). Enumerate all candidates (`PARTLABEL=boot` preferred, then fs labels), verify contents (`simplek8s/` + bootloader config), first-verifying wins, none ⇒ touch nothing. Device cached per pod lifetime, re-resolved on failure. |
| 3 | No override annotation (REJECTED 2026-09-11) | No flavor migration exists; every escape (legacy bootstrap, empty partition, mixed cleanup) is a one-time ssh. Permanent API surface for nonevents is declined. |
| 7 | Out-of-flavor pins follow normal W12 rules (no special case) | With no migration, a ts absent from your flavor's index is simply not verifiable: path-2 corrects exactly like a never-existed ts. No extra event, no extra code path. |
| 4 | Purge/prune/defensive scoped to own flavor | Cross-flavor deletion would be data loss by design (a stray foreign file is the operator's, like any foreign entry). |
| 5 | Generic `arm64` supported-but-unexercised | Strings are cheap; live proof waits on hardware. |
| 6 | rpi5 live E2E waits on rpi5-node drain approval | Same code path as rpi4 + unit matrix; the drain (postgres, gateway) is the cost, not the code. |

## 5. Behavior changes & migration

- arm64 nodes go from silent no-op to flavor-scoped updates. x86-64
  behavior byte-identical (same flavor string as before).
- Pins need no migration (bare `ts`, resolved per node).
- No annotation (unless D3), no RBAC, no ConfigMap change.

## 6. Implementation

### 6.1 Modules

| Module | Change |
|---|---|
| `internal/features/update` (`versions.go`) | flavor type + `ResolveFlavor` from staged filenames; `MapArch` removed (its `aarch64` output matched nothing — the bug). |
| `internal/features/update` (`bootstore.go`) | `findBootDevice` rework per §3.6: PARTLABEL preference, blkid export already parsed, candidate enumeration, contents verification (`simplek8s/` + bootloader config), per-pod device cache with re-resolve on failure. |
| `internal/features/update` (`check.go`) | filter index by flavor; `res.Arch` becomes the flavor. |
| `internal/features/update` (`staging.go`, `purge.go`, `bootloader.go` prune) | flavor filter on `listKernels`; purge/prune/defensive own-flavor only. |
| `internal/features/update` (`update.go`, `enqueue.go`, `reconcile.go`) | `MapArch` call sites take the resolved flavor (plumbing). |

### 6.2 Unit test matrix

- Resolution: rpi4-only / mixed / empty / x86-64-only / foreign-only
  partitions; unresolvable ⇒ empty flavor (callers skip).
- Discovery: PARTLABEL preferred over fs label; first-verifying-wins
  with a decoy `boot`-labeled layout (unit fixtures, temp dirs);
  none-verifying ⇒ no mount left behind, no write attempted;
  cached device reused, re-resolved after an injected mount failure.
- Name construction per flavor (`stored`/`artifact`).
- Check filtering: mixed-flavor index → only own flavor visible.
- Out-of-flavor pin: ts present under other flavors → skip staging +
  `UpdateStagingSkipped` (pin kept); ts absent everywhere → W12
  correction (unchanged).
- Legacy node: only `arm64` artifacts visible; newer ts → the
  out-of-flavor skip above (no silent no-op, no correction).
- Purge/prune/defensive ignore foreign-flavor files (x86-64 file on
  an rpi4 partition survives a purge).
- `latest`-style files ignored everywhere.

### 6.3 Phases

| Phase | Content |
|---|---|
| 1 | Flavor plumbing + device identification/verification + unit tests (no behavior change on x86-64 single-candidate layouts; arm64 goes no-op → flavor-scoped). |
| 2 | Live on rpi4-node (rpi4): F1–F3 (§7); bootloader rpi proven. |
| 3 | Live on rpi5-node (rpi5): F4 (§7) — needs drain approval. |

## 7. E2E (F-cases, live)

| # | Case | Trigger | Expect |
|---|---|---|---|
| F1 | Flavor detection on rpi4-node | read-only: controller logs after deploy (or a unit-driven annotation) | flavor resolves `rpi4` from existing staged files; no writes. |
| F2 | First rpi staging | `stage` + newer `rpi4` release in index (or pin an uncached `rpi4` ts) | staged `*.rpi4.efi` + `config.txt` `kernel=` re-pointed; **no reboot**; node untouched otherwise. First live exercise of the rpi writer. |
| F3 | Full auto-update on rpi4 (W4-shaped) | `full` + windows open | enqueue → reboot → running the new `rpi4` kernel → `UpdateApplied`. |
| F4 | rpi5 on rpi5-node (pending approval) | same as F2–F3 after a rpi5-node drain | same expectations on `rpi5` files. |
| F5 | Decoy-label drill (deferred lab) | on a scratch/test VM (never PROD): extra disk carrying a decoy `boot`-labeled vfat without SimpleK8s contents | controller ignores the decoy (verification fails), uses the real partition; with NO valid partition anywhere → Warn + zero writes (provable via block-layer trace or mount audit). |

## 8. Deferred

- Generic-`arm64` live proof (no hardware) + what boots it.
- UEFI/systemd-boot questions (distro).
- Empty-partition manual bootstrap doc (stage one file of the right
  flavor by hand, once; detection adopts it).

## 9. Risks & safety notes

- **Wrong-flavor staging bricks with the wrong DTB** — the failure
  mode this plan exists to avoid creating. Mitigations: flavor pinned
  at detection (never re-derived mid-cycle — cached per pod lifetime
  like `goalKnown`), foreign files never written by purge/prune/
  defensive; F2 stages but does not reboot (human verifies the file
  before F3).
- Detection runs once per pod lifetime (cached like `goalKnown`),
  not per cycle — a mid-life mix appearing later is Warn-logged, not
  adopted.
- No override, no migration: the flavor set is closed
  (`x86-64`/`arm64`/`rpi4`/`rpi5`); anything else never resolves.
- rpi `config.txt` has a single `kernel=` line: re-point is
  all-or-nothing per write (same as syslinux `DEFAULT`, no worse).
