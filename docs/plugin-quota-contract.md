# Generic plugin quota contract

This plugin publishes its subscription observation as a versioned,
provider-neutral payload carried inside CLIProxyAPI auth metadata. A manager UI
can render it without knowing that the provider is Cursor.

The payload lives under a single auth-metadata key:

```
metadata["plugin_quota"]
```

## Where a consumer reads it

CPA-Manager-Plus reads `metadata.plugin_quota` off each entry of

```
GET /v0/management/auth-files
```

**That endpoint does not carry auth metadata in a stock CLIProxyAPI build.**
`buildAuthFileEntryLocked` in `internal/api/handlers/management/auth_files.go`
lifts only `priority` and `note` out of `AuthData.Metadata`; it emits no
`metadata` object at all. This was checked against CLIProxyAPI `v7.2.141` (the
pinned version) and `v7.2.143`.

Displaying this contract therefore requires a CLIProxyAPI build that copies the
allowlisted plugin quota onto the list entry as `metadata.plugin_quota`. That
copy is deliberately narrow:

- only `plugin_quota` is copied; every other metadata key, known or unknown,
  stays omitted, so tokens, cookies, `access_token`, `refresh_token`, raw
  `id_token` strings, profile paths, `StorageJSON`, and raw upstream bodies
  cannot reach the endpoint through this path;
- the payload is copied only when it declares `schema: cliproxy.plugin.quota`
  and a numeric `version`, so an unrelated value parked under the key is never
  republished;
- inside a well-formed contract, fields are copied verbatim, so a producer can
  add optional fields without an intermediate release having to learn them.

Publishing this contract against a CLIProxyAPI build without that change is not
an error: the payload is still written to auth metadata, it is simply not
visible to a manager UI.

## Payload

```json
{
  "schema": "cliproxy.plugin.quota",
  "version": 1,
  "provider": "cursor-acp",
  "availability": "available",
  "observed_at": "2026-08-26T09:15:00Z",
  "ttl_seconds": 900,
  "windows": [
    {
      "id": "subscription",
      "label": "Monthly usage",
      "kind": "monthly",
      "unit": "requests",
      "used": 125,
      "limit": 500,
      "remaining": 375,
      "used_percent": 25,
      "unlimited": false,
      "window_start": "2026-08-01T00:00:00Z",
      "window_end": "2026-09-01T00:00:00Z",
      "reset_at": "2026-09-01T00:00:00Z",
      "reset_accuracy": "exact"
    }
  ]
}
```

`internal/plugin/testdata/plugin_quota_cursor_v1.json` is the golden fixture for
this shape. It is asserted by `internal/plugin/adapter_test.go` and is consumed
unchanged by the CPA-Manager-Plus parser test, so a producer change that drifts
from the contract fails on both sides.

## Field rules

| Field | Rule |
|---|---|
| `schema` | Always `cliproxy.plugin.quota`. A consumer must ignore any other value. |
| `version` | Integer. Incremented only for an incompatible change. A consumer must ignore a version it does not implement. |
| `provider` | Publishing provider identifier. Presentation only; a consumer must not branch on it. |
| `availability` | `available` or `unavailable`. Anything else is treated as `unavailable`. |
| `observed_at` | RFC3339 UTC. When the observation was taken. |
| `ttl_seconds` | Positive integer. How long the observation stays displayable. |
| `windows` | Array. Empty whenever `availability` is not `available`. |

Each window:

| Field | Rule |
|---|---|
| `id` | Stable, non-empty window identity within this provider. |
| `label` | Display label. Bounded length, control characters removed. |
| `kind` | One of `five_hour`, `daily`, `weekly`, `monthly`, `billing`, `payg`, `product`, `summary`, `unknown`. |
| `unit` | Optional unit for `used` / `limit` / `remaining`. |
| `used`, `limit`, `remaining` | Optional non-negative integers. |
| `used_percent` | Optional `0`-`100`. Absent when the plan is unlimited or has no finite allowance. |
| `unlimited` | Boolean. |
| `window_start`, `window_end`, `reset_at` | Optional RFC3339 UTC. Omitted when the upstream value could not be parsed. |
| `reset_accuracy` | `exact`, `derived`, `estimated`, or `unknown`. |

## Availability and freshness

- `availability: "unavailable"` means the provider could not observe quota. It
  never means the credential is unusable.
- An observation older than `observed_at + ttl_seconds` is **stale**. A consumer
  must present it as stale or unavailable rather than as a current value.
- A window with no parseable boundary is not published at all; the contract
  reports `unavailable` instead of a window with invented times.

## What is never published

Access tokens, cookies, profile directory paths, and raw upstream response
bodies never appear in this payload or anywhere else in auth metadata.

## Evolving the contract

Add optional fields without changing `version`; consumers ignore unknown fields.
Increment `version` only when an existing field changes meaning or a required
field is added. Producers may publish only one version at a time.
