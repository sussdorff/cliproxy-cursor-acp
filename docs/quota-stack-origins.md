# How to recover the Cursor quota stack

Chat completions and the Cursor OAuth login run on **stock** CLIProxyAPI plus
this plugin. The CPAMP **Quota** tab does not. That tab needs two extra
downstream builds that this repository does not ship.

## The two extra pieces

| Piece | What it is | What it is not |
|---|---|---|
| **Host patch** | A CLIProxyAPI *process* built from the Cognovis fork. It is the management API that lists auth files and calls `RefreshAuth`. | Not a Docker image of this plugin. Not CPAMP. |
| **Image fork** | A CPA Manager Plus *container image* built from the Cognovis fork. It is the browser UI that paints the Quota tab. | Not CLIProxyAPI. Not this plugin's `.so`. |

They sit on opposite sides of HTTP:

```text
CPAMP image fork          GET /v0/management/auth-files
(browser + Manager Server)  POST /v0/management/auth-files/refresh-quota
            |
            v
CLIProxyAPI host patch    metadata.plugin_quota (allowlisted)
            |
            v
this plugin (.so)         writes the contract onto the auth record
```

A stock CLIProxyAPI release (`router-for-me/CLIProxyAPI`, including the
`v7.2.141` pin in this module) never puts `metadata` on the auth-files list.
The plugin can still write `plugin_quota` to disk; the UI never sees it.

A stock CPAMP image (`seakee/CPA-Manager-Plus`) has no plugin-quota parser and
no Refresh-quota client for that contract. Replacing only the host, or only the
UI, leaves the Quota tab empty.

## Where to get each piece

Rebuild from these remotes. Do not use the isolated `cca-6wh-e2e` sandbox as
the source of truth; that tree was a local proof and is not a release.

| Role | Use this remote | Do not use this remote |
|---|---|---|
| Plugin source and store | `https://github.com/sussdorff/cliproxy-cursor-acp` branch `main` | An older GitHub release than the `main` you intend to run |
| Host patch | `https://github.com/sussdorff/CLIProxyAPI` branch `main` | `https://github.com/router-for-me/CLIProxyAPI` releases or `go.mod`'s `v7.2.141` pin |
| Image fork | `https://github.com/sussdorff/CPA-Manager-Plus` branch `main` | `https://github.com/seakee/CPA-Manager-Plus` images or the stock installer |

Landed GitHub merges (2026-08-28):

- plugin: [sussdorff/cliproxy-cursor-acp#8](https://github.com/sussdorff/cliproxy-cursor-acp/pull/8)
- host: [sussdorff/CLIProxyAPI#1](https://github.com/sussdorff/CLIProxyAPI/pull/1)
- UI: [sussdorff/CPA-Manager-Plus#1](https://github.com/sussdorff/CPA-Manager-Plus/pull/1)

The host patch on that `main` projects the version-1 `plugin_quota` allowlist
(including `spend` and `daily`) and serves
`POST /v0/management/auth-files/refresh-quota`. The image fork parses that
payload, draws the windows and spend histogram, and refreshes without clearing
the credential list.

Upstream contribution, if it happens later, is a separate PR into
`router-for-me/CLIProxyAPI` and `seakee/CPA-Manager-Plus`. Until those land,
production quota display stays on the two `sussdorff` remotes.

## How to rebuild if the server will not start

Identify which process is down, then rebuild only that piece from the table
above.

### Plugin

1. Checkout `sussdorff/cliproxy-cursor-acp` `main`.
2. Either install the GitHub release that matches
   `plugin-store/registry.json`, or `make build` and copy the shared library
   to `<plugins.dir>/<goos>/<goarch>/cliproxy-cursor-acp.so` (Darwin local
   installs use `.dylib`).
3. Reload CLIProxyAPI.

The store URL is:

```text
https://raw.githubusercontent.com/sussdorff/cliproxy-cursor-acp/main/plugin-store/registry.json
```

A store install older than the `main` you need will look like a product
failure: login works, Quota stays empty or stale.

### Host patch

1. Clone `https://github.com/sussdorff/CLIProxyAPI` and checkout `main`.
2. Build and run that binary (or `go run ./cmd/server`) with the same
   `config.yaml`, auth directory, plugin directory, and management key the
   host already uses.
3. Confirm the management API, not the chat API:

```sh
curl -sS -H "Authorization: Bearer <management-key>" \
  https://<cpa-host>/v0/management/auth-files
```

A Cursor plugin row must include `metadata.plugin_quota` with
`schema: cliproxy.plugin.quota`. If that object is missing, you are still
running stock CLIProxyAPI.

`remote-management.secret-key` must be set. A CPAMP container calling a
host-published CPA also needs `remote-management.allow-remote: true`.

### Image fork

1. Clone `https://github.com/sussdorff/CPA-Manager-Plus` and checkout `main`.
2. From that tree, with `CPAMP_IMAGE` unset:

```sh
docker compose -f docker-compose.manager.yml up --build -d
```

3. Open the Manager Server UI and connect it to the **host-patch** CPA URL
   plus the CPA management key (not the CPAMP admin key).

Do not pass `--build` together with a digest-pinned `CPAMP_IMAGE`. Confirm
the image labels point at `github.com/sussdorff/CPA-Manager-Plus`, not
`seakee`.

A stock seakee image against a patched host still shows no plugin windows.

## What each piece owns

- **This plugin** observes Cursor and writes `plugin_quota`. It does not
  expose a management HTTP API.
- **The host patch** is the only process that may republish that metadata on
  the management API. It allowlists fields; it does not copy the raw map.
- **The image fork** is the only process that renders those fields in
  Accounts → Quota and that calls `refresh-quota`.

The contract shape is [plugin-quota-contract.md](plugin-quota-contract.md).
