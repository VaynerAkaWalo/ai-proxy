# Changes compared to upstream

This fork tracks [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) and adds what the homelab deployment needs on top. Update this file when a fork-only change lands or upstream is merged.

## Accounting and usage

- Every request attempt produces a versioned accounting event, persisted in a bounded local bbolt outbox (`observability.accounting-outbox`). See [docs/accounting-outbox.md](docs/accounting-outbox.md).
- Events are priced with API-equivalent estimates from the LiteLLM model catalog, refreshed periodically, with a model fallback chain and provider mapping for Codex, Claude, Antigravity, AI Studio and Kimi.
- Priced events are exported to a stock LiteLLM instance, with each proxy access key mapped to a LiteLLM virtual key. See [docs/litellm-exporter.md](docs/litellm-exporter.md) and [docs/direct-harness-cutover.md](docs/direct-harness-cutover.md).
- Streams cancelled mid-send still publish an interrupted usage record for Codex (SSE and WebSocket), xAI WebSocket, OpenAI-compatible and Claude.

## Routing and rate limits

- `routing.strategy: nearest-reset` picks the credential whose quota window resets soonest, falling back to round robin when no reset time is known.
- A provider 429 forwards the upstream `Retry-After` header instead of only the proxy's own cooldown.
- Claude responses expose `Anthropic-Ratelimit-Unified-Reset` and `-Status` from the upstream reset deadline, so SDK clients can wait for the right moment even without `Retry-After`.
- The management Claude usage lookup is cached across quota refreshes and honors `Retry-After` deadlines.

## Logging

- Warnings, errors and startup logs can be shipped to Grafana Cloud Loki (`observability.loki`).

## Deployment and repo

- `deploy/` runs the proxy on the homelab under systemd and Docker Compose, with secrets rendered from OCI Vault and exposure over Tailscale. See [deploy/README.md](deploy/README.md).
- CI builds and runs the accounting tests on PRs and publishes the image to GHCR on merge to `main`.
- Upstream release, Docker Hub and PR guard workflows, sponsor content and the translated READMEs are removed.

## Upstream merges

- 2026-10-24: upstream `8289ec4c`
- 2026-10-19: upstream `e7d4c731`
- Forked from upstream `e1e3a811` (2026-10-05)
