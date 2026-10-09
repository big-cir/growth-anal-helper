# Concepts

## Computing, not searching

Growth Lab answers questions by running SQL on a snapshot of your data. It is not a document search tool: there are no embeddings, no vector database and no retrieval step. A question like "how many members signed up last week" is answered by counting rows, not by finding similar text.

Reusable metric definitions are written in the workspace:

- **Derived tables** (`derived.sql`) hold the rules, such as which members count as active or which rows to exclude.
- **The metric dictionary** (`metrics.json`) names each metric, its definition and the tables it reads.
- **The product guide** (`guide.md`) explains terms and defaults.

Each agent call includes the snapshot schema the agent can read, the metric dictionary and the guide. The agent may propose a panel outside the dictionary, but the user has to approve it.

## Table layers

| Prefix | Built by | Contents |
| --- | --- | --- |
| `r_*` | `collect`, from `tables.json` | Copies of source tables |
| `d_*` | `derived.sql` | Tables with your rules applied |

Exploration queries read the prefixes in `policy.readablePrefixes`. Panels read only `policy.panelReadablePrefixes`, which defaults to `d_*`. Engine tables (`snapshot_meta`, `snapshot_params`, collection logs) are not available to the agent.

## Why panels read derived tables

If panels read `r_*` tables directly, the agent would re-implement rules such as "exclude deleted members" in each panel, and two panels about the same metric could disagree. Writing the rule once in `derived.sql` keeps every panel on the same definition, and fixtures can test it (`bin/growth-lab test-derived`).

You can add `r_` to `policy.panelReadablePrefixes`, but the numbers then depend on how the agent interprets the raw data each time.

To add a new kind of analysis: collect the source table, build a `d_*` table from it, then add the metric to the dictionary and the guide. See [workspace.md](workspace.md) for the files. If only `derived.sql` or column roles changed, `bin/growth-lab derive` rebuilds the derived tables without reading the source again.

## Privacy

The agent works on a copy of the snapshot where identifier columns are replaced with random numbers and private columns are removed. Each panel runs on both the original and this copy, and is rejected if the results differ. IDs can therefore be used for counting, de-duplication and joins, but not for ordering or ranges. With `agent.dataMode: "schema_only"`, no result rows reach the agent at all.

## Snapshot files and resources

Each snapshot has three files:

| File | Contents |
| --- | --- |
| `<id>.sqlite` | Full snapshot |
| `<id>.agent.sqlite` | The agent's copy |
| `<id>.pseudo-map.sqlite` | Mapping between real and random identifiers |

- **Snapshot build:** creating the agent's copy keeps every distinct identifier value in memory, so memory grows with the number of distinct IDs. This memory can be reclaimed once the copy is written.
- **Queries:** each query opens the snapshot read-only in a worker process. `run.heapLimitMb` (default 2048) sets SQLite's heap limit there.
