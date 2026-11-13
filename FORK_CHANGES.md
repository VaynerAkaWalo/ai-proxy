# Changes compared to upstream

This fork tracks [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) and adds what the homelab deployment needs on top. Update this file when a fork-only change lands or upstream is merged.

## Accounting and usage

- Optionally, every request attempt produces a versioned accounting event that is queued into a bounded local bbolt outbox (`observability.accounting-outbox`). Persistence is best effort, so queue pressure, storage errors or a crash can drop events. See [docs/accounting-outbox.md](docs/accounting-outbox.md).
- Events are priced with API-equivalent estimates from the LiteLLM model catalog, refreshed periodically, with a model fallback chain and provider mapping for Codex, Claude, Antigravity, AI Studio and Kimi. Claude `speed: standard` prices at the base rate, `fast` needs an explicit `fast` tier override.
- Attempts are exported to the analytics gateway with their token counts, a `cost_status` and, with `analytics.model-prices.enabled`, the key of the price they selected. Analytics computes the spend from the price table, and the exporter sends no `cost_usd`. The public LiteLLM model catalog is only the price source.
- With `analytics.model-prices.enabled`, the price catalog is also published as a reusable table (`owner/llm_model_prices`, implementing `llm_prices`) with an `effective_at` per version, and every attempt carries the key of the price it selected, even when the model has no price yet, so adding the price later prices the model's history in analytics.
- `cmd/reconcile_llm_prices` gives attempts exported before their model had a price the key analytics needs to price them, and corrects the context band of large attempts. It is a dry run unless `-apply` is passed.
- Project tags come from `X-Project` or a working directory in the prompt. Claude Code's environment block is used even when its system prompt also says "working directory:" in Edit-tool guidance, and Grok's `**Working directory:**` line is recognized. A detected project is reused for later requests in that client session, including a delegate that omits its own workspace. An explicit header or a newly detected directory replaces it. A scratch directory, or any other workspace with no project, clears the remembered tag instead of leaving the previous project in place.
- Streams cancelled mid-send still publish an interrupted usage record for Codex (SSE and WebSocket), xAI WebSocket, OpenAI-compatible and Claude.

## Routing and rate limits

- `routing.strategy: nearest-reset` picks the credential whose quota window resets soonest, falling back to round robin when no reset time is known.
- The management Claude usage lookup is cached across quota refreshes and honors `Retry-After` deadlines.

## Logging

- Warnings, errors and startup logs can be shipped to Grafana Cloud Loki (`observability.loki`).

## Deployment and repo

- `deploy/` runs the proxy on the homelab under systemd and Docker Compose, with secrets rendered from OCI Vault and exposure over Tailscale. See [deploy/README.md](deploy/README.md).
- CI builds and runs the accounting tests on PRs and publishes the image to GHCR on merge to `main`.
- Upstream release, Docker Hub and PR guard workflows, sponsor content and the translated READMEs are removed.

## Upstream merges

- 2026-11-11: upstream `8bba7a77`
- 2026-10-24: upstream `8289ec4c`
- 2026-10-19: upstream `e7d4c731`
- Forked from upstream `e1e3a811` (2026-10-05)
