Upstream project and documentation: [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

## Deployment config

The deployed config is rendered from [`deploy/config.yaml.tmpl`](deploy/config.yaml.tmpl), not from `config.example.yaml`. Look there for what the live instance actually runs, anything it omits falls back to the defaults. See [`deploy/README.md`](deploy/README.md) for how secrets are injected and how the service is updated.

## Fork changes

[`FORK_CHANGES.md`](FORK_CHANGES.md) lists what this fork adds or removes compared to upstream and when upstream was last merged.
