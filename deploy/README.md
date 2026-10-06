# Homelab deployment

Runs ai-proxy under systemd with Docker Compose. Secrets are read from OCI Vault at every start, only the OCI API key of the `homelab-secrets-reader` user lives on the host.

## Bootstrap

Prerequisites: Docker with the `server` user in the `docker` group, the OCI CLI, `jq` and `envsubst`.

1. Clone this repository to `/opt/ai-proxy`.
2. Fetch the `homelab-secrets-reader-credentials` secret from the platform-security Vault. It is JSON with the private key and fingerprint.
3. Put the key in `/etc/ai-proxy/oci/key.pem` and write `/etc/ai-proxy/oci/config` with `user`, `fingerprint`, `tenancy`, `region=eu-frankfurt-1` and `key_file=/etc/ai-proxy/oci/key.pem`. Both files must be readable by `server` only.
4. Install the units and start them:

```bash
sudo cp /opt/ai-proxy/deploy/ai-proxy*.service /opt/ai-proxy/deploy/ai-proxy-update.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ai-proxy.service ai-proxy-update.timer
```

5. Log in to each provider once, for example `docker exec -it ai-proxy ./CLIProxyAPI --codex-login --no-browser`. Tokens persist in the `auths` volume, which needs backing up.

## Updates

`ai-proxy-update.timer` pulls the image every 15 minutes and restarts the service when it changed. A restart also re-reads the Vault secrets, so `systemctl restart ai-proxy` picks up a rotated secret.

Set `AI_PROXY_BIND` to a Tailscale or LAN address to expose the proxy beyond localhost, and `AI_PROXY_TAG` to pin a `sha-` tag.
