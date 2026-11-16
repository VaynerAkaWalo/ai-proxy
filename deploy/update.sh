#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(dirname "$(readlink -f "$0")")
image=ghcr.io/vaynerakawalo/ai-proxy:${AI_PROXY_TAG:-latest}

before=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true)
docker pull --quiet "$image" > /dev/null
after=$(docker image inspect --format '{{.Id}}' "$image")

if [ "$before" != "$after" ]; then
  systemctl restart ai-proxy.service

  digest=$(docker image inspect --format '{{join .RepoDigests ","}}' "$image")
  "$deploy_dir/with-changefeed-token" "$deploy_dir/report-deploy" ai-proxy "$digest" \
    || echo "could not report the ai-proxy deploy" >&2
fi
