
# Configuration

## `workspace.json`

```json
{
  "name": "my-product",
  "datasource": {
    "host": "mysql://127.0.0.1:3306",
    "user": "readonly",
    "password": "secret",
    "database": "mydb"
  },
  "policy": { "readablePrefixes": ["r_", "d_", "snapshot_"] },
  "params": { "calendar_start": "2024-01-01 00:00:00.000000" },
  "agent": { "model": "sonnet" },
  "server": { "port": 4170 }
}
```

| Key                                                      | Default             | Description                                                                                                                                                                |
| -------------------------------------------------------- | ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`                                                 | required            | Workspace name                                                                                                                                                             |
| `language` | `"en"` | Language of the web UI, server messages and agent-written text: `"en"` or `"ko"` |
| `datasource` | required | See [Data source](#data-source) |
| `outDir`                                               | the workspace       | Output folder (a subfolder of the workspace)                                                                                                                               |
| `policy.readablePrefixes`                              | required            | Table prefixes the agent's exploration queries may read                                                                                                                    |
| `policy.panelReadablePrefixes`                         | `["d_"]`          | Table prefixes panels may read (must be within`readablePrefixes`)                                                                                                        |
| `params`                                               | `{}`              | Values for derived and panel SQL: numbers, timestamps,`"timestamp~timestamp"` ranges, or arrays of those. Scalars can be used as `:name` in SQL. Free text is rejected |
| `params.calendar_start`                                | –                  | First week of`d_calendar_week`                                                                                                                                           |
| `params.quality_min_ts`, `params.quality_gap_ranges` | –                  | Date checks: values earlier than a minimum, values inside known data gaps                                                                                                  |
| `agent.provider` | `"claude-code"` | How the agent is called. See [Agent](#agent) |
| `agent.bin` | `"claude"` | Claude Code executable (`claude-code` only) |
| `agent.model` | `null` | Model name. With `claude-code`, `null` uses Claude Code's default (e.g. `"sonnet"`, `"opus"`). Required for API providers |
| `agent.maxTurns` / `maxProbes` / `maxFixes`        | 8 / 4 / 2           | Per-request limits on agent turns, exploration queries and panel fixes                                                                                                     |
| `agent.callBudgetUsd` / `requestBudgetUsd` | 0.5 / 1.0 | Cost caps per call and per request. For API providers they apply only when `pricing` is set |
| `agent.callTimeoutMs`                                  | 90000               | Timeout for one agent call                                                                                                                                                 |
| `agent.concurrency`                                    | 2                   | Concurrent heavy jobs (agent calls and queries)                                                                                                                            |
| `agent.dataMode`                                       | `"pseudonymized"` | `"schema_only"` sends no result rows to the agent, only statistics                                                                                                       |
| `run.heapLimitMb`                                      | 2048                | SQLite heap limit in the query worker process                                                                                                                              |
| `server.port`                                          | 4170                | Port on`127.0.0.1`                                                                                                                                                       |
| `server.auth` | `false` | `false`: no sign-in; every request is the admin `local`. `true`: accounts and roles (viewer, editor, admin) are required. Must be `true` with `publicOrigin` |
| `server.publicOrigin`, `server.proxyHops`            | –                  | Public HTTPS address when running behind a reverse proxy                                                                                                                   |
| `server.auditRetentionDays`                            | 90                  | Audit log retention                                                                                                                                                        |
| `ga4` | – | GA4 connection ([GA4](#ga4)) |

Restart the server after changing settings.

## Agent

The agent never gets tools: it only returns JSON in a fixed format.

**Claude Code** (default): runs `claude -p` with all tools turned off and checks this at startup. Requires the [Claude Code](https://docs.claude.com/en/docs/claude-code) CLI, logged in.

**Anthropic API:**

```json
"agent": { "provider": "anthropic", "model": "claude-sonnet-5-5", "apiKey": "sk-ant-..." }
```

**OpenAI-compatible API** (OpenAI, Gemini, OpenRouter, Ollama, vLLM, ...): set `baseUrl` to the service's OpenAI-compatible endpoint.

```json
"agent": {
  "provider": "openai",
  "model": "gemini-2.5-flash",
  "apiKey": "...",
  "baseUrl": "https://generativelanguage.googleapis.com/v1beta/openai",
  "pricing": { "inputPerMTok": 0.3, "outputPerMTok": 2.5 }
}
```

| Key | Default | Description |
|---|---|---|
| `apiKey` | – | API key. Required for `anthropic`; may be empty for a local server |
| `baseUrl` | `https://api.anthropic.com` / `https://api.openai.com/v1` | API address |
| `pricing` | – | Dollars per million input / output tokens from your provider's price list (the values above are examples), used for the cost caps |
| `maxOutputTokens` | 8192 | Response length limit (`anthropic`) |

With an API provider, the engine keeps each conversation in `agent-sessions/` and sends it with every call.

## Data source

`host` sets the database type and address: `mysql://host[:port]` or `postgres://host[:port]`. The default ports are 3306 and 5432. The engine connects directly, so no database client is needed. Use a read-only user.

For a SQLite file, set only `host`:

```json
"datasource": { "host": "sqlite://data/source.sqlite" }
```

## GA4

`collect` can import GA4 Data API reports into the snapshot as `r_ga4_*` tables.

**1. Connection** in `workspace.json`:

```json
"ga4": {
  "property_id": "123456789",
  "time_zone": "America/Los_Angeles",
  "key_file": "~/keys/service-account.json"
}
```

`time_zone` must match the property. `key_file` is the service account key downloaded from Google Cloud, with permissions `0600`.

**2. `ga4-reports.json`** in the workspace:

```json
{
  "start": "2024-01-01",
  "reports": ["daily_overview", "weekly_users", "daily_events", "daily_channel"],
  "events": { "SIGN_UP_COMPLETE": "sign_up", "POST_CREATE": "post_create" },
  "min_users": 10,
  "custom": [
    { "id": "daily_country", "range": "daily", "dimensions": ["countryId"], "metrics": ["activeUsers", "newUsers"] }
  ]
}
```

| Built-in report                     | Contents                                                                       |
| ----------------------------------- | ------------------------------------------------------------------------------ |
| `daily_overview`                  | Daily active, new and total users, sessions, engaged sessions, engagement time |
| `weekly_users`, `monthly_users` | Active and new users for complete weeks / months                               |
| `daily_events`                    | Only the events mapped in`events`; daily count and users                     |
| `daily_channel`                   | By first-user default channel group                                            |
| `daily_platform`                  | By platform and device category                                                |
| `daily_new_returning`             | New vs returning                                                               |
| `weekly_cohort`                   | Weekly acquisition cohorts, last 12 complete weeks                             |

**Custom reports** use an engine allow-list:

- Up to 10 reports. `id`: lowercase letters, digits and `_`, up to 31 characters; the table is `r_ga4_x_<id>`
- range: `daily`, `weekly`, `monthly`
- dimensions (0–2): `firstUserDefaultChannelGroup`, `sessionDefaultChannelGroup`, `platform`, `deviceCategory`, `newVsReturning`, `operatingSystem`, `dayOfWeek`, `countryId`, `languageCode`, `hour`, `eventName`
- metrics (1–8): `activeUsers`, `totalUsers`, `newUsers`, `sessions`, `engagedSessions`, `eventCount`, `screenPageViews`, `userEngagementDuration`, `averageSessionDuration`, `engagementRate`
- At most one fine-grained dimension (`countryId`, `languageCode`, `hour`, `eventName`) per report, combined with user-count metrics only (`eventName` requires `totalUsers` and also allows `eventCount`).

`min_users` (default 10): in broken-down reports, groups smaller than this are dropped and smaller values are stored as NULL.

Panels read only `d_*` tables, so copy the GA4 tables you need into `d_ga4_*` in `derived.sql`.
