# Local-only: on the agent host, every instances/*/meta.json must have the
# exact key set {id,name,dir_id,vcpus,mem_mib,pid,slot,ip,state} with nullable
# fields present-as-null (the Rust agent's adoption contract).
cf_inst_dir="${AGENT_DATA_DIR:-}/instances"
if [ -z "${AGENT_DATA_DIR:-}" ]; then
  skip "AGENT_DATA_DIR not set (local-only case, run on the agent host)"
elif ! sudo test -d "$cf_inst_dir" 2>/dev/null; then
  skip "$cf_inst_dir not present on this host"
else
  cf_metas=$(sudo find "$cf_inst_dir" -maxdepth 2 -name meta.json 2>/dev/null)
  if [ -z "$cf_metas" ]; then
    skip "no meta.json files under $cf_inst_dir"
  else
    cf_meta_ok=1
    for m in $cf_metas; do
      if ! sudo cat "$m" | jq -e '
          (keys | sort) == ["dir_id","id","ip","mem_mib","name","pid","slot","state","vcpus"]
          and (.id | type == "string") and (.name | type == "string")
          and (.dir_id | type == "string")
          and (.vcpus | type == "number") and (.mem_mib | type == "number")
          and ((.pid | type) | IN("number", "null"))
          and ((.slot | type) | IN("number", "null"))
          and ((.ip | type) | IN("string", "null"))
          and (.state | IN("creating","running","paused","stopped","sleeping","error","pooled"))
        ' >/dev/null 2>&1; then
        bad "meta.json shape violation: $m -> $(sudo cat "$m")"
        cf_meta_ok=0
      fi
    done
    [ "$cf_meta_ok" = "1" ] && ok "all meta.json files conform ($(printf '%s\n' "$cf_metas" | wc -l | tr -d ' ') checked)"
  fi
fi
