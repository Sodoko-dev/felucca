#!/bin/sh
# odoo-v18 provision script — Odoo 18 CE + Postgres compose stack with images
# pre-pulled, so first boot is seconds instead of a multi-GB pull.
# Build ON TOP of docker-base: build-template.sh --base docker-base --disk-gb 12.
set -eu

# Overwrite unconditionally (rm first — see docker-base.sh: a stub symlink
# would swallow the bytes into tmpfs).
rm -f /etc/resolv.conf
printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf

# dockerd must be running to pull (the docker-base template starts it at
# boot; this guard covers a builder booted from a fresh base).
if ! docker info >/dev/null 2>&1; then
  nohup dockerd >/var/log/dockerd.log 2>&1 &
  for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
fi
docker info >/dev/null 2>&1 || { echo "dockerd never came up" >&2; exit 1; }

mkdir -p /opt/odoo
cat > /opt/odoo/compose.yaml <<'YAML'
services:
  db:
    image: postgres:16
    environment:
      - POSTGRES_USER=odoo
      - POSTGRES_PASSWORD=${POSTGRES_PASSWORD:?set per sandbox by odoo-stack-env}
      - POSTGRES_DB=postgres
    volumes:
      - odoo-db:/var/lib/postgresql/data
  odoo:
    image: odoo:18
    depends_on:
      - db
    ports:
      - "${ODOO_BIND:?set per sandbox by odoo-stack-env}:8069:8069"   # web
      - "${ODOO_BIND:?set per sandbox by odoo-stack-env}:8072:8072"   # longpolling / websocket
    environment:
      - HOST=db
      - USER=odoo
      - PASSWORD=${POSTGRES_PASSWORD:?set per sandbox by odoo-stack-env}
    volumes:
      - odoo-web:/var/lib/odoo
volumes:
  odoo-db: {}
  odoo-web: {}
YAML

# Everything this script writes is captured into the template rootfs, so the
# database password cannot be created here: every sandbox any tenant forks from
# this template would ship the same one, and a tenant with root in their own VM
# (the expected design) would read it out of /opt/odoo and hold the credential
# for every other tenant's odoo fleet-wide. It is minted per sandbox on first
# boot instead, by the helper below.
cat > /usr/local/bin/odoo-stack-env <<'ENVSH'
#!/bin/sh
# Per-sandbox odoo credentials and publish address. Run before every start of
# the stack: the password is minted once and then preserved, the publish
# address is recomputed each time because a forked sandbox is re-IP'd and a
# value baked at first boot would go stale.
set -eu

ENV_FILE=/opt/odoo/.env
umask 077

pw=""
[ -f "$ENV_FILE" ] && pw="$(sed -n 's/^POSTGRES_PASSWORD=//p' "$ENV_FILE" | head -1)"
[ -n "$pw" ] || pw="$(od -An -vtx1 -N24 /dev/urandom | tr -d ' \n')"

# 0.0.0.0 would publish odoo on every interface this sandbox has or later
# gains; loopback is the fail-closed answer when the address cannot be resolved.
bind="${ODOO_BIND:-}"
[ -n "$bind" ] || bind="$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)"
if [ -z "$bind" ]; then
  bind=127.0.0.1
  echo "odoo-stack: guest address not resolvable — publishing on ${bind} only." >&2
  echo "odoo-stack: set ODOO_BIND=<address> in the unit environment for ingress." >&2
fi

{ printf 'POSTGRES_PASSWORD=%s\n' "$pw"; printf 'ODOO_BIND=%s\n' "$bind"; } > "$ENV_FILE"
chmod 0600 "$ENV_FILE"
ENVSH
chmod 0755 /usr/local/bin/odoo-stack-env

# Never capture an .env into the image — a rebuild on top of a booted sandbox
# would otherwise bake that sandbox's password into the new template.
rm -f /opt/odoo/.env

# The pull only needs the compose file to interpolate; these values live in the
# pull's environment and are never written to disk.
POSTGRES_PASSWORD=pull-time-placeholder ODOO_BIND=127.0.0.1 \
  docker compose -f /opt/odoo/compose.yaml pull

# Start the stack at boot (idempotent; volumes persist across sleep/wake/fork).
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  cat > /etc/systemd/system/odoo-stack.service <<'EOF'
[Unit]
Description=Odoo compose stack
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
RemainAfterExit=true
ExecStartPre=/usr/local/bin/odoo-stack-env
ExecStart=/usr/bin/docker compose --env-file /opt/odoo/.env -f /opt/odoo/compose.yaml up -d
ExecStop=/usr/bin/docker compose --env-file /opt/odoo/.env -f /opt/odoo/compose.yaml down

[Install]
WantedBy=multi-user.target
EOF
  systemctl enable odoo-stack
else
  cat > /etc/rc.local <<'EOF'
#!/bin/sh
[ -x /usr/bin/dockerd ] && nohup dockerd >/var/log/dockerd.log 2>&1 &
i=0; while [ $i -lt 30 ]; do docker info >/dev/null 2>&1 && break; sleep 1; i=$((i+1)); done
/usr/local/bin/odoo-stack-env
docker compose --env-file /opt/odoo/.env -f /opt/odoo/compose.yaml up -d || true
exit 0
EOF
  chmod +x /etc/rc.local
fi

docker image ls
