# TODO — deferred backlog

Cross-cutting items deliberately left out of the current plans
(M1 reboots, M2 updates and M3 windows all shipped — see PLAN.md).
Closed items leave this file (history in git log); what remains is
open work, each ready to be picked up as its own plan; nothing here
is blocking.

## 2. `simplek8s-update` CLI

Successor of the legacy `simplek8s-update` project:

- Reuses `internal/features/update` from this repo (k8s-agnostic core,
  M2 §3.16, in git) — check, GPG/sha verification, staging, purge,
  bootloader writers.
- Owns the power-user knobs the controller deliberately lacks: `--url`,
  `--keyring`, `--checksign` (skip verification, explicitly).
- Updates a single node without a cluster (or a fleet via a loop),
  e.g. for pre-cluster installs and for out-of-band maintenance.
- Once it exists, the keyring can leave the distro (item 5).

## 3. Pod split (security hardening)

Split the single privileged DaemonSet into:

- an **unprivileged controller pod** (engine, API, orchestration), and
- a **privileged host daemon** (nsenter reboot, boot partition mount,
  bootloader writes),

communicating via an annotation protocol (the controller requests a host
action in a node annotation, the daemon executes and reports back).
This removes most privileged capabilities from the long-running
orchestration code path. The current single privileged DS is the MVP;
the split is a future hardening milestone, not a correctness issue.

## 4. Rollback automation/ergonomics

Manual rollback is already one annotation edit
(`next-kernel :=` the older preserved version, M2 §3.5, in git) — no
feature to build. Remaining:

- document it in the operations guide (shipped during the updates era);
- (optional, later) a one-shot fleet-wide rollback helper (API or CLI)
  that re-anchors a set of nodes to a given version and reboots them via
  the M1 API.

## 5. Keyring leaves the distro

Today the SimpleK8s signing keyring lives in the distro
(`/usr/lib/systemd/import-pubring.gpg`) for the legacy
`simplek8s-update` tool. Once the CLI (item 2) exists, the keyring is
carried by the controller image (LFS, M2 §3.14, in git) and the CLI
defaulting to it — the distro file can be removed. Distro-side change,
tracked here for visibility.

## 7. Reboot orchestration success observability

The reboot orchestrator logs the failure path (`Error`) but emits no
`Info` line on the happy path: a successful cordon → drain → issue →
verify sequence is invisible in the controller log (it only shows up in
the node annotations and the `RebootCompleted` event). Add `Info`-level
log lines for the completed transitions so "it worked" is observable
without inspecting each node's annotations.

## 10. Leader-centralized update check + distribution

Today the release check, download and staging are **per-node and
autonomous**: every pod's `RunLocal` (`update/update.go`; cadence:
one check per window occurrence, PLAN.md §3.4) independently fetches
the index, downloads the artifact, extracts and stages it. The leader
runs only the per-node verification (`update/verify.go`; no plan
object since M3).

Desired: the **leader** performs the check once; when an update exists the
leader downloads it and **distributes it to the nodes** (channel + form to
study — node-pull via annotation/API, an in-cluster push, or object
storage) so each deploys it onto its own boot partition. A node, once it
has validated its local copy, sets `next-kernel`; once validated, it marks
itself reboot-eligible (eligibility is derived pod-side from
`next-kernel` vs `running` + the M1 state, as designed in PLAN.md
§3.4). Study the
trade-offs: a single download
at the leader vs N; the leader as a check-time SPOF; transfer reliability
and resumability; and how this composes with the per-node `preserve`/purge
and the pod split (item 3).

## 14. Boot failure fallback (grub + syslinux legacy) — accepted risk, no plan

W13 (PLAN.md §7.4) showed the shape: a correctly signed and staged
kernel that does not boot on a node leaves it at the GRUB menu
(syslinux legacy: the `boot:` prompt), stuck `rebooting` +
`NotReady`, with no automatic fallback — and
none is planned. (A native fallback was
verified to exist in the syslinux docs and sketched, but not
approved.) Rationale: bootability of a signed release is the distro
QA's job; the correlated bad-release case is contained by process,
not mechanism — roll out in `stage` mode with manual canary reboots
before `full`, keep N kernels preserved, and guarantee
machine-console access per node. The verified console recovery (pick
a good entry, repair, re-pin) is documented in the README runbook;
the `completed`+mismatch verification and no-auto-retry already hold
for whatever comes back.
