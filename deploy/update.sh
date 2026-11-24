#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(dirname "$(readlink -f "$0")")
image=ghcr.io/vaynerakawalo/ai-proxy:${AI_PROXY_TAG:-latest}

pull_attempts=3
pull_backoff_seconds=${AI_PROXY_PULL_BACKOFF_SECONDS:-20}

pull_with_retries() {
  local attempt
  for attempt in $(seq "$pull_attempts"); do
    docker pull --quiet "$image" > /dev/null && return 0
    if [ "$attempt" -lt "$pull_attempts" ]; then
      echo "pull attempt $attempt of $pull_attempts failed, retrying" >&2
      sleep $((attempt * pull_backoff_seconds))
    fi
  done
  return 1
}

before=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true)
pull_with_retries
after=$(docker image inspect --format '{{.Id}}' "$image")

if [ "$before" != "$after" ]; then
  systemctl restart ai-proxy.service

  "$deploy_dir/with-changefeed-token" "$deploy_dir/report-deploy" ai-proxy \
    "$(docker image inspect --format '{{join .RepoDigests ","}}' "$image")" \
    || echo "could not report the ai-proxy deploy" >&2
fi
