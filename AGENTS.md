# ai-proxy

Our Go proxy for OpenAI, Gemini, Claude and Codex compatible APIs, with OAuth credentials and load balancing. This repository is [VaynerAkaWalo/ai-proxy](https://github.com/VaynerAkaWalo/ai-proxy), a fork of [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Upstream is a source of code and documentation. Its contribution policies do not govern this fork.

## Project context

[README.md](README.md) is the entry point. [FORK_CHANGES.md](FORK_CHANGES.md) tracks our additions and upstream merges, including accounting, LiteLLM usage export, quota-aware routing and Loki logging.

The deployed configuration comes from [deploy/config.yaml.tmpl](deploy/config.yaml.tmpl). [config.example.yaml](config.example.yaml) is the general configuration reference. [deploy/README.md](deploy/README.md) covers the homelab deployment, secret injection and updates.

The service runs under systemd and Docker Compose, reads secrets from OCI Vault at startup and is exposed over Tailscale. CI publishes the image to GHCR on merge to `main`. A host timer pulls updated images, while deployment file changes require updating the host checkout.

The Go module still uses `github.com/router-for-me/CLIProxyAPI/v8`.

## Local development

The Go version is declared in [go.mod](go.mod).

```bash
go run ./cmd/server --config config.yaml
go build -o /tmp/ai-proxy ./cmd/server
go test ./...
go vet ./...
```

Local configuration defaults to `config.yaml`, and `.env` is loaded from the working directory. Provider credentials default to `auths/`.

## Code map

- `cmd/server/` starts the server.
- `internal/api/` contains HTTP routes, middleware and management handlers.
- `internal/runtime/executor/` sends requests to providers.
- `internal/translator/` translates provider protocols.
- `internal/thinking/` normalizes reasoning settings and applies provider-specific representations.
- `internal/registry/` manages the model catalog.
- `internal/config/` and `internal/watcher/` handle configuration and reloads.
- `internal/store/` provides credential storage backends.
- `sdk/cliproxy/` contains the embeddable service, authentication and usage accounting.
- `test/` contains integration tests.
- `deploy/` contains deployment configuration and tooling.
