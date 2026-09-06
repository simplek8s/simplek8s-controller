# PLAN-M2 — simplek8s-controller: distro updates

> **Planning index**
> - [PLAN-M1.md](PLAN-M1.md) — node reboots (shipped)
> - [PLAN-M2.md](PLAN-M2.md) — distro updates (this document; in planning)
> - [E2E.md](E2E.md) — reboots E2E campaign (28/28 PASS)
> - [E2E-UPDATE.md](E2E-UPDATE.md) — updates E2E campaign

Phase: **PLAN** (design & planning). Status: v1 — all design decisions
closed with the maintainer (2026-09-06), pending implementation.

## 1. Purpose

The second feature of the controller: distro updates. It detects a new
SimpleK8s kernel release in the release repository, stages it on each
node's boot partition (`/boot/simplek8s/`), points the bootloader at it
through the `next-kernel` annotation (the source of truth for what the
node should boot), and — in `full` mode — runs an all-or-nothing,
cluster-wide reboot plan on top of the existing M1 reboot machinery,
verifying each node after it comes back.

Release format (one file per version+arch, served over HTTP from the
release repo URL):

```
<url>/SHA256SUMS                          index: "<sha256>  simplek8s.<ts>.<arch>.kernel.zst"
<url>/SHA256SUMS.gpg                      detached GPG signature over SHA256SUMS
<url>/simplek8s.<ts>.<arch>.kernel.zst    zstd of kernel + initrd
```

- `ts` = timestamp of the release (the version), `arch` = `x86-64` /
  `aarch64`.
- The node's running version is the kernel release string
  (`simplek8s-<ts>`), read from `Node.status.nodeInfo.kernelVersion`.
- The node's arch is `Node.status.nodeInfo.architecture`
  (`amd64`→`x86-64`, `arm64`→`aarch64`). No annotation, no `uname`, no
  firmware probing.

## 2. Constraints

- **Dependency policy** (extends M1's stdlib-only): +
  `github.com/ProtonMail/go-crypto` (GPG signature verification) and
  `github.com/klauspost/compress` (zstd). No YAML library (the ConfigMap
  is `map[string]string` — §3.2), no `x/sys/unix` (kernel version comes
  from the API, not a syscall), no xz (releases ship zst only).
- **KISS for the end user**: exactly one dial (`updates.update-mode`).
  The end user touches the ConfigMap at install time and the
  `next-kernel` annotation only when intervening (re-launch, rollback,
  pin).
- **Timezone**: always UTC. Documented, not configurable (no key).
- **Reuse M1**: leader election, engine loop, reboot queue (cordon,
  PDB-aware drain, `nsenter` reboot, boot-ID verification,
  `on-reboot-failure`), API auth, nodestate RMW discipline.
- **Docs and code in English** (maintainer requirement).
- The update core (`internal/features/update`) is k8s-agnostic: check,
  GPG/sha verification, index parsing, staging, purge and bootloader
  reconciliation take explicit inputs. This keeps the package reusable
  by a future `simplek8s-update` CLI (see TODO.md) that owns the
  power-user knobs (`--url`, `--keyring`, `--checksign`) the controller
  deliberately lacks.

## 3. Design

### 3.1 Deployment model

Unchanged from M1: one privileged DaemonSet, one pod per node, NodePort
API. No new pods. The future split (unprivileged controller + privileged
host daemon) is out of scope (TODO.md).

### 3.2 Configuration: flat ConfigMap, feature-prefixed keys

K8s ConfigMaps are `data: map[string]string` — proven empirically on the
test cluster (`kubectl explain configmap.data`; live coredns wire JSON
shows string values). Nested documents, JSON-in-YAML and multi-line
YAML-in-a-string are therefore all out. Keys are flat with a
feature prefix: `<feature>.<key>`.

ConfigMap `simplek8s-controller` in namespace `simplek8s`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: simplek8s-controller
  namespace: simplek8s
data:
  engine.engine-interval: 2s
  reboots.max-concurrent-reboots: "1"
  reboots.on-reboot-failure: pause
  reboots.reboot-drain-timeout: 10m
  reboots.reboot-issue-grace: 5m
  updates.update-mode: "off"
  updates.url: "https://dl.simplek8s.org/simplek8s/stable"
  updates.check-interval: 12h
  updates.preserve: "3"
  updates.max-percent-usage: "75"
```

Note the quoting: `off` is a YAML 1.1 boolean and bare numbers are YAML
numbers — both must be quoted to stay strings.

| Key | Type | Default | Description |
|---|---|---|---|
| `engine.engine-interval` | duration | `2s` | Engine loop period (M1, migrated). |
| `reboots.max-concurrent-reboots` | int ≥ 0 | `1` | Max concurrent reboots (M1, migrated). |
| `reboots.on-reboot-failure` | `pause`\|`continue` | `pause` | Queue behavior on failure (M1, migrated). |
| `reboots.reboot-drain-timeout` | duration | `10m` | Drain deadline (M1, migrated). |
| `reboots.reboot-issue-grace` | duration | `5m` | No-effect-reboot grace (M1, migrated). |
| `updates.update-mode` | `off`\|`stage`\|`full` | `off` | The single update dial (§3.3). |
| `updates.url` | URL | `https://dl.simplek8s.org/simplek8s/stable` | Full release repo URL for the cluster; per-node override via the `update-url` annotation. |
| `updates.check-interval` | duration | `12h` | How often each node checks its repo. |
| `updates.preserve` | int ≥ 1 | `3` | How many versions to keep in `/boot/simplek8s/`. |
| `updates.max-percent-usage` | int 1–100 | `75` | Partition usage ceiling after purge. |

Semantics:

- **Key absent** → the key's default applies.
- **ConfigMap absent** → command-line flags apply (they become
  bootstrap defaults; backward compatible with the current M1 deploy).
- **Unknown key** → warn + ignore (forward compatibility).
- **Invalid value** (unparseable, out of range) → keep the last valid
  value (or the default), warn; the controller never crashes on a config
  typo.
- **Reload**: re-read at the start of each engine cycle (get, no watch).

M1 migration: `reboots.*` and `engine.*` move from flags to the
ConfigMap; the flags remain as bootstrap fallback only.

### 3.3 `updates.update-mode`: the single dial

| Mode | Behavior |
|---|---|
| `off` (default) | The update feature is inert: no checks, no downloads, no staging, no plans. (Bootloader reconciliation of an existing `next-kernel` still happens — that is node state, not "updates".) |
| `stage` | Check + stage + `next-kernel := V` on every node. No reboot plan. The operator reboots via the M1 API when ready. |
| `full` | Staging + all-or-nothing cluster-wide reboot plan (§3.8) + verification (§3.10). |

### 3.4 Annotations (exactly two)

| Annotation | Writers | Meaning |
|---|---|---|
| `simplek8s.org/next-kernel` | local pod (bootstrap, staging), leader (plan-cancel reset), operator | **Permanent** per-node boot intent; the source of truth for what the node should boot. Invariant: the value is always a version present in `/boot/simplek8s/`. Never deleted. |
| `simplek8s.org/update-url` | operator only | Optional full release repo URL for this node (heterogeneous fleets, custom repos). Precedence: annotation > `updates.url` > built-in default. |

Discarded during design: `node-info` (redundant with
`Node.status.nodeInfo`), `update-state` (replaced by the
`next-kernel` vs running comparison), `update-error` (failures go to
logs), `update-channel` (single `url` key instead).

### 3.5 `next-kernel` semantics and bootloader reconciliation

- `next-kernel == running` → **quiescent** (hold; nothing to do).
- `next-kernel != running` → **prepared to apply**: the bootloader
  points at it; it is applied on the next reboot — either by the plan
  (`full` mode) or by the operator (M1 API).
- **Bootloader reconciliation**: every cycle, the local pod ensures the
  bootloader default equals `next-kernel` — unconditionally, whatever
  changed the annotation. Detection: `<boot>/syslinux/syslinux.cfg` →
  syslinux (x86), `<boot>/config.txt` → RPi; entry writers ported from
  the reference project (`simplek8s-update`).
- **Bootstrap** (annotation absent, local pod):
  1. running version present in `/boot/simplek8s/` →
     `next-kernel := running` (the normal case).
  2. running version NOT present (legacy/custom layout) →
     `next-kernel :=` newest version present locally in
     `/boot/simplek8s/` (local, never the repo).
  3. `/boot/simplek8s/` missing or empty → no annotation; retry next
     cycle.
- **Writer discipline** (the annotation has three writer classes; every
  controller write is a conditional RMW on the M1 nodestate
  primitives — read → check precondition → patch carrying the read
  `resourceVersion` — so a concurrent operator edit is re-evaluated,
  never clobbered):
  - **Local pod**: (a) bootstrap, only when the annotation is absent;
    (b) staging anchor `next-kernel := V`, only when the value at
    **write time** is `== running` or absent (node quiescent). On
    precondition failure the anchor is skipped and the pin stands (the
    files were still staged to disk — harmless, and the node is ready
    if the operator releases the hold). (c) Defensive re-staging
    touches only files, never the annotation (the annotation already
    holds the missing version).
  - **Leader**: uniform reset on plan cancel (§3.8) —
    `next-kernel := running` per member, precondition: value still
    `== V` at **write time**. A member the operator re-pinned in the
    meantime is skipped, not clobbered.
  - **Operator**: any version present in `/boot/simplek8s/` —
    re-launch after a cancelled plan, rollback, manual pin.
    Unconditional; the operator wins by definition.
- **Operator operations** (all one annotation edit, the bootloader
  follows on the next cycle):
  - **Re-launch**: after a cancelled plan, set `next-kernel := V` on the
    cancelled nodes (V is still in `/boot`) and reboot them via the M1
    API. The updater does NOT auto-plan (V is already in `/boot`).
  - **Rollback**: on an updated node, set `next-kernel :=` the older
    version (preserved in `/boot` by the purge policy); the next reboot
    goes back. This is an operator action, not a feature — documented,
    nothing to build (see also TODO.md).
  - **Manual pin**: any version in `/boot`.

### 3.6 Check

Per node (local pod), every `updates.check-interval`:

1. `url` = node's `update-url` annotation, else `updates.url`, else the
   built-in stable default.
2. `GET <url>/SHA256SUMS` and `GET <url>/SHA256SUMS.gpg`.
3. **Verify the GPG signature — mandatory, always on.** There is no skip
   option in the controller (it is an unattended mechanism that writes
   bootloaders). `--checksign` stays a CLI concern.
4. `arch` = `nodeInfo.architecture` mapped to release naming
   (`amd64`→`x86-64`, `arm64`→`aarch64`).
5. `running` = `simplek8s-(\d+)` applied to
   `nodeInfo.kernelVersion`.
6. `latest` = highest `ts` for that arch in the verified index.
7. `available` = `latest > running` (by `ts`).

No available version, or any check failure (network, GPG, parse): no
state change; rate-limited error log + event. A bad signature rejects
the whole index (nothing from it is ever trusted).

**GPG keyring resolution** (fixed paths, not configurable):

- **Embedded**: the repo ships `keys/simplek8s-pubring.gpg` (Git LFS);
  the Dockerfile `ADD`s it to `/etc/simplek8s/pubring.gpg`.
- **Optional override**: Secret `simplek8s-controller-keyring`
  (key `pubring.gpg`), mounted `optional: true` at
  `/etc/simplek8s/custom/pubring.gpg`. The controller checks the file:
  exists → custom keyring; absent → embedded. Use case: a custom repo
  (paired with a custom `updates.url` / `update-url`) signed by a custom
  key.

### 3.7 Staging (local, per node)

Trigger: the check found `available` version V **and V is not in
`/boot/simplek8s/`**. If V already is in `/boot/simplek8s/` → do
nothing (a hold/reverted state is respected; only the operator can
re-anchor, §3.5).

Flow (on every node, cluster-wide for that version):

1. **Boot device**: `blkid` by label (`EFI` or `boot` — the partition
   holding the bootloader files). No labeled device → skip the node +
   event. Never guess a device.
2. **Mount**: `syscall.Mount` (vfat) at a private mountpoint the pod
   creates. Privileged pod → host `/dev` visible; no D-Bus, no systemd.
3. **Download** `<url>/simplek8s.<ts>.<arch>.kernel.zst`; verify sha256
   against the GPG-verified index. Capacity pre-check before download
   (free space < file size + margin → skip + event).
4. **Extract** zstd → `<boot>/simplek8s/simplek8s.<ts>.<arch>`
   (+ initrd).
5. **Bootloader entry**: add the version to the syslinux/rpi config
   (writers ported from the reference project).
6. **Purge**: keep the newest `updates.preserve` versions, then enforce
   usage ≤ `updates.max-percent-usage` on the partition. **Absolute
   rule: the running version is never deleted.**
7. **Unmount**.
8. **Anchor**: `next-kernel := V` (writer discipline §3.5 — only when
   the node is quiescent); the bootloader reconciliation then points the
   default at V.

Staging failure: no annotation change, no cluster impact (staging is
local), retried at the next check.

**Defensive re-staging**: if `next-kernel != running` and that version
is not in `/boot/simplek8s/` (file deleted, hand-edited layout) →
re-stage that version locally. No plan implications.

### 3.8 Reboot plan (`full` mode) — action-based, all-or-nothing

- **Trigger**: the plan is started by the updater **only as the
  consequence of having staged (downloaded) a version that was not in
  `/boot/simplek8s/` before staging**. There is no standing policy of
  "reboot whatever has `next-kernel != running`": nodes with
  `next-kernel != running` and no active plan are the operator's domain
  (M1 API).
- **Membership**: all nodes with `next-kernel == V`. Admission goes
  through the M1 reboot queue (serialization by
  `reboots.max-concurrent-reboots`, PDB-aware drain, `nsenter`, M1
  verification).
- **Durability** (leader restart mid-plan): the leader records active
  plans in an annotation on the leader Lease
  (`simplek8s-controller-leader`, ns `simplek8s`):
  `simplek8s.org/update-plans` = JSON `{ "<version>": {"startedAt":
  ..., "nodes": [...]} }`. The leader is the only writer (inside its
  existing conditional Lease update). On takeover, the new leader
  resumes from it: in-flight members continue (M1 reboot state is
  annotation-derivative), not-yet-admitted members are admitted, a
   failed member triggers cancel. The reset is idempotent and the
   entry is cleared **only after it completes**: a leader crash
   mid-reset is re-applied by the new leader from the still-present
   marker. (Edge: a crash between "staging done" and "marker written"
   leaves staged nodes without a plan — the operator reboots them via
   the M1 API; window is one engine cycle.)
- **Success**: member reaches M1 `completed` and
  `running == V` → quiescent (annotation == running, nothing to clean).
  All members done → marker entry cleared.
- **Failure** (drain timeout, no-effect reboot, or verification
  mismatch — node came back but `running != V`):
  - stop admitting new members; the in-flight reboot runs to completion
    (an issued reboot cannot be aborted);
   - **uniform reset**: for **every** member,
     `next-kernel :=` that node's `running`, written as a per-node
     conditional RMW (precondition: still `== V` at write time — an
     operator re-pin made during the cancel is skipped, not
     clobbered). Effect: updated nodes no-op (annotation == running
     == V); cancelled nodes revert (annotation and bootloader back to
     the old version, still in `/boot/simplek8s/`);
  - clear the marker entry.
  - No branching between "updated" and "cancelled" — one rule, applied
    to all.
- **Accepted consequences** (discussed and closed):
  1. Re-launching a cancelled plan is a two-step operator action
     (re-anchor `next-kernel` + M1 reboot API). The updater never
     re-plans a version already in `/boot`.
  2. Switching `stage`→`full` for an already-staged version does NOT
     start a plan (same reason).
  3. A failed version is never auto-retried: the check sees V in
     `/boot` → no-op. Only a new release or an operator re-anchor moves
     things forward.
- **Mixed per-node URLs**: plans are per-version and serialized (one
  active plan at a time; the second waits for the first to complete or
  cancel).

### 3.9 Plan durability details

See §3.8 (Lease annotation). Rationale: no new object, no new RBAC,
lifecycle tied to the leader, readable by the standby at takeover. The
ConfigMap is deliberately NOT used for plan state (it is
operator-owned configuration).

### 3.10 Verification

- **Leader-side**, per plan member, when the member reaches M1
  `completed` (or returns Ready after reboot): compare `running`
  (nodeInfo) with V. Equal → OK. Different → plan failure → cancel +
  uniform reset (§3.8).
- The local pod performs **no** update verification (the leader owns
  plan outcomes).
- Consistency with non-plan reboots: an operator reboots a node whose
  `next-kernel != running` — the bootloader already points at
  `next-kernel`, so the node comes back on it → quiescent. If the new
  version fails to boot and the bootloader falls back to the old one,
  `running != next-kernel` reads exactly like a plan failure → cancel +
  reset, which is the desired behavior (a failed boot is what we want to
  catch).

### 3.11 Events and logs

Events (namespace `default`, M1 rules — PLAN.FIXME #5):

| Event | Scope | When |
|---|---|---|
| `UpdateAvailable` | per node | a new version is available for this node (once per version) |
| `UpdateStaged` | per node | version staged on this node |
| `UpdateCheckError` | per node | check failure (network/GPG/parse), rate-limited |
| `UpdatePlanStarted` | cluster | plan admitted to the reboot queue |
| `UpdatePlanCanceled` | cluster | plan failed; uniform reset applied |
| `UpdateNodeFailed` | per node | a plan member failed (drain/no-effect/verify mismatch) |

Plan reboots additionally emit the existing M1 reboot events
(`RebootDraining`, `RebootIssued`, ...).

Logs: structured, per node, same discipline as M1.

### 3.12 HTTP API

**No new endpoints in the MVP.** Reboots go through the M1 API; updates
are driven by the ConfigMap + annotations. Visibility = annotations +
events + logs. (A read-only `GET /api/v1/updates` is a future candidate,
not planned.)

### 3.13 RBAC

M1 RBAC + one `Role` in ns `simplek8s`: `configmaps: [get, list]`.
(Lease annotation writes are covered by the existing `leases`
create/update.)

### 3.14 Deployment (image, DaemonSet, keyring)

- **Dockerfile**: `ADD keys/simplek8s-pubring.gpg
  /etc/simplek8s/pubring.gpg`; `apk add util-linux` (blkid); the two new
  Go dependencies.
- **LFS**: `.gitattributes` → `keys/simplek8s-pubring.gpg
  filter=lfs diff=merge text=auto`; the keyring (865 B, from the distro's
  `/usr/lib/systemd/import-pubring.gpg`) is committed via LFS.
- **DaemonSet**: one optional Secret volume
  (`simplek8s-controller-keyring`, `optional: true`) mounted at
  `/etc/simplek8s/custom` (key `pubring.gpg` →
  `/etc/simplek8s/custom/pubring.gpg`), read-only. **No new hostPath**:
  privileged + hostPID already expose `/dev` and `/proc`; the boot
  partition is mounted by the pod itself (§3.7).
- **ConfigMap manifest** in `deploy/` with the defaults of §3.2.
- **README**: operations section (update-mode, url, the two
  annotations, re-launch, rollback, keyring override, UTC note).

### 3.15 Dependencies

| Dependency | Purpose |
|---|---|
| stdlib | everything else (HTTP client, `syscall.Mount`, `os`, `crypto/sha256`, `time`, `regexp`) |
| `github.com/ProtonMail/go-crypto` | GPG signature verification (openpgp) |
| `github.com/klauspost/compress` | zstd decompression |

Explicitly NOT added: any YAML library (ConfigMap data is
`map[string]string`), `golang.org/x/sys/unix` (kernel version from the
API's nodeInfo, not `uname`), an xz decoder.

### 3.16 Package layout

```
internal/config/            new: flat-key ConfigMap loader (parse, validate,
                            last-valid-wins, unknown-key warn, absent→flags)
internal/features/update/   k8s-agnostic core: index/check, gpg+sha verify,
                            keyring resolution, versions, stage, purge,
                            reconcile (bootloader writers), boot device
internal/features/update/   engine hooks (same package, M1 style):
                            local executor task (bootstrap, check, stage,
                            reconcile, purge), orchestrator task (plan
                            admission, verification, cancel + reset,
                            Lease marker)
internal/nodestate/         + next-kernel and update-url accessors
deploy/                     ConfigMap, RBAC (configmaps role), DS volume
keys/                       simplek8s-pubring.gpg (LFS)
```

## 4. Risks & safety notes

1. **A bad release can brick nodes.** Mitigations: mandatory GPG +
   sha256, all-or-nothing plan with default concurrency 1, rollback by
   annotation (the old kernel is always preserved), purge never deletes
   the running version, boot device never guessed.
2. **vfat boot partition** (x86/EFI): filenames and sizes are fine
   (kernel+initrd are tens of MB in zst); capacity pre-check before
   download.
3. **Custom installs without labeled partitions**: the node is skipped
   with an event; updates never touch an unverified device.
4. **Leader crash mid-plan**: resumed via the Lease marker + M1's
   annotation-derivative reboot state (§3.8/§3.9).
5. **Race between staging and the marker write**: staged nodes wait;
   operator reboots via the M1 API (documented edge, one-cycle window).
6. **YAML 1.1 traps in the ConfigMap manifest** (`off`, numbers):
   quoted in the shipped manifest.
7. **Concurrent writers to `next-kernel`**: writer discipline (§3.5);
   the local pod never overwrites a non-quiescent value.
8. **Mixed per-node URLs**: plans serialized; operator expectation is
   documented.
9. **Failed boot with bootloader fallback**: detected as verify mismatch
   → cancel + reset (desired behavior, §3.10).

## 5. Milestones

| # | Milestone | Content |
|---|---|---|
| M1 | Config | `internal/config` (flat keys, validation, last-valid-wins, unknown-key warn, absent→flags); migrate `reboots.*` + `engine.*`; `deploy/` ConfigMap manifest; unit tests (absent key / absent ConfigMap / invalid value / unknown key / mixed); regression: re-run the reboot E2E subset (A1, B1, C1) on the test cluster to prove no regression. |
| M2 | Check | index fetch, GPG verification (go-crypto) + keyring resolution (embedded + optional Secret), sha256, arch/version mapping, per-node URL precedence, bootstrap (3 cases) + writer discipline, defensive re-staging, events; unit tests (fake HTTP server + test keyring); kubetest for annotation accessors. |
| M3 | Staging | boot-device discovery (blkid labels), `syscall.Mount`, download + verify + capacity check, zstd extraction, syslinux + rpi entry writers (ported from the reference project), purge (`preserve` + `max-percent-usage` + never-delete-running), unmount; unit tests (temp-dir writers, real zst fixtures, loop device for mount). |
| M4 | Plan + verify | Lease plan marker, admission of staged nodes to the M1 queue, leader-side verification, all-or-nothing cancel + uniform reset, takeover resume; unit tests with kubetest (including mid-plan leadership change). |
| M5 | Deployment | Dockerfile (deps + keyring ADD + util-linux), `.gitattributes` LFS + `keys/simplek8s-pubring.gpg`, DS optional Secret volume, ConfigMap manifest, RBAC `configmaps` role, README operations section. |
| M6 | E2E | E2E-UPDATE campaign (U0–U8) on the test cluster; U5 requires the maintainer to publish a new dev release. |

## 6. Decision log (closed)

1. **Single `updates.update-mode` enum** `off|stage|full` (KISS) — not
   separate enable/check/stage booleans.
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
   always reflect it (reconciliation every cycle).
8. **The plan is action-based**: triggered only by fresh staging (a
   version not previously in `/boot`), all-or-nothing, cluster-wide. No
   standing "pending → reboot" policy.
9. **Cancel semantics**: in-flight reboot completes; uniform
   `next-kernel := running` on all members; no branching between updated
   and cancelled nodes.
10. **Re-launch is two-step operator action** (re-anchor + M1 API); no
    `POST /updates` endpoint; no auto-retry of a failed version.
11. **Rollback = one annotation edit** to the older preserved version
    (documentation, not a feature).
12. **Purge never deletes the running version**; `preserve` and
    `max-percent-usage` apply to the rest.
13. **Always UTC**; no timezone key.
14. **`reboots.windows` deferred** to a future PLAN-M3 (semantics agreed,
    see TODO.md).
15. **Docs and code in English** (maintainer requirement).
16. **Plan durability via the leader Lease annotation** — no new object,
    no new RBAC, not the ConfigMap.
17. **No new API endpoints** in the MVP.
18. **No new hostPath in the DS** (privileged + hostPID is sufficient).
19. **All controller writes to `next-kernel` are conditional RMWs**
    (precondition checked at write time, not at decision time); the
    operator's unconditional write always wins a race by design.

## 7. Deferred (see TODO.md)

- Reboot maintenance windows (`reboots.windows`) — future PLAN-M3.
- `simplek8s-update` CLI (reuses this package; owns `--url`, `--keyring`,
  `--checksign`).
- Pod split (unprivileged controller + privileged host daemon).
- Rollback automation/ergonomics (the manual path is already one
  annotation edit).
- Keyring leaving the distro once the CLI exists.
