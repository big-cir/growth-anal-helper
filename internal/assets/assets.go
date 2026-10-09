// Package assets holds the files built into the engine: agent instructions and schemas, and the engine tables SQL.
package assets

import _ "embed"

// EngineGuide is the engine guide sent to the agent ({{LANGUAGE}} is filled in later).
//
//go:embed engine-guide.md
var EngineGuide string

// SummaryGuide is the guide for writing panel descriptions.
//
//go:embed summary-guide.md
var SummaryGuide string

// ActionsSchema is the JSON schema of agent actions.
//
//go:embed actions.schema.json
var ActionsSchema string

// SummarySchema is the JSON schema of panel descriptions.
//
//go:embed summary.schema.json
var SummarySchema string

// EngineTablesSQL creates the tables the engine adds to every snapshot.
//
//go:embed engine-tables.sql
var EngineTablesSQL string
