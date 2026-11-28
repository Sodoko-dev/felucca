# shellcheck shell=bash
# Shared helpers for the worker-side lab gates (test/lab/*.sh).
#
# Sourced, not run. These scripts execute ON a lab worker, as a user with
# passwordless sudo, against the agent's own API on loopback — they restart
# units, kill processes and edit files under /etc/felucca, none of which the
# conformance suite (plain HTTP from another VM) can do. `scripts/verify-*.sh`
# on the macOS host ship them into the VM and run them.
#
# Needs: FELUCCA_TOKEN (the lab token). Optional: AGENT_API, AGENT_DATA_DIR.

A="${AGENT_API:-http://127.0.0.1:9090}"
: "${FELUCCA_TOKEN:?FELUCCA_TOKEN is not set}"
H="Authorization: Bearer ${FELUCCA_TOKEN}"
DATA_DIR="${AGENT_DATA_DIR:-$(sudo sed -n 's/.*"data_dir"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /etc/felucca/felucca-agent.json 2>/dev/null | head -1)}"
DATA_DIR="${DATA_DIR:-/srv/felucca}"
D="${DATA_DIR%/}/instances"
UNIT=felucca-agent
PASS=0; FAIL=0; SKIP=0

say()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS+1)); say "  PASS: $*"; }
bad()  { FAIL=$((FAIL+1)); say "  FAIL: $*"; }
skip() { SKIP=$((SKIP+1)); say "  SKIP: $*"; }
# check <description> <command...> — PASS when the command succeeds.
check() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi; }
# eq <description> <got> <want>
eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 — got '$2', want '$3'"; fi; }
summary() { say ""; say "RESULT: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"; [ "${FAIL}" -eq 0 ]; }

# ---- agent API ---------------------------------------------------------------
api()      { curl -s -m "${API_TIMEOUT:-60}" -H "$H" "$@"; }
vm_create() { # <id> [mem_mib] [vcpus] -> http code
  api -o /dev/null -w '%{http_code}' -m 180 -X POST -d "{\"id\":\"$1\",\"name\":\"lab\",\"vcpus\":${3:-1},\"mem_mib\":${2:-256}}" "$A/v1/vms"; }
vm_act()   { api -o /dev/null -w '%{http_code}' -m 120 -X POST "$A/v1/vms/$1/$2"; }           # <id> <action> -> http code
vm_del()   { api -o /dev/null -w '%{http_code}' -m 60 -X DELETE "$A/v1/vms/$1"; }
vm_fork()  { api -o /dev/null -w '%{http_code}' -m 120 -X POST -d "{\"id\":\"$2\",\"name\":\"lab\"}" "$A/v1/vms/$1/fork"; }
vms()      { api -m 10 "$A/v1/vms"; }
vm_state() { vms | jq -r --arg id "$1" '.vms[]|select(.id==$id)|.state'; }
vm_pid()   { vms | jq -r --arg id "$1" '.vms[]|select(.id==$id)|.pid // empty'; }
vm_count() { vms | jq '.vms|length'; }
pooled_ids() { vms | jq -r '.vms[]|select(.state=="pooled")|.id'; }
set_pools() { api -o /dev/null -w '%{http_code}' -m 15 -X PUT -d "$1" "$A/v1/pools"; }
# The instance directory of a VM (a pool-claimed VM lives in its pool dir).
vm_dir()   { sudo sh -c "grep -l '\"id\":\"$1\"' $D/*/meta.json 2>/dev/null" | head -1 | xargs -r dirname; }
vm_ip()    { local d; d="$(vm_dir "$1")"; [ -n "$d" ] && sudo jq -r '.ip // empty' "$d/meta.json"; }
vm_slot()  { local d; d="$(vm_dir "$1")"; [ -n "$d" ] && sudo jq -r '.slot // empty' "$d/meta.json"; }
# <id> <jq filter> — a value out of the record ON DISK (the API shows the one in memory).
meta_field() { local d; d="$(vm_dir "$1")"; [ -n "$d" ] && sudo jq -r "$2" "$d/meta.json"; }

exec_ok()  { api -m 8 -X POST -d '{"cmd":["true"],"timeout_ms":3000}' "$A/v1/vms/$1/exec" | grep -q '"ok":true'; }
wait_exec() { # <id> [timeout-s] — guests take 10–15 s to bring the guest agent up
  local i=0 t="${2:-90}"
  while [ "$i" -lt "$t" ]; do exec_ok "$1" && return 0; sleep 1; i=$((i+1)); done
  return 1
}
guest()    { # <id> <sh command> -> stdout
  api -m 20 -X POST -d "$(jq -cn --arg c "$2" '{cmd:["sh","-c",$c],timeout_ms:8000}')" "$A/v1/vms/$1/exec" | jq -r '.stdout // empty'; }
guest_uptime() { guest "$1" 'cut -d. -f1 /proc/uptime' | tr -d '\n'; }
# What the VM's own Firecracker says its guest is doing: Running, Paused, "Not started".
vmm_state() { local d; d="$(vm_dir "$1")"; [ -n "$d" ] && sudo curl -s -m 3 --unix-socket "$d/fc.sock" http://localhost/ | jq -r '.state // empty'; }

# ---- processes and the unit -------------------------------------------------
fcs()       { pgrep -x firecracker | sort -n | tr '\n' ' ' | sed 's/ $//'; }
fc_count()  { pgrep -c -x firecracker || true; }
agent_pid() { systemctl show "$UNIT" -p MainPID --value; }
alive()     { [ -d "/proc/$1" ] && [ "$(sudo sed -n 's/^State:[[:space:]]*\(.\).*/\1/p' "/proc/$1/status" 2>/dev/null)" != Z ]; }
# shellcheck disable=SC2120  # the gates that source this file pass the timeout
wait_up()   { # [timeout-s] — the agent answers /healthz
  local i=0 t="${1:-40}"
  while [ "$i" -lt $((t*4)) ]; do curl -s -m1 "$A/healthz" | grep -q ok && return 0; sleep 0.25; i=$((i+1)); done
  return 1
}
restart_agent() { sudo systemctl restart "$UNIT"; wait_up; }
# (A unit that is between two starts reports MainPID 0, and `kill -9 0` signals the caller's own
# process group — this script. Never pass it on.)
kill9_agent()   { local old; old="$(agent_pid)"; [ "${old:-0}" -gt 1 ] || return 1; sudo kill -9 "$old"; local i=0; until [ "$(agent_pid)" != "$old" ] && [ "$(agent_pid)" != 0 ] || [ $i -ge 60 ]; do sleep 0.5; i=$((i+1)); done; wait_up; }
tap_count()   { ip -o link | grep -c 'hth-' || true; }
# Is the link ADMINISTRATIVELY up? `ip -br` prints the operational state, which
# is DOWN for a bridge that is up and merely has no port with carrier.
link_admin_up() { ip -o link show "$1" 2>/dev/null | grep -Eq '<([^>]*,)?UP(,[^>]*)?>'; }
dir_count()   { sudo sh -c "ls -1 $D 2>/dev/null | wc -l"; }
chain_count() { sudo nft list table netdev felucca 2>/dev/null | grep -c 'chain as_' || true; }
journal_since() { sudo journalctl -u "$UNIT" --since "$1" --no-pager -o cat; }
# Firecrackers that no record owns.
orphans() {
  local known p; known=" $(vms | jq -r '.vms[].pid // empty' | tr '\n' ' ')"
  for p in $(pgrep -x firecracker); do case "$known" in *" $p "*) ;; *) echo "$p";; esac; done
}

cleanup_prefix() { # delete every VM whose id starts with <prefix>
  local id; for id in $(vms | jq -r --arg p "$1" '.vms[]|select(.id|startswith($p))|.id'); do vm_del "$id" >/dev/null; done
}

# Refuse to run against a unit that a test drop-in is still patching: a gate
# that passes because of a left-over override proves nothing about the unit
# that ships.
preflight_unit() {
  local d; d="$(systemctl show "$UNIT" -p DropInPaths --value)"
  case "$d" in *w0-*|*lab-test*|*zz-*) bad "left-over test drop-in on $UNIT: $d"; return 1;; esac
  return 0
}

# ---- snapshots, disks and Firecrackers (added for test/lab/snapshot*.sh) -----
# <id> <action> -> "<http code> <body>" (the body on one line)
vm_act_body() { local r; r="$(api -m 120 -w '\n%{http_code}' -X POST "$A/v1/vms/$1/$2")"; printf '%s %s' "$(printf '%s' "$r" | tail -n 1)" "$(printf '%s' "$r" | sed '$d' | tr -d '\n')"; }
vm_fork_body() { local r; r="$(api -m 120 -w '\n%{http_code}' -X POST -d "{\"id\":\"$2\",\"name\":\"lab\"}" "$A/v1/vms/$1/fork")"; printf '%s %s' "$(printf '%s' "$r" | tail -n 1)" "$(printf '%s' "$r" | sed '$d' | tr -d '\n')"; }
# <id> -> "<state> <snapshot_valid>" of the record ON DISK (absent marker = false)
meta_snap() { meta_field "$1" '"\(.state) \(.snapshot_valid // false)"'; }
# <dir> -> "vmstate=yes|no mem=yes|no tmp=yes|no"
snap_files() { sudo sh -c "cd '$1' 2>/dev/null || exit 0; for f in vmstate.bin mem.bin vmstate.bin.tmp; do if [ -e \$f ]; then r=yes; else r=no; fi; case \$f in vmstate.bin) printf 'vmstate=%s ' \$r;; mem.bin) printf 'mem=%s ' \$r;; *) printf 'tmp=%s' \$r;; esac; done"; }
# <dir> -> pids of the Firecrackers whose API socket is in that directory (live ones only)
fc_in_dir() { local p; for p in $(pgrep -x firecracker); do alive "$p" && sudo cat "/proc/$p/cmdline" 2>/dev/null | tr '\0' ' ' | grep -qF -- "--api-sock $1/fc.sock" && echo "$p"; done; }
# <dir> [timeout-s] — wait until no Firecracker of that directory is alive
wait_no_fc() { local i=0 t="${2:-30}"; while [ -n "$(fc_in_dir "$1")" ] && [ "$i" -lt $((t*4)) ]; do sleep 0.25; i=$((i+1)); done; [ -z "$(fc_in_dir "$1")" ]; }
# <pid> -> the rootfs.ext4 files that process has open, sorted, space-separated
rootfs_open() { sudo ls -l "/proc/$1/fd" 2>/dev/null | grep -o '/[^ ]*rootfs\.ext4' | sort -u | tr '\n' ' ' | sed 's/ $//'; }
# <dir> -> path_on_host of the drive "rootfs" as that directory's Firecracker reports it
fc_drive() { sudo curl -s -m 5 --unix-socket "$1/fc.sock" http://localhost/vm/config | jq -r '.drives[]?|select(.drive_id=="rootfs")|.path_on_host'; }
# <image> <path> -> the file's content read out of an ext4 image with debugfs ("" when it is not there)
img_cat() { sudo debugfs -R "cat $2" "$1" 2>/dev/null; }
img_has() { sudo debugfs -R "stat $2" "$1" 2>&1 | grep -q '^Inode:'; }
# The memory witness: a 50 MB blob in the guest's tmpfs, and its sha256.
blob_make() { guest "$1" 'head -c 50000000 /dev/urandom > /dev/shm/blob && sha256sum /dev/shm/blob | cut -c1-64' | tr -d '\n'; }
blob_sum()  { guest "$1" 'sha256sum /dev/shm/blob 2>/dev/null | cut -c1-64' | tr -d '\n'; }
