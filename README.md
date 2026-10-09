# Growth Lab

Ask questions about your product data in plain language and get back an analytics panel. An AI agent writes the SQL, a local engine runs and checks it, and saved panels are recomputed when new data arrives.

## Quick start

Requires Go 1.27+ and a C compiler to build (SQLite is compiled into the binary), plus the [Claude Code](https://docs.claude.com/en/docs/claude-code) CLI (logged in) or an API key for Anthropic or an OpenAI-compatible service ([agent settings](guide/configuration.md#agent)).

```bash
git clone <this repository>
cd growth-lab
scripts/go build        # builds bin/growth-lab
bin/growth-lab demo
```

`scripts/go` runs the Go tool with the SQLite build options the engine expects; use it instead of plain `go build`.

This creates a fictional dataset, builds a snapshot and starts the app at http://127.0.0.1:4170. Open it and ask a question about the data, for example where new members drop off in their first 7 days.

## How it works

1. **Collect** — `collect` copies allow-listed tables from your database (and optionally GA4 reports) into a local SQLite snapshot, with derived tables that hold your metric rules.
2. **Ask** — the agent (an LLM with no tools) proposes SQL and a chart. The engine runs it read-only on the snapshot and rejects results that break basic checks.
3. **Save** — panels go to a dashboard and are recomputed on each new snapshot.

See [Concepts](guide/concepts.md) for table layers, metric definitions, privacy and resource use.

The agent only sees a copy where ID columns are replaced with random numbers and private columns are removed. Questions never touch your source database or GA4.

## Use your own data

Put everything specific to your product in a git-ignored `workspace/` folder. Start from the demo:

```bash
mkdir workspace
cp examples/demo/{workspace.json,tables.json,derived.sql,derived-columns.json,metrics.json,guide.md} workspace/
cp -r examples/demo/{seed-panels,quality,tests} workspace/
```

Then edit the files ([file reference](guide/workspace.md), [settings](guide/configuration.md)) and run:

```bash
bin/growth-lab collect
bin/growth-lab serve
```

Sign-in is off by default, so anyone who can reach the server uses it as an admin. To require accounts, set `"server": { "auth": true }` and add one with `bin/growth-lab account add <name> --role admin`.

## Commands

| Command | |
|---|---|
| `bin/growth-lab demo` | Run the demo |
| `bin/growth-lab serve` | Start the web app |
| `bin/growth-lab collect` | Build a new snapshot |
| `bin/growth-lab derive` | Rebuild derived tables only |
| `bin/growth-lab test-derived` | Test derived-table rules |
| `bin/growth-lab verify` | Compare the snapshot with the source |
| `bin/growth-lab eval` | Run evaluation cases |
| `bin/growth-lab trace` | Show where web requests spend their time |
| `bin/growth-lab account …` | Manage accounts |
| `scripts/go test ./...` | Run tests |

## Development checks

The engine and server do not require Node.js. The browser renderer has a small standalone JavaScript test suite; run it with Node.js when changing `web/`:

```bash
scripts/go test ./...
node --test test/web/charts.test.js
bin/growth-lab public-check --require-denylist
```
