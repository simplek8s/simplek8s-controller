#!/usr/bin/env bash
# E2E: drive one node through the full reboot lifecycle via the API and
# verify each transition, the cordon/uncordon, and the final DELETE.
#
# Usage:
#   scripts/e2e-reboot.sh <node>
#
# Env:
#   BASE_URL    API base. Default: kubectl port-forward to a Ready
#               controller pod on a node OTHER than the one being
#               rebooted (re-resolved here if no forward is up; the
#               forward survives the target's reboot by construction).
#               No Service exists by design — operators reach the API
#               the same way (port-forward with their own credentials).
#   PF_PORT     local port for the port-forward (default 18080).
#   TOKEN_FILE  file with the bearer token (default /tmp/opencode/api.token)
#   KUBEARGS    extra kubectl args (e.g. "--context simplek8s")
#   TIMEOUT_S   max wall time for the reboot (default 1800)
#
# State polling reads the Node annotations via kubectl (the source of
# truth), so it keeps working while the API is unreachable. The API is
# used only for POST/DELETE and liveness.
set -u

NODE="${1:?usage: e2e-reboot.sh <node>}"
TOKEN_FILE="${TOKEN_FILE:-/tmp/opencode/api.token}"
TIMEOUT_S="${TIMEOUT_S:-1800}"
read -r -a KUBE <<< "kubectl ${KUBEARGS:-}"

TOKEN="$(cat "$TOKEN_FILE")"
AUTH="Authorization: Bearer $TOKEN"

if [ -z "${BASE_URL:-}" ]; then
  # Forward to a Running controller pod whose node is NOT the one being
  # rebooted (its pod is down while it reboots); fall back to any
  # Running pod. Dies with the script (trap on EXIT).
  pick_pod() {
    local pods entry
    pods="$("${KUBE[@]}" get pods -n simplek8s -l app=simplek8s-controller -o jsonpath='{range .items[*]}{.metadata.name} {.spec.nodeName} {.status.phase}{"\n"}{end}' 2>/dev/null)"
    entry="$(printf '%s\n' "$pods" | awk -v node="$NODE" '$3=="Running" && $2!=node {print $1; exit}')"
    [ -z "$entry" ] && entry="$(printf '%s\n' "$pods" | awk '$3=="Running" {print $1; exit}')"
    [ -n "$entry" ] && printf '%s' "$entry"
  }
  PF_PORT="${PF_PORT:-18080}"
  POD="$(pick_pod)" && [ -n "$POD" ] || { echo "FATAL: no Running controller pod for port-forward"; exit 2; }
  "${KUBE[@]}" port-forward -n simplek8s "$POD" "$PF_PORT:8080" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 30); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:${PF_PORT}/livez")" = 200 ]; then
      BASE_URL="http://127.0.0.1:${PF_PORT}"
      break
    fi
    sleep 1
  done
  [ -n "${BASE_URL:-}" ] || { echo "FATAL: port-forward to $POD never answered"; exit 2; }
  trap 'kill ${PF_PID:-} 2>/dev/null' EXIT
fi

pass=0; fail=0
ok()   { pass=$((pass+1)); echo "PASS: $*"; }
bad()  { fail=$((fail+1)); echo "FAIL: $*"; }
check(){ desc="$1"; shift; if "$@"; then ok "$desc"; else bad "$desc"; fi; }

state_of() {  # from the node annotation (works while the API is down)
  "${KUBE[@]}" get node "$NODE" -o jsonpath='{.metadata.annotations.simplek8s\.org/reboot-state}' 2>/dev/null \
    | grep -o '"state":"[a-z]*"' | head -1 | cut -d'"' -f4
}
confirmed_at() {
  "${KUBE[@]}" get node "$NODE" -o jsonpath='{.metadata.annotations.simplek8s\.org/reboot-exec}' 2>/dev/null \
    | grep -o '"confirmedAt":"[^"]*"' | head -1 | cut -d'"' -f4
}
node_ready() {
  "${KUBE[@]}" get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null
}
cordoned() {
  "${KUBE[@]}" get node "$NODE" -o jsonpath='{.spec.unschedulable}' 2>/dev/null
}
annotations() {
  "${KUBE[@]}" get node "$NODE" -o jsonpath='{.metadata.annotations}' 2>/dev/null
}
api_up() {
  [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$BASE_URL/livez")" = 200 ]
}

node_exists()    { "${KUBE[@]}" get node "$NODE" --no-headers 2>/dev/null | grep -q .; }
node_is_ready()  { [ "$(node_ready)" = "True" ]; }
not_cordoned()   { [ "$(cordoned)" != "true" ]; }
no_reboot_state(){ ! annotations | grep -q 'reboot-state'; }
flag_set()       { [ "${1:-0}" = "1" ]; }
resp_has_node()  { printf '%s' "$1" | grep -q "\"$NODE\""; }
has_confirmed()  { [ -n "$(confirmed_at)" ] && [ "$(confirmed_at)" != "null" ]; }
reboot_happened(){ [ "$saw_notready" = 1 ] || has_confirmed; }
has_event()      { printf '%s\n' "$EV" | tr ' ' '\n' | grep -qx "$1"; }

echo "== API: $BASE_URL"
check "API reachable (livez)" api_up

echo "== preflight: $NODE"
check "node exists" node_exists
check "node Ready" node_is_ready
check "no pre-existing reboot state" no_reboot_state

echo "== POST /api/v1/reboots"
RESP="$(curl -s --max-time 10 -w '\n%{http_code}' -X POST "$BASE_URL/api/v1/reboots" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"nodes\":[\"$NODE\"],\"requestedBy\":\"e2e\"}")"
CODE="${RESP##*$'\n'}"
BODY="${RESP%$'\n'*}"
check "202 accepted" test "$CODE" = 202
check "node in accepted[]" resp_has_node "$BODY"

echo "== lifecycle (timeout ${TIMEOUT_S}s)"
saw_draining=0; saw_rebooting=0; saw_notready=0; final=""
start=$SECONDS
while [ $((SECONDS - start)) -lt "$TIMEOUT_S" ]; do
  st="$(state_of)"
  ready="$(node_ready)"
  c="$(cordoned)"
  echo "  t+$((SECONDS - start))s state=${st:-absent} ready=$ready cordoned=$c"
  case "$st" in
    draining)  saw_draining=1 ;;
    rebooting) saw_rebooting=1 ;;
  esac
  if [ "$ready" = "False" ] || [ "$ready" = "Unknown" ]; then saw_notready=1; fi
  if [ "$st" = "completed" ]; then final=completed; break; fi
  if [ "$st" = "failed" ]; then final=failed; break; fi
  # 2s: the draining window on an empty node is shorter than 5s.
  sleep 2
done

check "saw draining" flag_set "$saw_draining"
check "saw rebooting" flag_set "$saw_rebooting"
check "reboot happened (NotReady observed or confirmedAt set)" reboot_happened
check "reached completed" test "$final" = completed

echo "== post-completion: uncordon"
sleep 10
check "uncordoned" not_cordoned
check "node Ready again" node_is_ready

echo "== events"
EV="$("${KUBE[@]}" -n default get events --field-selector "involvedObject.name=$NODE" -o jsonpath='{range .items[*]}{.reason} {.end}' 2>/dev/null)"
for reason in RebootDraining RebootIssued RebootCommandIssued RebootCompleted; do
  check "event $reason" has_event "$reason"
done

echo "== waiting for the API to come back"
i=0
while ! api_up && [ $i -lt 12 ]; do sleep 5; i=$((i+1)); done
check "API reachable again" api_up

echo "== DELETE /api/v1/reboots/$NODE"
CODE="$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' -X DELETE -H "$AUTH" "$BASE_URL/api/v1/reboots/$NODE")"
check "204 no content" test "$CODE" = 204
sleep 5
check "annotations cleared" no_reboot_state
check "node still Ready" node_is_ready

echo
echo "== result: $pass passed, $fail failed"
[ "$fail" = 0 ]
