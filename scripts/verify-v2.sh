#!/usr/bin/env bash
# Hearth v2 end-to-end verification against the Lima lab.
# Run from the macOS host: bash scripts/verify-v2.sh [token]
# Exercises: auth, create+ip, sleep/wake (+latency), fork, both nodes, metrics.
set -u

TOKEN="${1:-hearth-lab-token}"
CP_VM=infra-saas-lab
API=http://127.0.0.1:8080
PASS=0; FAIL=0

say()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS+1)); say "  PASS: $*"; }
bad()  { FAIL=$((FAIL+1)); say "  FAIL: $*"; }

cp_curl() { # cp_curl <curl args...> — curl from inside the control-plane VM with auth
  limactl shell $CP_VM -- curl -s -m 30 -H "Authorization: Bearer $TOKEN" "$@"
}

say "== 1. auth =="
code=$(limactl shell $CP_VM -- curl -s -o /dev/null -w '%{http_code}' -m5 $API/api/v1/nodes)
[ "$code" = "401" ] && ok "request without token rejected (401)" || bad "expected 401 without token, got $code"
code=$(cp_curl -o /dev/null -w '%{http_code}' $API/api/v1/nodes)
[ "$code" = "200" ] && ok "request with token accepted" || bad "expected 200 with token, got $code"

say "== 2. nodes =="
nodes=$(cp_curl $API/api/v1/nodes)
ready=$(printf '%s' "$nodes" | grep -o '"status":"ready"' | wc -l | tr -d ' ')
[ "$ready" = "2" ] && ok "2 nodes ready" || bad "expected 2 ready nodes, got $ready: $nodes"

say "== 3. create (expects ip when net on) =="
sb=$(cp_curl -X POST $API/api/v1/sandboxes -d '{"name":"v2-verify","namespace":"verify","vcpus":1,"mem_mib":256}')
id=$(printf '%s' "$sb" | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)
state=$(printf '%s' "$sb" | grep -oE '"state":"[^"]+"' | head -1 | cut -d'"' -f4)
ip=$(printf '%s' "$sb" | grep -oE '"ip":"[0-9.]+"' | head -1 | cut -d'"' -f4)
[ "$state" = "running" ] && ok "created $id running" || bad "create returned state=$state: $sb"
[ -n "$ip" ] && ok "guest ip assigned: $ip" || bad "no ip in create response (net off or broken): $sb"

say "== 4. guest reachable from its worker =="
node_id=$(printf '%s' "$sb" | grep -oE '"node_id":"[^"]+"' | head -1 | cut -d'"' -f4)
node_addr=$(printf '%s' "$nodes" | tr '{' '\n' | grep "$node_id" | grep -oE '"addr":"[^"]+"' | cut -d'"' -f4)
worker_vm=""
case "$node_addr" in
  192.168.104.1*) worker_vm=kata-lab-0 ;;
  192.168.104.4*) worker_vm=kata-lab-1 ;;
esac
if [ -n "$worker_vm" ] && [ -n "$ip" ]; then
  sleep 3  # give the guest a moment to finish boot
  if limactl shell $worker_vm -- ping -c2 -W2 "$ip" >/dev/null 2>&1; then
    ok "guest $ip answers ping from $worker_vm"
  else
    bad "guest $ip does not answer ping from $worker_vm"
  fi
else
  bad "could not resolve worker VM for node $node_id (addr=$node_addr)"
fi

say "== 5. sleep =="
cp_curl -X POST $API/api/v1/sandboxes/$id/sleep >/dev/null
state=$(cp_curl $API/api/v1/sandboxes/$id | grep -oE '"state":"[^"]+"' | head -1 | cut -d'"' -f4)
[ "$state" = "sleeping" ] && ok "state=sleeping" || bad "expected sleeping, got $state"
if [ -n "$worker_vm" ]; then
  if limactl shell $worker_vm -- pgrep -f "$id" >/dev/null 2>&1; then
    bad "firecracker process for $id still alive after sleep"
  else
    ok "firecracker process gone after sleep"
  fi
fi

say "== 6. wake (target <500ms) =="
wres=$(cp_curl -X POST $API/api/v1/sandboxes/$id/wake)
wms=$(printf '%s' "$wres" | grep -oE '"wake_ms":[0-9]+' | cut -d: -f2)
state=$(printf '%s' "$wres" | grep -oE '"state":"[^"]+"' | head -1 | cut -d'"' -f4)
[ "$state" = "running" ] && ok "woke to running" || bad "wake returned state=$state: $wres"
if [ -n "$wms" ]; then
  [ "$wms" -lt 500 ] && ok "wake_ms=$wms (<500ms)" || bad "wake_ms=$wms (>=500ms)"
else
  bad "no wake_ms in wake response: $wres"
fi
# The restored guest must actually be alive, not just "running" in the API:
# a snapshot taken mid-guest-boot panics on resume (clock jump) and the FC
# exits ~1s later while the state still reads running.
if [ -n "$worker_vm" ] && [ -n "$ip" ]; then
  sleep 2
  if limactl shell $worker_vm -- ping -c2 -W2 "$ip" >/dev/null 2>&1; then
    ok "guest $ip answers ping after wake"
  else
    bad "guest $ip does not answer ping after wake (poisoned snapshot?)"
  fi
fi

say "== 7. fork =="
child=$(cp_curl -X POST $API/api/v1/sandboxes/$id/fork -d '{"name":"v2-verify-child"}')
cid=$(printf '%s' "$child" | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)
pid=$(printf '%s' "$child" | grep -oE '"parent_id":"[^"]+"' | head -1 | cut -d'"' -f4)
cstate=$(printf '%s' "$child" | grep -oE '"state":"[^"]+"' | head -1 | cut -d'"' -f4)
cip=$(printf '%s' "$child" | grep -oE '"ip":"[0-9.]+"' | head -1 | cut -d'"' -f4)
[ "$cstate" = "running" ] && ok "fork child $cid running" || bad "fork child state=$cstate: $child"
[ "$pid" = "$id" ] && ok "parent_id set correctly" || bad "parent_id=$pid expected $id"
# v3: the child must answer on ITS OWN ip (re-IP via the guest agent), and the
# parent must keep answering on its ip.
if [ -n "$worker_vm" ] && [ -n "$cip" ]; then
  sleep 5  # give the re-IP attempts time to land
  if limactl shell $worker_vm -- ping -c2 -W2 "$cip" >/dev/null 2>&1; then
    ok "fork child answers on its own ip $cip"
  else
    bad "fork child does not answer on $cip (re-IP failed?)"
  fi
  if limactl shell $worker_vm -- ping -c2 -W2 "$ip" >/dev/null 2>&1; then
    ok "parent still answers on $ip after fork"
  else
    bad "parent lost connectivity on $ip after fork"
  fi
fi

say "== 7b. exec (vsock guest agent) =="
eres=$(cp_curl -X POST $API/api/v1/sandboxes/$id/exec -d '{"cmd":["/bin/sh","-c","echo hearth-exec-ok; id -u"]}')
if printf '%s' "$eres" | grep -q '"exit_code":0' && printf '%s' "$eres" | grep -q 'hearth-exec-ok'; then
  ok "exec ran in guest (exit 0, output captured)"
else
  bad "exec failed: $eres"
fi

say "== 8. metrics =="
m=$(limactl shell $CP_VM -- curl -s -m5 $API/metrics)
printf '%s' "$m" | grep -q 'hearth_wake_total' && ok "wake metrics present" || bad "hearth_wake_total missing"
printf '%s' "$m" | grep -q 'hearth_forks_total' && ok "fork metrics present" || bad "hearth_forks_total missing"
printf '%s' "$m" | grep -q 'state="sleeping"' && ok "sleeping state in metrics" || bad "sleeping state missing from metrics"

say "== 9. cleanup verify resources =="
[ -n "$cid" ] && cp_curl -X DELETE -o /dev/null $API/api/v1/sandboxes/$cid
[ -n "$id" ] && cp_curl -X DELETE -o /dev/null $API/api/v1/sandboxes/$id
ok "cleanup requested"

say ""
say "RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" = "0" ]
