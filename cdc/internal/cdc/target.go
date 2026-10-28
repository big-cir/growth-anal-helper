package cdc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	gosql "github.com/go-sql-driver/mysql"
)

// positionTable holds one row per source: where the copy is, which tables it follows and their definition.
const positionTable = "growth_lab_cdc.position"

// testHooks stop or fail a commit at fixed points in tests.
var testHooks struct {
	afterApply   func() error
	beforeCommit func(tx *sql.Tx) error
	afterCommit  func() error
}

// Target is the copy: a MySQL server that receives the changes.
type Target struct {
	db       *sql.DB
	sourceID string
	tables   map[string]*targetTable
	tx       *sql.Tx
}

type targetTable struct {
	ins, upd, del string
	cols          int
}

// openTarget connects with found-rows counting, so an UPDATE that matches a row counts it even when no value changes.
func openTarget(s Source) (*sql.DB, error) {
	cfg := gosql.NewConfig()
	cfg.User, cfg.Passwd = s.User, s.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", s.Host, s.Port)
	cfg.AllowNativePasswords = true
	cfg.ClientFoundRows = true
	c, err := gosql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(2)
	return db, nil
}

// newTarget prepares the statements for the followed tables.
func newTarget(db *sql.DB, sourceID string, defs map[string]*tableDef) *Target {
	t := &Target{db: db, sourceID: sourceID, tables: map[string]*targetTable{}}
	for n, d := range defs {
		schema, table, _ := strings.Cut(n, ".")
		name := quoteName(schema) + "." + quoteName(table)
		var cols, marks, sets, match []string
		for _, c := range d.Columns {
			q := quoteName(c.Name)
			cols, marks, sets = append(cols, q), append(marks, "?"), append(sets, q+" = ?")
			// strings compare by bytes: the column collation ignores case and trailing spaces
			if c.DataType == "varchar" || c.DataType == "enum" {
				match = append(match, "BINARY "+q+" <=> BINARY ?")
			} else {
				match = append(match, q+" <=> ?")
			}
		}
		where := strings.Join(match, " AND ")
		t.tables[n] = &targetTable{
			ins:  "INSERT INTO " + name + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")",
			upd:  "UPDATE " + name + " SET " + strings.Join(sets, ", ") + " WHERE " + where,
			del:  "DELETE FROM " + name + " WHERE " + where,
			cols: len(d.Columns),
		}
	}
	return t
}

func quoteName(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// savedState is the stored row for a source.
type savedState struct {
	Position, Tables, SchemaFP string
}

// loadState creates the position table if needed and returns the stored state, if any.
// A stored row for another source stops the run.
func loadState(ctx context.Context, db *sql.DB, sourceID string) (*savedState, error) {
	for _, q := range []string{
		"CREATE DATABASE IF NOT EXISTS growth_lab_cdc",
		"CREATE TABLE IF NOT EXISTS " + positionTable + ` (
			source_id varchar(64) NOT NULL PRIMARY KEY,
			position varchar(512) NOT NULL,
			tables text NOT NULL,
			schema_fp char(64) NOT NULL,
			updated_at datetime(6) NOT NULL) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, err
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT source_id, position, tables, schema_fp FROM "+positionTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var st *savedState
	for rows.Next() {
		var id string
		var s savedState
		if err := rows.Scan(&id, &s.Position, &s.Tables, &s.SchemaFP); err != nil {
			return nil, err
		}
		if id != sourceID {
			return nil, &StopError{"the copy follows another source (server uuid " + id + ")"}
		}
		st = &s
	}
	return st, rows.Err()
}

func initState(ctx context.Context, db *sql.DB, sourceID string, s savedState) error {
	_, err := db.ExecContext(ctx, "INSERT INTO "+positionTable+" (source_id, position, tables, schema_fp, updated_at) VALUES (?, ?, ?, ?, NOW(6))",
		sourceID, s.Position, s.Tables, s.SchemaFP)
	return err
}

func (t *Target) Begin(ctx context.Context) error {
	if t.tx != nil {
		return &Error{"a copy transaction is already open"}
	}
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	t.tx = tx
	return nil
}

func (t *Target) Apply(ctx context.Context, c *Change) error {
	key := c.Schema + "." + c.Table
	tt := t.tables[key]
	if tt == nil || t.tx == nil {
		return &Error{key + ": not a followed table or no open transaction"}
	}
	var (
		q    string
		args []any
	)
	switch c.Kind {
	case Insert:
		q, args = tt.ins, c.After
	case Update:
		q, args = tt.upd, append(append([]any{}, c.After...), c.Before...)
	case Delete:
		q, args = tt.del, c.Before
	}
	res, err := t.tx.ExecContext(ctx, q, args...)
	if err != nil {
		var me *gosql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return &StopError{key + ": duplicate key in the copy; the copy differs from the source"}
		}
		return err
	}
	if h := testHooks.afterApply; h != nil {
		if err := h(); err != nil {
			return err
		}
	}
	if c.Kind == Insert {
		return nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		what := map[ChangeKind]string{Update: "update", Delete: "delete"}[c.Kind]
		return &StopError{fmt.Sprintf("%s: %s matched %d rows in the copy instead of 1; the copy differs from the source", key, what, n)}
	}
	return nil
}

func (t *Target) Commit(ctx context.Context, position string) error {
	if t.tx == nil {
		return &Error{"no open copy transaction"}
	}
	tx := t.tx
	t.tx = nil
	if err := savePosition(ctx, tx, t.sourceID, position); err != nil {
		tx.Rollback()
		return err
	}
	if h := testHooks.beforeCommit; h != nil {
		if err := h(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return &UncertainError{err}
	}
	if h := testHooks.afterCommit; h != nil {
		if err := h(); err != nil {
			return &UncertainError{err}
		}
	}
	return nil
}

func (t *Target) Rollback() {
	if t.tx != nil {
		t.tx.Rollback()
		t.tx = nil
	}
}

func (t *Target) Save(ctx context.Context, position string) error {
	if t.tx != nil {
		return &Error{"cannot save a position while a copy transaction is open"}
	}
	return savePosition(ctx, t.db, t.sourceID, position)
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func savePosition(ctx context.Context, e execer, sourceID, position string) error {
	res, err := e.ExecContext(ctx, "UPDATE "+positionTable+" SET position = ?, updated_at = NOW(6) WHERE source_id = ?", position, sourceID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return &StopError{"the position row for this source is missing in the copy"}
	}
	return nil
}
