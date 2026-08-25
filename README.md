# cliproxy-cursor-acp

`cliproxy-cursor-acp` is a native CLIProxyAPI v7 plugin that runs the official
Cursor Agent CLI in ACP mode (`agent acp`). It makes Cursor subscription
accounts usable through the existing CLIProxyAPI scheduler rather than adding a
second scheduler: CLIProxyAPI selects the `AuthID`, applies weights, cooldowns,
retries, and failover; this plugin executes only through that selected account.

Every configured account has a separate `CURSOR_CONFIG_DIR`. The plugin starts
an account-keyed ACP child process with direct argv (`agent acp`), a minimal
environment, a private process group, and an ACP session map that cannot be
shared between `AuthID`s.

This project does not implement Cursor's private Connect protocol, scrape the
Cursor billing dashboard, read Cursor credential files, or fabricate an exact
remaining subscription quota.

## Status and limitations

- Native CLIProxyAPI ABI registration exposes auth, model, executor, and usage capabilities.
- The current executor returns a completed OpenAI-compatible response after an ACP turn. Streaming and preflight token counting return explicit unsupported errors.
- CPA Manager Plus can use generic Auth Files and Quota Management views. Account label/status, configured model, retry/failure state, and observed ACP tokens are available. `subscription_quota_available` is always `false` until Cursor publishes an official exact quota source through the CLI or ACP.

## Build

Requires Go 1.26 or newer and a C toolchain for the native shared library.

```sh
go test ./...
go test -race ./...
go build -buildmode=c-shared -o build/cliproxy-cursor-acp.so ./cmd/cliproxy-cursor-acp
```

Install `build/cliproxy-cursor-acp.so` in the CLIProxyAPI plugin directory and
use the configuration below. Restarting CLIProxyAPI is an operator action.

## Configure two accounts

Create private, owner-only profile directories and authenticate each account
with Cursor's official CLI before starting CLIProxyAPI:

```sh
mkdir -m 700 -p /srv/cursor-profiles/cursor-a /srv/cursor-profiles/cursor-b
CURSOR_CONFIG_DIR=/srv/cursor-profiles/cursor-a agent login
CURSOR_CONFIG_DIR=/srv/cursor-profiles/cursor-b agent login
```

Then use [config/cliproxy-cursor-acp.yaml](config/cliproxy-cursor-acp.yaml).
`auth_id` is the bridge to CLIProxyAPI's scheduler and must remain stable. Do
not place tokens, browser cookies, or profile file contents in the YAML.

For operational and security detail, see [architecture](docs/architecture.md)
and [security](docs/security.md).

## License

MIT. See [LICENSE](LICENSE) and [third-party notices](THIRD_PARTY_NOTICES.md).
