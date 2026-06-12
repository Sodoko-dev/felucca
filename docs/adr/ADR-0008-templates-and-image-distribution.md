# ADR-0008 — Templates, image distribution, and per-template warm pools

- Status: Accepted
- Date: 2026-06-12
- Context: fifth phase (P4, "Sodoko workloads") of the v4 plan. Sandboxes must
  boot from custom rootfs images (docker-base, odoo-v18) with bigger shapes
  (up to 16 vCPU / 32 GiB / 128 GB disk), across the mixed fleet — including
  workers reachable only over the P2 overlay. Related: ADR-0004 (tenancy +
  store), ADR-0005 (guest networking), ADR-0007 (ingress), `docs/PLAN-v4.md`
  Phase 4.

## Decision

1. **A template is a store row + one image file on hearthd.** `templates`
   (id, unique name, image, image_sha256, vcpus, mem_mib, disk_gb,
   image_size_gb, pool_size, created_at). The image is
   `<images_dir>/<image>.ext4` (config `images_dir`, default
   `/var/lib/hearth/images`). Template names use the expose-label grammar
   (ADR-0007) — they double as image names and may surface in label-shaped
   contexts, so the reserved forms ("--", all-digits) apply. Image names are
   a strict path-component class (`[a-z0-9._-]`, no leading `.`/`-`, no
   `..`), validated identically on both sides of the wire before any
   filesystem use.
2. **Capture IS the image pipeline.** `POST /api/v1/templates
   {name, from_sandbox}` streams a STOPPED sandbox's rootfs from its agent
   (`GET /v1/vms/{id}/rootfs`, stopped-only, capture-guarded against
   concurrent start) into the images dir, hashing in flight; `image_file`
   registers a pre-provisioned image instead (e.g. ubuntu-base). The agent's
   `stop` waits (SIGTERM→SIGKILL, bounded) for the FC process to exit before
   reporting stopped, so a capture can never read a rootfs that is still
   being flushed. `scripts/build-template.sh` drives the whole loop over the
   public API: boot builder → upload provision script (base64 chunks over
   exec) → run it via nohup+poll (not bound by the 5-minute exec cap) →
   stop → capture → delete builder. First provision scripts:
   `deploy/templates/docker-base.sh`, `deploy/templates/odoo-v18.sh`.
3. **Distribution is pull-and-cache, sha256-addressed.** The agent caches
   images in its existing `{data_dir}/images/` layout with a `.sha256`
   sidecar written only after a verified download. On create, a missing or
   sha-mismatched image is pulled from
   `GET /api/v1/images/{name}` (admin/node-token only) via the established
   curl-on-stdin pattern (token never on argv). A cached file without a
   sidecar but with an expected sha is re-hashed and self-heals its sidecar
   (crash-safe ordering: verified sidecar lands before the image rename).
   Downloads serialize per image (not globally) so a multi-GB pull never
   blocks creates whose image is already cached. hearthd pushes
   `POST /v1/images/prefetch` to ready nodes at template creation so the
   common first-create path finds a warm cache; a cold-node create may still
   time out hearthd's 30s agent call — the agent's create is
   cancellation-safe (spawned, converges to running/error) and the retry
   finds the cache.
4. **disk_gb is a grow-only resize with an honest floor.** The agent copies
   the image then `truncate + e2fsck -fp + resize2fs` when disk_gb exceeds
   the image size; shrinking is refused. hearthd enforces the same floor at
   the API (`image_size_gb`, recorded at template creation): a template's
   disk_gb defaults to its image size, and per-sandbox overrides below the
   floor are 400s. Disk quota (`max_disk_gb`, P0 schema) charges
   `EffectiveDiskGB` — the declared disk_gb, or the 2 GiB base image when
   unset — so big-image templates cannot under-count.
5. **Warm pools are per-template specs pushed to agents.** `PUT /v1/pools`
   carries `[{image, image_sha256, vcpus, mem_mib, disk_gb, count}]`; the
   agent's refill loop tops up AND drains (a pooled VM matching no current
   spec — including a stale sha after a re-capture — is deleted; an empty
   push tears all template pools down). Claims match the full shape
   including sha, so a re-captured template can never serve a stale pooled
   rootfs. The push has exactly one channel: hearthd pushes on template
   create/delete and to each node right after it registers (the register
   response stays the frozen `{"id":...}`). The legacy `pool_size` config
   remains as an always-present default spec (ubuntu-base 1c/256), so
   pre-P4 behavior is unchanged. Prewarm failures are deleted (no record
   leak) and backed off per spec (no 5s re-download loops).
6. **The catalog is global-read, admin-write.** Tenants can list templates
   (they need the catalog to create from it) and create sandboxes from any
   template; mutations and raw image downloads are admin/node-token only
   (tenant keys get 404s). Per-template tenant visibility is deliberately
   deferred — templates are admin-curated in v1, and captures of
   tenant-owned sandboxes are an admin responsibility until a visibility
   field exists (P5 candidate).

## Rejected alternatives

- **Node-push distribution (hearthd uploads to every node).** The fleet is
  the variable: NAT'd home-lab workers come and go, and a push model needs
  fleet-wide success tracking. Pull-on-miss + prefetch hint needs none.
- **Registry-style content addressing (`<sha>.ext4` files).** The existing
  image-dir layout is name-addressed and shared with firecracker-assets.sh;
  sidecars give re-capture detection without breaking that layout or the
  ubuntu-base bootstrap.
- **Template pools in agent config files.** Pool shapes belong to the
  template entity (the catalog is the source of truth); config-only pools
  would drift per node and survive template deletion.
- **Capture from running/sleeping sandboxes.** A live FC owns the rootfs
  (dirty pages, torn ext4); sleeping VMs hold un-flushed page cache in the
  memory snapshot. Stopped-only keeps the sha meaningful.
- **Build pipeline as a hearthd-internal job queue.** A script over the
  public API exercises the same surface users get, needs no new job
  machinery, and the capture endpoint is the only new primitive required.

## Consequences

- Sodoko's odoo-v18 path: build docker-base once, build odoo-v18 on top
  (images pre-pulled in the template), create → compose stack serves in
  seconds, expose via ADR-0007, fork clones the running stack.
- Images are whole-file copies (no layering); a 12 GB template costs 12 GB
  per node cache and per transfer. Acceptable at current fleet size;
  content-defined chunking is a future optimization, not a contract change.
- The scheduler still places by vm_count, not free disk/mem — a node can
  run out of disk for big templates (FC create fails, sandbox errors).
  Capacity-aware placement lands with P6 HA/reschedule.
- `node.pool_size` in heartbeats/metrics now counts ALL pooled VMs (legacy +
  template pools) — dashboards keyed to the old single-pool meaning read
  higher numbers.

## Deferred (recorded during the P4 review loop)

- **Per-template tenant visibility / ownership** (P5): catalog is global;
  a visibility field should make the policy explicit in code.
- **Disk-capacity-aware scheduling** (P6): PickNode ignores free disk.
- **Template image GC for orphaned pulls on workers**: agent caches are
  append-only until P5 idle/GC policies own retention.
- **Resumable/delta image transfer**: whole-file HTTP today; revisit if
  template sizes or fleet counts make it the bottleneck.
