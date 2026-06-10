# Semantic normalizer for Hearth API JSON (used with `jq -S`).
# Erases volatile values but *asserts their type/format*: a wrong type or a
# null-vs-omitted drift produces a <BAD-*> marker or a missing key, which the
# golden diff catches. Key order is irrelevant (-S sorts); key SET is strict.

def nid:
  if . == null then null
  elif (type == "string" and test("^(sb|node)-[0-9a-f]{8}-[0-9]+$")) then "<ID>"
  else "<BAD-ID:\(tostring)>" end;

def nnum(tag):
  if . == null then null
  elif type == "number" then tag
  else "<BAD-NUM:\(tostring)>" end;

def naddr:
  if . == null then null
  elif (type == "string" and test("^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+(:[0-9]+)?$")) then "<ADDR>"
  else "<BAD-ADDR:\(tostring)>" end;

def nstr:
  if type == "string" then "<STR>" else "<BAD-STR:\(tostring)>" end;

walk(
  if type == "object" then
    with_entries(
      if   .key == "id" or .key == "node_id" or .key == "parent_id" then .value |= nid
      elif .key == "created_at" or .key == "last_heartbeat"          then .value |= nnum("<TS>")
      elif .key == "wake_ms"                                          then .value |= nnum("<MS>")
      elif .key == "ip" or .key == "addr"                             then .value |= naddr
      elif .key == "hostname"                                         then .value |= nstr
      elif .key == "cpus" or .key == "mem_total_mib" or .key == "mem_free_mib"
        or .key == "vm_count" or .key == "pool_size"
        or .key == "pid" or .key == "slot"                            then .value |= nnum("<DYN>")
      else . end)
  else . end)
