# Architecture

## Scheduling boundary

CLIProxyAPI remains the only scheduler. Its normal model route selects an
`AuthID` based on priority, weight, cooldown, retry, failover, and any session
affinity it already owns. The plugin receives that `AuthID` in the executor
request and refuses execution if it is empty or unknown.

```text
CLIProxyAPI scheduler -> selected AuthID -> account runtime -> agent acp
                             |                    |
                        safe metadata        CURSOR_CONFIG_DIR
```

The plugin has no round-robin or quota-selection code. This avoids a second
policy layer that could disagree with CLIProxyAPI or CPA Manager Plus.

## Account isolation

Each configured account creates an independent runtime containing:

- one account-specific `CURSOR_CONFIG_DIR` supplied only to the child process;
- one ACP client process started as direct argv `agent acp`;
- a conversation-to-ACP-session map scoped to that account;
- observed input/output token counters scoped to that account.

A conversation key is committed to its selected account after ACP session
creation. A failed first start rolls that provisional affinity back, so host
failover is not poisoned. A session is never migrated across accounts.
Concurrent initialization uses a mutex and closes surplus competing processes
instead of sharing one.

## ACP and host boundary

The production transport uses `github.com/coder/acp-go-sdk` for the standard
stdio JSON-RPC connection, `initialize`, `session/new`, prompt, session update,
and cancellation flow. The SDK's optional usage object becomes observed usage
only. ACP context usage is not a Cursor subscription quota.

The native plugin entrypoint speaks CLIProxyAPI's public ABI. It registers auth,
model, executor, and usage capabilities. The executor accepts a canonical
OpenAI chat payload. A host `derived_session_id`, then `execution_session_id`,
supplies affinity. Explicit payload `session_id`, `conversation_id`, and
`prompt_cache_key` are the fallback. Otherwise it creates a cryptographically
random stateless turn key so identical requests from separate callers cannot
reuse ACP state. It does not require client-added `conversation_id` or
`working_directory` fields and uses only the configured workspace root. The
host-selected `AuthID` is mandatory.

Stateless turns close their ACP session at completion and remove both account
session and conversation-affinity entries. Stable host session identities reuse
their account-scoped ACP session.

## Lifecycle and failures

Bounded concurrency applies before process/session work. An internal cancelled
context or configured execution timeout invalidates the affected account
runtime and is exposed as a retryable failure. The current native CLIProxyAPI
ABI does not carry a host cancellation handle; every ABI request is bounded by
the configured timeout. A child-process
or ACP error invalidates only the affected account runtime, closes its process
group, and returns a retryable error so CLIProxyAPI can apply its normal
cooldown/failover behavior. Invalid configuration, missing `AuthID`, and invalid
request shape are fatal errors and do not cause a different account to be used
silently.
