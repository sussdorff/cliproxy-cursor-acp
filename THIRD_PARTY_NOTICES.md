# Third-party notices

This project is distributed under the MIT License.

## ACP Go SDK

The production ACP transport uses
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk), version
`v0.13.5`. That dependency is licensed under the Apache License, Version 2.0.
Its license remains applicable to that dependency; it does not change the MIT
license of this repository's original source code.
The complete Apache-2.0 license text is included at
[`LICENSES/Apache-2.0.txt`](LICENSES/Apache-2.0.txt).

No source code from Cursor's private Connect protocol or from repositories
without a usable source license is included in this repository.

## CLIProxyAPI v7.2.141

The native plugin ABI and plugin API types depend on CLIProxyAPI v7.2.141.
CLIProxyAPI is MIT licensed. Its exact upstream MIT text and copyright notice
are included in [LICENSES/CLIProxyAPI-v7.2.141-MIT.txt](LICENSES/CLIProxyAPI-v7.2.141-MIT.txt).

## nyanjou/cliproxyapi-cursor-plugin

The managed Cursor Agent CLI installer in `internal/cursor/install.go` adapts the
approach and the two artifact-URL regular expressions published by the
MIT-licensed
[`nyanjou/cliproxyapi-cursor-plugin`](https://github.com/nyanjou/cliproxyapi-cursor-plugin):
strict parsing of `https://cursor.com/install` without executing it, a download
host allowlist, bounded downloads, safe tar extraction, `--version` verification,
and atomic activation. Its published v0.4.1 history is also the source of the
requirement to capture `agent login` output in files rather than inherited pipes,
because Cursor keeps a detached worker holding those descriptors. That project is
distributed under the MIT License; its terms remain applicable to the material
adapted from it and do not change the MIT license of this repository.

## gopkg.in/yaml.v3 v3.0.1

The YAML parser is dual licensed as shipped upstream: selected libyaml-derived
files are MIT licensed and remaining files are Apache-2.0 licensed. The exact
upstream MIT notice and NOTICE are in [LICENSES/yaml.v3-MIT.txt](LICENSES/yaml.v3-MIT.txt)
and [LICENSES/yaml.v3-NOTICE.txt](LICENSES/yaml.v3-NOTICE.txt); Apache-2.0 terms
are in [LICENSES/Apache-2.0.txt](LICENSES/Apache-2.0.txt).
