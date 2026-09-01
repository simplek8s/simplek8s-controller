# PLAN — simplek8s-controller

Phase: **PLAN** (design & planning). Status: draft v2, pending review.

## 1. Purpose

A node-management controller for a self-built Kubernetes cluster
(SimpleK8s distro: Buildroot, systemd, containerd, kubeadm). It runs as a
privileged
DaemonSet with one instance per node and provides operational
capabilities over the hosts.

**First feature (v1): scheduled node reboots**, with:

1. An internal HTTP REST API to schedule reboots for selected nodes.
2. A configurable limit on concurrently rebooting nodes (default: **1**).
3. Wait until a node is available again before starting the next reboot.
4. Do not reboot a node if a PodDisruptionBudget would be violated,
   unless the request passes `force`.
5. Control-plane nodes are also rebootable.

The controller is intended to grow: reboot scheduling is only the first
capability. The layout (feature packages, engine, API groups) must stay
extensible (see 3.7).

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

### 3.1 Deployment model: DaemonSet

- One `simplek8s-controller` pod per node.
- Pod is `privileged: true` + `hostPID: true`, so it can `nsenter` into
  the host's PID 1 (systemd) and issue the actual `reboot`.
- `nsenter` only reaches the local host ⇒ **each instance drives the
  reboot of its own node only**. Global coordination (queue, concurrency
  limit) happens through objects in the API server (node annotations),
  visible with `kubectl`.
- When a control-plane node reboots, the local instance dies and comes
  back with the DaemonSet; the state machine must be **fully
  recoverable from API-server state alone** (see 3.4).
- **Single-control-plane outage behavior**: while the only CP is
  rebooting, the API server is down. Non-CP instances simply skip engine
  cycles (one log line per outage; no state writes, nothing marked
  `failed`) and the API returns 503. The CP's own instance completes
  `rebooting → completed` on its first cycle after the pod restarts and
  the API answers again. If the CP comes back after the reappearance
  timeout, the local instance marks it `failed` instead.

### 3.2 Kubernetes access: minimal REST client (`internal/kube`)

Stdlib `net/http` + `crypto/tls` + `encoding/json`. Required verbs
(v1 scope):

| Verb | Where | Notes |
|---|---|---|
| GET | `nodes`, `pods` (all ns), `poddisruptionbudgets`, `healthz` | Pagination via `limit` + `continue`; retry on 429/5xx |
| PATCH | `nodes/{name}` | `application/merge-patch+json` with `metadata.resourceVersion` for optimistic concurrency |
| CREATE (POST) | `namespaces/{ns}/pods/{pod}/eviction` | `policy/v1` Eviction body |
| DELETE | `pods/{pod}` | Fallback for `force` drain |

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
  nodeName, ownerReferences, labels, deletionTimestamp, local ephemeral
  storage), `PodDisruptionBudget`, list envelopes with
  `resourceVersion`/`continue`.
- Small retry wrapper: exponential backoff for 429/503, honor
  `Retry-After`; context deadlines everywhere.

### 3.3 Reboot state: annotations on `Node`

No CRDs (MVP, stdlib-only). State per node in `Node` annotations:

| Annotation | Format | Meaning |
|---|---|---|
| `simplek8s.dev/reboot-state` | `requested` \| `draining` \| `rebooting` \| `completed` \| `failed` | Current lifecycle state. A **string enum, not a timestamp**; its presence means the node participates in the reboot lifecycle. |
| `simplek8s.dev/reboot-request-id` | `<RFC3339>-<6 random>` | Correlates one API request with its execution |
| `simplek8s.dev/reboot-requested-at` | RFC3339 | When the request was admitted (queue ordering) |
| `simplek8s.dev/reboot-state-since` | RFC3339 | When the node entered its current state (drives timeouts) |
| `simplek8s.dev/reboot-force` | `true`/`false` | Request bypasses PDB checks |
| `simplek8s.dev/reboot-cordoned-prev` | `true` | Node was already cordoned before we started (do not uncordon on completion) |
| `simplek8s.dev/reboot-blocked-by` | comma-separated `ns/pdb` | PDBs currently blocking the node (only while `requested` and blocked) |
| `simplek8s.dev/reboot-error` | text | Last error (state `failed`) |

- `reboot-state` is the only state marker; the rest are attributes of the
  current request/state.
- Queue = nodes in `requested`, ordered by `reboot-requested-at`; ties
  broken by **workers before control planes**, then node name.
- In-flight = nodes in `draining` or `rebooting`.
- **Writer rule**: only the local instance of a node mutates that node
  (its transitions); any instance may perform absent→`requested` (API) and
  cancel a `requested` node. All patches are atomic merge patches
  including `resourceVersion`; losers of a race re-read.
- `completed`/`failed` remain as history until the next request for the
  same node. Cleanup is out of scope for v1.

### 3.4 State machine & ownership (per node)

```
              POST /reboots
                    │
                    ▼
             ┌────────────┐  (slot free, PDB OK)
             │ requested  │──────────────────────────┐
             └────────────┘  DELETE (cancel)         │
                  │                                  ▼
             (cancelled:             ┌──────────┐  drain OK  ┌───────────┐
              annotations removed)   │ draining │──────────▶ │ rebooting │
                                     └──────────┘            └───────────┘
                                          │   (timeout/err)      │
                                          ▼                      ▼
                                       ┌────────┐     node Ready after the
                                       │ failed │     reboot (NotReady seen
                                          ▲       │     in between)  ┌───────────┐
                                          └──────────┴─────────────▶ │ completed │
                                         (reappear timeout)          └───────────┘
```

Rules:

- **Writer rule** (restated): every *other* instance is a pure observer.
  In particular, timeout/`failed` evaluation for a node is done only by
  its local instance; a remote instance can never mark a foreign node
  `failed` (this is also what keeps the cluster consistent while the only
  CP is down — see 3.1).
- **Ownership via Lease** (per-node lock, `coordination.k8s.io/v1`):
  - *What it is*: a small cluster object `simplek8s-reboot-<node>` (in
    the controller's namespace) with two fields that matter:
    `spec.holderIdentity` (who holds the lock) and `spec.renewTime`
    (last heartbeat). Same primitive Kubernetes uses for leader election.
  - *How it is used*: before driving its node, the local pod **acquires**
    the Lease (creates it if absent, or takes it if stale) and **renews**
    it every 10 s while the node is `draining`/`rebooting`.
    `holderIdentity` = `<pod-name>/<pod-uid>`.
  - *Why it exists*: annotations say *what state* a node is in, but not
    *whether someone is actively driving it*. After a crash (pod or node
    restart) the new pod on that node sees the previous holder with a
    stale `renewTime` (> 30 s) and takes over deterministically instead
    of guessing. It also makes ownership visible: `kubectl get lease
    simplek8s-reboot-<node>` shows who is driving and when it last
    beat.
  - *Rule*: holding the Lease is a precondition for any transition on
    that node; a pod that finds a fresh Lease held by another identity
    backs off. The node annotations remain the source of truth for
    state; the Lease only arbitrates *who may write*.
- Slot acquisition: the local instance of a `requested` node, while the
  global in-flight count < `--max-concurrent-reboots`, acquires the Lease
  and patches the node to `draining`. Non-local instances never advance
  a node (nsenter is local).
- PDB check fails → back to `requested`, retried on a later engine cycle
  (logged); user can cancel via API.
- `completed`: node is `Ready` **after** the reboot. Detection:
  `Ready=True` and (we observed `NotReady` since `reboot-since` OR the
  Ready condition `lastTransitionTime` is later than `reboot-since`).
  On completion: uncordon the node if we cordoned it.
- Timeouts: `--reboot-drain-timeout` (default 10 m) and
  `--reboot-timeout` (default 15 m, reappearance). Exceeding either →
  `failed`, and the node is uncordoned if we cordoned it (the node may be
  partially drained; operator intervention expected, documented).
- Crash recovery: after any restart (controller or node), the engine
  re-reads annotations and resumes: a node stuck in `draining`/`rebooting`
  past its timeout is marked `failed`; a `rebooting` node that is Ready
  again is marked `completed`.

### 3.5 Reboot flow (local instance of the target node)

1. **Admission** (API, any instance): validate nodes exist, patch
   absent→`requested` (409 if an active request already exists).
2. **Slot acquisition** (engine, ~2 s loop): see 3.4.
3. **PDB check** (skipped if `force`): see 3.6. It runs *before* cording:
   a blocked node stays `requested`, records the blocking PDBs in
   `reboot-blocked-by`, and is **never cordoned**.
4. **Drain** (only after the PDB check passed):
   - Cordon (`spec.unschedulable=true`), remembering the previous state.
   - Evict pods via the **Eviction API** (`policy/v1`), skipping
     daemonset-managed and already-terminating pods; pods using local
     ephemeral storage require the eviction annotation
     (`forceDeleteLocalData`). The Eviction API itself respects PDBs; a
     PDB-denied eviction without `force` aborts the drain → `failed`.
   - With `force`: after eviction attempts, `DELETE` remaining
     evictable pods (aggressive; documented).
   - Wait until no evictable pods remain on the node
     (timeout `--reboot-drain-timeout`).
   - **Control-plane nodes**: static pods (kube-apiserver, etcd,
     controller-manager, scheduler) are not evicted; the kubelet
     recreates them after reboot. Only user/addon pods are drained.
5. **Reboot**: patch to `rebooting`, then
   `nsenter -t 1 -m -u -i -n -p -- reboot`. The local process dies with
   the host — expected.
6. **Wait for reappearance**: see 3.4. The DaemonSet restarts the pod;
   the engine completes the transition after restart.

### 3.6 PDB evaluation per node

For target node N, over every PDB in the cluster (pod label selector):

1. Select pods cluster-wide matching the PDB's selector (phase
   Running, not terminating).
2. `disruptableOnN` = selected pods running on N that the drain would
   remove (excluding daemonset pods).
3. `available` = selected available pods (per PDB definition: running and
   ready) minus disruptions in progress.
4. Blocked if `|disruptableOnN| > 0` and
   `available − |disruptableOnN| < minAvailable` (or the PDB disallows
   disruption of incomplete pods and N hosts incomplete selected pods).
5. `force=true` → skip the check; the Eviction API still denies protected
   evictions, and `force` drain falls back to direct pod deletion
   (documented as aggressive).

MVP: direct computation (list pods + list PDBs, in-memory). Fine for
small clusters.

**"All nodes blocked" scenario — the cluster cannot wedge:**

- The PDB check happens *before* cording, so a blocked node is never
  cordoned and nothing on it changes. Worst case: the node simply stays
  `requested` until the operator acts. The cluster keeps running
  normally.
- At most `--max-concurrent-reboots` nodes are cordoned at any time —
  exactly the ones actively draining/rebooting.
- Visibility: `reboot-blocked-by` annotation and `GET /reboots` show
  exactly which PDBs block which node.
- Escapes: fix the workload/PDB (scale up, lower `minAvailable`),
  `DELETE` the request, or re-issue with `force` (bypasses the check).
- A drain that started with a passing check but later meets a PDB denial
  from the Eviction API aborts to `failed` **and uncordons** (3.4), so a
  node never stays cordoned without cause.

### 3.7 Extensibility

The controller is a platform for node-management features. Structure:

```
cmd/simplek8s-controller/   wiring, flags, lifecycle
internal/kube/              minimal k8s REST client + types
internal/nodestate/         annotation read/write, state types
internal/engine/            generic loop: registered feature tasks
internal/api/               HTTP server: registered route groups
internal/features/reboot/   v1 feature: drain, pdb, rebooter, task, routes
deploy/                     kustomize (SA, RBAC, DaemonSet)
```

- Each feature = one package registering (a) an engine task and
  (b) API routes under `/api/v1/<feature>/...`. Adding feature #2 must
  not touch the reboot feature.
- The kube client is feature-agnostic; new verbs are added as methods.

### 3.8 HTTP API (per instance)

| Method | Route | Description |
|---|---|---|
| POST | `/api/v1/reboots` | `{"nodes":["n1","n2"],"force":false}`; `"nodes":["*"]` = all nodes (workers queued first, CPs last). → `requested`. 202. Errors 404/409/422. |
| GET | `/api/v1/reboots` | Reboot state for all nodes. |
| GET | `/api/v1/reboots/{node}` | State of one node. |
| DELETE | `/api/v1/reboots/{node}` | Cancel if `requested` (409 otherwise). |
| GET | `/healthz` | Engine alive + API server reachable. |

- Auth: optional fixed token via env `SIMPLEK8S_API_TOKEN` →
  `Authorization: Bearer <token>`. Unset ⇒ no auth (trusted internal
  network).
- The API is callable on any instance (state is global); execution
  happens on the target node's instance.
- Access: a `Service` (NodePort, `externalTrafficPolicy: Local`) fronts
  the per-instance API. Any instance can serve admission (state is
  global); the Service routes to a running instance. Token auth required
  in production.

### 3.9 Configuration (flags / env)

| Flag | Default | Description |
|---|---|---|
| `--max-concurrent-reboots` | `1` | Globally rebooting nodes at once. |
| `--control-plane-reboots` | `allow` | `allow`\|`deny` — global policy for control-plane nodes. Checked at admission (422 when denied) and re-checked by the engine. |
| `--reboot-timeout` | `15m` | Max wait for node reappearance. |
| `--reboot-drain-timeout` | `10m` | Max drain duration. |
| `--engine-interval` | `2s` | Engine poll period. |
| `--listen` | `:8080` | API bind address. |
| `--kube-apiserver` | in-cluster env | Override API endpoint. |
| `--creds-dir` | `/var/run/secrets/kubernetes.io/serviceaccount` | Serviceaccount files. |
| env `NODE_NAME`, `POD_NAMESPACE` | downward API | Local node / namespace. |
| env `SIMPLEK8S_API_TOKEN` | — | Optional API token. |

### 3.10 RBAC (ServiceAccount `simplek8s-controller`)

- `nodes`: get, list, watch, patch, update
- `pods`: get, list, watch, delete
- `pods/eviction`: create
- `poddisruptionbudgets` (policy): get, list, watch
- `leases` (coordination.k8s.io): get, list, create, update
- `events`: create (one Event per state transition, see 6.4)

### 3.11 DaemonSet pod spec (sketch)

```yaml
containers:
- name: controller
  image: ghcr.io/simplek8s/simplek8s-controller:v1
  securityContext:
    privileged: true
  hostPID: true
  env: [NODE_NAME (downward API), POD_NAMESPACE (downward API)]
  ports: [{name: api, containerPort: 8080}]
  livenessProbe: httpGet /healthz
serviceAccountName: simplek8s-controller
```

## 4. Risks & safety notes (production cluster)

1. **Control-plane reboot**: control-plane nodes are rebootable by
   default (`--control-plane-reboots=allow`; set `deny` to lock CPs out).
   With a single CP and `max-concurrent=1`, a failed recovery leaves the
   cluster down; while the CP is down the other instances idle safely
   (3.1). Mitigation: validate on a worker first; keep default
   concurrency at 1; document the risk prominently.
2. **Incomplete drains**: pods with local ephemeral storage or
   eviction-disallowing behaviors ⇒ drain times out, the node is **not**
   rebooted, state `failed`, unless `force`.
3. **False "back" detection**: node may stay Ready through a fast reboot;
   require NotReady-in-between or a newer Ready `lastTransitionTime`.
4. **Idempotency / crash recovery**: full state in annotations; queue and
   in-flight survive controller (or node) restarts.
5. **Concurrent writers**: atomic merge patches + resourceVersion; a node
   is mutated only by its local instance; the API only enters the
   `requested` state.
6. **Privileged pod**: by design (nsenter). RBAC scoped to the
   ServiceAccount; API token in production.

## 5. Milestones

| # | Milestone | Deliverable |
|---|---|---|
| M0 | Scaffold | git, go.mod (stdlib only), package layout, Makefile, kustomize skeleton (SA+RBAC+DaemonSet+Service), README |
| M1 | `internal/kube` | REST client (auth, TLS, retries, pagination), minimal types, unit tests (httptest) |
| M2 | `internal/nodestate` | annotation read/write, state types, conflict handling, tests |
| M3 | Engine | loop, task registration, queue, concurrency, state machine (fake rebooter in tests) |
| M4 | Drain | cordon + evictions + wait, timeouts, force fallback |
| M5 | PDB | per-node evaluation + force bypass |
| M6 | Rebooter | nsenter, post-reboot Ready detection, crash-recovery paths |
| M7 | HTTP API | endpoints, optional token auth, healthz |
| M8 | Deploy & validate | image build, deploy, validate on one worker node (reboot ok, PDB block, force, cancel, timeout, crash recovery); then control-plane path |
| M9 | Docs | final README: operation guide (scheduling reboots, states, troubleshooting) |

## 6. Decisions (closed)

1. Control-plane nodes are rebootable **by default**; the policy is
   selected by a **deployment** flag `--control-plane-reboots=allow|deny`
   (default `allow`), not per request — reboot policy is an operational
   decision, and per-request flags are a footgun in production.
2. Images are hosted on **GitHub Container Registry**
   (`ghcr.io/simplek8s/simplek8s-controller`).
3. The API is exposed through a **Service** (NodePort,
   `externalTrafficPolicy: Local`) instead of per-node host binding.
4. **Events in v1 (recommended, included)**: one Event per state
   transition. Cost is low (single `create` verb) and it gives an
   operator-visible audit trail (`kubectl describe node`); structured
   logs remain the detailed source. Easy to drop if unwanted.
