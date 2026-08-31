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
(including `spend`) and serves
`POST /v0/management/auth-files/refresh-quota`. The image fork parses that
payload, draws the Cursor window plus satellite allowances, and refreshes
without clearing the credential list.

Upstream contribution, if it happens later, is a separate PR into
`router-for-me/CLIProxyAPI` and `seakee/CPA-Manager-Plus`. Until those land,
production quota display stays on the two `sussdorff` remotes.

## Production image contract

Every push to `main` in the two forks publishes one multi-architecture manifest
for `linux/amd64` and `linux/arm64`:

- `ghcr.io/sussdorff/cli-proxy-api:<full-commit-sha>` and `:main`
- `ghcr.io/sussdorff/cpa-manager-plus:<full-commit-sha>` and `:main`

The `main` tag is only a discovery convenience. Production configuration must
use the manifest digest returned by GHCR, for example
`ghcr.io/sussdorff/cpa-manager-plus@sha256:<64-hex-digest>`. A digest pins the
bytes; the OCI labels independently bind those bytes to the exact fork source
and full revision. Both checks are required.

Copy `deploy/production-images.override.yml` beside the production Compose file.
This tracked override is the deployment artifact that prevents a later
`docker compose up` from silently selecting a local or mutable image. Validate
the rendered configuration and remote image metadata before applying it:

```sh
export CLI_PROXY_IMAGE='ghcr.io/sussdorff/cli-proxy-api@sha256:<digest>'
export CLI_PROXY_REVISION='<full-CLIProxyAPI-commit>'
export CPAMP_IMAGE='ghcr.io/sussdorff/cpa-manager-plus@sha256:<digest>'
export CPAMP_REVISION='<full-CPA-Manager-Plus-commit>'
./scripts/verify-production-images.sh pre-deploy \
  --compose-file /opt/cli-proxy-api/docker-compose.yml \
  --compose-file /opt/cli-proxy-api/production-images.override.yml
```

Use those same two files for the deployment, then validate the running
containers against the same rendered configuration:

```sh
docker compose \
  --file /opt/cli-proxy-api/docker-compose.yml \
  --file /opt/cli-proxy-api/production-images.override.yml up -d
./scripts/verify-production-images.sh post-deploy \
  --compose-file /opt/cli-proxy-api/docker-compose.yml \
  --compose-file /opt/cli-proxy-api/production-images.override.yml
```

Both modes require `jq` and inspect `docker compose config --format json`,
asserting the exact image assigned to each named service; the pre-deploy mode pulls
the digest-pinned images and verifies their OCI provenance without requiring
running containers. The post-deploy mode additionally verifies the actual
container references and image IDs. The verifier fails closed for mutable tags,
any namespace other than `sussdorff`, wrong or incomplete source/revision
labels, rendered Compose drift, or a running image whose repository digest
differs from the configured digest. The default container names are
`cli-proxy-api` and `cpa-manager-plus`; set `CLI_PROXY_CONTAINER` or
`CPAMP_CONTAINER` when the deployment uses different names.

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

1. Select the approved full commit on `sussdorff/CLIProxyAPI` `main` and obtain
   its GHCR manifest digest.
2. Configure the digest-pinned `ghcr.io/sussdorff/cli-proxy-api` reference with
   the same `config.yaml`, auth directory, plugin directory, and management key
   the host already uses.
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

1. Select the approved full commit on `sussdorff/CPA-Manager-Plus` `main` and
   obtain its GHCR manifest digest.
2. Configure the digest-pinned `ghcr.io/sussdorff/cpa-manager-plus` reference.
3. Open the Manager Server UI and connect it to the **host-patch** CPA URL
   plus the CPA management key (not the CPAMP admin key).

Do not pass `--build` with the production compose invocation. Run the provenance
verifier instead of judging an image by a local tag.

A stock seakee image against a patched host still shows no plugin windows.

## What each piece owns

- **This plugin** observes Cursor and writes `plugin_quota`. It does not
  expose a management HTTP API.
- **The host patch** is the only process that may republish that metadata on
  the management API. It allowlists fields; it does not copy the raw map.
- **The image fork** is the only process that renders those fields in
  Accounts → Quota and that calls `refresh-quota`.

The contract shape is [plugin-quota-contract.md](plugin-quota-contract.md).
