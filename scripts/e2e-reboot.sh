#!/usr/bin/env bash
# E2E: drive one node through the full reboot lifecycle via the API and
# verify each transition, the cordon/uncordon, and the final DELETE.
#
# Usage:
#   scripts/e2e-reboot.sh <node>
#
# Env:
#   BASE_URL    API base. Default: auto-detected NodePort of any cluster
#               node (robust: survives the reboot of the node hosting the
#               pod you were port-forwarded to).
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
  # Pick a healthy NodePort on a node that is NOT the one being rebooted
  # (its NodePort is down while it reboots). Some clusters have CNI IP
  # collisions that make specific nodes' NodePort flaky, so verify each
  # candidate with /livez and take the first that answers 200.
  NP="$("${KUBE[@]}" get svc simplek8s-controller -n simplek8s -o jsonpath='{.spec.ports[0].nodePort}')"
  TARGET_IP="$("${KUBE[@]}" get node "$NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
  for NIP in $("${KUBE[@]}" get node -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address} {end}'); do
    [ "$NIP" = "$TARGET_IP" ] && continue
    if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://${NIP}:${NP}/livez")" = 200 ]; then
      BASE_URL="http://${NIP}:${NP}"
      break
    fi
  done
  [ -n "${BASE_URL:-}" ] || { echo "FATAL: no healthy NodePort found"; exit 2; }
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
