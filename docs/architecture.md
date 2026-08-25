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

The same conversation key may exist under two accounts, but it creates two ACP
sessions. A session is never migrated across accounts. Concurrent initialization
uses a mutex and closes surplus competing processes instead of sharing one.

## ACP and host boundary

The production transport uses `github.com/coder/acp-go-sdk` for the standard
stdio JSON-RPC connection, `initialize`, `session/new`, prompt, session update,
and cancellation flow. The SDK's optional usage object becomes observed usage
only. ACP context usage is not a Cursor subscription quota.

The native plugin entrypoint speaks CLIProxyAPI's public ABI. It registers auth,
model, executor, and usage capabilities. The executor accepts a JSON payload
containing `prompt`, `conversation_id`, and `working_directory`; it returns a
completed OpenAI-compatible response. The host-selected `AuthID` is mandatory.

## Lifecycle and failures

Bounded concurrency applies before process/session work. A cancelled context
causes ACP cancellation and is exposed as a retryable failure. A child-process
or ACP error invalidates only the affected account runtime, closes its process
group, and returns a retryable error so CLIProxyAPI can apply its normal
cooldown/failover behavior. Invalid configuration, missing `AuthID`, and invalid
request shape are fatal errors and do not cause a different account to be used
silently.
