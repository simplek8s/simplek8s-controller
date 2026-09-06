# E2E campaign (M8)

Live validation on a real cluster. Order: cheap → disruptive. The
happy-path case is automated: `scripts/e2e-reboot.sh <node>` (env:
`BASE_URL`, `TOKEN_FILE`, `KUBEARGS`, `TIMEOUT_S`).

Conventions: `$API` = `http://127.0.0.1:1880` (or NodePort),
`-H "Authorization: Bearer $TOKEN"` abbreviated as `-H $AUTH`.
"Worker" = any non-CP node; keep the CP node for the last phases.

Prereq: the cluster needs a CNI with real pod networking (cross-node
pod-to-pod + NodePort). The distro's flat-bridge CNI shares one L2 segment
with no routing, so pod IPs collide and NodePort fails. Calico
(v3.30.1) is installed on the test cluster (cp1/wk1/wk2, podCIDR
10.244.0.0/24, .1.0/24, .2.0/24). Events for nodes live in the
`default` namespace (see PLAN.FIXME entry 5).

## Progress

| Case | Result | Date | Notes |
|---|---|---|---|
| M1-regression | PASS | 2026-09-06 | config-driven binary `7bf8c43` deployed to all 3 nodes (ctr import, nodeSelector rollout); live ConfigMap checks: invalid value → `WARN feature config ... keeping previous` (last-valid-wins), CM deleted → pods stay Running on built-in defaults, CM restored → 3/3; re-verified A1 (401s + livez 200), B1 (`scripts/e2e-reboot.sh wk1` 20/20), C1 (PDB `maxUnavailable:0` → `requested` + `blockedBy=[default/pdb-target]` + `PDBBlocked` event, DELETE 204) |
| B1 | PASS | 2026-09-06 | `scripts/e2e-reboot.sh wk1`, 20/20 checks, ~20 s reboot. First run had 5 spurious failures: port-forward died with the node (fixed: NodePort on a healthy node), polling through the API (fixed: node annotations), 5 s polling missed the short `draining` window (fixed: 2 s), and the 4 lifecycle events were never created (fixed: events must live in `default`, PLAN.FIXME #5). |
| A1 | PASS | 2026-09-06 | bad token → 401 `{"code":401,"reason":"Unauthorized"}` |
| A2 | PASS | 2026-09-06 | livez 200, readyz 200 |
| A3 | PASS | 2026-09-06 | empty list → `[]` (bare JSON array, see table note) |
| A4 | PASS | 2026-09-06 | unknown node → 202, `rejected:[{node:nope,code:404,reason:node not found}]` |
| A5 | PASS | 2026-09-06 | 2nd POST → 202, `rejected:[{node:wk2,code:409,reason:"state is not re-requestable: requested"}]` |
| A6 | PASS | 2026-09-06 | POST+DELETE in 0.14 s → 204, all four annotations gone, `spec.unschedulable` untouched, node Ready, no events emitted |
| C1 | PASS | 2026-09-06 | pause Deployment (1 rep, pinned wk1) + PDB `maxUnavailable:0`; POST wk1 → stays `requested`, `reboot-status.blockedBy=["default/pdb-target"]`, Warning event `PDBBlocked`; pod untouched |
| C2 | PASS | 2026-09-06 | with wk1 PDB-blocked, POST wk2 → wk2 full lifecycle to `completed`; wk1 still `requested` (skip rule) |
| C3 | PASS | 2026-09-06 | `kubectl delete pdb` → wk1 drain proceeds next cycle, `completed` ~27 s later; `reboot-status` cleared |
| C5 | PASS | 2026-09-06 | unmanaged pod on wk1, `POST {"force":true}` → pod deleted, drain completes, wk1 `completed` |
| C6 | PASS | 2026-09-06 | PDB on wk2, `POST {"force":true}` → eviction rejected by PDB, force-DELETE removes the pod, drain completes, wk2 `completed` (Deployment recreated the pod after the reboot) |
| A7 | PASS | 2026-09-06 | `POST [wk1,wk2]` → strict serialization: wk1 `completed` first, wk2 admitted only after (slot=1) |
| B3 | PASS | 2026-09-06 | re-POST after `completed` → accepted (completed is re-requestable), full lifecycle again |
| C4 | PASS | 2026-09-06 | unmanaged pod on wk1, `force:false` → drain skipped the pod, hit `reboots.reboot-drain-timeout` exactly (10m0s) → `failed` + uncordon + `RebootFailed` event; node back to Ready |
| D1 | PASS | 2026-09-06 | leader pod deleted mid-drain → standby took over via lease; drain finished and node reached `completed` |
| D2 | PASS | 2026-09-06 | DS rollout (pod restart) while node `rebooting` → no re-issue: `attempt` stayed 1, no second reboot |
| D3 | PASS | 2026-09-06 | DELETE while `draining` → 409 (in-flight drain not interruptible); `failed`/`completed` DELETE → 204 |
| D4 | PASS | 2026-09-06 | queued node stopped (kubelet down) → `QueueHeldNotReady` event, held while NotReady, admitted + `completed` when it returned |
| D5 | PASS* | 2026-09-06 | `kubectl delete node wk1` while wk1 draining: slot freed, wk2 admitted and `completed` (plan continues). `NodeDisappeared` event MISSED: the leader pod ran on wk2 and died with the host reboot in the same window, dropping the in-memory `prevInflight` (see PLAN.FIXME #6). Escape: kubelet 1.37 on this distro does NOT re-register a runtime-deleted Node object — `systemctl restart kubelet` on wk1 restored it. |
| D6 | PASS | 2026-09-06 | corrupt `reboot-state` (bad JSON) → `parseError` (API 200, no 500), slot held, `CorruptRebootState` event; DELETE cleared it, node re-requestable |
| D7 | PASS | 2026-09-06 | corrupt `cordonedPrev` (wrong type) on `completed` → uncordon skipped (idempotency guard), `UncordonBlocked` event, cordon preserved; fixed value → clean uncordon |
| D8 | PASS | 2026-09-06 | pre-cordoned node (`kubectl cordon`) → `completed` → still cordoned (operator cordon preserved) |
| D9 | PASS | 2026-09-06 | `onRebootFailure=pause`: `failed` node paused the queue; DELETE of the failed node → queue resumed, next node admitted |
| D10 | PASS | 2026-09-06 | variant image with no-op `nsenter` (exit 0) → command "succeeded", boot ID unchanged → `failed` at exactly 5m0s: `reboot did not take effect: host boot ID unchanged 5m0s after issuedAt` |
| D11 | PASS | 2026-09-06 | variant image without `nsenter` → `failed` seconds after issue: `reboot command failed to start: exec: "nsenter": executable file not found in $PATH`, uncordoned, `RebootFailed` event |
| D13 | PASS | 2026-09-06 | `POST *` with wk2 NotReady (kubelet stopped) → 202 partial: Ready nodes accepted, `{node:wk2,code:422,reason:"node not Ready"}` rejected |
| A8 | PASS | 2026-09-06 | `POST [wk1,cp1]` (max-concurrent=1): wk1 `rebooting` while cp1 stayed `requested` (workers-before-CP tiebreak + CP gate); cp1 admitted only after wk1 `completed` |
| D17 | PASS | 2026-09-06 | same campaign: cp1 admitted alone (nothing else in-flight), rebooted, API down ~1 min, node returned → `completed`; everything resumed from annotations |
| D12 | PASS | 2026-09-06 | `virsh destroy` (hard power-off, no reboot) 1 s after issue: node NotReady, stays `rebooting` past the 5 m grace (no executor → no boot-ID check → **no auto-fail**); queued node stayed `requested` (slot held); DELETE cleared annotations on the bricked node; `virsh start` → Ready. First attempt void: the Buildroot guest reboots in ~17 s, completing before a late power-off lands. |
| D16 | PASS | 2026-09-06 | `virsh suspend` (freeze) in the same second as issue, held 6.5 min (past grace): node stayed `rebooting`, **not** `failed` (executor pod frozen with the host); `virsh resume` → node returned 6m26s after issuedAt → `completed` |
| B2 | PASS | 2026-09-06 | same campaign: late return (>30 s after issuedAt) → `completed` via the NotReady-after-issuedAt evidence path (no `confirmedAt` in reboot-exec: the local pod never confirmed, exactly as designed) |
| D14 | PASS | 2026-09-06 | `reboots.max-concurrent-reboots: "2"`: `POST [wk1,cp1]` → BOTH `rebooting` concurrently; cp1 reboot took the API down ~1 min (< drain-timeout); on recovery both `completed`, nothing `failed`; leader handover on recovery |
| D15 | PASS | 2026-09-06 | unmanaged pod on wk1 (drain pending); CP apiserver is a static pod on this distro — outage by `mv`-ing its manifest off for 13 min (> drain-timeout); `failed` at 05:46:33, i.e. **on recovery**, not at the 05:43:18 deadline while API was down (wall-clock); `drain timed out after 10m0s`, uncordoned next cycle |

## Phase A — API & admission (no reboot)

| # | Case | Trigger | Expect |
|---|---|---|---|
| A1 | invalid token | `curl -H "Authorization: Bearer wrong" $API/api/v1/reboots` | 401 |
| A2 | liveness/readiness | `curl $API/livez $API/readyz` | 200 / 200 |
| A3 | empty list | `curl -H $AUTH $API/api/v1/reboots` | `[]` (bare JSON array of plan entries) |
| A4 | unknown node | `POST -d '{"nodes":["nope"]}'` | 202, rejected `[{node:nope, code:404}]` |
| A5 | re-request while in flight | POST twice for the same worker | 2nd: 202 rejected 409 (not re-requestable) |
| A6 | cancel `requested` | POST, then `DELETE /reboots/<node>` | 204; all four annotations gone; cordon untouched |
| A7 | batch queueing | `POST -d '{"nodes":["w1","w2"]}'` (max-concurrent=1) | both accepted; w1 admitted first (oldest `since`), w2 stays `requested` until w1 completes |
| A8 | workers before CP | `POST -d '{"nodes":["w1","cp"]}'` | w1 `draining` while cp stays `requested` |

## Phase B — happy path

| # | Case | Trigger | Expect |
|---|---|---|---|
| B1 | full worker reboot | `scripts/e2e-reboot.sh <worker>` | requested→draining(cordon)→rebooting→completed→uncordon; events RebootDraining/RebootIssued/RebootCommandIssued/RebootCompleted; DELETE 204; annotations cleared |
| B2 | late return | B1, but let the node return > 30 s after `issuedAt` | still `completed` (NotReady-after-issuedAt evidence), not `failed` |
| B3 | re-request after `completed` | POST again on the same node | accepted (re-requestable) |

## Phase C — PDB, unmanaged, force

| # | Case | Trigger | Expect |
|---|---|---|---|
| C1 | PDB block | Deployment + PDB (`maxUnavailable:0`) pinned to the worker; POST | stays `requested`; `reboot-status.blockedBy` set; `PDBBlocked` event; queue continues past it |
| C2 | 2-node PDB skip | C1 on w1 + POST w2 | w1 blocked, w2 drains/completes (skip rule) |
| C3 | PDB unblock | delete the PDB (or scale dep to 0) | drain proceeds on next cycle |
| C4 | unmanaged pod, no force | `kubectl run orphan --rm=false ...` on the worker (no owner); POST | drain blocks; at `reboots.reboot-drain-timeout` → `failed` + uncordon + `error` set; queue pauses (pause mode) |
| C5 | unmanaged pod, force | same, `POST -d '{"force":true}'` | pod deleted, drain completes, reboot proceeds |
| C6 | force bypasses PDB | C1 setup, `POST -d '{"force":true}'` | drain proceeds despite PDB |

## Phase D — fault injection (disruptive)

| # | Case | Trigger | Expect |
|---|---|---|---|
| D1 | leader takeover mid-drain | while w1 `draining`: `kubectl -n simplek8s delete pod <leader-pod>` | new leader takes over (~30 s); drain continues; no double transitions |
| D2 | duplicate executor | while node `rebooting`: roll the DaemonSet (new pod on the node) | new pod does not re-issue beyond the bound; `attempt` never exceeds the single 1→2 re-issue; one reboot only |
| D3 | DELETE on `draining` | `DELETE /reboots/<draining-node>` | 409 (not interruptible) |
| D4 | queued node NotReady | POST w1,w2; make w2 NotReady before its turn | `QueueHeldNotReady` event; resumes when w2 is Ready again |
| D5 | node deleted mid-plan | while w1 `draining`: `kubectl delete node w1` | `NodeDisappeared` event; slot freed; plan continues with the rest |
| D6 | corrupt `reboot-state` | `kubectl annotate <node> simplek8s.org/reboot-state='{"state":"bogus' --overwrite` (on any state) | GET shows `parseError`; slot held; `CorruptRebootState` event with both escapes + raw value; DELETE clears, no auto-uncordon |
| D7 | corrupt `cordonedPrev` | on a `failed` cordoned node: set `reboot-status='{"cordonedPrev":"yes"}'` (wrong type) | uncordon **skipped** (cordon preserved), `UncordonBlocked` event; `kubectl uncordon` repairs |
| D8 | `cordonedPrev` respected | `kubectl cordon <node>` first, then reboot to `failed`/`completed` | node stays cordoned (operator's cordon) |
| D9 | pause → resume | let a node reach `failed` (e.g. C4); POST another node | `QueuePaused` event, new node stays `requested`; DELETE the failed node → queue resumes |
| D10 | no-effect reboot | image variant whose reboot shim is a no-op (or patch the container command) | boot ID unchanged; `failed` after `reboots.reboot-issue-grace` ("reboot did not take effect") |
| D11 | nsenter failure | image without `nsenter` (e.g. scratch-based) | `RebootFailed` command-failure path, node `failed`, uncordoned |
| D12 | bricked node | POST a worker, then power off the VM (do not reboot) | stays `rebooting`; NotReady; queue holds the slot; no auto-assumption; DELETE clears |
| D13 | batch `*` with NotReady | make one node NotReady; `POST -d '{"nodes":["*"]}'` | partial 202: Ready nodes accepted, the NotReady one rejected 422 |
| D14 | API outage mid-drain (short) | `reboots.max-concurrent-reboots: "2"`, w1 draining + cp rebooting; stop apiserver < drain-timeout | nothing marked `failed` while down; leader handover on recovery; drain resumes and completes |
| D15 | API outage > drain-timeout | same, outage longer than `reboots.reboot-drain-timeout` | draining node `failed` + uncordoned on recovery (wall-clock) |
| D16 | host hang during shutdown | hang the host's shutdown (e.g. qemu pause at reboot) | its pod dies, so the boot-ID check cannot fire: node stays `rebooting` until Ready (does **not** fail after the grace); escape = DELETE |
| D17 | CP reboot last | batch including the CP node | CP admitted only when nothing else in-flight; single-CP: API down until the node returns, then everything resumes from annotations |

## Notes

- B1 first, always: it is the reference for every later case.
- D14/D15/D16 need host-level access (VM control) — schedule them last.
- VM control on the test host: `sudo virsh` (sk8s-cp1/sk8s-wk1/sk8s-wk2).
  The guests are Buildroot: systemd (kubelet/containerd) + runit (calico
  daemons); the CP components (apiserver, etcd, scheduler, CM) are static
  pods managed by the kubelet — there is no `kube-apiserver` systemd unit.
  To stop the API: move `/etc/kubernetes/manifests/kube-apiserver.yaml`.
  Guests reboot in ~17 s; a power-off for D12 must land within ~10 s of
  the reboot issue.
- Every case must end with: node Ready, annotations cleared or in a
  well-defined state, queue unblocked, and a `kubectl -n simplek8s
  get events` scan for unexpected reasons.
