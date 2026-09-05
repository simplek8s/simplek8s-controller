# PLAN — simplek8s-controller

Phase: **PLAN** (design & planning). Status: v9 (4-annotation state
design adopted, reviewed by a design→review→adapt→review agent chain,
plus Gemini, Claude, and ChatGPT review rounds with verified cherry-
picks), pending final sign-off.

## 1. Purpose

A node-management controller for a self-built Kubernetes cluster
(SimpleK8s distro: Buildroot, systemd, containerd, kubeadm). It runs as a
privileged DaemonSet with one instance per node and provides operational
capabilities over the hosts.

**First feature (v1): scheduled node reboots**, with:

1. An internal HTTP REST API to schedule reboots for selected nodes.
2. A configurable limit on concurrently rebooting nodes (default: **1**).
3. Wait until a node is available again before starting the next reboot.
4. Do not reboot a node if a PodDisruptionBudget would be violated,
   unless the request passes `force`.
5. All nodes are rebootable, control-plane included (no policy flag in
   v1 — a `deny` option can be added later when needed).

The controller is intended to grow: reboot scheduling is only the first
capability. The layout (feature packages, engine, API groups) must stay
extensible (see 3.9).

## 2. Constraints

- **Go 1.27, stdlib only.** No client-go, no controller-runtime, no other
  third-party dependencies. Kubernetes API access is done with a minimal
  in-house REST client over HTTPS.
- **In-cluster credentials**: the standard serviceaccount mount
  (`/var/run/secrets/kubernetes.io/serviceaccount/{token,ca.crt,namespace}`),
  path configurable. Token rotation handled by re-reading the file on
  each request (the kubelet keeps the file fresh).
- **Production cluster.** Every deploy and test runs against the real
  cluster. Rollout discipline: validate on a single non-critical worker
  node first; control-plane reboots only after full validation.
- Code and docs in English. Repo: `github.com/simplek8s/simplek8s-controller`
  (project: simplek8s.org). No environment-specific details (hostnames,
  IPs) in committed files.

## 3. Design

### 3.1 Deployment model: DaemonSet + global orchestrator

- One `simplek8s-controller` pod per node.
- Pod is `privileged: true` + `hostPID: true`, so it can `nsenter` into
  the host's PID 1 (systemd) and issue the actual `reboot`.
- **Two roles, one binary:**
  - **Orchestrator** (global, single actor): elected via a
    `coordination.k8s.io/v1` Lease `simplek8s-controller-leader`
    (leader election: renew every 10 s, takeover when `renewTime` is
    stale > 30 s, `holderIdentity` = `<pod-name>/<pod-uid>`). A Lease is
    coordination, **not hard fencing**: it guarantees at most one pod
    *believes* it is leader, not that only one *acts* — if the leader's
    renewal is delayed (GC pause, API latency > 30 s), a successor can
    take over while the old leader is still running its loop.
    Mitigation: the orchestrator **stops making lifecycle decisions when
    its local renewal deadline expires** (renewal failures accumulate
    locally; no further transition patches), and before **each**
    lifecycle transition patch it re-reads the Lease and verifies
    `holderIdentity` and aborts the action if it no longer owns it. This
    minimizes, but cannot mathematically eliminate, the write-after-
    takeover race because the Lease update and the Node patch are
    separate API operations; a rare one-cycle overshoot of the
    concurrency limit during a handover race is still possible and
    accepted. The
    orchestrator is the **sole decision-maker and sole writer of
    lifecycle states**: it owns the queue, the concurrency limit, PDB
    checks, drain (evictions are API calls — they do not need to be
    local), and the `completed`/`failed` transitions. The Lease name is
    deliberately generic: it leads the *controller*, not the reboot
    feature; future features are orchestrated by the same leader.
  - **Local executor** (one per node, all pods): watches its own node.
    Its actions: when its node is `rebooting`, write the host boot ID,
    `issuedAt`, `attempt`, and `executorPodUID` in the `reboot-exec`
    annotation in one patch — the `nsenter -t 1 -m -u -i -n -p -- reboot`
    runs **only after that patch succeeds** (the reboot command must be
    local; nsenter reaches only the local host). `attempt=1` is the
    initial issue; `attempt=2` is the single crash-recovery re-issue
    within `--reboot-issue-grace` of `issuedAt` with the boot ID
    unchanged (fresh `issuedAt`/`bootId` patch, then `nsenter`) — this
    covers a crash between patch and `nsenter`. A pod issues **at most
    once** per lifecycle: it (re)issues only while it reads
    `attempt=1` (or no `reboot-exec` at all) — this bounds the total
    number of `nsenter … reboot` executions to two and stops a second
    pod on the same node (DaemonSet rollout) from re-issuing (3.3.1). While
    its node is `rebooting` it verifies the reboot actually happened:
    host boot ID changed vs `reboot-exec.bootId` ⇒ add `confirmedAt` to
    `reboot-exec`; unchanged past `--reboot-issue-grace` ⇒ mark `failed`
    ("reboot did not take effect"). If the command fails to start, it
    marks the node `failed` with the error. Before every write it
    re-reads its node and proceeds only while `reboot-state.state ==
    "rebooting"` (3.3).
  - Every write by any actor follows the writer rule and conditional-
    transition discipline of 3.3.
- Every other pod for a given node is a pure observer (API, health).
- `nsenter` only reaches the local host ⇒ the reboot *command* is local;
  everything else is centralized in the orchestrator. This eliminates
  cross-pod races on the queue (single writer) — see 3.5.
- **Single-control-plane outage behavior**: while the only CP is
  rebooting, the API server is down. Pods simply skip engine cycles (log
  rate-limited to one line per up→down transition; no state writes,
  nothing marked `failed`) and the API returns 503. While the API is
  down the worker orchestrator **cannot renew its Lease**; when the API
  answers again the Lease is stale and leadership typically transfers
  (any API outage > 30 s has the same effect). The new leader re-reads
  everything from annotations and completes the transition: the
  `completed` rule (3.4) relies on `reboot-exec.confirmedAt` (written by
  the node's own pod once it is scheduled again) or on a `NotReady`
  observed by the current leader after `reboot-exec.issuedAt` (per-
  leader, in-memory — 3.4) — never on a fresh
  `lastTransitionTime`, which is exactly what is unreliable in this
  scenario.
- When a CP node reboots, the local pod dies and comes back with the
  DaemonSet; the state machine is **fully recoverable from API-server
  state alone**.

### 3.2 Kubernetes access: minimal REST client (`internal/kube`)

Stdlib `net/http` + `crypto/tls` + `encoding/json`. Required verbs
(v1 scope):

| Verb | Where | Notes |
|---|---|---|
| GET | `nodes`, `pods` (all ns), `poddisruptionbudgets`, `leases`, API `/healthz` | Pagination via `limit` + `continue`; retry on 429/5xx |
| PATCH | `nodes/{name}` | `application/merge-patch+json` with `metadata.resourceVersion` for optimistic concurrency |
| CREATE (POST) | `namespaces/{ns}/pods/{pod}/eviction` | `policy/v1` Eviction body; 200 = accepted (the pod goes away up to `terminationGracePeriodSeconds` later), 429 = denied (PDB), 404 = pod already gone (not an error) — there is no separate "pending" code: a pending eviction is simply a pod that still exists (3.5) |
| DELETE | `pods/{pod}` | Fallback for `force` drain |
| CREATE/UPDATE | `leases/{simplek8s-controller-leader}` | Leader election: create if absent; **renew** = conditional UPDATE requiring `holderIdentity == self`; **acquire** a stale lease (stale > 30 s) via conditional UPDATE; 409 → re-read, re-evaluate (3.1) |
| CREATE | `events` (namespaced, regarding the Node) | one per state transition + rate-limited PDB-blocked and no-leader entries (empty namespace, 3.11) |

- TLS: CA from the serviceaccount `ca.crt`; endpoint from
  `KUBERNETES_SERVICE_HOST`/`KUBERNETES_SERVICE_PORT` env (standard
  in-cluster) or `--kube-apiserver` flag.
- Auth: `Authorization: Bearer <token>`, token re-read from file per
  request (rotation-safe).
- No `watch` in v1 — the engine polls (default 2 s). Reboot operations
  are measured in minutes, not milliseconds; polling is far simpler to
  get right with a hand-rolled client (no resync/reconnect edge cases)
  and trivially robust. Watch can be added later if the cluster grows.
- Hand-written minimal types (only the fields we use): `Node`
  (spec.unschedulable, status.conditions, metadata), `Pod` (phase,
  nodeName, ownerReferences, labels, deletionTimestamp, spec.volumes for
  the local-storage report), `PodDisruptionBudget`, `Lease`, list
  envelopes with `resourceVersion`/`continue`.
- Small retry wrapper: exponential backoff for 429/503, honor
  `Retry-After`; context deadlines everywhere.

### 3.3 Reboot state: four JSON annotations on `Node`

No CRDs (MVP, stdlib-only). State per node in **four** `Node`
annotations, grouped by concern and by writer ownership. **A node
participates in the reboot lifecycle iff `reboot-state` is present.**

| Annotation | Format | Writer(s) | Written when |
|---|---|---|---|
| `simplek8s.org/reboot-state` | `{"state":"requested\|draining\|rebooting\|completed\|failed","since":"<RFC3339 UTC>"}` | API (absent→`requested`, re-request); orchestrator (all other transitions, incl. its `failed`s); local pod (its two `failed`s only) | Admission; re-request; every state transition. `state`+`since` are **one atomic unit**: every writer that changes `state` writes both in the same patch. |
| `simplek8s.org/reboot-request` | `{"id":"<RFC3339>-<6 random>","by":"<text, omitted if absent>","force":false}` | **API only** | Admission and re-request — written once per request, never rewritten afterwards. |
| `simplek8s.org/reboot-exec` | `{"issuedAt":"<RFC3339>","bootId":"<hex>","attempt":1\|2,"executorPodUID":"<uid>","confirmedAt":"<RFC3339, added later>"}` | **Local pod only** (its own node) | `issuedAt`+`bootId`+`attempt`+`executorPodUID` together in one patch just before `nsenter … reboot`; `attempt` bumped to 2 on the single permitted re-issue; `confirmedAt` added in a later patch when the pod observes the host boot ID changed. Does not exist until the reboot is actually issued. |
| `simplek8s.org/reboot-status` | `{"blockedBy":["<ns>/pdb",...],"error":"<text>","cordonedPrev":true}` — fields present only while relevant (`error` and `cordonedPrev` persist until DELETE or re-request) | Shared: orchestrator (`blockedBy`, `cordonedPrev`, its `error`s); local pod (its `error`s); API (removes the whole annotation on re-request) | `blockedBy`: each cycle while `requested` and blocked. `cordonedPrev`: at `requested→draining`, if the node was already cordoned. `error`: on a `failed` transition, in the same patch as the state. |

All four are cleared together: `DELETE /reboots/{node}` sets all four
keys to `null` in one patch (3.9). `completed`/`failed` remain as
history until cleared (DELETE) or replaced by a new request.

#### 3.3.1 Field notes

- **`reboot-state`**: `since` is stable for the whole duration of the
  state (no writer can bump it without changing `state`). Consumers:
  queue = `.state=="requested"` ordered by `.since` (while `requested`,
  `.since` **is** the request time — the old separate
  `reboot-requested-at` is gone), ties broken by **workers before
  control planes**, then node name; drain timeout = now − `.since` while
  `.state=="draining"` (**wall-clock**: keeps running during API-server
  outages; a node already over the timeout at recovery is `failed` +
  uncordoned on the next cycle); `GET /reboots` renders `state`+`since`.
  Unparseable value: see 3.3.3.
- **`reboot-request`**: `id` correlates the API response, the admission
  Event, and all later Events (orchestrator and local pod read it from
  the node); `by` is the operator-supplied origin (field **omitted**
  when absent — no empty-string values); `force` is read by the
  orchestrator to skip the PDB check and use the force-delete fallback —
  it lives in the node's annotation precisely because it must survive
  leader/pod restarts, and `id`/`by` ride along at ~60 bytes. Unparseable: fields render as
  absent, `force` defaults to `false` (never assume force from a corrupt
  annotation).
- **`reboot-exec`**: `bootId` is the host boot ID
  (`/proc/sys/kernel/random/boot_id`, kernel-global, readable by the
  pod). `attempt` is `1` (initial issue) or `2` (the single crash-
  recovery re-issue within `--reboot-issue-grace`); `executorPodUID`
  records which pod issued the command (env `POD_UID`) so a duplicate
  executor is diagnosable in `GET /reboots`. **Issue bound**: a pod
  issues at most once per lifecycle; it (re)issues only while it reads
  no `reboot-exec` or `attempt=1`, the boot ID unchanged, and now within
  `issuedAt + --reboot-issue-grace`. A second pod on the same node (a
  DaemonSet rollout briefly leaves the old and the new pod) therefore
  never re-issues when it sees `attempt=2` (or its own execution
  already recorded). Consumers: local pod — no-effect check (now >
  `issuedAt + --reboot-issue-grace` and boot ID unchanged ⇒ `failed`)
  and confirmation (boot ID changed ⇒ add `confirmedAt`); orchestrator —
  `completed` requires `confirmedAt` **or** an observed `NotReady` after
  `issuedAt`, and **both conjuncts require `reboot-exec` to exist**: a
  `rebooting` node without it has **no completion evidence** and stays
  `rebooting` until the local pod issues the reboot (issuing is
  idempotent, 3.4). A node in `requested`/`draining`/`completed`/`failed`
  (command never started) may lack the annotation entirely.
- **`reboot-status`**: `blockedBy` (JSON array of `ns/pdb`) is present
  only while `.state=="requested"` and blocked; `error` is present only
  while `.state=="failed"` (history until DELETE or re-request);
  `cordonedPrev` is `true` when the node was already cordoned before the
  drain started (set at `requested→draining`, read at `completed` **and
  at every `failed`** to decide whether to uncordon — 3.3.2). This is
  the **only shared-value annotation**; the RMW discipline in 3.3.2
  makes it safe. "Clearing a field" = rewriting the annotation value
  without that field (merge-patch acts on the whole string value).
- **Timestamps**: all RFC3339, UTC, second precision (Kubernetes
  convention, lexicographically sortable); written by the acting pod
  from its clock.

#### 3.3.2 Writer rule and write discipline

- **Orchestrator** writes all lifecycle transitions
  (`requested→draining`, `draining→rebooting`, `rebooting→completed`,
  and its `failed` transitions: drain timeout — including a PDB denial
  that survives retries, 3.5 — and force-delete error), and owns
  `reboot-status.blockedBy`, `reboot-status.cordonedPrev`, and
  `reboot-status.error` for its own `failed` transitions.
- **Any pod (API)** may perform absent→`requested` (admission) and
  re-request, and may delete (clear) the state. It is the **only writer
  of `reboot-request`** and the only actor that removes annotations.
- **Local pod** writes `reboot-exec` (its own node only) and may mark
  `failed` (state + `reboot-status.error`) on command-start failure or
  no-effect reboot. Before every write it re-reads its node and proceeds
  only if `state=="rebooting"` (and `reboot-exec` is present when adding
  `confirmedAt`); otherwise it writes nothing.
- **Conditional transitions (universal)**: every state-transition patch
  is conditional. After any 409 — or any re-read for any reason —
  re-evaluate the transition's precondition against the **fresh** node
  before re-patching. If it no longer holds (state already the target,
  something else, or not the transition's source), **abort without
  writing** — in particular a `failed` loser writes no `error` (covers
  the failed-vs-failed and failed-vs-completed races).
- **Uncordon on `failed`**: the orchestrator uncordons any node it
  cordoned (i.e. `cordonedPrev` is **known** absent) whenever the node is
  in `failed`, **regardless of which actor wrote the transition** — as
  its own idempotent `spec.unschedulable: false` patch, skipped when
  `cordonedPrev` is present, when the fresh read shows the node is
  already unschedulable=false (an operator may have uncordoned manually
  mid-drain — do not overwrite it), or when `cordonedPrev` is
  **unreadable**: an unreadable value is not guessed as absent — the
  cordon is **preserved**, a rate-limited log line + Event states that
  explicit operator repair is required (`kubectl uncordon` or repair the
  annotation), and the node otherwise stays `failed`. This also covers
  local-pod-caused `failed` (no-effect, command-start failure) on
  drained, cordoned nodes.
- **Serialization / 409**: all writes are atomic
  `application/merge-patch+json` including a fresh
  `metadata.resourceVersion`. Node status updates (~10 s, kubelet) bump
  `resourceVersion` often, so on 409 the writer re-reads and retries
  **immediately within the same cycle**, then applies the
  conditional-transition rule above. Two writers may both attempt a
  `failed` transition; the loser of the state patch sees the target
  state already set on re-read and backs off without writing `error`.
  Whoever wins the state patch owns `error` for that transition (written
  in the *same* merge-patch as the state).
- **RMW discipline for `reboot-status`** (the only shared-value
  annotation): before patching, re-read the node; build the new status
  JSON by applying **only your own field changes** on top of the fresh
  value (never overwrite fields you don't touch); include
  `metadata.resourceVersion`; on 409 re-read and retry within the same
  cycle. A "clear" is a field removed from the JSON value, or the key
  set to `null` to delete the whole annotation.

Example patches (one merge-patch each, all include fresh
`metadata.resourceVersion`):

- **Admission / re-request** (API): `reboot-state:
  {"state":"requested","since":"<now>"}`, `reboot-request:
  {"id":"<new>","by":...,"force":<bool>}`, `reboot-exec: null`,
  `reboot-status: null`. Atomic: a re-request can never leave stale
  exec/status behind.
- **`requested→draining`** (orchestrator): `reboot-state:
  {"state":"draining","since":"<now>"}`, `reboot-status:` fresh value
  minus `blockedBy`, plus `cordonedPrev` if the node was already
  cordoned, and `spec.unschedulable: true` in the same patch.
- **Local pod issues reboot** (only while `state=="rebooting"`):
  `reboot-exec: {"issuedAt":"<now>","bootId":"<hex>"}`.
- **Local pod confirms** (only while `state=="rebooting"` and
  `reboot-exec` present): `reboot-exec: <fresh + confirmedAt>`.
- **Local pod no-effect / command failure** (only while
  `state=="rebooting"`): `reboot-state:
  {"state":"failed","since":"<now>"}`, `reboot-status: <fresh +
  error>`. (Uncordon is the orchestrator's job, next cycle — 3.3.2.)
- **Orchestrator `failed`** (drain timeout — including an unrecovered
  PDB denial — / force-delete error): `reboot-state: {"state":"failed","since":"<now>"}`,
  `reboot-status: <fresh + error>`, and `spec.unschedulable: false`
  only if `cordonedPrev` is absent.
- **`completed`** (orchestrator): `reboot-state:
  {"state":"completed","since":"<now>"}`, and `spec.unschedulable:
  false` in the same patch only if `cordonedPrev` is absent.
- **DELETE** (any pod): all four annotation keys → `null` in one patch, **plus `spec.unschedulable: false`** when `reboot-status.cordonedPrev` is **known** absent (a `requested`/`rebooting` node may be cordoned; when `cordonedPrev` is unreadable the cordon is preserved and the operator uncordons explicitly — 3.3.3).

#### 3.3.3 Corrupted annotations (fault handling)

- **`reboot-state` unparseable** (or missing `state`/`since`): the node
  counts as **in-flight** (slot-holding, conservative for concurrency),
  is shown with `parseError` in `GET /reboots`, and gets one
  rate-limited log line + Event. **This holds a concurrency slot
  indefinitely until the operator DELETEs the node's reboot state (works
  regardless of content) or repairs the annotation; with
  `--max-concurrent-reboots=1` a single corrupt annotation halts the
  whole queue** — the Event must state both escapes and include the
  offending raw value. Accepted over auto-clearing: silently clearing an
  unparseable state could restart a reboot lifecycle on a node that is
  actually mid-reboot — worse than a pause with a loud Event.
  `GET /reboots/{node}` returns the node with `parseError` (not 404);
  `DELETE` is the guaranteed escape — the uniform 4-key clear (plus
  uncordon when `cordonedPrev` is known absent; an unreadable
  `cordonedPrev` keeps the cordon) — and works on any content.
- **Other annotations unparseable**: fields render as absent; `force`
  defaults to `false` (never assume force from a corrupt annotation);
  exec checks (no-effect, confirmation) simply do not run until fixed;
  the orchestrator proceeds on what it can read.
- Writers never panic on bad input: log, Event, skip the node for that
  cycle (same rate-limiting discipline as "API unreachable").
  `GET /reboots` must never 500 on bad annotations.

#### 3.3.4 Mapping: 12 old annotations → 4

| Old (v4 §3.3) | New location |
|---|---|
| `reboot-state` | `reboot-state` JSON `.state` |
| `reboot-state-since` | `reboot-state` JSON `.since` (atomic with state) |
| `reboot-requested-at` | **removed** — while `.state=="requested"`, `.since` **is** the request time; no observable behavior change |
| `reboot-request-id` | `reboot-request` JSON `.id` |
| `reboot-requested-by` | `reboot-request` JSON `.by` (omitted when absent) |
| `reboot-force` | `reboot-request` JSON `.force` |
| `reboot-cordoned-prev` | `reboot-status` JSON `.cordonedPrev` |
| `reboot-blocked-by` | `reboot-status` JSON `.blockedBy` (array, was CSV) |
| `reboot-issued-at` | `reboot-exec` JSON `.issuedAt` |
| `reboot-boot-id` | `reboot-exec` JSON `.bootId` |
| `reboot-confirmed-at` | `reboot-exec` JSON `.confirmedAt` |
| `reboot-error` | `reboot-status` JSON `.error` |

12 annotations → 4; 11 distinct data points remain
(`requested-at` merged into `state-since`).

### 3.4 State machine (per node)

```mermaid
stateDiagram-v2
    [*] --> requested: POST /reboots (admission, any pod)
    requested --> requested: PDB blocked — reboot-status.blockedBy updated, retried each cycle
    requested --> draining: slot free + PDB OK (or force)
    requested --> [*]: DELETE (cancel — annotations cleared)
    draining --> rebooting: no evictable pods remain (evicted, or force-deleted)
    draining --> failed: drain timeout (incl. PDB denial or unmanaged pod surviving retries) / force-delete error
    rebooting --> failed: command failed to start / reboot no effect (boot ID unchanged)
    rebooting --> completed: Ready + reboot confirmed (no timeout)
    completed --> [*]: DELETE (clear history)
    failed --> [*]: DELETE (clear history — resumes a paused queue)
    completed --> requested: new POST (re-request)
    failed --> requested: new POST (re-request)
```

While `rebooting`, the node's local pod records the host boot ID and
`issuedAt` in `reboot-exec`, issues `nsenter … reboot`, and later adds
`confirmedAt` when the boot ID has changed (see 3.5 step 5).

A node deleted from the cluster exits the machine entirely: its state
disappears with it, and the orchestrator continues without error (see
the "Node disappears mid-plan" rule below).

Rules:

- **`completed`**: orchestrator sees `rebooting` + node `Ready` +
  evidence that the reboot really happened ⇒ `completed`, and uncordons
  the node if the controller cordoned it (`cordonedPrev` absent). Both
  evidence conjuncts require `reboot-exec` to exist (a `rebooting` node
  without it has no completion evidence): the local pod added
  `confirmedAt` to `reboot-exec` (host boot ID changed vs
  `reboot-exec.bootId`), or the **current** leader observed the node `NotReady` at least
  once after `reboot-exec.issuedAt` — this memory is per-leader and
  in-memory: it is lost on takeover, so a taken-over `rebooting` node
  with no `confirmedAt` and no observed `NotReady` simply waits for
  evidence (escape: `DELETE`). This stays correct in the
  single-CP case without trusting condition `lastTransitionTime`: while
  the API was down no `NotReady` is observable, but the node's own pod
  writes `confirmedAt` as soon as it is scheduled again. It also closes
  the no-effect hole: a `reboot` command that is accepted but never
  reboots the host can never produce `completed`, because the boot ID
  never changes (and the grace rule below fails the node).
- **No reappearance timeout (KISS)**: a node stays `rebooting` until it
  is `Ready` again. A node that never comes back is visible through the
  platform itself (`NotReady` node + `NodeNotReady` events) *and* the
  `rebooting` annotation; it keeps its slot, so the queue does not
  advance. The operator's escape is `DELETE /reboots/{node}` (see 3.10)
  — "stop watching this node". No `lost` state, no give-up timer.
- **`failed`** occurs only on definite errors: drain timeout (e.g. pod
  stuck `Terminating` on finalizers), eviction still PDB-denied when the
  drain timeout expires (without `force`; denials are retried with
  backoff in the meantime — 3.5, decision 19), an unmanaged pod (no
  owner) surviving the drain timeout without `force` (3.5), force-delete error, the
  reboot command failing to start, or
  the reboot having no effect (host boot ID unchanged
  `--reboot-issue-grace` after `reboot-exec.issuedAt`, detected by the
  local pod — distinguished from "node has not reappeared"). On every
  `failed` the orchestrator uncordons the node if it cordoned it
  (`cordonedPrev` absent), regardless of which actor wrote the transition
  (3.3.2).
- **Queue pause on failure**: with `--on-reboot-failure=pause` (default),
  the orchestrator stops admitting new nodes while any node is
  `failed`; it resumes when the operator clears the `failed` state via
  `DELETE`. `--on-reboot-failure=continue` advances to the next node
  immediately. Rationale: a failed reboot may indicate a defective
  distro build; continuing could brick the rest of the fleet.
- **PDB-blocked** is not a failure: the node stays `requested`, the
  blocking PDBs are recorded (+ one rate-limited Event on entry), and the
  check is retried each cycle. A PDB-blocked node is **skipped**: later
  queued nodes may proceed — the block is per-node, visible in
  `GET /reboots`, with the `force`/`DELETE` escapes (3.6).
- **Queue hold on `NotReady`**: if any node still in `requested` (queued,
  reboot not yet issued) is `NotReady`, the orchestrator holds the whole
  queue (no slot acquisition) and logs/Events rate-limited; it resumes
  when every queued node is `Ready` again. A `NotReady` node is never
  drained — eviction on a dead node cannot work — and admission (3.9)
  only accepts `Ready` nodes, so this catches nodes that went `NotReady`
  after admission. Nodes already `draining`/`rebooting` keep their own
  rules (drain timeout / no timeout).
- **Node disappears mid-plan**: state lives in the node's annotations,
  so a node deleted from the cluster loses its state with the object.
  The orchestrator never treats disappearance as an error, never
  pauses or holds the queue because of it, and keeps no in-memory
  per-node state for nodes that are gone — every cycle is derived
  purely from the current node list. A node that disappears while
  `draining` or `rebooting` simply frees its slot; one that disappears
  while `failed` clears the pause condition. One log line (rate-
  limited) + an Event on detection.
- **Crash/leader recovery**: all state is in annotations; on leader
  takeover the new orchestrator re-reads everything and continues. A
  node in `rebooting` without `reboot-exec` (local pod died before
  issuing) is issued by the local pod on its next cycle; a node with
  `reboot-exec` whose boot ID is unchanged is (re)issued once within the
  grace window (crash between patch and `nsenter`), and fails past it —
  issuing is idempotent. The boot-ID checks (`reboot-exec` / no-effect failure) are
  annotation-driven, so they survive pod and leader restarts. A node in
  `draining` past `--reboot-drain-timeout` is marked `failed` and
  uncordoned.
- **Lease during `rebooting` of the leader's own node**: the leader dies
  with the host; the next leader takes over after ~30 s. Expected and
  benign.

### 3.5 Reboot flow

1. **Admission** (API, any pod): see 3.10. Patches absent→`requested`
   (409/422 otherwise).
2. **Slot acquisition** (orchestrator, ~2 s loop): while in-flight count
   < `--max-concurrent-reboots` (with at most **one** control-plane node
   in-flight — hard limit, not configurable) and the queue is not paused
   or held (3.4), take the oldest `requested` node, **skipping PDB-
   blocked nodes** (3.4), and patch it to `draining`. Single writer — no races (Lease
   re-validated before every transition patch, 3.1).
3. **PDB check** (orchestrator, skipped if `force`): see 3.7. It runs
   *before* cording: a blocked node stays `requested`, records
   `reboot-status.blockedBy`, and is **never cordoned**.
4. **Drain** (orchestrator, only after the PDB check passed):
   - Cordon (`spec.unschedulable=true`), recording
     `reboot-status.cordonedPrev` if it was already cordoned.
   - Evict pods via the **Eviction API** (`policy/v1`), skipping
     daemonset-managed pods, mirror pods of static pods (detected by the
     `kubernetes.io/config.mirror` annotation plus an ownerReference of
     kind `Node` — the API objects of CP static pods; they are never
     evicted, and an eviction/delete against one is harmless but useless:
     the kubelet simply recreates the mirror), and already-terminating
     pods. Eviction is asynchronous: 200 = accepted (the pod goes away up
     to `terminationGracePeriodSeconds` later), 429 = denied (PDB),
     404 = pod already gone — the drain loop distinguishes accepted /
     denied / gone, and "pending" is simply a pod that still exists (with
     or without `deletionTimestamp`). A PDB-denied eviction without
     `force` is **not** immediately fatal: it is retried with backoff
     while `--reboot-drain-timeout` remains, because a PDB's state can
     fluctuate due to activity unrelated to the draining node (same
     rationale as `kubectl drain`'s retry loop); still denied at the
     timeout → `failed` (decision 19).
   - **Unmanaged pods** (no `ownerReferences` — no controller will
     recreate them) are a deliberate exception to the skip list: without
     `force` the drain **blocks on them** (node stays `draining` until
     `--reboot-drain-timeout` expires → `failed` + uncordon, giving the
     operator the timeout window to delete or recreate the pod); with
     `force` they are deleted by the fallback step below. Documented in
     the drain section (3.5), the API docs (3.9), and the M9 runbook.
   - With `force`: after eviction attempts, `DELETE` remaining
     evictable pods — **including unmanaged pods** (aggressive,
     destructive, documented). `force` **never removes
     finalizers** — a pod stuck on a finalizer survives the DELETE and
     simply runs the drain out to `--reboot-drain-timeout` → `failed`
     (accepted behavior, not an extra escape hatch).
   - **No local-storage gate** (unlike `kubectl drain`): a reboot
     destroys node-local ephemeral storage anyway, so gating on it would
     protect against an inevitable outcome. Documented prominently:
     *scheduling a reboot wipes the node's local ephemeral storage*.
   - Wait until no evictable pods remain on the node (timeout
     `--reboot-drain-timeout` → `failed` + uncordon).
   - **Control-plane nodes**: static pods (kube-apiserver, etcd,
     controller-manager, scheduler) are not evicted; the kubelet
     recreates them after reboot. Only user/addon pods are drained.
5. **Reboot**: orchestrator patches the node to `rebooting`. The local
   pod (next cycle) writes `reboot-exec` (`{"issuedAt","bootId",
   "attempt","executorPodUID"}` — `/proc/sys/kernel/random/boot_id` is
   kernel-global, so the pod reads it directly; `executorPodUID` comes
   from env `POD_UID`) in one patch; the `nsenter … reboot` runs **only after
   that patch succeeds** (a crash in between is covered by the re-issue
   rule, 3.1/3.4). If the command fails to start (binary missing,
   permissions), the local pod marks `failed` with the error. Reboot verification is annotation-driven and
   idempotent: while the state is `rebooting`, the node's local pod
   compares the current boot ID with `reboot-exec.bootId` — changed ⇒
   adds `confirmedAt` to `reboot-exec`; unchanged for
   `--reboot-issue-grace` ⇒ marks `failed` ("reboot did not take
   effect"), the "command succeeded but the host never rebooted" path.
   A host that is genuinely down cannot fire this check (its pod is
   dead), so slow boots are never false-failed.
6. **Wait for reappearance**: orchestrator, per 3.4 — until `Ready`
   again, no timeout. The local pod restarts with the node and adds
   `confirmedAt` to `reboot-exec` once the boot ID has changed; the
   orchestrator
   (or its successor) completes `completed` per the verification rule
   in 3.4.

### 3.6 "All nodes blocked" — the cluster cannot wedge

- The PDB check happens *before* cording, so a blocked node is never
  cordoned and nothing on it changes. Worst case: the node stays
  `requested` until the operator acts (later queued nodes are not
  blocked by it — skip rule, 3.4); the cluster keeps running normally.
- At most `--max-concurrent-reboots` nodes are cordoned at any time —
  exactly the ones actively draining/rebooting.
- Visibility: `reboot-status.blockedBy` and `GET /reboots` show exactly
  which PDBs block which node.
- Escapes: fix the workload/PDB (scale up, lower `minAvailable`, raise `maxUnavailable`),
  `DELETE` the request, or re-issue with `force` (bypasses the check;
  the Eviction API still denies protected evictions and `force` falls
  back to direct pod deletion — documented as aggressive).

### 3.7 PDB evaluation per node

For target node N, over every PDB **whose namespace contains selected
pods running on N** (a PDB is namespace-scoped — it only applies to
pods in its own namespace):

1. Select pods **in the PDB's namespace** matching the PDB's selector
   (phase Running, not terminating). Pods in other namespaces are
   invisible to this PDB and never count against it.
2. `disruptableOnN` = selected pods running on N that the drain would
   remove (the drain's skip list, 3.5 step 4: daemonset-managed, mirror
   pods of static pods, terminating).
3. Blocked if `|disruptableOnN| > 0` and `status.disruptionsAllowed <
   |disruptableOnN|` — the PDB's `status.disruptionsAllowed` is
   namespace-scoped and computed by the API server for both
   `minAvailable` and `maxUnavailable` specs (it already subtracts
   in-flight disruptions), so the check covers every PDB.
4. `force=true` → skip the check; the Eviction API remains the final
   arbiter (denied evictions handled per 3.5 step 4).

MVP: direct computation (list pods + list PDBs, in-memory). Fine for
small clusters. The in-memory pre-check and the server-side Eviction API
can diverge (different views of available/disruptions); the API is the
arbiter. Tests must cover deliberate divergence, not just the happy
path.

### 3.8 Extensibility

The controller is a platform for node-management features. Structure:

```
cmd/simplek8s-controller/   wiring, flags, lifecycle
internal/kube/              minimal k8s REST client + types
internal/nodestate/         annotation read/write, state types
internal/engine/            leader election, orchestrator loop, local loop
internal/api/               HTTP server: registered route groups
internal/features/reboot/   v1 feature: drain, pdb, rebooter, task, routes
deploy/                     kustomize (SA, RBAC, DaemonSet, Service, NetworkPolicy)
```

- Each feature = one package registering (a) an orchestrator task,
  (b) a local executor, and (c) API routes under
  `/api/v1/<feature>/...`. Adding feature #2 must not touch the reboot
  feature; the leader is feature-agnostic (runs registered tasks).
- The kube client is feature-agnostic; new verbs are added as methods.
- The reboot feature keeps its queue, slots, limits, and holds inside
  `features/reboot`; `internal/nodestate` stores per-feature annotation
  sections, so a second feature's annotations never touch the reboot
  ones.

### 3.9 HTTP API (per instance)

| Method | Route | Description |
|---|---|---|
| POST | `/api/v1/reboots` | `{"nodes":["n1","n2"],"force":false,"requestedBy":"..."}`; `"nodes":["*"]` = all nodes (workers queued first, CPs last). → `requested`. 202. |
| GET | `/api/v1/reboots` | The plan: every node with a `reboot-state` annotation — in-flight (`requested`/`draining`/`rebooting`) plus `completed`/`failed` history until cleared (state, since, force, by, blockedBy, error, issued, confirmed, and the node's current `Ready` condition). Nodes without state are not listed; nodes with a corrupt `reboot-state` are listed with `parseError` (never a 500, 3.3.3). |
| GET | `/api/v1/reboots/{node}` | State of one node. The lifecycle `state` is sourced from the four annotations alone (including the `parseError` sentinel); the node's current `Ready` status is a **separate** field sourced from `Node.status` — the two are not conflated. 404 if the node has no reboot state; a corrupt `reboot-state` is returned with `parseError` (3.3.3), not 404. |
| DELETE | `/api/v1/reboots/{node}` | `requested`: cancel. `rebooting`: stop watching (operator escape for a node that never returns). `completed`/`failed`: clear history (also resumes a paused queue). `draining`: 409 (an in-flight drain is not interruptible; if it fails the node is `failed` and the plan pauses). No state: 404. In every allowed case the clearing is uniform: **one patch setting all four annotation keys to `null`**, plus `spec.unschedulable: false` when `cordonedPrev` is **known** absent (no orphaned cordon); an unreadable `cordonedPrev` preserves the cordon — the operator uncordons explicitly (3.3.2/3.3.3). On 409 the DELETE re-reads and re-evaluates the per-state semantics against the fresh state. |
| GET | `/livez` | Process alive (never queries the API server). |
| GET | `/readyz` | Pod up + engine loop healthy: last successful engine cycle within 30 s (tolerant of brief API-server outages, so a single-CP reboot does not flap every pod out of the Service endpoints). |

**Admission checks** (per node; all must pass to be accepted):

1. Node exists (404).
2. Node is `Ready` (422 — draining a dead node would waste the slot).
3. The controller pod on that node is `Running` (422 — otherwise no one
   can issue the reboot).
4. State is re-requestable: absent, `completed`, or `failed` (else 409).
   On success the API writes, in one atomic patch: fresh `reboot-state`
   (`requested`) + fresh `reboot-request` (`id`/`by`/`force`), and
   removes `reboot-exec: null` and `reboot-status: null` — a re-request
   can never leave stale exec/status behind. On 409 from the patch:
   re-read, re-evaluate the state check against the fresh value, retry
   within the request (a node that raced into `draining` correctly
   yields 409).

Control-plane nodes pass the same checks as workers; there is no
policy gate in v1 (decision 1).

With `"nodes":["*"]` (or a mixed list), admission is **per-node and
partial**; the response is 202 with
`{"accepted": ["n1", ...],
  "rejected": [{"node": "n2", "code": 422, "reason": "node not Ready"}, ...]}`
(rejection `code` is the admission-check status: 404/409/422).

Malformed request body → 400. A single-node POST returns the same
partial shape (one entry). A successful `DELETE` returns 204. While the
API server is unreachable, `GET`/`POST`/`DELETE /reboots` return
**503** (the endpoints need the API server); after ~30 s the pod also
drops out of the Service endpoints (readyz) and the Service then
answers 404 — the expected 404 window of 3.12.

- Auth: **mandatory** fixed token via env `SIMPLEK8S_API_TOKEN` →
  `Authorization: Bearer <token>` (missing/mismatching token ⇒ 401; a
  pod started without the token **refuses to start**). The production
  deployment **supplies** the token via the Secret (created by a secure
  generation mechanism — Kustomize composes/injects the Secret
  reference, it does not itself generate random material); a deployment
  without a token is not supported, so the API is never accidentally
  exposed tokenless.
- The API is callable on any pod (state is global); the orchestrator
  executes decisions, the target node's pod issues the reboot.
- Access: a `Service` (NodePort, `externalTrafficPolicy: Cluster`) fronts
  the per-pod API; the Service routes to a ready pod, so a NodePort hit
  on a node whose local pod is not ready still reaches a live API (with
  `Local` such a hit would drop the connection). Production
  kustomize ships a **NetworkPolicy** restricting ingress to the Service
  (the plain-HTTP token is not the only wall); `kubectl port-forward`
  to any pod is the documented alternative that needs no reachable
  NodePort at all.
- The engine rate-limits "API server unreachable" log lines (one per
  up→down transition) and emits the same rate-limited Event when no
  valid leader is observed while reboot state exists (lease stale/absent)
  — so `kubectl describe node` shows why a plan is stuck.

### 3.10 Configuration (flags / env)

| Flag | Default | Description |
|---|---|---|
| `--max-concurrent-reboots` | `1` | In-flight nodes at once (`draining` + `rebooting`; corrupt-state slot holders also count, 3.3.3). |
| `--on-reboot-failure` | `pause` | `pause`\|`continue` — queue behavior while any node is `failed`. |
| `--reboot-drain-timeout` | `10m` | Max drain duration (wall-clock; guards pods stuck on finalizers). |
| `--reboot-issue-grace` | `5m` | After `reboot-exec.issuedAt`, time for the host to actually reboot (boot ID change) before the local pod fails the node as no-effect (the reboot is (re)issued once within this window, 3.1). |
| `--engine-interval` | `2s` | Engine poll period. |
| `--listen` | `:8080` | API bind address. |
| `--kube-apiserver` | in-cluster env | Override API endpoint. |
| `--creds-dir` | `/var/run/secrets/kubernetes.io/serviceaccount` | Serviceaccount files. |
| env `NODE_NAME`, `POD_NAME`, `POD_UID`, `POD_NAMESPACE` | downward API | Local node / pod identity (`POD_UID` feeds `reboot-exec.executorPodUID`, 3.3.1) / namespace. |
| env `SIMPLEK8S_API_TOKEN` | — | **Mandatory** API token (401 without it). |

Constants (not flags in v1): leader election renew 10 s, takeover at
30 s stale; **at most one control-plane node `draining`/`rebooting` at
a time**, independent of `--max-concurrent-reboots` (protects etcd
quorum on multi-CP clusters).

### 3.11 RBAC (ServiceAccount `simplek8s-controller`)

- **ClusterRole** (cluster-wide binding) — cluster-scoped and
  cross-namespace resources:
  - `nodes`: get, list, patch
  - `pods`: get, list, delete
  - `pods/eviction`: create
  - `poddisruptionbudgets` (policy): get, list
- **Role in the empty namespace** (+ RoleBinding for the ServiceAccount
  there):
  - `leases` (coordination.k8s.io): get, create, update — the leader
    Lease `simplek8s-controller-leader` lives in the **empty namespace**
  - `events`: create — Node-related events live in the empty namespace
    so `kubectl describe node` shows them. One per state transition +
    rate-limited PDB-blocked and no-leader entries (decision 4)

### 3.12 DaemonSet pod spec (sketch)

```yaml
containers:
- name: controller
  # base image must contain nsenter AND reboot (util-linux):
  # alpine or debian-slim — NOT distroless
  image: ghcr.io/simplek8s/simplek8s-controller:v1
  securityContext:
    privileged: true
  hostPID: true
  env: [NODE_NAME, POD_NAME, POD_UID, POD_NAMESPACE (all downward API)]
  ports: [{name: api, containerPort: 8080}]
  livenessProbe: httpGet /livez
  readinessProbe: httpGet /readyz
serviceAccountName: simplek8s-controller
priorityClassName: system-node-critical   # ops controller: survive scheduling pressure
# REQUIRED: without the CP toleration there is no pod on control-plane
# nodes, and CP reboots are impossible (admission check 3 would 422).
tolerations:
- key: node-role.kubernetes.io/control-plane
  operator: Exists
  effect: NoSchedule
```

Liveness must **not** depend on API-server reachability: during a
single-CP outage, `/livez` stays 200 on every node (no restart storm);
`/readyz` failing only removes the pod from the Service endpoints.

## 4. Risks & safety notes (production cluster)

1. **Control-plane reboot**: control-plane nodes are rebootable exactly
   like workers, and v1 offers **no way to lock them out** (decision
   1). With a single CP and `max-concurrent=1`, a failed recovery leaves
   the
   cluster down; while the CP is down the other pods idle safely (3.1).
   On multi-CP clusters, rebooting two CPs at once can drop etcd below
   quorum (API down even though a CP survives); the hard limit of one
   concurrent CP reboot (3.5) prevents this regardless of
   `--max-concurrent-reboots`. Mitigation: validate on a worker
   first; keep default concurrency at 1; document the risk
   prominently (operators must be aware that a batch
   `*` always includes the control planes).
2. **Node that never returns** (bricked distro, hardware): stays
   `rebooting`, holding its slot — the queue does not advance on its
   own. Visibility: `NotReady` + annotation + Events. Escape: `DELETE`
   (3.9). No automatic assumption is made about the node's fate.
3. **Queue pause on failure**: a `failed` node pauses the queue by
   default (`--on-reboot-failure=pause`) — a defective build must not
   cascade into the rest of the fleet.
4. **Incomplete drains**: pods stuck `Terminating` (finalizers) ⇒ drain
   times out, the node is **not** rebooted, state `failed`, uncordoned.
5. **Rolling updates of the DaemonSet**: a new version deployed while a
   node is mid-drain kills the orchestrator/executor; leader takeover
   (~30 s) + annotation state resumes everything. The most frequent
   crash-recovery case is a crash between the `reboot-exec` patch and
   the `nsenter` (re-issue within the grace window, 3.1/3.4); both are
   in the M8 validation list.
6. **Concurrent writers**: four JSON annotations, each with a single
   owner except `reboot-status` (shared: orchestrator / local pod /
   API), which is governed by the RMW + conditional-transition
   discipline of 3.3.2 (a discipline requirement on the implementer —
   write windows are disjoint per state, and M2 unit tests must
   simulate a same-instant cross-field RMW on `reboot-status` and
   assert no field loss).
7. **Corrupt annotation**: a hand edit or a bug can corrupt a
   `reboot-state`; the node then holds a concurrency slot until
   `DELETE` or repair — at the default concurrency of 1 the whole queue
   halts, with a loud Event stating both escapes (3.3.3). Accepted over
   auto-clearing.
8. **Privileged pod**: by design (nsenter). RBAC scoped to the
   ServiceAccount; API token supplied by the production deployment
   (mandatory, 3.9).
9. **Plain-HTTP operator API**: the NodePort API is unencrypted HTTP
   with a mandatory static bearer token — anyone who can reach the
   NodePort **and hold the token** can schedule reboots, control planes
   included (accepted in
   v1: trusted internal network; TLS termination in front can be added
   later without API changes).

## 5. Milestones

| # | Milestone | Deliverable |
|---|---|---|
| M0 | Scaffold | git, go.mod (stdlib only), package layout, Makefile, kustomize skeleton (SA+RBAC+DaemonSet+Service+NetworkPolicy+token Secret), README |
| M1 | `internal/kube` | REST client (auth, TLS, retries, pagination, leases, events), minimal types, unit tests (httptest): eviction status codes (200/429/404: accepted/denied/gone) and the merge-patch `resourceVersion` discipline (200 / 409 / 200) |
| M2 | `internal/nodestate` | 4-annotation JSON model (parse/serialize, tolerant parse, per-field RMW for `reboot-status`), state types, conditional-transition + 409-retry discipline, conflict tests (incl. same-instant cross-field RMW on `reboot-status` and the failed-vs-completed race), corrupt-annotation deserialization tests (tolerant parse, never a panic — 3.3.3) |
| M3 | Engine | leader election (Lease), orchestrator loop (queue, concurrency, pause), local loop, state machine (fake rebooter in tests); split-brain test (two orchestrator loops against a fake API with a delayed renew: the stale leader must abort its transition); Lease protocol tests (a stale leader's delayed renew cannot flip `holderIdentity`; takeover only after > 30 s stale) |
| M4 | Drain | cordon + evictions (200/429/404: accepted/denied/gone; pending = pod still present), PDB-denial retry-with-backoff until drain timeout (decision 19), drain timeout (wall-clock), force fallback |
| M5 | PDB | per-node evaluation + force bypass + deliberate-divergence tests |
| M6 | Rebooter | nsenter, `reboot-exec` marker (`issuedAt`/`bootId`/`attempt`/`executorPodUID` in one patch), issue bound (re-issue only while `attempt=1` or absent — never beyond the single 1→2 re-issue), command-failure path, unit tests incl. a duplicate-executor case (a second pod on the node must not re-issue when it reads `attempt=2`) |
| M7 | HTTP API | endpoints, admission checks, partial responses, mandatory token auth (401 path; refuses to start without the token), GET per-node (`state` from annotations, `Ready` from `Node.status`), /livez /readyz |
| M8 | Deploy & validate | image build, deploy, then on one worker: reboot ok; late return; PDB block + `reboot-status.blockedBy`; force; cancel (`requested` and `rebooting`); drain timeout; nsenter failure (e.g. broken image); leader takeover mid-drain (rolling update); CP reboot last; bricked node (power off: verify visibility + DELETE escape); host hang during shutdown (its pod dies, so the boot-ID check cannot fire — the node must stay `rebooting` until Ready, not fail after `--reboot-issue-grace`; escape = DELETE); no-effect reboot (boot ID unchanged ⇒ `failed` after `--reboot-issue-grace`); queued node goes `NotReady` (queue holds, then resumes); node deleted mid-plan (slot freed, plan continues); 401 with an invalid API token (verifies the deployment-supplied token is injected and enforced); an unmanaged pod (no owner) on a target node without `force` (drain must block on it until `--reboot-drain-timeout` → `failed` + uncordon — and with `force` the pod is deleted and the drain completes); a duplicate executor (DaemonSet rollout while a node is `rebooting`: the new pod must not re-issue — `attempt=2` in `reboot-exec`); corrupt a `reboot-status.cordonedPrev` value by hand on a `failed` node (uncordon must be **skipped**: cordon preserved, Event logged, node stays `failed` — explicit `kubectl uncordon` repairs it); batch `*` including a `NotReady` node (partial 202: `Ready` nodes accepted, the `NotReady` one rejected with 422); API outage mid-drain (`--max-concurrent-reboots=2`, worker draining + CP rebooting, outage **shorter** than `--reboot-drain-timeout`: nothing marked `failed` while down, leader handover on recovery, drain resumes and completes); a 2-node PDB case (worker A PDB-blocked, worker B proceeds — skip rule); DELETE on a `draining` node → 409; `workers before CP` queue tie-break (batch with 2 workers + 1 CP: workers admitted first, CP waits); explicit pause→resume (`failed` node pauses the queue; `DELETE` resumes); `cordonedPrev` set and respected (a node already cordoned stays cordoned after `failed`); re-request after `failed` and after `completed`; corrupt a `reboot-state` annotation by hand (verify `parseError` in `GET /reboots` and in `GET /reboots/{node}`, slot held, Event shows both escapes + raw value, DELETE clears — uncordons when `cordonedPrev` is known absent); no-effect reboot induced with a distro image variant whose reboot shim is a no-op; API-outage variant **longer** than `--reboot-drain-timeout` (a draining node must be `failed` + uncordoned on recovery — wall-clock timeout). M8 is a multi-day campaign on the production cluster; order cases from cheap to disruptive. |
| M9 | Docs | final README: operation guide (scheduling reboots, states, pause/resume, troubleshooting), local-storage-wipe warning, CP risk, plain-HTTP API note; JSON-annotation inspection: `jsonpath` one-liners for `state`/`since`/`force`, the exact repair `kubectl annotate` command, and the DELETE endpoint; hand-repair caveats (deleting `reboot-status` loses `cordonedPrev` — repair fields, not whole annotations) and the drain-stuck escape (wait out the timeout, or clear all four keys + `kubectl uncordon`). |

Note: M4 is only operationally meaningful once M5 exists (the PDB check
gates the drain); validation order reflects that.

## 6. Decisions (closed)

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
   via `kubectl describe node` (events live in the empty namespace,
   3.11); structured logs remain the detailed source.
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
8. **`--on-reboot-failure=pause|continue`** (default `pause`): queue
   behavior while any node is `failed`; resumed by the operator clearing
   the `failed` state.
9. **Reboot verification via boot ID** (`reboot-exec`:
   `issuedAt`/`bootId`/`attempt`/`executorPodUID`/`confirmedAt` +
   `--reboot-issue-grace`):
   `completed` requires `reboot-exec` plus evidence that the host
   actually rebooted (boot ID change — durable via `confirmedAt` — or a
   `NotReady` the current leader observed, per-leader in-memory);
   a `reboot` that is accepted but has no effect is `failed`, never
   `completed`. Chosen over grace-only heuristics: the boot ID is
   exact, local, and works in the single-CP case.
10. **Cap on concurrent control-plane reboots: 1, not configurable**
    (constant): independent of `--max-concurrent-reboots`, so a batch
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
    plan pauses per `--on-reboot-failure`.
15. **API read semantics**: `GET /reboots` shows the plan — every node
    with a reboot state (in-flight states plus `completed`/`failed`
    history until cleared); nodes without state are never listed, and
    `GET`/`DELETE` `/reboots/{node}` on a node without state is 404.
16. **No `/metrics` in v1**: observability is Events + structured logs
    + `GET /reboots`; a metrics endpoint can be added later without API
    changes.
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
    beyond `--reboot-issue-grace`, or an operator unbricking it), the
    plan is not resumed or completed; the operator starts a **new** plan
    or clears the history (`DELETE`). The pause-on-failure principle
    outranks self-healing bookkeeping.
19. **A PDB denial mid-drain is retried, not immediately fatal**: a 429
    from the Eviction API during the drain is retried with backoff for
    as long as `--reboot-drain-timeout` remains — a PDB's state can
    fluctuate due to activity unrelated to the draining node (the same
    rationale as `kubectl drain`'s retry loop, and consistent with the
    documented pre-check/API divergence, 3.7); only a denial that
    survives until the timeout fails the node.
