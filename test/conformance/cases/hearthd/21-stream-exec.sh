# Streamed exec (v4 P5.1): POST /sandboxes/{id}/exec?stream=1 returns SSE
# whose data: payloads are JSON frames — {"stream":"stdout"|"stderr","data"}
# chunks, then one terminal {"done":true,...}. The buffered path must be
# byte-for-byte unaffected. Uses the suite's long-lived sandbox CF_SB_ID
# (created by 05, deleted by 11)... which is gone by now — create our own.
CF_SE_RUN="$$"
CF_SE_SB=""

hd POST /api/v1/sandboxes "{\"name\":\"cf-stream-$CF_SE_RUN\"}"
assert_status 201 "POST /api/v1/sandboxes (stream-exec)"
CF_SE_SB=$(printf '%s' "$R_BODY" | jq -r '.id // empty')

cf_se_wait_exec() { # until the guest agent answers
  local i=0
  while [ "$i" -lt 45 ]; do
    hd POST "/api/v1/sandboxes/$CF_SE_SB/exec" '{"cmd":["true"],"timeout_ms":2000}' 2>/dev/null
    printf '%s' "$R_BODY" | grep -q '"ok":true' && return 0
    sleep 2; i=$((i+1))
  done
  return 1
}

if [ -n "$CF_SE_SB" ]; then
  if cf_se_wait_exec; then
    ok "stream-exec guest agent ready"
  else
    bad "stream-exec guest agent never answered"
  fi

  # Happy path: stdout + stderr chunks, then a done frame with the exit code.
  hd POST "/api/v1/sandboxes/$CF_SE_SB/exec?stream=1" \
    '{"cmd":["/bin/sh","-c","echo out-line; echo err-line 1>&2; exit 4"],"timeout_ms":15000}'
  assert_status 200 "streamed exec -> 200"
  # Frames are parsed with jq — key order inside a frame is not contract.
  if printf '%s' "$R_BODY" | grep '^data: ' | sed 's/^data: //' | \
     jq -e -s '[.[] | select(.stream == "stdout" and (.data | contains("out-line")))] | length >= 1' >/dev/null; then
    ok "stdout chunk frame present"
  else
    bad "stdout chunk frame missing: $(printf '%s' "$R_BODY" | head -c 300)"
  fi
  if printf '%s' "$R_BODY" | grep '^data: ' | sed 's/^data: //' | \
     jq -e -s '[.[] | select(.stream == "stderr" and (.data | contains("err-line")))] | length >= 1' >/dev/null; then
    ok "stderr chunk frame present"
  else
    bad "stderr chunk frame missing"
  fi
  cf_se_done=$(printf '%s' "$R_BODY" | grep '^data: ' | tail -1 | sed 's/^data: //')
  if printf '%s' "$cf_se_done" | jq -e '.done == true and .ok == true and .exit_code == 4' >/dev/null 2>&1; then
    ok "terminal done frame carries exit_code 4"
  else
    bad "bad terminal frame: $cf_se_done"
  fi

  # Multi-chunk ordering: two writes with a pause arrive as separate frames
  # in order (the buffered transcript still proves frame order end-to-end).
  hd POST "/api/v1/sandboxes/$CF_SE_SB/exec?stream=1" \
    '{"cmd":["/bin/sh","-c","printf first; sleep 1; printf second"],"timeout_ms":15000}'
  assert_status 200 "streamed exec (two writes) -> 200"
  cf_se_all=$(printf '%s' "$R_BODY" | grep '^data: ' | sed 's/^data: //' | jq -r 'select(.stream == "stdout") | .data' | tr -d '\n')
  if [ "$cf_se_all" = "firstsecond" ]; then
    ok "stdout chunks concatenate in order"
  else
    bad "stdout chunks wrong: $cf_se_all"
  fi
  cf_se_frames=$(printf '%s' "$R_BODY" | grep -c '"stream":"stdout"' || true)
  if [ "$cf_se_frames" -ge 2 ]; then
    ok "paused writes arrive as separate frames ($cf_se_frames)"
  else
    bad "expected >=2 stdout frames, got $cf_se_frames"
  fi

  # Guest-side timeout surfaces as exit_code 124 in the done frame.
  hd POST "/api/v1/sandboxes/$CF_SE_SB/exec?stream=1" \
    '{"cmd":["/bin/sh","-c","echo early; sleep 30"],"timeout_ms":2000}'
  assert_status 200 "streamed exec with timeout -> 200"
  cf_se_done=$(printf '%s' "$R_BODY" | grep '^data: ' | tail -1 | sed 's/^data: //')
  if printf '%s' "$cf_se_done" | jq -e '.done == true and .exit_code == 124' >/dev/null 2>&1; then
    ok "guest timeout -> done frame exit_code 124"
  else
    bad "timeout terminal frame: $cf_se_done"
  fi

  # Buffered exec unchanged (regression guard for the frozen v2 shape).
  hd POST "/api/v1/sandboxes/$CF_SE_SB/exec" '{"cmd":["/bin/sh","-c","echo hi"]}'
  assert_status 200 "buffered exec still 200"
  assert_jq '.ok == true and .exit_code == 0 and .stdout == "hi\n"' "buffered exec shape unchanged"

  # Streamed exec against a stopped sandbox -> 409 JSON (not SSE).
  hd POST "/api/v1/sandboxes/$CF_SE_SB/stop"
  assert_status 200 "stop stream-exec sandbox"
  hd POST "/api/v1/sandboxes/$CF_SE_SB/exec?stream=1" '{"cmd":["true"]}'
  assert_status 409 "streamed exec on stopped sandbox -> 409"
  assert_jq '.error == "not running"' "409 body is plain JSON"

  hd DELETE "/api/v1/sandboxes/$CF_SE_SB"
  assert_status 204 "DELETE stream-exec sandbox"
fi
