# CLIProxyAPI client routing

Tracked templates and a deploy script for pointing **Claude Code**, **Codex**,
and **Grok** at a private CLIProxyAPI origin. Cursor Agent stays native.

Client keys are created on the gateway and never belong in Git. Copy a
mode-`0600` key onto the target machine first, then run the deploy script
there.

## What gets installed

| Client | Destination | How it authenticates |
| --- | --- | --- |
| Claude Code | `~/.claude/settings.local.json` plus `ANTHROPIC_BASE_URL` | `apiKeyHelper` prints the local key. Do not also export `ANTHROPIC_AUTH_TOKEN`. |
| Codex | `~/.codex/config.toml` `model_providers.cliproxy` | bearer token injected from the key file at deploy time |
| Grok | `~/.grok/config.toml` endpoints and `grok-4.6` / `grok-4.5` | `env_key = "CLIPROXY_API_KEY"` |

The script also writes `~/.config/cliproxy/env.sh` and `print-key.sh`, copies
the key to `~/.config/cliproxy/<client-name>.key`, and sources `env.sh` from
`~/.bashrc` (before the interactive-only return), `~/.profile`, and
`~/.zshenv`. Existing Claude, Codex, and Grok settings are merged, not
replaced.

## Deploy on a machine

```sh
# From this repository, on the target host:
./clients/deploy.sh \
  --key-file ~/.config/cliproxy/yakushido.key \
  --client-name yakushido
```

Defaults to `http://192.168.60.32:8317`. Override the origin when needed:

```sh
./clients/deploy.sh \
  --key-file /path/to/mode-0600-key \
  --client-name macbook \
  --gateway-url http://192.168.60.32:8317
```

`--client-name` is only the local key basename (`macbook`, `yakushido`, or
another `[A-Za-z0-9_-]` name). It is not sent to the gateway.

After deploy, open a new shell or `source ~/.config/cliproxy/env.sh`, then
restart Claude Code, Codex, and Grok.

To confirm the key reaches the gateway without printing it:

```sh
CLI_PROXY_API_CLIENT_KEY_FILE=~/.config/cliproxy/<client-name>.key \
  /path/to/cli-proxy-api/verify-live.sh
```

## Templates

| File | Role |
| --- | --- |
| `templates/env.sh` | exports `CLIPROXY_API_KEY` and Anthropic base URL |
| `templates/print-key.sh` | Claude `apiKeyHelper` |
| `templates/claude.settings.local.json` | Claude local settings merge source |
| `templates/codex.provider.toml` | Codex provider block; no bearer token |
| `templates/grok.cliproxy.toml` | Grok endpoint and model overrides |

`GATEWAY_URL`, `KEY_FILE`, and `API_KEY_HELPER` are placeholders. The deploy
script substitutes them. `experimental_bearer_token` is written only onto the
target Codex config from the key file.

## Tests

```sh
./clients/tests/test-deploy.sh
```

The test uses a disposable home directory and a throwaway key. It refuses a
key that is not mode `0600` and checks that deploy does not print the key.
