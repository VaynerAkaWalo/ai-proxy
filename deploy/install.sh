#!/usr/bin/env bash
set -euo pipefail

: "${AI_PROXY_USER:=server}"
: "${UNIT_DIR:=/etc/systemd/system}"

deploy_dir=$(dirname "$(readlink -f "$0")")

for unit in ai-proxy.service ai-proxy-update.service; do
  sed -e "s#@DEPLOY_DIR@#$deploy_dir#g" -e "s#@USER@#$AI_PROXY_USER#g" "$deploy_dir/$unit" > "$UNIT_DIR/$unit"
done
cp "$deploy_dir/ai-proxy-update.timer" "$UNIT_DIR/"

if [ -z "${SKIP_SYSTEMCTL:-}" ]; then
  systemctl daemon-reload
  systemctl enable --now ai-proxy.service ai-proxy-update.timer
fi
