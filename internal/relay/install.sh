#!/bin/sh
# Runs on the relay machine as root, from the directory sp relay deploy copied
# its files to: sp, relay.list, env, sp-relay.service.
set -eu
cd "$(dirname "$0")"

install -m 755 sp /usr/local/bin/sp
install -d -m 755 /etc/sp-relay
install -m 644 relay.list /etc/sp-relay/relay.list
install -m 600 env /etc/sp-relay/env
install -m 644 sp-relay.service /etc/systemd/system/sp-relay.service

systemctl daemon-reload
systemctl enable sp-relay >/dev/null
systemctl restart sp-relay
sleep 2
if ! systemctl is-active --quiet sp-relay; then
  echo "sp-relay did not start:" >&2
  journalctl -u sp-relay -n 20 --no-pager >&2
  exit 1
fi
echo "sp-relay is running"

# Each check goes through the relay, which waits out a sleeping sprite's wake.
addr=$(sed -n 's/^TAILNET_ADDR=//p' env)
echo "Checking each sprite through the relay..."
while read -r port name _; do
  case "$port" in ''|\#*) continue ;; esac
  code=$(curl -s -o /dev/null --max-time 60 -w '%{http_code}' "http://$addr:$port/.well-known/t3/environment" || true)
  printf '  %-28s port %s  [%s]\n' "$name" "$port" "$code"
done < relay.list
