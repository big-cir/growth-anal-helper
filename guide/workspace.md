# Workspace files

A workspace is a git-ignored folder for one product. `examples/demo/` is a complete example.

| File                                 | Required    | Purpose                                                                                     |
| ------------------------------------ | ----------- | ------------------------------------------------------------------------------------------- |
| `workspace.json`                   | yes         | Source, policy, parameters, agent and server settings ([configuration.md](configuration.md)) |
| `tables.json`                      | yes         | Collection spec: tables, columns, keys, cutoff column, column roles                         |
| `derived.sql`                      | yes         | SQL that builds derived tables (`d_*`)                                                    |
| `derived-columns.json`             | yes         | Role of every derived column                                                                |
| `guide.md`                         | recommended | Product guide for the agent                                                                 |
| `metrics.json`                     | recommended | Metric dictionary                                                                           |
| `seed-panels/*.json`               | recommended | Example panels for the agent and suggested questions                                        |
| `tests/<case>/`                    | recommended | Fixtures for derived rules                                                                  |
| `quality/<id>.sql` + `<id>.json` | optional    | Product-specific data quality checks                                                        |
| `verify.json`                      | optional    | Source cross-check query pairs                                                              |
| `ga4-reports.json`                 | optional    | GA4 reports ([configuration.md](configuration.md#ga4))                                       |

## `tables.json`

```json
[
  {
    "source": "member", "target": "r_member", "key": ["id"], "cutoffColumn": "created_at",
    "columns": [
      { "expr": "id",         "as": "id",         "kind": "int",  "role": { "identifier": "member" } },
      { "expr": "created_at", "as": "created_at", "kind": "ts",   "role": "ordinary" },
      { "expr": "deleted_at", "as": "deleted_at", "kind": "ts",   "role": "ordinary", "nullAfterCutoff": true },
      { "expr": "country",    "as": "country",    "kind": "text", "maxLength": 8, "role": "ordinary" }
    ]
  }
]
```

| Key                     | Meaning                                                                                       |
| ----------------------- | --------------------------------------------------------------------------------------------- |
| `source` / `target` | Source table / snapshot table. Targets start with`r_`; `r_ga4_` is reserved for GA4       |
| `key`                 | Integer key columns (composite allowed). Rows are read in this order and the order is checked |
| `cutoffColumn`        | Timestamp column used to drop rows created after the cutoff                                   |
| `columns[].expr`      | A single column name, or`col IS NULL` / `col IS NOT NULL`. No other SQL                   |
| `columns[].kind`      | `int`, `ts` (timestamp), `text` (with `maxLength`, default 64), `bool`              |
| `nullAfterCutoff`     | Set the value to NULL if it is later than the cutoff (use for deleted/left timestamps)        |

### Column roles

| Role                             | Meaning                                                                          | In the agent's copy              |
| -------------------------------- | -------------------------------------------------------------------------------- | -------------------------------- |
| `{ "identifier": "<domain>" }` | Integer ID. Columns of the same domain get the same mapping, so joins still work | Replaced by random large numbers |
| `"ordinary"`                   | Safe to show (timestamps, categories, counts)                                    | As is                            |
| `"private"`                    | Sensitive; collected but not for the agent                                       | Removed                          |

Every column needs a role.

## `derived.sql` and `derived-columns.json`

- SQLite SQL that creates `d_*` tables from `r_*` tables. Also available: `snapshot_meta` (`source_cutoff_at`), `snapshot_params` (`workspace.json` params); `d_calendar_week` is added afterwards.
- Timestamps are strings `YYYY-MM-DD HH:MM:SS.ffffff`.
- `derived-columns.json` maps every `"d_table.column"` to a role.

## Fixtures (`tests/<case>/`)

- `input.sql`: small input data (`r_*` rows, one `snapshot_meta` row, `snapshot_params`).
- `expected.json`: `[{ "query": "SELECT …", "rows": [ { … } ] }]`.

`node src/cli.ts test-derived` runs `derived.sql` on each case and compares results.

## `guide.md`

Explains your product to the agent: terms, rules already in derived tables, defaults, and a column table (``| `d_table.column` | description |``).

## `metrics.json`

```json
{
  "dimension_tables": ["d_calendar_week"],
  "metrics": [
    {
      "id": "first_week_activation",
      "name": "First-week activation",
      "asks": ["Where do new members stop during their first 7 days?"],
      "tables": ["d_member_first_week"],
      "definition": [["Population", "Members 7+ days after sign-up"], ["Window", "First 7 days"]],
      "seed_panel": "activation_funnel"
    }
  ]
}
```

A panel may read only its metric's `tables` plus `dimension_tables`. Panels outside the dictionary need user approval.

## Example panels (`seed-panels/<id>.json`)

Example panels for the agent: `id`, `metric`, `title`, `question`, `sql`, `display`, `definition`, `caveats`, `answers`.

| `display.type`  | Required result columns                                                                     |
| ----------------- | ------------------------------------------------------------------------------------------- |
| `number`        | `numerator` + `denominator`, or `value`                                               |
| `line`, `bar` | `x`, `numerator`, `denominator` (optional `series`)                                 |
| `funnel`        | `step_no`, `step_name`, `reached`, `eligible`, `unknown` (optional `cohort`)    |
| `cohort`        | `cohort`, `period`, `numerator`, `denominator` (optional `series`, `deleted_n`) |
| `table`         | free form                                                                                   |

Return integer numerators and denominators; the engine computes ratios.

## Quality checks (`quality/<id>.sql` + `<id>.json`)

```json
{ "title": "Rows with missing references", "display": "table" }
```

Shown in the admin data quality tab.

## `verify.json`

```json
{
  "checks": [
    {
      "id": "weekly_signups",
      "title": "Members who signed up that week",
      "source_sql": "SELECT count(*) AS n FROM member WHERE created_at >= :week_start AND created_at < :week_end",
      "snapshot_sql": "SELECT count(*) AS n FROM d_member WHERE signup_at >= :week_start AND signup_at < :week_end"
    }
  ]
}
```

Each pair must return one row with the same columns. The week defaults to five weeks before the snapshot; use `--week YYYY-MM-DD` to change it.
