# Security constraints

## Credential ownership

Cursor credentials are owned by the official Cursor Agent CLI inside each
private profile directory. This plugin never reads, parses, copies, edits, or
serializes those files. Operators authenticate each profile through `agent
login` and ensure the profile directory is owner-only.

## Process isolation

The child process gets direct argv only: `agent acp`. No shell is invoked. Its
environment keeps a small allowlist (`PATH`, `HOME`, temporary-directory,
terminal, and locale settings), removes `CURSOR_API_KEY`, replaces any inherited
`CURSOR_CONFIG_DIR`, and sets the selected account's directory. It runs in its
own process group; cancellation and shutdown terminate that group.

Never configure two accounts with the same profile directory. Do not mount a
profile directory into other containers, log its contents, or place credentials
in CLIProxyAPI auth records/configuration.

## Management data

The plugin emits only account label, selected model, availability, and observed
ACP token counters. It does not return private paths, tokens, cookies, raw
child output, or numeric subscription balances. `exact_subscription_quota` is
null and `subscription_quota_available` is false by design.

## Operational review

Before production use, review file ownership of every configured profile,
CLIProxyAPI management API access, child process limits, logs, and the deployed
shared-library version. The security boundary is account isolation; any change
that makes profiles shared, imports arbitrary environment variables, or adds a
private Cursor protocol requires a new security review.
