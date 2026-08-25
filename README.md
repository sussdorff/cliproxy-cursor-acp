# cliproxy-cursor-acp

ACP-backed multi-account Cursor provider for CLIProxyAPI.

The project uses the official Cursor Agent ACP interface for execution while
CLIProxyAPI remains responsible for credential selection, weights, cooldowns,
and failover. Each Cursor account runs from an isolated local profile. CPA
Manager can expose account state and observed usage without reading Cursor
credential files.

The project is under active development. Its implementation does not use
Cursor's private Connect protocol.

## License

MIT. See [LICENSE](LICENSE).
