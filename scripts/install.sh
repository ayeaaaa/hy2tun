#!/usr/bin/env bash
# Install a released hy2tun binary and systemd unit.
#
# Run from an extracted release archive root:
#   sudo ./scripts/install.sh server
#   sudo ./scripts/install.sh client
#
# Existing /etc/hy2tun configuration is never overwritten.
set -euo pipefail

role=${1:-}
case "$role" in
  server|client) ;;
  *)
    echo "usage: $0 server|client" >&2
    exit 2
    ;;
esac

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

if [ "$role" = "server" ]; then
  binary=hy2tund
  unit=hy2tund.service
  example=server.yaml
else
  binary=hy2tun
  unit=hy2tun.service
  example=client.yaml
fi

[ -x "./$binary" ] || {
  echo "missing ./$binary; run this from an extracted release archive" >&2
  exit 1
}
[ -f "scripts/systemd/$unit" ] || {
  echo "missing scripts/systemd/$unit" >&2
  exit 1
}

install -d -m 0755 /etc/hy2tun /var/lib/hy2tun
install -m 0755 "./$binary" "/usr/local/bin/$binary"
install -m 0644 "scripts/systemd/$unit" "/etc/systemd/system/$unit"

if [ ! -f "/etc/hy2tun/$example" ]; then
  install -m 0600 "examples/$example" "/etc/hy2tun/$example"
  echo "created /etc/hy2tun/$example; edit it before starting the service"
fi

systemctl daemon-reload
systemctl enable "$unit"

echo "installed $binary and $unit"
echo "edit /etc/hy2tun/$example, then run: systemctl start $unit"
