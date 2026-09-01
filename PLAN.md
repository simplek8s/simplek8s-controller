# PLAN — simplek8s-controller

Phase: **PLAN** (design & planning). Status: draft v2, pending review.

## 1. Purpose

A node-management controller for a self-built Kubernetes cluster
(custom Buildroot-based distro, kubeadm). It runs as a privileged
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
- No `watch` in v1 — the engine polls (default 2 s). Watch support can be
  added later if needed.
- Hand-written minimal types (only the fields we use): `Node`
  (spec.unschedulable, status.conditions, metadata), `Pod` (phase,
  nodeName, ownerReferences, labels, deletionTimestamp, local ephemeral
  storage), `PodDisruptionBudget`, list envelopes with
  `resourceVersion`/`continue`.
- Small retry wrapper: exponential backoff for 429/503, honor
  `Retry-After`; context deadlines everywhere.

### 3.3 Reboot state: annotations on `Node`

No CRDs (MVP, stdlib-only). State per node in `Node` annotations:

| Annotation | Values / format |
|---|---|
| `simplek8s.dev/reboot` | `requested` \| `draining` \| `rebooting` \| `completed` \| `failed` |
| `simplek8s.dev/reboot-request-id` | id to correlate request ↔ execution |
| `simplek8s.dev/reboot-since` | RFC3339, time entered current state |
| `simplek8s.dev/reboot-force` | `true`/`false` |
| `simplek8s.dev/reboot-cordoned-prev` | `true` if node was cordoned before (do not uncordon on completion) |
| `simplek8s.dev/reboot-error` | error message (state `failed`) |

- Queue = nodes in `requested`, ordered by `reboot-since`.
- In-flight = nodes in `draining` or `rebooting`.
- Transitions are atomic merge patches including `resourceVersion`;
  losers of a race retry and re-read. A node is only mutated by its own
  local instance; the API only performs absent→`requested`.
- `completed`/`failed` remain as history until the next request for the
  same node. Cleanup is out of scope for v1.

### 3.4 State machine (per node)

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
                                       │ failed │     reboot (NotRequired seen
                                          ▲       │     in between)  ┌───────────┐
                                          └──────────┴─────────────▶ │ completed │
                                         (reappear timeout)          └───────────┘
```

Rules:

- Only the **local instance of the target node** performs
  `draining → rebooting → completed/failed` (nsenter is local). Every
  instance observes global state to enforce the concurrency limit.
- Slot acquisition: engine sees a `requested` node while in-flight count
  < `--max-concurrent-reboots` → patches it to `draining`. With several
  instances racing, the atomic patch decides; losers observe and skip.
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
3. **PDB check** (skipped if `force`): see 3.6.
4. **Drain**:
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
| POST | `/api/v1/reboots` | `{"nodes":["n1","n2"],"force":false}` → `requested`. 202. Errors 404/409/422. |
| GET | `/api/v1/reboots` | Reboot state for all nodes. |
| GET | `/api/v1/reboots/{node}` | State of one node. |
| DELETE | `/api/v1/reboots/{node}` | Cancel if `requested` (409 otherwise). |
| GET | `/healthz` | Engine alive + API server reachable. |

- Auth: optional fixed token via env `SCK_API_TOKEN` →
  `Authorization: Bearer <token>`. Unset ⇒ no auth (trusted internal
  network).
- The API is callable on any instance (state is global); execution
  happens on the target node's instance.
- Access pattern: `hostNetwork: true` on the DaemonSet, API bound to
  `0.0.0.0:8080` (open question 3), token required in production.

### 3.9 Configuration (flags / env)

| Flag | Default | Description |
|---|---|---|
| `--max-concurrent-reboots` | `1` | Globally rebooting nodes at once. |
| `--reboot-timeout` | `15m` | Max wait for node reappearance. |
| `--reboot-drain-timeout` | `10m` | Max drain duration. |
| `--engine-interval` | `2s` | Engine poll period. |
| `--listen` | `:8080` | API bind address. |
| `--kube-apiserver` | in-cluster env | Override API endpoint. |
| `--creds-dir` | `/var/run/secrets/kubernetes.io/serviceaccount` | Serviceaccount files. |
| env `NODE_NAME`, `POD_NAMESPACE` | downward API | Local node / namespace. |
| env `SCK_API_TOKEN` | — | Optional API token. |

### 3.10 RBAC (ServiceAccount `simplek8s-controller`)

- `nodes`: get, list, watch, patch, update
- `pods`: get, list, watch, delete
- `pods/eviction`: create
- `poddisruptionbudgets` (policy): get, list, watch
- `events`: create (optional — open question 4)

### 3.11 DaemonSet pod spec (sketch)

```yaml
containers:
- name: sck
  image: <registry>/simplek8s-controller:v1   # see open question 2
  securityContext:
    privileged: true
  hostPID: true
  hostNetwork: true
  env: [NODE_NAME (downward API), POD_NAMESPACE (downward API)]
  ports: [{name: api, containerPort: 8080}]
  livenessProbe: httpGet /healthz
serviceAccountName: simplek8s-controller
```

## 4. Risks & safety notes (production cluster)

1. **Control-plane reboot**: with one control plane and
   `max-concurrent=1`, a failed recovery leaves the cluster down.
   Mitigation: `--allow-control-plane` flag (default **off** — open
   question 1); test on a worker first; document the risk.
2. **Incomplete drains**: pods with local ephemeral storage or
   eviction-disallowing behaviors ⇒ drain times out, the node is **not**
   rebooted, state `failed`, unless `force`.
3. **False "back" detection**: node may stay Ready through a fast reboot;
   require NotRequired-in-between (typo guard: "NotReady-in-between") or
   a newer Ready `lastTransitionTime`.
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
| M0 | Scaffold | git, go.mod (stdlib only), package layout, Makefile, kustomize skeleton (SA+RBAC+DaemonSet without image), README |
| M1 | `internal/kube` | REST client (auth, TLS, retries, pagination), minimal types, unit tests (httptest) |
| M2 | `internal/nodestate` | annotation read/write, state types, conflict handling, tests |
| M3 | Engine | loop, task registration, queue, concurrency, state machine (fake rebooter in tests) |
| M4 | Drain | cordon + evictions + wait, timeouts, force fallback |
| M5 | PDB | per-node evaluation + force bypass |
| M6 | Rebooter | nsenter, post-reboot Ready detection, crash-recovery paths |
| M7 | HTTP API | endpoints, optional token auth, healthz |
| M8 | Deploy & validate | image build, deploy, validate on one worker node (reboot ok, PDB block, force, cancel, timeout, crash recovery); then control-plane path |
| M9 | Docs | final README: operation guide (scheduling reboots, states, troubleshooting) |

## 6. Open questions

1. `--allow-control-plane` (default off) vs. control plane always allowed?
2. Image registry for the DaemonSet image (naming follows the `simplek8s`
   org; which registry to host it in is TBD).
3. API exposure: `hostNetwork` + bind `0.0.0.0:8080` per node vs.
   NodePort/Service per node.
4. Create `Events` for auditability in v1, or log-only?
