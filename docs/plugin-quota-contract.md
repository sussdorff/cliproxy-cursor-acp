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

Displaying this contract therefore requires two downstream builds that this
plugin repository does not ship: a CLIProxyAPI **host patch** that projects
`metadata.plugin_quota` on the list (and serves refresh-quota), and a CPAMP
**image fork** that renders it. Exact remotes and rebuild steps are in
[quota-stack-origins.md](quota-stack-origins.md).

The host does not pass the payload through; it rebuilds it from an allowlist:

- only `plugin_quota` is projected; every other metadata key, known or unknown,
  stays omitted, so tokens, cookies, `access_token`, `refresh_token`, raw
  `id_token` strings, profile paths, `StorageJSON`, and raw upstream bodies
  cannot reach the endpoint through this path;
- the payload is projected only when it declares `schema:
  cliproxy.plugin.quota` and a `version` the host implements, so an unrelated
  value parked under the key is never republished;
- **inside the contract, the host emits a version-1 field allowlist and drops
  every unknown field.** Auth metadata is plugin-controlled all the way down, so
  a well-formed envelope is no evidence that what it wraps is safe: a value
  nested inside an otherwise valid contract is dropped exactly like one placed
  beside it;
- the allowlisted fields are themselves bounded - at most 32 windows, and text
  values dropped rather than truncated past 256 bytes.

The allowlist is the version-1 envelope (`schema`, `version`, `provider`,
`availability`, `observed_at`, `ttl_seconds`, `windows`) and window field set
documented below, so a contract this plugin publishes today survives the
projection intact.

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
the minimal single-window shape. It is asserted by `internal/plugin/adapter_test.go`
and is consumed unchanged by the CPA-Manager-Plus parser test, so a producer
change that drifts from the contract fails on both sides.

A live Cursor observation can publish several windows in that same schema.
They follow CodexBar's included-plan mapping from
`GET https://cursor.com/api/usage-summary` and, when the account has an included
Bot allowance, `POST https://cursor.com/api/dashboard/get-sand-usage-status`:

| `id` | Label | Source |
|---|---|---|
| `cursor` | Cursor | `plan.autoPercentUsed` (Auto + Composer); this is the only interval window |
| `third_party` | Third Party | `plan.apiPercentUsed` (named / API models); published without interval boundaries |
| `grok_bot` | Grok Bot | included Sand usage; omitted when `hasNonZeroIncludedLimit` is false; published without interval boundaries |

The included-plan Total window and the daily cost histogram are observed internally and not published. A generic UI then renders Cursor as the main quota card and the satellite allowances as other quota items.

Optional `spend` comes from
`POST https://cursor.com/api/dashboard/get-filtered-usage-events` over the last
30 days. `spend.metered_cents` is the plan deduction (`chargedCents`);
`today_cents` / `period_cents` are vendor list-price
(`tokenUsage.totalCents`). Raw events never leave the plugin.

A consumer treats an observation older than `ttl_seconds` as stale. This
producer uses a six-hour freshness budget so a missed host refresh does not
blank the Quota tab.

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

Adding an optional field is compatible but not immediately visible. The host
projects a fixed allowlist, so a field it has not been taught is dropped before
any consumer sees it: a new optional field needs a CLIProxyAPI release that
learns it before a manager UI can display it. Roll out in that order - host
first, then producer - or the field is silently absent rather than ignored.
