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

- **Stdlib only, no new annotation (proposed).** Flavor resolution
  reads the partition scan the pod already does; the only candidate
  addition is an operator override annotation, and only if the empty
  case forces it (§4, open).
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
// ResolveFlavor returns the node's flavor:
//  1. operator override annotation (if present and valid);
//  2. inferred from staged filenames on the boot partition
//     (first arch seen in versionFromStoredKernel matches);
//  3. OPEN: fail closed (clear event, no updates) or default to the
//     arch-derived generic ("arm64"/"x86-64")? — decision needed (§4).
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
- The override annotation (if approved): `simplek8s.org/board-flavor`
  hmm — name TBD in review; values from the flavor set; invalid →
  ignored with a Warning event (same discipline as `update-url`).

### 3.6 Legacy `arm64` lineage + out-of-flavor pins

`arm64` is a first-class flavor (legacy nodes keep working within
their old artifacts), with no live E2E (no such hardware here).
Two rules for the lineage end:

- **No auto-migration across flavors, ever.** Unattended moves
  between boot flavors (e.g. `arm64` → `rpi4`) are brick-adjacent
  cleverness. Migration is manual only: set the override (§3.5/D3)
  to the target flavor, then pin the ts — staging, re-point and
  reboot follow the normal paths.
- **Out-of-flavor pins skip loudly, never correct.** A pin whose ts
  exists in the index under *other* flavors (but not the node's) is
  NOT a W12 case (the ts genuinely exists): skip staging and fire
  `UpdateStagingSkipped` (`no <flavor> artifact; version exists for
  other flavors — manual board migration needed`), keep the pin
  untouched. Only a ts absent from the whole index corrects (§3.10
  path 2 unchanged). Rationale: correcting would silently destroy an
  operator's migration intent.

## 4. Decision log (M5, open — numbers restart per era)

| # | Decision | Rationale / status |
|---|---|---|
| 1 | Flavor set `{x86-64, arm64, rpi4, rpi5}`; `aarch64` dropped (repo has none) | Match reality, not Debian naming. `MapArch` keeps existing for node-arch mapping; flavor is separate. |
| 2 | Resolution: override → local files → **OPEN** (fail-closed vs generic-default) | Fail-closed is safer (never stage a foreign DTB); generic-default is more available. Needs maintainer call — the plan's only blocking open question. |
| 3 | Override annotation: `simplek8s.org/board-flavor` (proposed name) | Consistent with the `update-url` override precedent; invalid values ignored loudly. Doubles as the manual board-migration tool (§3.6). Name open in review; necessity follows from D7. |
| 7 | Out-of-flavor pins skip + `UpdateStagingSkipped`, never W12-correct | A ts existing under other flavors is evidence the operator means migration, not a typo. Correcting it away would destroy intent; skipping loudly preserves it and points at the override. |
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
| `internal/features/update` (`versions.go`) | flavor type + `ResolveFlavor` (+ override parse); `MapArch` kept. |
| `internal/features/update` (`check.go`) | filter index by flavor; `res.Arch` becomes the flavor. |
| `internal/features/update` (`staging.go`, `purge.go`, `bootloader.go` prune) | flavor filter on `listKernels`; purge/prune/defensive own-flavor only. |
| `internal/features/update` (`update.go`, `enqueue.go`, `reconcile.go`) | `MapArch` call sites take the resolved flavor (plumbing). |

### 6.2 Unit test matrix

- Resolution: override valid/invalid/absent × partition with
  rpi4-only / mixed / empty / x86-64-only files.
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
| 1 | Flavor plumbing + unit tests (no behavior change on x86-64; arm64 goes no-op → flavor-scoped). |
| 2 | Live on rpi4-node (rpi4): F1–F3 (§7); bootloader rpi proven. |
| 3 | Live on rpi5-node (rpi5): F4 (§7) — needs drain approval. |

## 7. E2E (F-cases, live)

| # | Case | Trigger | Expect |
|---|---|---|---|
| F1 | Flavor detection on rpi4-node | read-only: controller logs after deploy (or a unit-driven annotation) | flavor resolves `rpi4` from existing staged files; no writes. |
| F2 | First rpi staging | `stage` + newer `rpi4` release in index (or pin an uncached `rpi4` ts) | staged `*.rpi4.efi` + `config.txt` `kernel=` re-pointed; **no reboot**; node untouched otherwise. First live exercise of the rpi writer. |
| F3 | Full auto-update on rpi4 (W4-shaped) | `full` + windows open | enqueue → reboot → running the new `rpi4` kernel → `UpdateApplied`. |
| F4 | rpi5 on rpi5-node (pending approval) | same as F2–F3 after a rpi5-node drain | same expectations on `rpi5` files. |

## 8. Deferred

- Generic-`arm64` live proof (no hardware) + what boots it.
- UEFI/systemd-boot questions (distro).
- Empty-partition default if D2 goes fail-closed (document the manual
  bootstrap: stage one file of the right flavor by hand or via the
  override, once).

## 9. Risks & safety notes

- **Wrong-flavor staging bricks with the wrong DTB** — the failure
  mode this plan exists to avoid creating. Mitigations: flavor pinned
  at detection (never re-derived mid-cycle... see below), override
  requires an exact valid value, foreign files never written by
  purge/prune/defensive; F2 stages but does not reboot (human verifies
  the file before F3).
- Detection runs once per pod lifetime (cached like `goalKnown`),
  not per cycle — a mid-life mix appearing later is Warn-logged, not
  adopted.
- rpi `config.txt` has a single `kernel=` line: re-point is
  all-or-nothing per write (same as syslinux `DEFAULT`, no worse).
