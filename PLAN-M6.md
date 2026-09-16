# PLAN-M6 — `simplek8sctl` node CLI (TODO 2) — APPROVED v2

Era tag: **M6** = node CLI. Status: **approved for implementation**
(2026-09-15, v2 renames the binary — see D17). Like PLAN-M3/M5, it
folds into `PLAN.md` when shipped.

## 1. Purpose

Successor of the legacy `simplek8s-update` project
(`/workspace/simplek8s-update`), rebuilt against this repo's
k8s-agnostic update core as the single-node admin CLI
`simplek8sctl`. It manages **its own node only**: no
controller, no cluster access, no `internal/kube`, no annotations, no
API. Use cases (TODO 2, in git): pre-cluster installs and
out-of-band maintenance (or a fleet via an operator loop).

Agreed scope for this iteration (2026-09-15 conversation):

- RPi supported day 1; GRUB also day 1 (syslinux stays legacy).
- Boot-goal flag is `next-kernel` (bool, default `true`), not
  `set-default` and not legacy `next-boot`: it sets the bootloader
  default to the staged version, using the controller's
  `next-kernel` vocabulary (`simplek8s.org/next-kernel`, the release
  `ts`).
- Keyring selects trust: embedded by default; `--keyring <path>`
  overrides; `--keyring /dev/null` skips GPG verification
  (power-user only, loud warning — sha256 of downloads still
  enforced).
- Breaking changes allowed: legacy is reference only.
- Reuse via two new pure helpers in `internal/updatecore`
  (D8): `FilterIndexByFlavor` + `DetectArchAuto`. No behavior change
  to the controller.
- CLI contract (D9): human text on stdout, errors on stderr, exits
  `0` ok / `1` operational error / `2` misuse or unresolvable
  auto-detect; `check --verbose` subsumes legacy `search`.
- Runtime (D11/D12/D16): unresolvable flavor auto-detection is
  fail-closed (exit 2); root + `flock` single-instance + guaranteed
  `umount`; bootloader is always auto-detected, no override;
  capacity pre-check runs before download.
- Build (D10/D13/D14/D17): `make build-simplek8sctl` in this repo (static
  `amd64`/`arm64`, `git describe` stamping) + port of the legacy
  publish flow (upx + GPG sign + upload) as `publish-simplek8sctl`; keyring via `go:embed`
  of `keys/simplek8s-pubring.gpg` with `--keyring` override;
  minimal flag×command matrix (§3.2). E2E fleet confirmed (D15).
- Shape (D17): flat subcommands `simplek8sctl
  check|update|list|purge|boot` (no `simplek8sctl update update`
  nesting); `install` reserved for a later spec, out of M6 scope.
  No compat symlink: clean break, legacy stays frozen as reference.

## 2. Constraints

- **Stdlib CLI.** No `urfave/cli`, no `logrus`, no `xz`, no
  `dbus`/`systemd` client libs. `flag` + `log/slog` like
  `cmd/simplek8s-controller/main.go`; the only non-stdlib deps are
  the repo's existing two (`go-crypto`, `klauspost/compress`).
- **Reuse boundary.** The CLI links only `internal/updatecore`
  (the k8s-agnostic core, no `internal/kube` in its dep graph —
  enforced by `make cli-no-kube`): `index.go`, `gpg.go`,
  `download.go`, `extract.go` (zst only), `staging.go`, `purge.go`,
  `bootloader.go`, `bootstore.go` (`PhysicalStore`), `disk.go`,
  `versions.go`, `localcli.go`, plus the two new pure helpers (D8):
  `FilterIndexByFlavor(sums, flavor)` and
  `DetectArchAuto(staged, model, compatible, goarch)`.
  `internal/features/update` (cluster wiring: `update.go`, `check.go`,
  `enqueue.go`, `verify.go`, `reconcile.go`, `migrate.go`) is never
  imported by the CLI.
- **Local-only, root + shared flock.** Must run as root on the node itself with
  an exclusive non-blocking `flock` on `/run/simplek8s/update.lock`
  (holder wins, other exits 1) — the same file the controller locks
  via a `hostPath` mount (§3.6, D12): it
  mounts the boot partition (`PARTLABEL=boot` preferred, then
  `EFI`/`boot` fs labels — same enumeration + contents verification
  as PLAN.md §3.12), writes kernels under `simplek8s/`, re-points the
  bootloader, and guarantees `umount` (defer, no mounts left behind).
  Flavor, boot device and bootloader are always auto-detected (no
  overrides). It never contacts the k8s API and never reads/writes
  node annotations.
- **Exit codes + output (D9).** Human text on stdout, diagnostics on
  stderr: `0` ok, `1` operational error (network, IO, GPG, mount),
  `2` misuse or unresolvable auto-detection (no staged flavor nor
  device-tree). `check --verbose` subsumes legacy
  `search`.
- **Static binary, shipped in the distro (D1/D10/D17).** `CGO_ENABLED=0`,
  `linux/amd64` + `linux/arm64` via `make build-simplek8sctl` in this repo
  (`git describe` stamping like the controller), plus a port of the
  legacy publish flow (`publish-simplek8sctl`: upx + GPG sign + upload to the public channel),
  released alongside the kernels
  (signed `SHA256SUMS.gpg` channel). No container image: a container
  would need privileged + host `/dev` + boot mounts just to replicate
  what the binary already has outside, and it cannot work
  pre-cluster. No `selfupdate` subcommand (legacy `cmds.go`
  `selfupdate`): the CLI updates like any other distro payload.
- **KISS.** Flat subcommands, five max for M6 (`check|update|list|purge|boot`);
  `install` reserved, not nested (`simplek8sctl update ...` is one level only);
  one mechanism per concern; kebab-case
  long flags only (no single-letter aliases — legacy `-u/-k/-bd/-bl`
  collide and are dropped).

## 3. Design

### 3.1 Commands (`simplek8sctl <cmd>`; M6 scope, `install` deferred to §8)

| Command | Effect |
| --- | --- |
| `check` | Fetch + verify index at `--url`; print newest `ts` for the node's flavor vs running (`uname -r` → `RunningVersion`) vs staged. Read-only (no mount write). Prints: `flavor`, `running`, `staged-newest`, `remote-newest`, verdict (`up-to-date` \| `update available <ts>`); `--verbose` adds all remote `ts` of the flavor, all staged, keyring ID, URL used. |
| `update [<ts>]` | `check` + download + verify + extract + `stagePartition` + purge (retention) + bootloader re-point iff `--next-kernel=true`. No arg = newest. Idempotent: already-staged `ts` is a no-op (log, exit 0). Prints plan, then result (`staged <ts>`, `default -> <ts>` \| `unchanged`, `purged [...]`). |
| `list` | Local staged versions (`listPartitionVersions`) + running + current bootloader default. Read-only. Prints: `staged[]` (own flavor), `running`, `bootloader default`. |
| `purge` | Retention-only (`--preserve`, `--max-percent-usage`) + bootloader prune in the same mounted session (grub + syslinux; rpi `config.txt` has a single `kernel=`, nothing to prune — PLAN.md §3.9). Prints: `deleted [...]`, `kept [...]` (running + default protected), `pruned entries`. Never prompts. |
| `boot [show\|set <ts>]` | Inspect / re-point the bootloader default without downloading. `set` refuses a `ts` whose file is absent (file-first, PLAN.md §3.10). `show` prints `default` + known entries; `set` prints `default <old> -> <new>`. |

Dropped from legacy (`cmd/.../cmds.go`): `selfupdate`, `download`
(folded into `update`), `search` (folded into `check --verbose`).
`boot` subsumes the legacy `boot` command's display half; its
write half is `boot set` / `update --next-kernel`.

### 3.2 Flags (final set)

| Flag | Default | Notes |
| --- | --- | --- |
| `--url` | `https://dl.simplek8s.org/simplek8s/stable`, accepts `dev\|rolling\|stable\|<custom-URL>` (legacy `flags.go` short-expansion kept) | Same default as controller (`config.go:48`). |
| `--keyring` | embedded via `go:embed` of `keys/simplek8s-pubring.gpg`; `--keyring <path>` overrides; `--keyring /dev/null` skips GPG verification (loud warning, sha256 still enforced) | Legacy default `/usr/lib/systemd/import-pubring.gpg` (`flags.go:144`) is dropped — this enables TODO 5 (keyring leaves the distro). |
| `--next-kernel` | `true` | (D4) Legacy name `next-boot` (`flags.go:256`) and generic `set-default` both rejected: the flag means "make this `ts` the node's boot goal", i.e. the local equivalent of the `next-kernel` annotation. `--next-kernel=false` stages without re-pointing. |
| `--preserve` | `3` | Aligns with controller (`config.go:49`), not legacy `5` (`flags.go:264`). |
| `--max-percent-usage` | `75` | Same as controller (`config.go:50`) and legacy (`flags.go:277`). |
| `--dry-run` | `false` | Kept from legacy; prints planned writes, touches nothing. |

Overwrites are automatic, not a flag (legacy `--overwrite`
dropped): the signed index carries both the `.efi.zst` and the bare
`.efi` hashes (verified live against the PROD repo), so a staged
file whose sha256 equals the index `.efi` hash needs no download at
all (no-op); otherwise download → verify → extract → write, and
the extracted bytes are additionally checked against the index
`.efi` hash (decompression integrity beyond the zstd frame
checksum). Same name with different bytes is replaced and warned —
that should never happen on honest infra. If the index ever lacks
the `.efi` entry, fallback is download → extract → byte-compare vs
staged (skip the write when equal).

Deliberately **removed**: `--distribution`, `--component`
(single-distro/single-component now), `--output`,
`--syslinux-config`, `--rpi-config`, `--ucode` (sane defaults +
auto-detect; `--grub-config` exists with default `grub/grub.cfg`
for symmetry but is hidden advanced), `--arch` (flavor is always
auto-detected: staged filenames → device-tree, fail-closed),
`--bootdevice` (boot device is always auto-enumerated + verified),
`--bootloader` (bootloader is always auto-detected by config
presence: `grub/grub.cfg`, `syslinux/syslinux.cfg`, `config.txt`),
`--no-confirm` (`purge` never prompts; `--dry-run` previews),
all short aliases.

**Flag×command matrix (D14, updated 2026-09-16).** Global:
`--url`, `--keyring`, `--verbose` (`check` only, subsumes `search`). Scoped:
`--next-kernel` (`update` only), `--preserve`/`--max-percent-usage`
(`update` + `purge`), `--dry-run` (`update` + `purge` + `boot set`).
No flag is silently ignored outside its commands. Flavor, boot device and bootloader are
always auto-detected (no `--arch`, `--bootdevice`, `--bootloader`);
`purge` never prompts (no `--no-confirm`).

### 3.3 Check / stage flow (local equivalents, no kube)

- `check`: `httpGet` index + `SHA256SUMS.gpg` → `VerifyIndex`
  (skipped iff `--keyring /dev/null`) → `ParseIndex` → `FilterIndexByFlavor`
  (extracted from the logic currently inline in `check.go`) → compare newest vs
  `RunningVersion(uname -r)` vs `listPartitionVersions`.
  Network failures are fatal to the command (exit 1), never
  silently ignored — there is no "next occurrence" here (unlike the
  controller's per-occurrence claim, PLAN.md §3.4). Unresolvable
  flavor auto-detection is exit 2 and performs no network I/O.
- `update`: claim-free single pass — capacity pre-check
  (`PathInfo`, `disk.go:13`, before any download, D16) → fetch +
  verify index (one fetch per run, shared with the `check` half) →
  sha256(staged file, if present) vs index `.efi` hash: equal →
  done, no download (report no-op exit 0); else `downloadAndVerify`
  (.zst sha256) → `extractZstd` → extracted bytes vs index `.efi`
  hash (decompression check) → write (replace + warn when a
  same-name file differed; the core `stagePartition` skip at
  `staging.go:119` is name-only, so the CLI gates before
  delegating) → `stagePartition` → retention purge →
  bootloader re-point (`SetBootloaderDefault`, iff
  `--next-kernel`) → prune (iff purge deleted something) →
  temp-file + synchronous `copyOver` discipline (PLAN.md §3.9).
  Any step failing aborts before the re-point; a staged-but-
  unpointed result is reported, never half-pointed.
- Running kernel is never purged; the bootloader default is never
  pruned (belt-and-braces, PLAN.md §3.9).

### 3.4 Boot device + bootloader writers

Reuse `PhysicalStore.mountedBoot` (plus `candidates` /
`verifyCandidate` / `parseBlkidCandidates`, PLAN.md §3.12 D2 as
shipped by M5 `f013580`): enumerate `PARTLABEL=boot` (GPT only) →
by-label `EFI`/`boot` → blkid scan, mount-verify each candidate
(`simplek8s/` + a bootloader config), first verifying wins, none
fails closed with no mounts left behind. Proven live (PLAN.md §7.5
F5 decoy drill: decoy skipped, fail-closed with zero mounts, heal
without restart). The per-pod verified-device cache is per-process,
so a one-shot CLI simply gets one resolution per run; stale-cache
retry (`invalidate` + re-resolve on mount failure) is reused as is.
Empty-partition consequence (README bootstrap, F5 run H): a
partition with no `simplek8s/` dir fails verification by design, so
`update` cannot bootstrap a wiped partition alone — that stays
`install` territory (§8); C8 preconditions a distro-fresh partition
(bootloader config present). Reuse the
three writers (`setGrubDefault`, `setSyslinuxDefault`,
`setRPIDefault` in `bootloader.go`). Newest-first GRUB menu order
and MOK-last invariant stay as in the controller.

### 3.5 Keyring + verification
`ResolveKeyring(custom, embedded)` (`gpg.go:27`): `--keyring`
override wins, else `go:embed` of `keys/simplek8s-pubring.gpg` (D13). `--keyring /dev/null`
bypasses `VerifyIndex` but keep sha256 `downloadAndVerify` (transport
integrity without identity — stated in output). The distro file
`/usr/lib/systemd/import-pubring.gpg` becomes removable once this
ships (TODO 5).

### 3.6 Shared lock (D12)

Mutual exclusion between CLI runs and the controller's boot-partition
sessions rides on one file: `/run/simplek8s/update.lock`, locked with
non-blocking `flock`. The host path is the pod's own `/run` tmpfs
(private per mount namespace), so the chart mounts only the dedicated
subdir as `hostPath` (`/run/simplek8s`, `DirectoryOrCreate`) at the
same path in the pod — not `/run` wholesale. Holder wins: a second
CLI exits 1; the controller skips update work for the cycle
(warn-log, retries next cycle — no Events, no annotation churn).
Lock is FD-bound (dies with the process, nothing stale) and `/run`
clears on reboot. Both sides hold it only around mounted sessions,
never across them.

## 4. Decision log (M6, closed — numbers restart per era)

| # | Decision | Rationale / status |
| --- | --- | --- |
| 1 | Static binary in the distro, no container (CLOSED 2026-09-15) | Local-only + pre-cluster + root mounts: container adds privilege plumbing for zero benefit. Legacy precedent (`-extldflags=-static`, `x86-64`+`arm64`). |
| 2 | `grub` + `rpi` day 1, `syslinux` legacy-only (CLOSED 2026-09-15) | GRUB is the managed bootloader; rpi writer first proven live in PLAN.md §7.5 F2 — CLI must exercise both from day 1. systemd-boot stays rejected (PLAN.md §4.4). |
| 3 | Skip-verification via `--keyring /dev/null` (CLOSED 2026-09-16, supersedes `--checksign`) | Escape hatch for air-gapped/custom repos; loud warning, never default; `--checksign` removed as redundant. |
| 4 | Boot-goal flag named `--next-kernel` (CLOSED 2026-09-15) | Aligns with the annotation (`next-kernel` = target `ts`); `next-boot` (legacy) and `set-default` (grub jargon) rejected. |
| 5 | Flag set §3.2; short aliases dropped (CLOSED 2026-09-15) | Kebab-case longs only; hidden `--grub-config` for symmetry. |
| 6 | `selfupdate`/`download`/`search` dropped (CLOSED 2026-09-15) | Folded into `update`/`check --verbose`; CLI updates via distro releases, not self-replacement. |
| 7 | `--preserve=3` to match controller (CLOSED 2026-09-15) | Legacy `5` vs controller `3`: one retention story. |
| 8 | Two new pure helpers (CLOSED 2026-09-15) | `FilterIndexByFlavor` + `DetectArchAuto(staged, model, compatible, goarch)` in `internal/updatecore`; no controller behavior change. |
| 9 | Human output + 0/1/2 exits (CLOSED 2026-09-15) | stdout human, stderr diagnostics; `0` ok / `1` operational / `2` misuse-unresolvable; `check --verbose` subsumes `search`. |
| 10 | `make build-simplek8sctl` + ported publish, `git describe` stamping (CLOSED 2026-09-15, renamed by D17) | Static `amd64`/`arm64` here; legacy upx+GPG-sign+upload flow ported as `publish-simplek8sctl`, version via `git describe` like the controller. |
| 11 | Flavor auto-detection fail-closed exit 2 (CLOSED 2026-09-16) | No staged flavor nor device-tree → no network, no writes; no `--arch` override exists. |
| 12 | Shared lock `/run/simplek8s/update.lock` (CLOSED 2026-09-16) | Non-blocking `flock`; chart mounts only the dedicated subdir as `hostPath` (same path both sides); second CLI exits 1, controller skips the cycle. |
| 13 | Keyring `go:embed` + override (CLOSED 2026-09-15) | Embed `keys/simplek8s-pubring.gpg`; `--keyring` wins; `--keyring /dev/null` skips only `VerifyIndex`. |
| 14 | Minimal flag×command matrix (CLOSED 2026-09-15) | §3.2; no silently-ignored flags. |
| 15 | E2E fleet confirmed (CLOSED 2026-09-15) | x86-64 + rpi4 + rpi5 + pre-cluster node; C1–C8 runnable as written. |
| 16 | Pre-download capacity check (CLOSED 2026-09-16) | `PathInfo` check before any download; no explicit flavor/device/bootloader flags remain to mismatch. |
| 17 | Single local binary `simplek8sctl`, flat subcommands (CLOSED 2026-09-15) | `cmd/simplek8sctl` with `check\|update\|list\|purge\|boot`; `install` reserved (deferred, §8); `make build-simplek8sctl` + `publish-simplek8sctl`; no compat symlink (clean break). `sk8sctl` rejected (cryptic, inconsistent). |
| 18 | Flag cull: no `--arch`/`--bootdevice`/`--bootloader`/`--no-confirm`/`--checksign`/`--overwrite` (CLOSED 2026-09-16) | All detection auto (fail-closed); `purge` never prompts; skip-verification via `--keyring /dev/null`; overwrites automatic by index-`.efi`-hash compare (verified live: repo publishes `.efi` + `.efi.zst` hashes). Remaining flags: `url`, `keyring`, `next-kernel`, `preserve`, `max-percent-usage`, `dry-run`, `verbose` (check only). |
| 19 | K8s-agnostic core split into `internal/updatecore` (CLOSED 2026-09-16) | `make cli-no-kube` proved the transitive `internal/kube` import through `internal/features/update`; the pure machinery moved to a kube-free package (doc.go invariant), wiring imports it. CLI links only `updatecore` (zero `k8s.io` in dep graph). No behavior change (full suite green). |

## 5. Behavior changes & migration

- Legacy CLI stays usable until v2 ships; no flag-compat promise
  (aliases removed, filters removed, keyring default moved).
  Distro ships `simplek8sctl` only; no `simplek8s-update` symlink (D17).
- Nodes managed by the controller need no migration: the CLI writes
  the same files + bootloader default the controller writes; the
  controller's change-triggered reconciliation (PLAN.md §3.7)
  adopts a CLI-staged default via the normal `next-kernel`
  comparison once the annotation is set (CLI never writes the
  annotation itself).
- Distro change (TODO 5): after v2 ships with the embedded keyring,
  remove `/usr/lib/systemd/import-pubring.gpg`.

## 6. Implementation

### 6.1 Modules

| Module | Change |
| --- | --- |
| `cmd/simplek8sctl` (new) | `main.go` (`flag`+`slog`), flat `check/update/list/purge/boot` subcommands (`install` reserved), osrelease + device-tree helpers (`/sys/firmware/devicetree/base/model`, `compatible`: `bcm2711`→`rpi4`, `bcm2712`→`rpi5`; partition scan = staged basenames via `ResolveFlavor` order; `amd64` build arch implies `x86-64`), root + shared/exclusive `flock` (reads shared, writes exclusive), exit 0/1/2. Links only `internal/updatecore` (no `internal/kube` in dep graph, enforced by `make cli-no-kube`). `Makefile`: `build-simplek8sctl` (static `amd64`/`arm64`, `git describe` stamping, `go:embed` keyring copied from `keys/`; aborts if the keyring file is still an LFS pointer) + ported `publish-simplek8sctl` (upx + GPG sign with fingerprint `33BAAC4BFB20C2327429730A9F16C69F2B9DD678` + upload to `https://publisher.simplek8s.org/upload/simplek8sctl`). |
| `internal/updatecore` (new) | K8s-agnostic core split out of `internal/features/update` (D19): `index/fetch`, `gpg`, `download`, `extract`, `staging` (+ `NoRepoint`/`DryRun`/`EfiChecksum` knobs, `StagePartition`/`PurgePartition`/`PreviewPurge` exports), `bootloader`, `bootstore` (`PhysicalStore`, `MountedBoot`, lock), `disk`, `versions`, `localcli` (`FilterIndexByFlavor`, `LookupRelease`, `DetectArchAuto`, `StoredKernelName`, `FetchVerifiedIndex`, `LoadKeyringBytes`, `LockFile`). No `internal/kube` in dep graph (see `doc.go`). |
| `internal/features/update` | Cluster wiring only (`update.go`, `check.go`, `enqueue.go`, `verify.go`, `reconcile.go`, `migrate.go`): qualifies moved identifiers via `updatecore`, plus the shared-lock acquisition (D12, §3.6) on `Stage`/`EnsureBootGoal` (contention → `ErrBootBusy` → existing skip paths). Otherwise no behavior change to the controller (full suite green). |
| `chart/` | One `hostPath` volume (`/run/simplek8s`, `DirectoryOrCreate`) mounted at the same path, carrying only `update.lock`. |
| Legacy `/workspace/simplek8s-update` | Frozen reference; not modified. |

### 6.2 Unit test matrix

- `FilterIndexByFlavor`: mixed index → own flavor only;
  `latest`-style files ignored.
- `DetectArchAuto`: staged-first, device-tree (`model` substring
  or `compatible` bcm2711/bcm2712) fallback, `amd64` build arch
  implies `x86-64`, bare `arm64` unresolvable; unresolvable → error
  (caller exits 2).
- `ParseStoredKernel` / `versionFromStoredKernel` naming per
  flavor (`stored`/`artifact`).
- Purge planning own-flavor only; foreign files survive.
- Prune fixtures: grub named-default sample + syslinux sample
  (PLAN.md §3.9) — candidate pruned, foreign kept, default kept,
  globals verbatim.
- `boot set` refuses absent-`ts`; `update` with `--next-kernel=false`
  leaves default untouched; `--dry-run` writes nothing.
- `update` hash rule: staged sha256 == index `.efi` hash → zero
  artifact traffic (assert in test the `.zst` is never fetched);
  staged != index → download → extract → extracted-vs-index check →
  replace + warn (unit: all three cases incl. missing `.efi`
  entry fallback).
- Keyring: custom override wins; `--keyring /dev/null` skips
  `VerifyIndex` but still sha256-verifies downloads.
- Shared lock (§3.6): second non-blocking holder fails (CLI exits
  1, controller skips cycle); unit with two FDs on a temp file.
- No-kube import test (CLI package graph contains no
  `internal/kube`/`internal/engine`).

### 6.3 Phases

| Phase | Content |
| --- | --- |
| 1 | `cmd/simplek8sctl` skeleton + `check`/`list` (read-only) + unit matrix (no writes) + `go list` no-kube check. |
| 2 | `update`/`purge`/`boot` writes + prune + `go:embed` keyring + root/`flock`/`umount`; static `linux/amd64,arm64` builds via `build-simplek8sctl`. |
| 3 | Live E2E (§7) on x86-64 (grub + syslinux-legacy) and rpi4/rpi5 (rpi, fleet confirmed D15); ported `publish-simplek8sctl` dry-run; distro keyring removal (TODO 5) after. |

## 7. E2E (live, own-node)

| # | Case | Trigger | Expect |
| --- | --- | --- | --- |
| C1 | `check` on each flavor | run on x86-64, rpi4, rpi5 nodes | Newest remote `ts` reported correctly per flavor; exit 0; zero writes. |
| C2 | `update --dry-run` | same fleet | Plan printed, partition untouched (mount audit). |
| C3 | `update <ts> --next-kernel` (grub) | x86-64 node, uncached `ts` | File staged under `simplek8s/`, `grub/grub.cfg` default re-pointed newest-first, running untouched, reboot left to operator. |
| C4 | `update --next-kernel=false` | x86-64 node | File staged, default unchanged. |
| C5 | rpi update | rpi4 + rpi5 nodes | `*.rpi4/rpi5.efi` staged, `config.txt` re-pointed, running untouched. |
| C6 | `purge --preserve 3` + prune | node with ≥5 staged | Oldest deleted, grub/syslinux entries pruned, default + running protected, foreign files kept. |
| C7 | `boot set` validation | `boot set <absent-ts>` | Refused (file-first); exit non-zero; default unchanged. |
| C8 | Pre-cluster install | distro-fresh node, no kubelet, bootloader config present (not a wiped partition — empty `simplek8s/` fails verification by design, README bootstrap) | Full `update` works with only userspace + boot partition; a wiped partition stays `install` territory (§8). |

### 7.1 Results (2026-09-16, x86-64 fleet)

Binary `simplek8sctl-linux-amd64` (static, `build-simplek8sctl`),
fleet cp1/cp2/cp3/wk1/wk2 (release `202609161303`, kernel
`6.18.52-simplek8s-202609161303`, no kubelet anywhere). No node was
rebooted during E2E (running `ts` untouched throughout).

- C1 PASS (×5): stable newest `202508191449` correctly reported per
  flavor, `up-to-date`, exit 0; mount audit on wk2 (grub.cfg sha256 +
  file list before/after) identical → zero writes. Dev-channel
  variant: newest `202609161935`, `update available 202609161935`.
  rpi4/rpi5 flavors: not runnable (no ARM hardware).
- C2 PASS: `update --dry-run --url dev` printed the exact plan
  (61 MB download to scratch, would stage + re-point), exit 0,
  partition untouched (same audit).
- C3 PASS: `update --url dev --next-kernel 202609090435` → staged +
  default re-pointed, running untouched, exit 0.
- C4 PASS: `update --url dev --next-kernel=false 202609061935` →
  staged, default unchanged, exit 0.
- C5 PENDING: no rpi4/rpi5 hardware in this fleet.
- C6 PASS: `purge --preserve 3` on 7 staged @79% → oldest deleted,
  default + running protected, foreign file kept, 1 grub entry
  pruned (`grub stale entries pruned entries=1`), usage back to 69%,
  exit 0.
- C7 PASS: `boot set 199901010000` refused (file-first), exit 1,
  default unchanged.
- C8 PASS: full `update --url dev 202609121031` on wk2 (no kubelet)
  → staged + re-pointed, exit 0.
- Cross-node spot: `update --next-kernel=false 202609121031` on cp1
  → staged, default (already newest) unchanged.
- syslinux-legacy fleet (wk1 recreated with August release
  `202608291203`, 2026-09-16): `list` detects `syslinux`, `check
  --url dev` reports newest, `boot set <absent>` refused exit 1,
  `update --next-kernel=false` stages file-only (DEFAULT unchanged),
  `update` re-points (DEFAULT moved, LABEL added, rest verbatim),
  `purge --preserve 3` over cap deleted oldest non-protected,
  default + running protected, foreign file kept, 1 syslinux LABEL
  pruned (`syslinux stale entries pruned entries=1`); default
  restored to running. No reboots.

Findings folded back into the plan/code:

- stdlib `flag` stops at the first positional: flags must precede
  `<ts>` (`update --url dev <ts>`, `boot [--dry-run] set <ts>`);
  usage strings fixed accordingly (code only).
- `purge` deletes only while over the usage cap (`preserve` is the
  keep-floor, not a count cap): on a roomy partition it is a
  correct no-op. Same code path as the controller.
- `--next-kernel=false` stages file-only (no grub menuentry until a
  later re-point); `boot set` adds the missing entry.

## 8. Deferred

- `install` subcommand (node provisioning from scratch) — reserved
  name, spec pending; not in M6 scope (D17).
- Fleet loop helper (ssh-for over nodes) — operator shell, not CLI scope.
- Shell completions / man pages (legacy had none worth keeping).
- `systemd-boot`/UEFI questions — REJECTED (PLAN.md §4.4 M5 D5-era):
  systemd-boot is UEFI-only, SimpleK8s keeps BIOS boot; GRUB (+
  syslinux legacy, + rpi `config.txt`) stays the managed set.

## 9. Risks & safety notes

- **Wrong-flavor staging bricks with the wrong DTB** (PLAN.md §9): flavor
  pinned at detection, never re-derived mid-run; foreign files never
  written/purged/pruned.
- **Silent `--keyring /dev/null`**: always warn; never persist as
  default; document as break-glass only.
- **CLI vs controller racing on one node**: last writer of the
  bootloader default wins; both writers use temp-file + sync
  `copyOver`, so the file never corrupts — but concurrent
  `update` (CLI) + controller staging is operator error; document
  as mutually exclusive (CLI is for pre-cluster / out-of-band with
  `updates.windows: []`, or controller paused).
- **Single `kernel=` on rpi**: re-point is all-or-nothing per write
  (same as syslinux `DEFAULT`, no worse — PLAN.md §9).

## 10. Approval baseline + M5 review

Approved 2026-09-15 on `083f89f` (= rewritten `322a151` after the
origin history rewrite; M6 commit `7c1387a` = rewritten `49e7dde`).
M5 then shipped on top (`f013580` feat D2 enumerate-then-verify +
per-pod cache, `9cfa12b` bootstrap/F5 evidence, `e714ac5` fold M5
into `PLAN.md` removing `PLAN-M5.md`, `d1a9cf7` gofmt). Reviewed
2026-09-16 at `d1a9cf7`:

- Reuse boundary: `bootstore.go` rewritten — `findBootDevice` is now
  cached enumerate-then-verify; all callers go through `mountedBoot`
  (resolve + mount, stale-cache retry). §3.4 updated to reuse
  `mountedBoot`/`candidates`/`verifyCandidate`; one-shot CLI gets one
  resolution per run, no negative caching. Public `PhysicalStore`
  method signatures unchanged.
- M5 refs updated: `PLAN-M5.md` gone → `PLAN.md` §3.12 (design) /
  §4.4 (decisions incl. generic-`arm64` unsupported D5, systemd-boot
  rejected) / §7.5 (F1–F5 incl. decoy drill).
- New constraint from M5: empty partition fails verification by
  design → C8 scoped to distro-fresh partitions; wiped-partition
  bootstrap stays `install` territory (§8, README procedure, F5 H).
- Revalidate further M5 work with:

      git diff 49e7dde..HEAD -- internal/features/update internal/updatecore cmd/ keys/ Makefile Dockerfile chart/

  Any change to that boundary (signatures, `check.go`,
  `flavor.go`, writers, `staging`/`purge`/`disk`/`gpg`/`bootstore`)
  can invalidate D8/D12/D16.
