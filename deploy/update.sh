#!/usr/bin/env bash
set -euo pipefail

image=ghcr.io/vaynerakawalo/ai-proxy:${AI_PROXY_TAG:-latest}

before=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true)
docker pull --quiet "$image" > /dev/null
after=$(docker image inspect --format '{{.Id}}' "$image")

if [ "$before" != "$after" ]; then
  systemctl restart ai-proxy.service
fi
