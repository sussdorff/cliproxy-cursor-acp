# Security constraints

## Credential ownership

Cursor credentials are owned by the official Cursor Agent CLI inside each
private profile directory. This plugin never reads, parses, copies, edits, or
serializes those files. It creates the profile directory with mode `0700`, hands
its path to the CLI as `CURSOR_CONFIG_DIR`, and stores only the account
identity and that path in the CLIProxyAPI auth record.

A stored auth record contains exactly: record type, auth id, profile directory,
label, account email, and model. Any token, cookie, or credential-file content
in an auth record would be a defect.

## Login isolation

`agent login` runs with:

- direct argv, never a shell;
- the same small environment allowlist used for execution (`PATH`, `HOME`,
  `TMPDIR`, `TERM`, `NO_COLOR`, `LANG`, `LC_ALL`), plus `NO_OPEN_BROWSER=1` and
  an explicitly emptied `CURSOR_API_KEY`;
- a fresh private profile directory unique to that login attempt;
- its own process group.

Its stdout and stderr are captured into files inside the profile directory
rather than inherited pipes, because Cursor leaves a detached worker process
holding inherited descriptors open after the main process exits. Those files are
bounded **on disk**, not only on read: a watcher checks their combined size for
the whole life of the login and, on breach, kills the process group, fails the
login with `login_output_too_large`, and removes the profile.

A login expires as a hard gate. Once its deadline passes, polling refuses it and
removes the profile even if the CLI had already finished successfully, so a
stale approval can never be turned into an auth record. Expired attempts are
swept on every login start and every poll, so no operator action is needed to
reclaim an abandoned one, and plugin shutdown waits for any login that is still
starting before terminating the rest.

Only a URL that parses as `https://cursor.com/...` or `https://www.cursor.com/...`
is ever presented as an approval link. Zero matches after the timeout, or more
than one distinct match at any point, is a typed failure. Any failure kills the
process group and removes the profile directory, so a failed login cannot leave
a half-authenticated account behind.

## Process isolation

The execution child gets direct argv only: `agent acp`. Its environment keeps
the same small allowlist, removes `CURSOR_API_KEY`, replaces any inherited
`CURSOR_CONFIG_DIR`, and sets the selected account's directory. It runs in its
own process group; shutdown sends TERM, then a bounded KILL, and reaps the child.
ACP output is collected per serialized turn and capped before it reaches a
response.

An unrelated global Cursor API key, including one used by other local ACP tools,
is intentionally stripped. Plugin authentication remains the per-account
`CURSOR_CONFIG_DIR` profile; forwarding a global key would collapse `AuthID`
account isolation.

Profile and workspace paths are canonicalized before use. A profile directory
must be a real directory owned by the service user with no group or other
permissions, **and a direct child of `<data_root>/profiles`** — the directory the
login flow creates them in. A stored auth record therefore cannot aim the
official CLI at the CLIProxyAPI auth directory, the plugin data root, or any
other path; such a record is refused and the account never becomes executable.
Two accounts may not resolve to the same profile directory. Do not mount a
profile directory into other containers or log its contents.

### Profile lifetime

Re-authenticating a Cursor account produces a new profile directory. Because the
account identity is derived from the account email, the new record replaces the
old one; the plugin then closes the old ACP process and deletes the old profile,
which still holds live credential material. Only paths inside
`<data_root>/profiles` are ever deleted.

Two residual cases are deliberately **not** cleaned automatically, because the
plugin cannot distinguish an orphan from a live account whose auth record the
host has not parsed yet, and deleting the wrong one would destroy a working
account:

- A crash between a login start and its first poll strands that attempt's
  profile until the next login start or poll sweeps it past its expiry.
- Deleting an auth record in the management UI does not delete its profile
  directory. Remove it manually from `<data_root>/profiles` if you need the
  credential material gone immediately.

There is no startup garbage collector over `<data_root>/profiles`.

### Executable resolution

The official CLI is resolved as: the `executable` configuration key, then the
managed install under `<data_root>/agent/current`, then `agent` on `PATH`. The
managed install outranks `PATH` because its artifact digest was verified against
a trusted pin, while anything on `PATH` was not. The authenticated status route
reports `resolved_executable` so an operator can see which one actually runs.

The freshly downloaded binary's first execution is the `--version` check, and it
runs under the same isolation as every other child: the environment allowlist
only (no inherited process environment), the staging directory as its working
directory and `CURSOR_CONFIG_DIR`, and its own process group.

Every child's process group is signalled by group id only while its leader is
still un-reaped. After the leader has been reaped the group's existence is
probed first, because an empty group id can be recycled by the kernel and
signalling it blind could reach an unrelated process group.

## Managed installer

The installer is the only code path that fetches from the network.

### Artifact trust

Cursor publishes no checksum file for the Agent CLI package, so a digest the
plugin fetches at runtime would prove nothing. The plugin therefore ships its
own pin.

**No artifact is ever extracted or executed before its sha256 has been verified
against a trusted digest.** There are exactly two trusted digests:

| Source | Version | Trusted digest |
|---|---|---|
| `agent_install_source: pinned` (default) | `pinnedAgentVersion` embedded in `internal/cursor/install.go` | `pinnedAgentDigests` for the running platform, or `agent_package_sha256` when the operator sets one |
| `agent_install_source: latest` | parsed from `https://cursor.com/install` | `agent_package_sha256`, which is **mandatory** in this mode |

Selecting `latest` without `agent_package_sha256` fails with a typed
`agent_package_pin_required` error naming that key, before any request is made.
A platform with no embedded digest and no operator pin fails the same way. A
digest mismatch fails with `agent_package_checksum_mismatch` while the payload
is still only bytes in memory: nothing has been written to the staging
directory and nothing has been executed.

The pinned default means a compromised or silently swapped CDN artifact is
rejected, and it also means the plugin installs a known-good Cursor release
rather than whatever is current.

**Bumping the pin** (maintainer task, once per Cursor release you want to adopt):

1. Pick the new `<version>` from `https://cursor.com/install`.
2. For every platform in `pinnedAgentDigests`, run
   `curl -sL https://downloads.cursor.com/lab/<version>/<os>/<arch>/agent-cli-package.tar.gz | shasum -a 256`,
   ideally twice from different networks.
3. Update `pinnedAgentVersion` and `pinnedAgentDigests` together, in one commit,
   and bump the plugin version in `plugin-store/registry.json`.

Operators who cannot wait for a plugin release verify the digest themselves and
set `agent_package_sha256`.

### Transport and archive constraints

- `https://cursor.com/install` is fetched under a size limit and **parsed**. It
  is never executed and never passed to a shell. It is fetched only in `latest`
  mode; the pinned default builds the artifact URL from its own constants and
  never reads the script.
- Exactly one artifact URL candidate must be present and must match the
  canonical `https://downloads.cursor.com/lab/<version>/${OS}/${ARCH}/agent-cli-package.tar.gz`
  template. Anything else is refused rather than guessed at.
- Every request **must use HTTPS**, on the initial URL and on every redirect
  hop. A redirect to `http://` is refused with `agent_download_insecure`; the
  installer has no cleartext path at all.
- Every request, including redirects, must target an allowlisted host
  (`cursor.com`, `www.cursor.com`, `downloads.cursor.com`). Redirect depth is
  bounded.
- The artifact download is size-bounded.
- Extraction is pure Go and refuses absolute paths, `..` traversal, symbolic
  links, hard links, device and other non-regular entries, duplicate paths, more
  entries than the configured limit, and an expanded size over the configured
  limit. Extracted files are created with `O_EXCL` and mode `0600`/`0700`.
- The extracted binary must stay inside the staging directory and must report the
  expected version through `--version` before activation.
- Activation is atomic: the staged directory is renamed into
  `<data_root>/agent/versions/<version>` and the `current` symlink is replaced
  through a rename. A failed activation restores the previous version.
- The authenticated setup status route reports the recorded digest and effective
  trust mode. The unauthenticated setup page shows only the embedded pin.

## Management surface

The status and install routes ride CLIProxyAPI's management authentication. The
install additionally requires an explicit `{"confirm": true}` request body, so no
download happens as a side effect of loading a page.

The setup page is registered as a browser resource route, which CLIProxyAPI does
**not** management-authenticate. It is therefore rendered from compile-time
constants only:

- It discloses the plugin's embedded pinned Cursor Agent version and digest.
  Those values are already public in this repository's source and in every
  release, so serving them adds no information an attacker did not have.
- It discloses **no deployment state**. Serving it does not read the plugin
  configuration, does not resolve or create the data root, does not inspect the
  managed install, and does not scan `PATH`. The effective install source,
  whether an operator pin is configured, the data root, the installed digest,
  and the resolved executable are available only from the authenticated status
  route.
- It contains no key and no account data. It asks the operator for the
  management key in the browser and sends it as an `Authorization: Bearer`
  header to the two authenticated routes.

The page loads no external assets and is served with
`Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'`,
`X-Frame-Options: DENY`, and `Referrer-Policy: no-referrer`. Its own path is
HTML-escaped before interpolation.

The authenticated routes and the resource route are registered with distinct
handler values, so a request that arrived on the unauthenticated resource route
cannot reach an authenticated route and vice versa; the trust level travels with
the dispatch target rather than being re-derived per branch.

Install failures are mapped to fixed messages. Download bodies, filesystem
paths, and child output never reach the page.

## Store distribution trust

The plugin is distributed through CLIProxyAPI's `github-release` install type.
When an operator installs it, CLIProxyAPI downloads
`cliproxy-cursor-acp_<version>_linux_amd64.zip` and the `checksums.txt` from the
**same GitHub release** and refuses the archive on a mismatch.

What that protects against: a corrupted download, and an archive swapped in
transit or by a CDN without the release's checksum file also being changed.

What it does **not** protect against: anyone who can publish or edit a release in
this repository can replace both the archive and its `checksums.txt` together.
The checksum file is not an independent anchor — it shares a trust root with the
artifact it describes.

The independent anchor is the **build provenance attestation** produced by the
release workflow (`actions/attest-build-provenance`, Sigstore-backed). It binds
the published archive to the workflow, repository, and commit that built it, and
is verifiable without trusting the release assets:

```sh
gh attestation verify cliproxy-cursor-acp_<version>_linux_amd64.zip \
  --repo sussdorff/cliproxy-cursor-acp
```

Operators with a strict supply-chain policy should verify that attestation
rather than relying on `checksums.txt` alone. The release workflow itself pins
every GitHub Action by commit SHA and the Go builder image by digest, and fails
the release if the embedded Cursor Agent digests no longer match upstream.

## Management data

The plugin emits only account label, selected model, availability, and observed
ACP token counters. It does not return tokens, cookies, raw child output, or
numeric subscription balances. `exact_subscription_quota` is null and
`subscription_quota_available` is false by design.

## Operational review

Before production use, review the persistence of the plugin data root, file
ownership of every profile directory, CLIProxyAPI management API access, child
process limits, logs, and the deployed shared-library version. The security
boundary is account isolation plus the installer's download and extraction
constraints; any change that makes profiles shared, imports arbitrary
environment variables, executes fetched content, widens the download allowlist,
or adds a private Cursor protocol requires a new security review.
