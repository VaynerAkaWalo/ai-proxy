# Homelab deployment

Runs ai-proxy under systemd with Docker Compose. Secrets are read from OCI Vault at every start, only the OCI API key of the `homelab-secrets-reader` user lives on the host.

## Bootstrap

Prerequisites: Docker with the `server` user in the `docker` group, and the OCI CLI, `jq` and `envsubst` on the system `PATH`:

```bash
sudo apt install -y pipx jq gettext-base
sudo PIPX_HOME=/opt/pipx PIPX_BIN_DIR=/usr/local/bin pipx install oci-cli
```

1. Clone this repository to `/opt/ai-proxy`.
2. From a machine with admin OCI access, fetch the credentials and write them on the host in one go:

```bash
oci secrets secret-bundle get-secret-bundle-by-name \
  --vault-id ocid1.vault.oc1.eu-frankfurt-1.envlmxzraacwi.abtheljt7ekigp2dmv3oewuol7daxchhaebkj7iwy4ihoyx4b2bteqqjswha \
  --secret-name homelab-secrets-reader-credentials \
  --query 'data."secret-bundle-content".content' --raw-output | base64 -d \
  | ssh <host> 'sudo /opt/ai-proxy/deploy/bootstrap-oci.sh'
```

   Run it without the `ssh` part on the host itself if the admin login is there. It creates `/etc/ai-proxy/oci/{key.pem,config}` owned by `server` with mode 600.
3. Check the access: `sudo -u server OCI_CLI_CONFIG_FILE=/etc/ai-proxy/oci/config /opt/ai-proxy/deploy/render-secrets.sh /tmp` should write `config.yaml` and `env` into `/tmp`. Delete them afterwards.
4. Write `/etc/ai-proxy/ai-proxy.env` with `AI_PROXY_BIND=<this host's tailnet IP>` so the API and management endpoints are reachable over Tailscale. Without it the proxy only listens on localhost.
5. Install the units and start them:

```bash
sudo cp /opt/ai-proxy/deploy/ai-proxy*.service /opt/ai-proxy/deploy/ai-proxy-update.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ai-proxy.service ai-proxy-update.timer
```

6. Log in to each provider once, for example `docker exec -it ai-proxy ./CLIProxyAPI --codex-login --no-browser`. Tokens persist in the `auths` volume, which needs backing up.

## Updates

`ai-proxy-update.timer` pulls the image every 15 minutes and restarts the service when it changed. A restart also re-reads the Vault secrets, so `systemctl restart ai-proxy` picks up a rotated secret.

Set `AI_PROXY_TAG` in the same env file to pin a `sha-` tag.
