# Architecture

## Scheduling boundary

CLIProxyAPI remains the only scheduler. Its normal model route selects an
`AuthID` based on priority, weight, cooldown, retry, failover, and any session
affinity it already owns. The plugin receives that `AuthID` in the executor
request and refuses execution if it is empty or not registered.

```text
CLIProxyAPI scheduler -> selected AuthID -> account runtime -> agent acp
                             |                    |
                        safe metadata        CURSOR_CONFIG_DIR
```

The plugin has no round-robin or quota-selection code. This avoids a second
policy layer that could disagree with CLIProxyAPI or CPA Manager Plus.

## Account lifecycle

Accounts are created at runtime, never from plugin configuration.

1. **Login.** `StartLogin` creates a fresh private profile directory under
   `<data_root>/profiles/<random>` with mode `0700`, spawns the official
   `agent login` inside it, and returns the single approval URL it printed
   together with a random login state. `PollLogin` reports pending while the CLI
   runs, then confirms the result with `agent status` and enriches it with
   `agent about`.
2. **Identity.** The `AuthID` is `cursor-` plus the first twelve hex characters
   of the sha256 of the lowercased account email, or `cursor-<profile-id>` when
   no email is available. Re-authenticating one Cursor account therefore
   refreshes one host auth record instead of multiplying it.
3. **Persistence.** The plugin returns an auth record whose stored payload names
   only the type, auth id, profile directory, label, email, and model. The host
   persists it.
4. **Restart.** `ParseAuth` rebuilds the runtime account from that stored record
   alone. There is no `accounts:` configuration block to keep in sync.
5. **Failure.** A failed, expired, ambiguous, or over-talkative login kills the
   child process group and removes the private profile directory, so no partial
   account is left behind. Expiry is a hard gate: once the deadline passes the
   attempt is refused even if the CLI already succeeded. Abandoned attempts are
   swept on every start and every poll, and a running login is registered before
   its approval URL exists so shutdown can always find it.

## Path resolution

CLIProxyAPI only reveals its auth directory on auth and model requests, so the
data root is resolved lazily and cached:

1. the `data_root` configuration key, when set;
2. otherwise `<host auth dir>/cliproxy-cursor-acp`, using the first non-empty
   auth directory the host ever supplies;
3. otherwise a typed failure naming the `data_root` configuration key.

`<data_root>/workspace`, `<data_root>/profiles`, and `<data_root>/agent` are
derived from it and created with mode `0700`.

The official CLI is resolved in a fixed order: the `executable` configuration
key, then the managed install at `<data_root>/agent/current`, then `agent` on
`PATH`. The managed install outranks `PATH` because its artifact digest was
verified against a trusted pin.

## Account isolation

Each registered account owns an independent runtime containing:

- one account-specific `CURSOR_CONFIG_DIR` supplied only to its child process;
- one ACP client process started as direct argv `agent acp`;
- a conversation-to-ACP-session map scoped to that account;
- observed input/output token counters scoped to that account.

Registration refuses two accounts that resolve to the same profile directory. A
conversation key is committed to its selected account after ACP session
creation; a failed first start rolls that provisional affinity back so host
failover is not poisoned. A session is never migrated across accounts.
Concurrent initialization uses a mutex and closes surplus competing processes
instead of sharing one.

## ACP and host boundary

The production transport uses `github.com/coder/acp-go-sdk` for the standard
stdio JSON-RPC connection, `initialize`, `session/new`, prompt, session update,
and cancellation flow. The SDK's optional usage object becomes observed usage
only. ACP context usage is not a Cursor subscription quota.

The native plugin entrypoint speaks CLIProxyAPI's public ABI. It registers auth,
model, executor, usage, and management capabilities. The executor accepts
OpenAI Chat Completions and Responses in streaming and non-streaming form. It
decodes caller tools and results from `ExecutorRequest.OriginalRequest` and
`SourceFormat`; the translated payload is not allowed to erase the caller's
protocol. A host `derived_session_id`, then
`execution_session_id`, supplies affinity. Explicit payload `session_id`,
`conversation_id`, and `prompt_cache_key` are the fallback. Otherwise it creates
a cryptographically random stateless turn key so identical requests from
separate callers cannot reuse ACP state. It does not require client-added
`conversation_id` or `working_directory` fields and uses only the resolved
workspace root. The host-selected `AuthID` is mandatory.

Stateless turns close their ACP session at completion and remove both account
session and conversation-affinity entries. Stable host session identities reuse
their account-scoped ACP session.

## Caller-owned tool bridge

The tool bridge is a two-request continuation protocol around one live ACP
prompt. The initial HTTP request can finish while the ACP prompt goroutine stays
blocked on its callback:

```text
OpenCode request                   official Cursor ACP prompt
      |                                      |
      | tools + prompt                       |
      +------------ StartTurn -------------->|
                                             | fs/read_text_file
      <----- OpenAI function call -----------+

OpenCode executes locally under its permission policy

      | tool result                          | same blocked callback
      +------------ ResumeTurn ------------->|
                                             | callback response
      <----- next tool call or final text ----+
```

Relative filesystem paths and terminal working directories are resolved below
`workspace_root`; anything outside that lexical boundary is refused. OpenCode
and the ACP process must therefore share the resulting absolute workspace path
namespace; the bridge deliberately has no host or container path mapper.

Paused prompts retain their global concurrency permit and their selected
account's serialized ACP turn permit. This keeps the existing process and
scheduler bounds authoritative while the callback is outstanding; operators
must configure `max_concurrent` for their intended number of pending prompts.
`StartTurn` acquires that global permit before it allocates persistent turn
state or starts the ACP goroutine, so requests waiting for capacity do not grow
the pending-turn registry.

`StartTurn` creates a background context bounded by the configured timeout, not
by the lifetime of the HTTP handler that returned the tool call. The pending
turn holds the selected `AuthID`, conversation, account process generation, ACP
session ID, monotonically increasing turn generation, and exact supported tool
map. Every callback receives a cryptographically random opaque call ID. A result
is checked against every binding and removed atomically before delivery, so it
is consumed exactly once. Wrong-account and wrong-conversation attempts do not
consume the valid pending call; duplicate, stale, malformed, or generation-
mismatched results are refused.

The only mappings are OpenCode `read`, `write`, and `bash`. The adapter validates
their documented schemas from the original request and converts ACP arguments
without inspecting the named files. A `bash` result becomes an in-memory
synthetic ACP terminal scoped to the active turn; subsequent output, exit, wait,
kill, and release callbacks read or erase only that result. No plugin process
executes the command. Environment overrides are refused because OpenCode's
documented `bash` schema has no exact field for them.

ACP permission callbacks are cancelled so the plugin never authorizes an
agent-owned operation. Filesystem and terminal client callbacks take the
separate caller-owned path above. OpenCode remains the outer permission
boundary, and its denied or failed result aborts that callback and invalidates
the affected ACP process.

The same event is rendered directly into the caller's protocol. Chat
Completions uses `tool_calls` plus `finish_reason: tool_calls`; Responses uses a
completed `function_call` output item. Streaming emits protocol-valid SSE tool
and final-message events. Cursor usage is present only on the final result and
is omitted from paused tool-call events when ACP supplied none.

## Managed installer

The management capability registers two authenticated routes under the host's
management base path and one browser-navigable resource:

| Route | Method | Purpose |
|---|---|---|
| `/v0/management/plugins/cursor-acp/setup/status` | GET | Reports `agent_installed`, `agent_version`, `data_root`, and `resolved_executable`. |
| `/v0/management/plugins/cursor-acp/setup/install` | POST | Installs the official CLI after an explicit `{"confirm": true}`. |
| `<resource base>/setup` | GET | Self-contained setup page that drives both routes. |

By default the install downloads the release-pinned artifact whose version and
per-platform sha256 are embedded in `internal/cursor/install.go`, over HTTPS,
from the canonical `downloads.cursor.com` URL, with every redirect hop held to
HTTPS and the download-host allowlist. It verifies the digest **before**
extraction and **before** running `--version`, then extracts in pure Go with
traversal/link/entry-count/expanded-size protection and activates atomically
under `<data_root>/agent/versions/<version>` behind a `current` symlink. A JSON
manifest inside the version directory records the archive-relative binary path
and the verified digest, so executable resolution never has to guess the package
layout and the status route can report what was installed.

`agent_install_source: latest` is an explicit operator override that parses
`https://cursor.com/install` without executing it; it requires
`agent_package_sha256`, because this build carries no trustworthy digest for a
release it does not know. Every path ends at the same rule: no unverified bytes
are extracted or executed.

## Lifecycle and failures

Bounded concurrency applies before process/session work. An internal cancelled
context or configured execution timeout invalidates the affected account runtime
and is exposed as a retryable failure. The current native CLIProxyAPI ABI does
not carry a host cancellation handle; every ABI request is bounded by the
configured timeout. A child-process or ACP error invalidates only the affected
account runtime, closes its process group, and returns a retryable error so
CLIProxyAPI can apply its normal cooldown/failover behavior. Invalid
configuration, missing `AuthID`, and invalid request shape are fatal errors and
do not cause a different account to be used silently.

Plugin shutdown terminates every unfinished login, removes its private profile
directory, cancels every pending tool callback, erases its call and synthetic
terminal state, and closes every account-owned ACP process group.
