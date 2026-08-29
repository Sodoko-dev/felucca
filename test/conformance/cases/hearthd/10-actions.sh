# shellcheck shell=bash
# Lifecycle actions are empty-200 and the state transitions are observable:
# pause/resume on the parent, stop/start (cold boot) on the fork child.
hd POST "/api/v1/sandboxes/$CF_SB_ID/pause"
assert_status 200 "POST .../pause"
assert_body_empty "pause response"
hd GET "/api/v1/sandboxes/$CF_SB_ID"
assert_jq '.state == "paused"' "paused after pause"

# v3.1 regression: start on a paused sandbox is refused; hearthd forwards the
# agent's 409 instead of mapping it to 502.
hd POST "/api/v1/sandboxes/$CF_SB_ID/start"
assert_status 409 "POST .../start on paused refused"
hd GET "/api/v1/sandboxes/$CF_SB_ID"
assert_jq '.state == "paused"' "still paused after refused start"

hd POST "/api/v1/sandboxes/$CF_SB_ID/resume"
assert_status 200 "POST .../resume"
assert_body_empty "resume response"
hd GET "/api/v1/sandboxes/$CF_SB_ID"
assert_jq '.state == "running"' "running after resume"

hd POST "/api/v1/sandboxes/$CF_CHILD_ID/stop"
assert_status 200 "POST .../stop"
assert_body_empty "stop response"
hd GET "/api/v1/sandboxes/$CF_CHILD_ID"
assert_jq '.state == "stopped"' "stopped after stop"

hd POST "/api/v1/sandboxes/$CF_CHILD_ID/start"
assert_status 200 "POST .../start"
assert_body_empty "start response"
hd GET "/api/v1/sandboxes/$CF_CHILD_ID"
assert_jq '.state == "running"' "running after start"
