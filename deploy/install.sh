#!/usr/bin/env bash
set -euo pipefail

root=/home/tray/project/jeff
unit="$root/deploy/jeff.service"

[[ -x "$root/bin/jeff" ]] || { echo "missing executable: $root/bin/jeff" >&2; exit 1; }
[[ -f "$root/.env" ]] || { echo "missing environment file: $root/.env" >&2; exit 1; }
[[ -f "$root/config.yaml" ]] || { echo "missing config file: $root/config.yaml" >&2; exit 1; }

install -m 0644 "$unit" /etc/systemd/system/jeff.service
systemctl daemon-reload
systemctl enable --now jeff.service
