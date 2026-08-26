# Generic plugin quota contract

This plugin publishes its subscription observation as a versioned,
provider-neutral payload carried inside CLIProxyAPI auth metadata. A manager UI
can render it without knowing that the provider is Cursor.

The payload lives under a single auth-metadata key:

```
metadata["plugin_quota"]
```

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
