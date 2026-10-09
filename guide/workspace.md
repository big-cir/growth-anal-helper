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
| `eval/cases/*.json` | optional | Evaluation cases ([Evaluation](#evaluation)) |
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

`bin/growth-lab test-derived` runs `derived.sql` on each case and compares results.

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

## Evaluation

`eval/cases/<id>.json` holds questions with the expected outcome. `examples/demo/eval/cases/` has one of each kind.

```json
{
  "id": "board_join_12w",
  "kind": "metric",
  "question": "For each of the last 12 complete signup weeks, what share of new members joined a board within 7 days?",
  "tags": ["core"],
  "expect": { "action": "panel", "metric": "first_week_activation", "pattern": ["line", "bar"] },
  "reference": "reference/board_join_12w.json",
  "x_grain": "week"
}
```

| Key | Meaning |
|---|---|
| `kind` | `metric` or `breakdown` (expects a panel), `ambiguous` (expects a question back), `refuse` (expects a refusal) |
| `expect` | `panel`: metric id (or `null`) and allowed patterns (`line` and `bar` are interchangeable). `refuse`: `"via": "preflight"` (blocked by the input check) or `"agent"` |
| `reference` | A complete panel (same format as an example panel) whose result is the correct answer |
| `x_grain` | `day`, `week` or `month`: align time keys before comparing |
| `label_aliases` | Accepted alternative step names or labels |

```bash
bin/growth-lab eval check                 # validate cases, check the snapshot matches the workspace
bin/growth-lab eval freeze                # store the reference results for the current snapshot
bin/growth-lab eval --runs 2              # run every case twice and save a report
bin/growth-lab eval compare <a.json> <b.json>
```

Runs use the workspace's agent settings (`--model` overrides the model). Results are compared exactly by pattern role; labels that differ are listed for review. Reports and stored results go to `eval/` in the output folder.

## Tracing

The web server writes one line per step of each request (agent call, probe query, panel run, wait for an answer) to `logs/trace/<date>.jsonl` under the output folder: times, token counts and outcomes, never question text, SQL or results.

```bash
bin/growth-lab trace --days 7   # request time percentiles, time outside the model API, cache hit rate, failures, slowest requests
```

Eval reports keep the same steps per run (with SQL) under `steps`.
