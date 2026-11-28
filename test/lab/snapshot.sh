#!/usr/bin/env bash
# Snapshot gate (FLU-4, FLU-70, FLU-72) — runs ON a lab worker (see scripts/verify-snapshot.sh).
#
#   snapshot.sh [all | "S1 S5 ..."]
#
# A sandbox is asleep only when its record says so, and the record says so
# only once the snapshot is complete and on disk; a snapshot that cannot be
# written leaves the guest running; a fork child runs on its own disk.
#   S1  sleep and wake, three rounds: the record, the files, the memory witness
#   S2  a Firecracker that dies while the agent runs -> error, files gone, wake 409
#   S3  a Firecracker that dies while the agent is stopped (also with a stale
#       vmstate.bin put back) -> stopped, never sleeping; start cold-boots, disk kept
#   S4  stop of a sleeping sandbox discards its snapshot
#   S5  a sleep that cannot be written (full disk): 5xx, the guest runs on
#   S6  three sleeps of one 2 GiB guest at once: one snapshot
#   S7  wake, stop and delete during a sleep
#   S8  fork from a running and from a sleeping parent, fork into an existing id
#   S9  a wake the previous agent did not finish (loaded but not resumed / resumed)
#   S11 upgrade from the previous agent binary and rollback to it (PREV_AGENT_BIN)
#   S12 a fork child runs on its own disk, also after its own sleep and for
#       grandchildren (FLU-72)
#   S13 a sleep is durable on disk when it is acknowledged
# Case 10 (worker reboot and power cut) is test/lab/snapshot-pre.sh and -post.sh.
#
# The memory witness throughout: a 50 MB blob in the guest's tmpfs and its
# sha256 taken before; "memory intact" = the same sum after.
#
# Every sandbox is named sn-*. S5 fills the data disk for a few seconds; S9
# starts Firecrackers by hand; S11 swaps the agent binary and puts it back.
# The trap undoes all of it. Do not run it on a worker that holds anything you
# care about.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"
SECTIONS="${1:-all}"
want() { [ "$SECTIONS" = all ] || case " $SECTIONS " in *" $1 "*) return 0;; *) return 1;; esac; }
PREV="${PREV_AGENT_BIN:-}"
BIN=/usr/local/bin/felucca-agent
KEEP=/var/tmp/felucca-lab-sn-agent.keep
FILLDIR="${DATA_DIR%/}/felucca-lab-fill"
HAND_PIDS=""
LOGS=/var/tmp/felucca-lab-sn

undo() {
  rm -rf "$FILLDIR" 2>/dev/null; sudo rm -rf "$FILLDIR"
  for p in $HAND_PIDS; do alive "$p" && sudo kill -9 "$p"; done
  if sudo test -f "$KEEP"; then
    if [ "$(sudo sha256sum "$BIN" | cut -c1-64)" != "$(sudo sha256sum "$KEEP" | cut -c1-64)" ]; then
      sudo install -m 0755 "$KEEP" "$BIN"; sudo systemctl restart "$UNIT"; wait_up 60
      say "  (trap: the agent binary that was installed at the start is back)"
    fi
    sudo rm -f "$KEEP"
  fi
  [ "$(systemctl is-active "$UNIT")" = active ] || { sudo systemctl start "$UNIT"; wait_up 60; }
  cleanup_prefix sn-
  sudo rm -rf "$LOGS"
}
trap 'undo' EXIT
sudo mkdir -p "$LOGS"; sudo chmod 0777 "$LOGS"

now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN{printf "%.2f", b-a}'; }
st() { vm_state "$1"; }
# <label> <id> <timeout-s> — the guest answers exec
ready() { wait_exec "$2" "$3" && ok "$1: $2 answers exec" || bad "$1: $2 does not answer exec within $3 s"; }
# <label> <id> <sum> — the memory witness is intact
intact() { local s; s="$(blob_sum "$2")"; eq "$1: $2 memory intact (blob sha256)" "${s:0:16}" "${3:0:16}"; }
# <label> <id> — the record says sleeping (API) and sleeping with the marker (disk), both snapshot files, no Firecracker
asleep() {
  local d; d="$(vm_dir "$2")"
  eq "$1: $2 API state" "$(st "$2")" sleeping
  eq "$1: $2 record on disk" "$(meta_snap "$2")" "sleeping true"
  eq "$1: $2 snapshot files" "$(snap_files "$d")" "vmstate=yes mem=yes tmp=no"
  wait_no_fc "$d" 30; eq "$1: $2 has no Firecracker (30 s limit)" "$(fc_in_dir "$d" | tr '\n' ' ')" ""
}
# <label> <id> — awake: running (API, disk), no marker, vmstate.bin gone, mem.bin kept
awake() {
  local d; d="$(vm_dir "$2")"
  eq "$1: $2 API state" "$(st "$2")" running
  eq "$1: $2 record on disk" "$(meta_snap "$2")" "running false"
  eq "$1: $2 snapshot files" "$(snap_files "$d")" "vmstate=no mem=yes tmp=no"
}
# <label> <code+body> <want code> <want state> — a 409 that names the state
refused() {
  local c="${2%% *}" b="${2#* }"
  eq "$1: code" "$c" "$3"
  eq "$1: state in the body" "$(printf '%s' "$b" | jq -r '.state // empty' 2>/dev/null)" "$4"
}
# <id> -> its Firecracker's pid as the record has it, or the one in its directory
fcpid() { local p; p="$(vm_pid "$1")"; [ -n "$p" ] || p="$(fc_in_dir "$(vm_dir "$1")" | head -1)"; echo "$p"; }
# <label> <id> — its Firecracker has its OWN rootfs.ext4 open and nothing else, and says so in /vm/config
own_disk() {
  local d p; d="$(vm_dir "$2")"; p="$(fcpid "$2")"
  if [ -z "$p" ]; then bad "$1: $2 has no Firecracker to look at"; return; fi
  eq "$1: $2 (pid $p) has open (ls -l /proc/$p/fd)" "$(rootfs_open "$p")" "$d/rootfs.ext4"
  eq "$1: $2 GET /vm/config drive rootfs" "$(fc_drive "$d")" "$d/rootfs.ext4"
}
# Background calls: <file> <id> <action> [max-time] -> writes "<code> <start> <end>" to the file
bg_act() { ( s=$(now); c=$(api -o /dev/null -w '%{http_code}' -m "${4:-120}" -X POST "$A/v1/vms/$2/$3"); echo "$c $s $(now)" > "$1" ) & }
fill_disk() { # <leave-kb>: fill the data dir's file system so that about <leave-kb> KiB stay free (0: nothing)
  sudo install -d -o "$(id -u)" -m 0755 "$FILLDIR"
  local free; free=$(df --output=avail "$DATA_DIR" | tail -1 | tr -d ' ')
  if [ "$1" -gt 0 ]; then fallocate -l $(( (free - $1) * 1024 )) "$FILLDIR/a"
  else fallocate -l $(( (free - 2048) * 1024 )) "$FILLDIR/a"; dd if=/dev/zero of="$FILLDIR/b" bs=4096 >/dev/null 2>&1; fi
}
free_disk() { rm -rf "$FILLDIR"/* 2>/dev/null; sudo rm -rf "$FILLDIR"; }

say "== 0. the worker =="
preflight_unit
cleanup_prefix sn-
eq "agent active" "$(systemctl is-active $UNIT)" active
say "  agent $(sudo sha256sum $BIN | cut -c1-16) pid $(agent_pid); data dir $DATA_DIR"
DC0=$(dir_count); FC0=$(fc_count)
say "  at the start: instance dirs $DC0, Firecrackers $FC0"

if want S1; then
say "== S1. sleep and wake, three rounds =="
eq "create sn-sw" "$(vm_create sn-sw 256)" 201; ready S1 sn-sw 150
B=$(blob_make sn-sw); [ -n "$B" ] && say "  blob ${B:0:16}" || bad "S1: no blob"
for r in 1 2 3; do
  eq "S1.$r sleep" "$(vm_act sn-sw sleep)" 200
  asleep "S1.$r after sleep" sn-sw
  eq "S1.$r wake" "$(vm_act sn-sw wake)" 200
  awake "S1.$r after wake" sn-sw
  ready "S1.$r" sn-sw 60
  intact "S1.$r" sn-sw "$B"
done
vm_del sn-sw >/dev/null
fi

if want S2; then
say "== S2. a Firecracker dies while the agent runs =="
eq "create sn-die" "$(vm_create sn-die 256)" 201; ready S2 sn-die 150
eq "S2 sleep" "$(vm_act sn-die sleep)" 200; eq "S2 wake" "$(vm_act sn-die wake)" 200; ready S2 sn-die 60
DD="$(vm_dir sn-die)"; P=$(vm_pid sn-die)
say "  files before the kill: $(snap_files "$DD")"
sudo kill -9 "$P"
i=0; until [ "$(st sn-die)" = error ] || [ $i -ge 20 ]; do sleep 1; i=$((i+1)); done
eq "S2: record becomes error (after ${i}s, 20 s limit)" "$(st sn-die)" error
i=0; until [ "$(snap_files "$DD")" = "vmstate=no mem=no tmp=no" ] || [ $i -ge 10 ]; do sleep 1; i=$((i+1)); done
eq "S2: snapshot files gone (10 s limit)" "$(snap_files "$DD")" "vmstate=no mem=no tmp=no"
refused "S2: wake of the dead sandbox" "$(vm_act_body sn-die wake)" 409 error
eq "S2: still error after the refused wake, no Firecracker" "$(st sn-die) $(fc_in_dir "$DD" | wc -l | tr -d ' ')" "error 0"
vm_del sn-die >/dev/null
fi

if want S3; then
say "== S3. a Firecracker dies while the agent is stopped =="
for v in sn-d1 sn-d2; do eq "create $v" "$(vm_create $v 256)" 201; done
for v in sn-d1 sn-d2; do ready S3 $v 150; done
eq "S3 sleep sn-d1" "$(vm_act sn-d1 sleep)" 200; eq "S3 sleep sn-d2" "$(vm_act sn-d2 sleep)" 200
D2="$(vm_dir sn-d2)"; sudo cp -p "$D2/vmstate.bin" "$LOGS/d2-vmstate.bin" || bad "S3: could not keep sn-d2's vmstate.bin"
for v in sn-d1 sn-d2; do eq "S3 wake $v" "$(vm_act $v wake)" 200; done
for v in sn-d1 sn-d2; do ready S3 $v 60; eq "S3: $v writes its disk" "$(guest $v "echo disk-$v > /root/diskmark; sync; cat /root/diskmark" | tr -d '\n')" "disk-$v"; done
P1=$(vm_pid sn-d1); P2=$(vm_pid sn-d2); D1="$(vm_dir sn-d1)"
sudo systemctl stop $UNIT
sudo kill -9 "$P1" "$P2"; wait_no_fc "$D1" 30; wait_no_fc "$D2" 30
sudo cp -p "$LOGS/d2-vmstate.bin" "$D2/vmstate.bin"
say "  agent stopped, both Firecrackers killed; sn-d1 files: $(snap_files "$D1"); sn-d2 files (stale vmstate.bin put back): $(snap_files "$D2")"
T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl start $UNIT; wait_up 60; sleep 2
for v in sn-d1 sn-d2; do
  eq "S3: $v API state after start-up" "$(st $v)" stopped
  eq "S3: $v record on disk" "$(meta_snap $v)" "stopped false"
  eq "S3: $v has no vmstate.bin" "$(snap_files "$(vm_dir $v)" | cut -d' ' -f1)" "vmstate=no"
  refused "S3: wake $v" "$(vm_act_body $v wake)" 409 stopped
  eq "S3: start $v (cold boot)" "$(vm_act $v start)" 200
done
for v in sn-d1 sn-d2; do ready S3 $v 150; eq "S3: $v kept the file written to its disk before the kill" "$(guest $v 'cat /root/diskmark' | tr -d '\n')" "disk-$v"; done
cleanup_prefix sn-d
fi

if want S4; then
say "== S4. stop of a sleeping sandbox =="
eq "create sn-st" "$(vm_create sn-st 256)" 201; ready S4 sn-st 150
eq "S4 sleep" "$(vm_act sn-st sleep)" 200
eq "S4 stop of the sleeping sandbox" "$(vm_act sn-st stop)" 200
eq "S4: API state" "$(st sn-st)" stopped
eq "S4: record on disk" "$(meta_snap sn-st)" "stopped false"
eq "S4: snapshot files" "$(snap_files "$(vm_dir sn-st)")" "vmstate=no mem=no tmp=no"
refused "S4: wake after the stop" "$(vm_act_body sn-st wake)" 409 stopped
vm_del sn-st >/dev/null
fi

if want S5; then
say "== S5. a sleep that cannot be written (full disk) =="
eq "create sn-f1 (never slept)" "$(vm_create sn-f1 256)" 201; eq "create sn-f2 (slept and woke before)" "$(vm_create sn-f2 256)" 201
ready S5 sn-f1 150; ready S5 sn-f2 150
B2=$(blob_make sn-f2); eq "S5 sleep sn-f2" "$(vm_act sn-f2 sleep)" 200; eq "S5 wake sn-f2" "$(vm_act sn-f2 wake)" 200; ready S5 sn-f2 60
# sn-f1: less free than its memory (the approach of repro-flu70.sh). sn-f2 writes
# its memory file over the one it was woken from, which needs no new space, so
# its disk is filled to the last block. Nothing is printed while the disk is
# full: this script's own output lives on the same file system.
for v in sn-f1 sn-f2; do
  DV="$(vm_dir $v)"
  if [ $v = sn-f1 ]; then fill_disk 20000; else fill_disk 0; fi
  LEFT="$(df -k --output=avail "$DATA_DIR" | tail -1 | tr -d ' ')"
  R="$(vm_act_body $v sleep)"
  free_disk
  say "  $v: sleep with ${LEFT} KiB free answered: $(printf '%s' "$R" | cut -c1-200)"
  case "${R%% *}" in 5??) ok "S5: $v sleep answers 5xx (${R%% *})";; *) bad "S5: $v sleep — got '${R%% *}', want 5xx";; esac
  i=0; until [ "$(vmm_state $v)" = Running ] || [ $i -ge 10 ]; do sleep 1; i=$((i+1)); done
  eq "S5: $v Firecracker says (10 s limit)" "$(vmm_state $v)" Running
  IP="$(vm_ip $v)"; ping -c2 -W1 "$IP" >/dev/null 2>&1 && ok "S5: $v answers ping" || bad "S5: $v does not answer ping"
  say "  $v exec after the failed sleep: $(exec_ok $v && echo works || echo 'fails (FLU-15 if after a plain pause+resume)')"
  eq "S5: $v API state" "$(st $v)" running
  eq "S5: $v record on disk" "$(meta_snap $v)" "running false"
  if [ $v = sn-f1 ]; then eq "S5: $v files (never slept: no mem.bin either)" "$(snap_files "$DV")" "vmstate=no mem=no tmp=no"
  else eq "S5: $v files (woken before: its mem.bin is the guest's memory)" "$(snap_files "$DV")" "vmstate=no mem=yes tmp=no"; fi
done
eq "S5: sn-f2 sleep with space again" "$(vm_act sn-f2 sleep)" 200
asleep "S5 later sleep" sn-f2
eq "S5: sn-f2 wake" "$(vm_act sn-f2 wake)" 200; ready S5 sn-f2 60; intact S5 sn-f2 "$B2"
cleanup_prefix sn-f
fi

if want S6 || want S7; then
eq "create sn-big (2048 MiB)" "$(vm_create sn-big 2048)" 201; ready "S6/S7" sn-big 150
BB=$(blob_make sn-big); DB="$(vm_dir sn-big)"
# Touch 800 MB of guest memory so that a snapshot takes seconds, not tenths.
dirty() { api -m 70 -X POST -d '{"cmd":["sh","-c","dd if=/dev/zero of=/dev/shm/dirty bs=1M count=800 2>/dev/null && echo done"],"timeout_ms":60000}' "$A/v1/vms/$1/exec" | jq -r '.stdout // .error // empty' | tr -d '\n'; }
eq "S6/S7: 800 MB of guest memory touched" "$(dirty sn-big)" done
fi

if want S6; then
say "== S6. three sleeps of one 2 GiB guest at once =="
rm -f "$LOGS"/s6-*
bg_act "$LOGS/s6-1" sn-big sleep 1; sleep 0.1
bg_act "$LOGS/s6-2" sn-big sleep;   sleep 1
bg_act "$LOGS/s6-3" sn-big sleep
i=0; until [ "$(st sn-big)" = sleeping ] || [ $i -ge 90 ]; do sleep 1; i=$((i+1)); done
INO1="$(sudo stat -c '%i %y' "$DB/vmstate.bin" 2>/dev/null)"
wait; sleep 3
for k in 1 2 3; do say "  sleep #$k: $(cat "$LOGS/s6-$k" 2>/dev/null)"; done
eq "S6: #1 (1 s client timeout) gave up on the client" "$(cut -d' ' -f1 "$LOGS/s6-1")" 000
for k in 2 3; do c="$(cut -d' ' -f1 "$LOGS/s6-$k")"; case "$c" in 200|409) ok "S6: #$k answered $c";; *) bad "S6: #$k — got '$c', want 200 or 409 busy";; esac; done
eq "S6: one snapshot (vmstate.bin is the same file from the moment it slept to the end)" "$(sudo stat -c '%i %y' "$DB/vmstate.bin" 2>/dev/null)" "$INO1"
asleep S6 sn-big
eq "S6 wake" "$(vm_act sn-big wake)" 200; ready S6 sn-big 60; intact S6 sn-big "$BB"
fi

if want S7; then
say "== S7. wake, stop and delete during a sleep =="
# <file> <t of the second call> — was the sleep still under way when the second call was sent?
overlapped() { local e; e="$(cut -d' ' -f3 "$1" 2>/dev/null)"; [ -n "$e" ] && awk -v e="$e" -v t="$2" 'BEGIN{exit !(e>t)}'; }
rm -f "$LOGS"/s7-*
bg_act "$LOGS/s7-w" sn-big sleep; sleep 0.5; TW=$(now); R="$(vm_act_body sn-big wake)"; wait
if overlapped "$LOGS/s7-w" "$TW"; then
  eq "S7: wake during a sleep: code" "${R%% *}" 409
  eq "S7: wake during a sleep: body" "$(printf '%s' "${R#* }" | jq -r '.error // empty' 2>/dev/null)" busy
else bad "S7: the sleep ended before the wake was sent ($(cat "$LOGS/s7-w")): the case was not exercised"; fi
i=0; until [ "$(st sn-big)" = sleeping ] || [ $i -ge 60 ]; do sleep 1; i=$((i+1)); done
eq "S7: the sleep completed" "$(st sn-big) $(cut -d' ' -f1 "$LOGS/s7-w")" "sleeping 200"
eq "S7 wake" "$(vm_act sn-big wake)" 200; ready S7 sn-big 60; intact S7 sn-big "$BB"

bg_act "$LOGS/s7-s" sn-big sleep; sleep 0.5; TS=$(now); C="$(vm_act sn-big stop)"; TOOK=$(since "$TS"); wait
overlapped "$LOGS/s7-s" "$TS" || bad "S7: the sleep ended before the stop was sent ($(cat "$LOGS/s7-s")): the case was not exercised"
say "  stop during a sleep answered $C after ${TOOK}s; the sleep answered $(cut -d' ' -f1 "$LOGS/s7-s")"
eq "S7: stop during a sleep" "$C" 200
eq "S7: then the record says (API, disk)" "$(st sn-big) $(meta_snap sn-big)" "stopped stopped false"
wait_no_fc "$DB" 30; eq "S7: no Firecracker after the stop (30 s limit)" "$(fc_in_dir "$DB" | wc -l | tr -d ' ')" 0
eq "S7: no snapshot files after the stop" "$(snap_files "$DB")" "vmstate=no mem=no tmp=no"
eq "S7 start" "$(vm_act sn-big start)" 200; ready S7 sn-big 150
eq "S7: 800 MB of guest memory touched again after the cold boot" "$(dirty sn-big)" done

bg_act "$LOGS/s7-d" sn-big sleep; sleep 0.5; TD=$(now); C="$(vm_del sn-big)"; TOOK=$(since "$TD"); wait
overlapped "$LOGS/s7-d" "$TD" || bad "S7: the sleep ended before the delete was sent ($(cat "$LOGS/s7-d")): the case was not exercised"
eq "S7: delete during a sleep" "$C" 204
awk -v t="$TOOK" 'BEGIN{exit !(t<=20)}' && ok "S7: the delete answered within 20 s (${TOOK}s)" || bad "S7: the delete took ${TOOK}s, want <= 20"
wait_no_fc "$DB" 30; eq "S7: no Firecracker after the delete (30 s limit)" "$(fc_in_dir "$DB" | wc -l | tr -d ' ')" 0
sudo test -e "$DB" && bad "S7: the directory is still there after the delete" || ok "S7: no directory after the delete"
eq "S7: no record after the delete" "$(st sn-big)" ""
fi
want S6 && ! want S7 && vm_del sn-big >/dev/null

if want S8; then
say "== S8. fork =="
eq "create sn-p1" "$(vm_create sn-p1 256)" 201; eq "create sn-p2" "$(vm_create sn-p2 256)" 201
ready S8 sn-p1 150; ready S8 sn-p2 150
BP1=$(blob_make sn-p1); BP2=$(blob_make sn-p2); DP1="$(vm_dir sn-p1)"
eq "S8: fork the running sn-p1 -> sn-k1" "$(vm_fork sn-p1 sn-k1)" 201
eq "S8: the parent's directory has no snapshot files" "$(snap_files "$DP1" | sed 's/ mem=[a-z]*//')" "vmstate=no tmp=no"
eq "S8: the parent's directory has no mem.bin (it never slept)" "$(snap_files "$DP1" | cut -d' ' -f2)" "mem=no"
eq "S8: the child runs" "$(st sn-k1)" running
eq "S8: the child's directory holds no vmstate.bin" "$(snap_files "$(vm_dir sn-k1)" | cut -d' ' -f1)" "vmstate=no"
ready S8 sn-k1 60; intact "S8 parent" sn-p1 "$BP1"; intact "S8 child has the parent's blob" sn-k1 "$BP1"
eq "S8: sleep sn-p2" "$(vm_act sn-p2 sleep)" 200
eq "S8: fork the sleeping sn-p2 -> sn-k2" "$(vm_fork sn-p2 sn-k2)" 201
eq "S8: the parent is still asleep (API, disk)" "$(st sn-p2) $(meta_snap sn-p2)" "sleeping sleeping true"
eq "S8: the parent's snapshot files" "$(snap_files "$(vm_dir sn-p2)")" "vmstate=yes mem=yes tmp=no"
eq "S8: the child runs" "$(st sn-k2)" running; ready S8 sn-k2 60
eq "S8: wake the parent" "$(vm_act sn-p2 wake)" 200; ready S8 sn-p2 60; intact "S8 parent" sn-p2 "$BP2"
eq "S8: stop sn-k2 (a fork target whose files must not change)" "$(vm_act sn-k2 stop)" 200; sleep 1
DK2="$(vm_dir sn-k2)"
LIST0="$(sudo sh -c "cd '$DK2' && stat -c '%n %s %y' *")"
R="$(vm_fork_body sn-p1 sn-k2)"
eq "S8: fork into the existing id sn-k2: code" "${R%% *}" 409
say "  body: ${R#* }"
eq "S8: the existing sandbox's files are untouched (names, sizes, mtimes)" "$(sudo sh -c "cd '$DK2' && stat -c '%n %s %y' *")" "$LIST0"
eq "S8: the existing sandbox is still stopped" "$(st sn-k2)" stopped
eq "S8: the refused fork left nothing in the parent's directory" "$(snap_files "$DP1")" "vmstate=no mem=no tmp=no"
eq "S8: the parent still runs" "$(st sn-p1) $(vmm_state sn-p1)" "running Running"
cleanup_prefix sn-p; cleanup_prefix sn-k
fi

if want S9; then
say "== S9. a wake the previous agent did not finish =="
# <id> <resume yes|no> — with the agent stopped, start a Firecracker by hand in the
# sleeping sandbox's directory and load its snapshot without resuming (and resume).
handwake() {
  local d slot body p i; d="$(vm_dir "$1")"; slot="$(vm_slot "$1")"
  sudo rm -f "$d/fc.sock" "$d/v.sock"
  p="$(sudo sh -c "cd '$d'; setsid firecracker --api-sock '$d/fc.sock' </dev/null >'$LOGS/hand-$1.log' 2>&1 & echo \$!")"
  HAND_PIDS="$HAND_PIDS $p"
  # Start-up adopts only Firecrackers in the agent unit's own cgroup (others are
  # "foreign" and left alone), and the agent's Firecrackers stay in it while the
  # agent is stopped: put the hand-started one there too.
  echo "$p" | sudo tee "/sys/fs/cgroup${CG}/cgroup.procs" >/dev/null || bad "S9: could not move $p into ${CG}"
  i=0; until sudo test -S "$d/fc.sock" || [ $i -ge 100 ]; do sleep 0.05; i=$((i+1)); done
  body="{\"snapshot_path\":\"$d/vmstate.bin\",\"mem_backend\":{\"backend_path\":\"$d/mem.bin\",\"backend_type\":\"File\"},\"network_overrides\":[{\"iface_id\":\"eth0\",\"host_dev_name\":\"hth-$slot\"}],\"resume_vm\":false}"
  say "  $1: hand Firecracker $p; load: $(sudo curl -s -m 60 -w 'HTTP %{http_code}' --unix-socket "$d/fc.sock" -X PUT -H 'Content-Type: application/json' -d "$body" http://localhost/snapshot/load)"
  [ "$2" = yes ] && say "  $1: resume by hand: $(sudo curl -s -m 10 -w 'HTTP %{http_code}' --unix-socket "$d/fc.sock" -X PATCH -H 'Content-Type: application/json' -d '{"state":"Resumed"}' http://localhost/vm)"
  HP="$p"
}
# sn-h0 keeps running: its Firecracker keeps the unit's cgroup in existence while the agent is stopped.
for v in sn-h0 sn-h1 sn-h2; do eq "create $v" "$(vm_create $v 256)" 201; done
for v in sn-h0 sn-h1 sn-h2; do ready S9 $v 150; done
CG="$(sudo sed -n 's/^0:://p' "/proc/$(vm_pid sn-h0)/cgroup")"; say "  the agent's Firecrackers live in cgroup $CG"
BH1=$(blob_make sn-h1); BH2=$(blob_make sn-h2)
for v in sn-h1 sn-h2; do eq "S9 sleep $v" "$(vm_act $v sleep)" 200; done
sudo systemctl stop $UNIT
handwake sn-h1 no; HP1=$HP
handwake sn-h2 yes; HP2=$HP
eq "S9 (a): the hand Firecracker of sn-h1 is loaded, not resumed" "$(sudo curl -s -m 3 --unix-socket "$(vm_dir sn-h1)/fc.sock" http://localhost/ | jq -r .state)" Paused
eq "S9 (b): the hand Firecracker of sn-h2 runs" "$(sudo curl -s -m 3 --unix-socket "$(vm_dir sn-h2)/fc.sock" http://localhost/ | jq -r .state)" Running
T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl start $UNIT; wait_up 60; sleep 2
say "  start-up: $(journal_since "$T0" | grep 'reconcile: took over' | tail -1 | sed 's/\x1b\[[0-9;]*m//g; s/.*took over what the previous agent left *//')"
i=0; while alive "$HP1" && [ $i -lt 30 ]; do sleep 1; i=$((i+1)); done
alive "$HP1" && bad "S9 (a): the loaded, never-resumed Firecracker is still alive (30 s limit)" || ok "S9 (a): the loaded, never-resumed Firecracker was killed"
asleep "S9 (a)" sn-h1
eq "S9 (a) wake" "$(vm_act sn-h1 wake)" 200; ready "S9 (a)" sn-h1 60; intact "S9 (a)" sn-h1 "$BH1"
eq "S9 (b): adopted as running with the hand Firecracker" "$(st sn-h2) $(vm_pid sn-h2)" "running $HP2"
eq "S9 (b): the record on disk" "$(meta_field sn-h2 '"\(.state) \(.pid) \(.snapshot_valid // false)"')" "running $HP2 false"
eq "S9 (b): its vmstate.bin was removed" "$(snap_files "$(vm_dir sn-h2)")" "vmstate=no mem=yes tmp=no"
ready "S9 (b)" sn-h2 30; intact "S9 (b)" sn-h2 "$BH2"
cleanup_prefix sn-h
for p in $HAND_PIDS; do alive "$p" && { bad "S9: hand-started Firecracker $p still alive after its sandbox was deleted; killed"; sudo kill -9 "$p"; }; done
fi

if want S11; then
say "== S11. upgrade from the previous agent binary, and rollback =="
if [ -z "$PREV" ]; then skip "S11 upgrade and rollback (PREV_AGENT_BIN is not set)"
elif ! sudo test -x "$PREV"; then bad "S11: PREV_AGENT_BIN=$PREV is not an executable file"
else
  sudo cp -p "$BIN" "$KEEP"
  NEWSUM="$(sudo sha256sum "$KEEP" | cut -c1-64)"; PREVSUM="$(sudo sha256sum "$PREV" | cut -c1-64)"
  say "  installed (new): ${NEWSUM:0:16}; previous: ${PREVSUM:0:16}"
  [ "$NEWSUM" != "$PREVSUM" ] || bad "S11: the previous binary is the installed one"
  swap_to() { sudo install -m 0755 "$1" "$BIN"; sudo systemctl restart $UNIT; wait_up 60; sleep 2; }
  swap_to "$PREV"; eq "S11: the previous agent runs" "$(sudo sha256sum $BIN | cut -c1-16) $(systemctl is-active $UNIT)" "${PREVSUM:0:16} active"
  for v in sn-u1 sn-u2 sn-u3; do eq "S11 (old) create $v" "$(vm_create $v 256)" 201; done
  for v in sn-u1 sn-u2 sn-u3; do ready S11 $v 150; done
  BU1=$(blob_make sn-u1); BU2=$(blob_make sn-u2)
  eq "S11 (old) sleep sn-u1" "$(vm_act sn-u1 sleep)" 200
  for v in sn-u2 sn-u3; do eq "S11 (old) sleep $v" "$(vm_act $v sleep)" 200; eq "S11 (old) wake $v" "$(vm_act $v wake)" 200; ready S11 $v 60; done
  D3="$(vm_dir sn-u3)"
  guest sn-u3 'echo after-the-snapshot > /root/diskmark; sync' >/dev/null
  i=0; until [ "$(sudo stat -c %.9Y "$D3/rootfs.ext4")" \> "$(sudo stat -c %.9Y "$D3/vmstate.bin" 2>/dev/null || echo 9)" ] || [ $i -ge 30 ]; do sleep 1; i=$((i+1)); done
  say "  sn-u3: rootfs.ext4 $(sudo stat -c %y "$D3/rootfs.ext4"), vmstate.bin $(sudo stat -c %y "$D3/vmstate.bin" 2>&1)"
  P3=$(vm_pid sn-u3); sudo systemctl stop $UNIT; sudo kill -9 "$P3"; wait_no_fc "$D3" 30; sudo systemctl start $UNIT; wait_up 60; sleep 2
  say "  old agent after sn-u3's Firecracker died: $(st sn-u3); expose: $(api -o /dev/null -w '%{http_code}' -m 15 -X POST -d '{"guest_port":8000}' "$A/v1/vms/sn-u3/expose")"
  eq "S11 precondition: the old agent wrote sn-u3 as sleeping (no marker)" "$(meta_snap sn-u3)" "sleeping false"
  eq "S11 precondition: sn-u1 written asleep by the old agent (no marker)" "$(meta_snap sn-u1)" "sleeping false"
  # A fork child made by the old agent runs on its parent's image (FLU-72).
  eq "S11 (old) create sn-u5" "$(vm_create sn-u5 256)" 201; ready S11 sn-u5 150
  eq "S11 (old) fork sn-u5 -> sn-u6" "$(vm_fork sn-u5 sn-u6)" 201; ready S11 sn-u6 60
  eq "S11 precondition: the old agent's child sn-u6 runs on its parent's image" "$(rootfs_open "$(vm_pid sn-u6)")" "$(vm_dir sn-u5)/rootfs.ext4"
  PU2=$(vm_pid sn-u2)
  T0=$(date '+%Y-%m-%d %H:%M:%S'); swap_to "$KEEP"; eq "S11: the new agent runs" "$(sudo sha256sum $BIN | cut -c1-16)" "${NEWSUM:0:16}"
  say "  start-up: $(journal_since "$T0" | grep 'reconcile: took over' | tail -1 | sed 's/\x1b\[[0-9;]*m//g; s/.*took over what the previous agent left *//')"
  say "  journal about sn-u3: $(journal_since "$T0" | grep 'sn-u3' | sed 's/\x1b\[[0-9;]*m//g' | cut -c1-260 | tail -3 | tr '\n' '|')"
  eq "S11 upgrade: sn-u1 (asleep, clean) still sleeping" "$(st sn-u1)" sleeping
  eq "S11 upgrade: sn-u1 wake" "$(vm_act sn-u1 wake)" 200; ready S11 sn-u1 60; intact "S11 upgrade" sn-u1 "$BU1"
  eq "S11 upgrade: sn-u2 running, same Firecracker" "$(st sn-u2) $(vm_pid sn-u2)" "running $PU2"
  eq "S11 upgrade: sn-u2's vmstate.bin removed" "$(snap_files "$(vm_dir sn-u2)" | cut -d' ' -f1)" "vmstate=no"
  eq "S11 upgrade: sn-u3 (ran after its snapshot) is stopped (API, disk)" "$(st sn-u3) $(meta_snap sn-u3)" "stopped stopped false"
  eq "S11 upgrade: sn-u3 has no vmstate.bin and no mem.bin" "$(snap_files "$D3" | sed 's/ tmp=no//')" "vmstate=no mem=no"
  say "  sn-u3's directory: $(sudo ls "$D3" | tr '\n' ' ') (3ac73d6 and later keep the stale pair as *.outrun)"
  # The old agent's fork child, under the new agent (needs 3ac73d6 or later: e56ffea forks and wakes it).
  R="$(vm_fork_body sn-u6 sn-u7)"; say "  fork of the old agent's child: $(printf '%s' "$R" | cut -c1-200)"
  eq "S11 upgrade: a child that runs on its parent's image is not forked (code)" "${R%% *}" 409
  case "${R#* }" in *image*|*rootfs*|*disk*) ok "S11 upgrade: the refusal names the image";; *) bad "S11 upgrade: the refusal does not name the image: '${R#* }'";; esac
  eq "S11 upgrade: no sn-u7 left" "$(st sn-u7)" ""
  eq "S11 upgrade: sleep the old agent's child sn-u6" "$(vm_act sn-u6 sleep)" 200
  guest sn-u5 'echo parent-after > /root/parent-after; sync' >/dev/null; sleep 2
  R="$(vm_act_body sn-u6 wake)"; say "  wake of sn-u6 after its parent's image changed: $(printf '%s' "$R" | cut -c1-200)"
  refused "S11 upgrade: wake of a child whose parent's image changed since it slept" "$R" 409 stopped
  eq "S11 upgrade: the parent sn-u5 still runs" "$(st sn-u5) $(vmm_state sn-u5)" "running Running"
  cleanup_prefix sn-u5; cleanup_prefix sn-u6; cleanup_prefix sn-u7
  eq "S11 (new) create sn-u4" "$(vm_create sn-u4 256)" 201; ready S11 sn-u4 150; BU4=$(blob_make sn-u4)
  eq "S11 (new) sleep sn-u4" "$(vm_act sn-u4 sleep)" 200
  PU1=$(vm_pid sn-u1)
  swap_to "$PREV"; eq "S11 rollback: the previous agent runs" "$(sudo sha256sum $BIN | cut -c1-16)" "${PREVSUM:0:16}"
  eq "S11 rollback: sn-u4 (put to sleep by the new agent) is sleeping" "$(st sn-u4)" sleeping
  eq "S11 rollback: sn-u4 wakes under the old agent" "$(vm_act sn-u4 wake)" 200; ready "S11 rollback" sn-u4 60; intact "S11 rollback" sn-u4 "$BU4"
  eq "S11 rollback: sn-u1 (woken by the new agent) is running, same Firecracker" "$(st sn-u1) $(vm_pid sn-u1)" "running $PU1"
  check "S11 rollback: exec on sn-u1" exec_ok sn-u1
  swap_to "$KEEP"; sudo rm -f "$KEEP"
  eq "S11: the new agent is installed again" "$(sudo sha256sum $BIN | cut -c1-16) $(systemctl is-active $UNIT)" "${NEWSUM:0:16} active"
  cleanup_prefix sn-u
fi
fi

if want S12; then
say "== S12. a fork child runs on its own disk (FLU-72) =="
TICKS='setsid nohup sh -c '\''i=0; while :; do i=$((i+1)); echo "$i $(date +%s.%N)" >> /root/ticks; sync; sleep 0.2; done'\'' >/dev/null 2>&1 < /dev/null &'
eq "create sn-fp" "$(vm_create sn-fp 256)" 201; ready S12 sn-fp 150
guest sn-fp "rm -f /root/ticks; $TICKS" >/dev/null; sleep 3
NB="$(guest sn-fp 'wc -l < /root/ticks' | tr -d ' \n')"
eq "S12: fork the running sn-fp -> sn-fk" "$(vm_fork sn-fp sn-fk)" 201
DP="$(vm_dir sn-fp)"; DK="$(vm_dir sn-fk)"
say "  the parent had ${NB} ticks just before the fork"
# (d) exec in both right after the fork
wait_exec sn-fk 15 && ok "S12 (d): exec in the child right after the fork (15 s limit)" || bad "S12 (d): no exec in the child within 15 s"
wait_exec sn-fp 15 && ok "S12 (d): exec in the parent right after the fork (15 s limit)" || bad "S12 (d): no exec in the parent within 15 s"
# (a) open files and Firecracker's own view
own_disk "S12 (a) child" sn-fk
own_disk "S12 (a) parent" sn-fp
# (e) the child's kernel log
eq "S12 (e): ext4 or I/O errors in the child's dmesg" "$(guest sn-fk 'dmesg | grep -iE "EXT4-fs (error|warning)|I/O error|buffer_io_error|blk_update_request" | head -3' | sed '/^$/d' | tr '\n' '|')" ""
say "  child dmesg virtio_blk lines: $(guest sn-fk 'dmesg | grep virtio_blk' | tr '\n' '|' | cut -c1-240)"
# (b) a file in each after the fork
guest sn-fk 'echo child-wrote > /root/from-child; sync' >/dev/null; guest sn-fp 'echo parent-wrote > /root/from-parent; sync' >/dev/null
# The fix across a sleep of the child, and for grandchildren
eq "S12: sleep the child" "$(vm_act sn-fk sleep)" 200; eq "S12: wake the child" "$(vm_act sn-fk wake)" 200
wait_exec sn-fk 30 || bad "S12: the child does not answer exec after its sleep and wake"
own_disk "S12 child after its own sleep and wake" sn-fk
eq "S12: fork the running child -> sn-fg1" "$(vm_fork sn-fk sn-fg1)" 201; wait_exec sn-fg1 30 || bad "S12: sn-fg1 does not answer exec"
own_disk "S12 grandchild of a running child" sn-fg1
own_disk "S12 child after it was forked" sn-fk
eq "S12: sleep the child again" "$(vm_act sn-fk sleep)" 200
eq "S12: fork the sleeping child -> sn-fg2" "$(vm_fork sn-fk sn-fg2)" 201; wait_exec sn-fg2 30 || bad "S12: sn-fg2 does not answer exec"
own_disk "S12 grandchild of a sleeping child" sn-fg2
cleanup_prefix sn-fg
sleep 2
# Stop both, read both images
eq "S12: stop the child" "$(vm_act sn-fk stop)" 200; eq "S12: stop the parent" "$(vm_act sn-fp stop)" 200
wait_no_fc "$DK" 30; wait_no_fc "$DP" 30
img_has "$DK/rootfs.ext4" /root/from-child && ok "S12 (b): the child's file is in the child's image" || bad "S12 (b): the child's file is NOT in the child's image"
img_has "$DP/rootfs.ext4" /root/from-child && bad "S12 (b): the child's file is in the PARENT's image" || ok "S12 (b): the child's file is not in the parent's image"
img_has "$DP/rootfs.ext4" /root/from-parent && ok "S12 (b): the parent's file is in the parent's image" || bad "S12 (b): the parent's file is NOT in the parent's image"
img_has "$DK/rootfs.ext4" /root/from-parent && bad "S12 (b): the parent's later file is in the CHILD's image" || ok "S12 (b): the parent's later file is not in the child's image"
# (c) the tick histories
# The guests were killed, not shut down: what they synced last is in the ext4
# journal and debugfs does not replay it. Read the files out of copies whose
# journal has been replayed (e2fsck -E journal_only), else the count is short.
for x in p:"$DP" k:"$DK"; do
  sudo cp --sparse=always "${x#*:}/rootfs.ext4" "$LOGS/img-${x%%:*}"
  sudo e2fsck -y -E journal_only "$LOGS/img-${x%%:*}" >/dev/null 2>&1
  img_cat "$LOGS/img-${x%%:*}" /root/ticks > "$LOGS/ticks-${x%%:*}"; sudo rm -f "$LOGS/img-${x%%:*}"
done
NP=$(wc -l < "$LOGS/ticks-p" | tr -d ' '); NK=$(wc -l < "$LOGS/ticks-k" | tr -d ' ')
N=$(awk 'NR==FNR{a[FNR]=$0; next} (FNR in a) && a[FNR]==$0 {n=FNR; next} {exit} END{print n+0}' "$LOGS/ticks-p" "$LOGS/ticks-k")
LATER_IN_K=$(tail -n +"$((N+1))" "$LOGS/ticks-p" | grep -cxFf - "$LOGS/ticks-k" 2>/dev/null || true)
say "  parent image: $NP ticks; child image: $NK ticks; identical up to line $N (parent: '$(sed -n "${N}p" "$LOGS/ticks-p")', next: parent '$(sed -n "$((N+1))p" "$LOGS/ticks-p")' child '$(sed -n "$((N+1))p" "$LOGS/ticks-k")')"
[ "$N" -ge "${NB:-1}" ] && ok "S12 (c): the histories are identical up to the fork ($N lines >= the $NB the parent had before it)" || bad "S12 (c): identical only up to line $N, the parent had ${NB} before the fork"
[ $((NK - N)) -ge 5 ] && ok "S12 (c): the child's image has its own later history ($((NK - N)) lines after the fork)" || bad "S12 (c): the child's image has $((NK - N)) lines after the fork, want >= 5 (the child did not write its own image)"
[ $((NP - N)) -ge 5 ] && ok "S12 (c): the parent's image has its own later history ($((NP - N)) lines)" || bad "S12 (c): the parent's image has $((NP - N)) lines after the fork, want >= 5"
eq "S12 (c): the parent's later lines found in the child's image" "${LATER_IN_K:-0}" 0
cleanup_prefix sn-fk; cleanup_prefix sn-fp

say "  -- (f) from a sleeping parent"
eq "create sn-fq" "$(vm_create sn-fq 256)" 201; ready S12 sn-fq 150; BQ=$(blob_make sn-fq)
eq "S12 (f): sleep sn-fq" "$(vm_act sn-fq sleep)" 200
eq "S12 (f): fork the sleeping sn-fq -> sn-fqk" "$(vm_fork sn-fq sn-fqk)" 201
DQ="$(vm_dir sn-fq)"; DQK="$(vm_dir sn-fqk)"
wait_exec sn-fqk 15 && ok "S12 (f): exec in the child right after the fork" || bad "S12 (f): no exec in the child within 15 s"
own_disk "S12 (f) child" sn-fqk
eq "S12 (f): the sleeping parent has no Firecracker" "$(fc_in_dir "$DQ" | wc -l | tr -d ' ')" 0
guest sn-fqk 'echo child-wrote > /root/from-child; sync' >/dev/null
eq "S12 (f): stop the child" "$(vm_act sn-fqk stop)" 200; wait_no_fc "$DQK" 30
img_has "$DQK/rootfs.ext4" /root/from-child && ok "S12 (f): the child's file is in the child's image" || bad "S12 (f): the child's file is NOT in the child's image"
img_has "$DQ/rootfs.ext4" /root/from-child && bad "S12 (f): the child's file is in the sleeping PARENT's image" || ok "S12 (f): the child's file is not in the parent's image"
eq "S12 (f): wake the parent" "$(vm_act sn-fq wake)" 200; ready "S12 (f)" sn-fq 60; intact "S12 (f)" sn-fq "$BQ"
eq "S12 (f): the parent does not see the child's file" "$(guest sn-fq 'test -e /root/from-child && echo yes || echo no' | tr -d '\n')" no
guest sn-fq 'echo parent-wrote > /root/from-parent; sync' >/dev/null
eq "S12 (f): stop the parent" "$(vm_act sn-fq stop)" 200; wait_no_fc "$DQ" 30
img_has "$DQ/rootfs.ext4" /root/from-parent && ok "S12 (f): the parent's file is in the parent's image" || bad "S12 (f): the parent's file is NOT in the parent's image"
img_has "$DQK/rootfs.ext4" /root/from-parent && bad "S12 (f): the parent's later file is in the CHILD's image" || ok "S12 (f): the parent's later file is not in the child's image"
cleanup_prefix sn-fq
fi

if want S13; then
say "== S13. a sleep is durable when it is acknowledged =="
eq "create sn-dur" "$(vm_create sn-dur 256)" 201; ready S13 sn-dur 150; DU="$(vm_dir sn-dur)"; PD=$(vm_pid sn-dur)
C="$(vm_act sn-dur sleep)"
REC="$(sudo cat "$DU/meta.json" | jq -r '"\(.state) \(.snapshot_valid // false)"')"; FILES="$(snap_files "$DU")"; LIVE="$(alive "$PD" && echo alive || echo gone)"
eq "S13: sleep" "$C" 200
eq "S13: meta.json right after the 200" "$REC" "sleeping true"
eq "S13: the files right after the 200" "$FILES" "vmstate=yes mem=yes tmp=no"
eq "S13: its Firecracker (pid $PD) right after the 200" "$LIVE" gone
vm_del sn-dur >/dev/null
fi

say "== end =="
cleanup_prefix sn-
i=0; until { [ "$(fc_count)" -eq "$FC0" ] && [ "$(dir_count)" -eq "$DC0" ]; } || [ $i -ge 60 ]; do sleep 1; i=$((i+1)); done
eq "instance directories as at the start" "$(dir_count)" "$DC0"
eq "Firecrackers as at the start" "$(fc_count)" "$FC0"
eq "no Firecracker without a record" "$(orphans | wc -l | tr -d ' ')" 0
eq "the agent binary is the one installed at the start" "$(sudo test -f "$KEEP" && echo swapped || echo kept)" kept
summary
