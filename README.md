# cliproxy-cursor-acp

`cliproxy-cursor-acp` is a native plugin for
[CLIProxyAPI v7](https://github.com/router-for-me/CLIProxyAPI). It makes one or
more **Cursor subscription accounts** usable as ordinary CLIProxyAPI providers,
so any OpenAI-compatible client that already talks to your CLIProxyAPI instance
can send requests to a `cursor/...` model.

It does that by running Cursor's **official Cursor Agent CLI** in
[Agent Client Protocol](https://agentclientprotocol.com/) mode (`agent acp`),
speaking ACP over stdio with
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk). The plugin
implements no part of Cursor's private protocol, never reads or transports
Cursor credential files, and never fabricates a subscription balance.

## How the pieces fit together

```text
OpenAI-compatible client
        |
        v
CLIProxyAPI  ── selects the AuthID (priority, weight, cooldown, failover)
        |
        v
cliproxy-cursor-acp  ── one ACP child process per account
        |
        v
official Cursor Agent CLI  ── private CURSOR_CONFIG_DIR per account
        |
        v
Cursor
```

- **CLIProxyAPI** is the only scheduler. This plugin has no account-selection
  logic of its own: it executes exactly the `AuthID` the host selected.
- **[CPA Manager Plus](https://github.com/seakee/CPA-Manager-Plus)** (CPAMP) is
  the management UI used throughout this guide. It renders the plugin store, the
  configuration panel, and an OAuth login card for this provider automatically.
  The **Quota** tab for this plugin is not in the stock seakee image; see
  [docs/quota-stack-origins.md](docs/quota-stack-origins.md).
- **The official [Cursor Agent CLI](https://cursor.com/docs/cli)** owns every
  credential. Each account gets its own private `CURSOR_CONFIG_DIR` created by
  this plugin with mode `0700`; the plugin only ever passes that path to the CLI.

## Quickstart for an operator

The chat and login flow happens in a browser against stock CLIProxyAPI plus
this plugin. You do not need a custom host image for `cursor/auto` requests.
The Accounts **Quota** tab is a different stack: it needs the CLIProxyAPI
**host patch** and the CPAMP **image fork** documented in
[docs/quota-stack-origins.md](docs/quota-stack-origins.md).

### 1. Enable plugins and add this store source

In CPAMP open **Config Panel** and make sure the plugin system is enabled, then
add this repository's registry to `plugins.store-sources`:

```text
https://raw.githubusercontent.com/sussdorff/cliproxy-cursor-acp/main/plugin-store/registry.json
```

CLIProxyAPI merges every configured store source with its official one. The
matching CLIProxyAPI configuration keys look like this:

```yaml
plugins:
  enabled: true
  dir: /root/.cli-proxy-api/plugins
  store-sources:
    - https://raw.githubusercontent.com/sussdorff/cliproxy-cursor-acp/main/plugin-store/registry.json
  configs:
    cliproxy-cursor-acp:
      data_root: /root/.cli-proxy-api/cliproxy-cursor-acp
```

Keep `data_root` inside a persistent volume and set it explicitly. The path
above reuses CLIProxyAPI's usual Docker auth-volume mount.

### 2. Install the plugin from the CPAMP plugin store

Open **Plugin Store**, find **Cursor ACP**, and install it. CLIProxyAPI
downloads `cliproxy-cursor-acp_<version>_linux_amd64.zip` from this repository's
GitHub release, verifies its sha256 against the release `checksums.txt`, unpacks
the shared library into `plugins/<goos>/<goarch>/`, enables it, and hot-reloads.

Only `linux/amd64` is published. Build from source for any other platform.

### 3. Install the official Cursor Agent CLI

The plugin does not ship Cursor's CLI. Open the plugin's setup page:

```text
https://<your-cliproxyapi-host>/v0/resource/plugins/cliproxy-cursor-acp/setup
```

CPAMP also links this page from the plugin's resource menu. Enter the
**CLIProxyAPI management key** (not the separate CPAMP admin key) and press
**Install official Cursor Agent CLI**. The install:

- downloads a **release-pinned** Cursor Agent version over HTTPS from the
  canonical `https://downloads.cursor.com/lab/<version>/<os>/<arch>/agent-cli-package.tar.gz`
  URL, holding every redirect hop to HTTPS and to an allowlisted download host;
- **verifies the artifact sha256 against the digest embedded in this plugin
  before extracting anything and before executing anything**, and aborts on a
  mismatch;
- extracts the archive in pure Go, refusing path traversal, symbolic links, hard
  links, unknown entry types, duplicate paths, oversized content, and excessive
  entry counts;
- runs the extracted binary with `--version` and activates it only when the
  version matches;
- activates atomically under `<data_root>/agent/versions/<version>` behind a
  `current` pointer.

The authenticated status route reports the version and digest actually trusted
and installed. The unauthenticated setup page shows only this build's embedded
pinned version and digest.

If you already provide the CLI yourself — on `PATH` or through the `executable`
configuration key — skip this step.

#### Installing a Cursor version other than the pinned one

Cursor publishes no checksum file, so the plugin ships its own pin and never
installs an artifact it cannot verify. Do not use a digest calculated from the
same download as evidence for a custom version. Only use this override when an
independently trusted 64-character sha256 is available through your
organization's software-approval process and recorded before downloading the
archive.

```sh
EXPECTED_SHA256='<independently trusted sha256>'
curl -fsSLo agent-cli-package.tar.gz https://downloads.cursor.com/lab/<version>/linux/x64/agent-cli-package.tar.gz
printf '%s  %s\n' "$EXPECTED_SHA256" agent-cli-package.tar.gz | shasum -a 256 -c -
```

Configure exactly that independently trusted value only after the comparison
reports `OK`:

```yaml
plugins:
  configs:
    cliproxy-cursor-acp:
      agent_install_source: latest   # parse cursor.com/install for the current release
      agent_package_sha256: "<independently trusted sha256>"
```

`agent_install_source: latest` **requires** `agent_package_sha256`; without it
the install is refused before any request is made. Keeping the default
`pinned` source and setting only `agent_package_sha256` replaces the embedded
digest for the pinned version instead.

Maintainers bump the embedded pin (`pinnedAgentVersion` and
`pinnedAgentDigests` in `internal/cursor/install.go`) per plugin release; the
procedure is in [docs/security.md](docs/security.md).

### 4. Log in to a Cursor account

Open **OAuth Login** in CPAMP. The plugin card is rendered automatically as
**cliproxy-cursor-acp**. Press **Start cliproxy-cursor-acp Login**:

1. the plugin creates a fresh private profile directory (mode `0700`);
2. it starts the official `agent login` inside that profile with
   `NO_OPEN_BROWSER=1`, an inherited-environment allowlist, an emptied
   `CURSOR_API_KEY`, and its own process group;
3. it captures the CLI's output to bounded files and returns the single
   `https://cursor.com/...` approval URL;
4. you approve the login in your browser;
5. the card polls until the CLI finishes, then the plugin confirms the account
   through `agent status` and enriches it through `agent about`.

The result is one CLIProxyAPI auth record whose stored payload contains only the
account identity and its profile directory path. Repeat for every Cursor account
you want to add. A failed or expired login leaves no partial account and removes
its profile directory.

Before starting the next login, sign out of `cursor.com` in the approval browser
or use a fresh private browser profile. Otherwise Cursor may approve the account
that is already signed in instead of the additional account you intended.

If no Cursor CLI is resolvable, the login card reports a setup-required state and
links the setup page from step 3 instead of failing opaquely.

### 5. Send the first request

```sh
curl -sS https://<your-cliproxyapi-host>/v1/chat/completions \
  -H "Authorization: Bearer <your-cliproxyapi-api-key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"cursor/auto","messages":[{"role":"user","content":"Say hello."}]}'
```

Each account exposes one model, `cursor/auto`, and Cursor resolves `auto`
against that account's own entitlements. CLIProxyAPI picks which account serves
the request.

## Configuration reference

Configuration is optional; every key has a working default. Place the mapping
under `plugins.configs.cliproxy-cursor-acp` — see
[`config/cliproxy-cursor-acp.yaml`](config/cliproxy-cursor-acp.yaml).

| Key | Default | Meaning |
|---|---|---|
| `executable` | unset | Absolute path to the official Cursor Agent CLI. When unset, the plugin uses its own digest-verified managed install first, then `agent` from `PATH`. |
| `data_root` | `<CLIProxyAPI auth dir>/cliproxy-cursor-acp` when the host exposes its auth directory; otherwise required | Persistent directory holding login profiles, the workspace, and the managed CLI. Set it explicitly for Docker deployments. |
| `agent_install_source` | `pinned` | `pinned` installs the release-pinned Cursor Agent version verified against a digest embedded in this build. `latest` parses `cursor.com/install` and requires `agent_package_sha256`. |
| `agent_package_sha256` | unset | sha256 of the official Cursor Agent package. Optional with `pinned` (replaces the embedded digest), mandatory with `latest`. |
| `workspace_root` | `<data_root>/workspace` | Working directory offered to the Cursor Agent. Must be absolute, mode `0700`, owned by the service user. |
| `max_concurrent` | `2` | Maximum concurrent ACP turns across all accounts. |
| `max_prompt_bytes` | `524288` | Maximum prompt size accepted from a client. |
| `max_output_bytes` | `1048576` | Maximum collected ACP output per turn. |
| `timeout` | `2m` | Bound on one ACP execution. |

There is **no** `accounts:` block. Accounts exist only as CLIProxyAPI auth
records created by the login flow, and are reconstructed from those records
after a restart.

## Persistence for Docker deployments

Two directories must survive container recreation:

- **The plugin directory** (`plugins.dir`). Without it, a recreated container
  loses the installed shared library and you have to reinstall from the store.
  You can point `plugins.dir` at a path inside an existing mounted volume, for
  example `/root/.cli-proxy-api/plugins` when `/root/.cli-proxy-api` is already
  mounted, instead of adding a second mount.
- **The plugin data root** (`data_root`). It holds every login profile and the
  managed Cursor CLI. Set it explicitly to a path inside a persistent mount; the
  example above places it below CLIProxyAPI's usual auth-volume mount.

If the data root is lost, the stored auth records still exist but their profile
directories do not, and each account has to log in again.

## Security model

- The official Cursor Agent CLI owns all credential material inside each private
  `CURSOR_CONFIG_DIR`. This plugin never reads, parses, copies, or transports
  those files, and stores none of their contents in auth records.
- Every child process gets direct argv (no shell), a small environment
  allowlist, a stripped `CURSOR_API_KEY`, the account's own
  `CURSOR_CONFIG_DIR`, and its own process group.
- Profile directories are created with mode `0700` and are refused at
  registration if they grant group or other access or are not owned by the
  service user. Two accounts can never share one profile directory.
- A profile directory must be a direct child of `<data_root>/profiles`, so a
  stored auth record cannot aim the Cursor CLI at the host auth directory or any
  other path. Re-authenticating an account deletes its previous profile.
- Login output is bounded on disk, a login expires as a hard gate that also
  refuses an already-finished attempt, and abandoned logins are swept on every
  start and poll.
- The managed installer never extracts or executes an artifact whose sha256 was
  not verified first, and has no cleartext HTTP path. The first execution of the
  downloaded binary is isolated like every other child process.
- The unauthenticated setup page renders only compile-time constants: no
  deployment configuration, no host state, and a strict Content-Security-Policy.
- Releases carry a Sigstore build-provenance attestation, verifiable with
  `gh attestation verify`, as an anchor independent of the release's own
  `checksums.txt`.
- The setup install route is management-authenticated and requires an explicit
  `{"confirm": true}` body. The setup page itself is a browser resource route and
  carries no host state or secrets.
- Errors returned to clients and to the setup page are redacted: no child
  output, no paths, no credential material.

See [docs/security.md](docs/security.md) for the full boundary and
[docs/architecture.md](docs/architecture.md) for the runtime design.

## Limitations

- **No streaming.** `ExecuteStream` returns an explicit unsupported error; a turn
  is returned after it completes.
- **No preflight token counting.** `CountTokens` returns an explicit unsupported
  error.
- **Best-effort subscription quota.** The plugin publishes the account-scoped
  windows and spend it can observe from the managed profile, as the generic
  contract in [docs/plugin-quota-contract.md](docs/plugin-quota-contract.md).
  When the profile cannot be observed, the contract reports `unavailable` and the
  account stays usable. A manager UI only sees that payload on a patched
  CLIProxyAPI host and a forked CPAMP image; recover those remotes from
  [docs/quota-stack-origins.md](docs/quota-stack-origins.md).
  ACP-observed token counters are reported separately and are not a paid
  balance.
- **No raw HTTP bridging** to Cursor endpoints.
- **linux/amd64 releases only.**

## Build from source

Requires Go 1.26 or newer and a C toolchain for the native shared library.

```sh
make vet
make test
make test-race
make build            # host shared library
make build-linux      # linux/amd64 build in the release Docker image
make release-archive  # produces the exact plugin-store asset names in dist/
```

Install `build/cliproxy-cursor-acp.so` into
`<plugins.dir>/<goos>/<goarch>/cliproxy-cursor-acp.so` and restart or reload
CLIProxyAPI.

## References

- CLIProxyAPI: <https://github.com/router-for-me/CLIProxyAPI>
- CLIProxyAPI host patch (quota list + refresh): <https://github.com/sussdorff/CLIProxyAPI>
- CPA Manager Plus: <https://github.com/seakee/CPA-Manager-Plus>
- CPA Manager Plus image fork (plugin Quota tab): <https://github.com/sussdorff/CPA-Manager-Plus>
- Recovering the quota stack: [docs/quota-stack-origins.md](docs/quota-stack-origins.md)
- Official Cursor CLI documentation: <https://cursor.com/docs/cli>
- Agent Client Protocol: <https://agentclientprotocol.com/>
- ACP Go SDK: <https://github.com/coder/acp-go-sdk>

## License

MIT. See [LICENSE](LICENSE) and [third-party notices](THIRD_PARTY_NOTICES.md).
