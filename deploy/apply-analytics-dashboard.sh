#!/usr/bin/env bash
set -euo pipefail

: "${ANALYTICS_URL:?Set the analytics base URL}"
: "${ANALYTICS_DASHBOARD_TOKEN:?Set an unrestricted API token belonging to the dashboard owner}"

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
definition="$repo_root/dashboards/llm-usage.json"
dashboard_id=$(jq -er '.id | select(test("^[a-z0-9-]+$"))' "$definition")

response=$(mktemp)
trap 'rm -f "$response"' EXIT

status=$(curl --silent --show-error --fail-with-body \
  --request PUT \
  --header "Authorization: Bearer $ANALYTICS_DASHBOARD_TOKEN" \
  --header 'Content-Type: application/json' \
  --data-binary "@$definition" \
  --output "$response" \
  --write-out '%{http_code}' \
  "${ANALYTICS_URL%/}/api/dashboards/$dashboard_id") || {
  cat "$response" >&2
  exit 1
}

case "$status" in
  200|201) printf 'Applied dashboard %s\n' "$dashboard_id" ;;
  *) printf 'Unexpected analytics response HTTP %s\n' "$status" >&2; exit 1 ;;
esac
