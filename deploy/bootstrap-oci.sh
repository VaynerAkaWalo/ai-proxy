#!/usr/bin/env bash
set -euo pipefail

: "${OCI_DIR:=/etc/ai-proxy/oci}"
: "${AI_PROXY_USER:=server}"
: "${OCI_USER_ID:=ocid1.user.oc1..aaaaaaaa6easvcjldewoc5lkwaaca2s75fag7ckh7spfgllpo42kl5dru4uq}"
: "${OCI_TENANCY_ID:=ocid1.tenancy.oc1..aaaaaaaa6tn2jsijsn2tnxhhz2o6yrj6btebigr2igvddqab2ex75ajkpz5a}"
: "${OCI_REGION:=eu-frankfurt-1}"

credentials=$(cat)

install -d -m 700 -o "$AI_PROXY_USER" "$OCI_DIR"
umask 077

jq -er .private_key_pem <<< "$credentials" > "$OCI_DIR/key.pem"
fingerprint=$(jq -er .fingerprint <<< "$credentials")

cat > "$OCI_DIR/config" <<CONFIG
[DEFAULT]
user=$OCI_USER_ID
fingerprint=$fingerprint
tenancy=$OCI_TENANCY_ID
region=$OCI_REGION
key_file=$OCI_DIR/key.pem
CONFIG

chown "$AI_PROXY_USER" "$OCI_DIR/key.pem" "$OCI_DIR/config"
