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
      - POSTGRES_PASSWORD=odoo
      - POSTGRES_DB=postgres
    volumes:
      - odoo-db:/var/lib/postgresql/data
  odoo:
    image: odoo:18
    depends_on:
      - db
    ports:
      - "8069:8069"   # web
      - "8072:8072"   # longpolling / websocket
    environment:
      - HOST=db
      - USER=odoo
      - PASSWORD=odoo
    volumes:
      - odoo-web:/var/lib/odoo
volumes:
  odoo-db: {}
  odoo-web: {}
YAML

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
ExecStart=/usr/bin/docker compose -f /opt/odoo/compose.yaml up -d
ExecStop=/usr/bin/docker compose -f /opt/odoo/compose.yaml down

[Install]
WantedBy=multi-user.target
EOF
  systemctl enable odoo-stack
else
  cat > /etc/rc.local <<'EOF'
#!/bin/sh
[ -x /usr/bin/dockerd ] && nohup dockerd >/var/log/dockerd.log 2>&1 &
i=0; while [ $i -lt 30 ]; do docker info >/dev/null 2>&1 && break; sleep 1; i=$((i+1)); done
docker compose -f /opt/odoo/compose.yaml up -d || true
exit 0
EOF
  chmod +x /etc/rc.local
fi

docker image ls
