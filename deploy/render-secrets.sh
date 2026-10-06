#!/usr/bin/env bash
set -euo pipefail

: "${OCI_CLI_CONFIG_FILE:=/etc/ai-proxy/oci/config}"
: "${OCI_VAULT_ID:=ocid1.vault.oc1.eu-frankfurt-1.envlmxzraajjm.abtheljtih4bfnjucm5t73kmdb6tczsluu2prgsig4c4mcm3k2zno6hwgtaq}"
export OCI_CLI_CONFIG_FILE

deploy_dir=$(dirname "$(readlink -f "$0")")
out_dir=${1:-/run/ai-proxy}

fetch_secret() {
  oci secrets secret-bundle get-secret-bundle-by-name \
    --vault-id "$OCI_VAULT_ID" \
    --secret-name "$1" \
    --query 'data."secret-bundle-content".content' \
    --raw-output | base64 -d
}

json_string() {
  jq -Rn --arg value "$1" '$value'
}

umask 077

access_token=$(fetch_secret ai-proxy-access-key)
LLM_ACCESS_TOKEN=$(json_string "$access_token")
LITELLM_KEY_HASH=$(printf %s "$access_token" | sha256sum | cut -d' ' -f1)
LITELLM_CLIENT_ID=$({ printf 'client\0'; printf %s "$access_token"; } | sha256sum | cut -d' ' -f1)
export LLM_ACCESS_TOKEN LITELLM_KEY_HASH LITELLM_CLIENT_ID

envsubst '${LLM_ACCESS_TOKEN} ${LITELLM_KEY_HASH} ${LITELLM_CLIENT_ID}' < "$deploy_dir/config.yaml.tmpl" > "$out_dir/config.yaml.new"
mv "$out_dir/config.yaml.new" "$out_dir/config.yaml"

{
  echo "MANAGEMENT_PASSWORD=$(fetch_secret cli-proxy-api-management-password)"
  echo "LITELLM_ACCOUNTING_ADMIN_KEY=$(fetch_secret litellm-masterkey)"
  echo "GRAFANA_CLOUD_LOGS_TOKEN=$(fetch_secret grafana-alloy-token)"
} > "$out_dir/env.new"
mv "$out_dir/env.new" "$out_dir/env"
