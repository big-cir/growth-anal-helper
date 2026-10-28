package cdc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"

	gosql "github.com/go-sql-driver/mysql"
)

const (
	// positionTable holds one row per source: where the copy is, which tables it follows and their definition.
	positionTable = "growth_lab_cdc.position"
	// ownerTable lists the staging tables an init created, so a restart removes only those.
	ownerTable = "growth_lab_cdc.staging_owner"
	stagingDB  = "growth_lab_cdc_staging"
	oldDB      = "growth_lab_cdc_old"
)

// Phases of the stored row. run starts only in phaseReady.
const (
	phaseCopying    = "copying"
	phaseConverging = "converging"
	phaseCatchingUp = "catching_up"
	phaseSwapping   = "swapping"
	phaseReady      = "ready"
	// phaseLegacy marks a row stored before init existed; its schema fingerprint is in an older form.
	phaseLegacy = "legacy"
)

// testHooks stop or fail at fixed points in tests.
var testHooks struct {
	afterApply   func() error
	beforeCommit func(tx *sql.Tx) error
	afterCommit  func() error
	initStep     func(step string) error
}

func hook(step string) error {
	if h := testHooks.initStep; h != nil {
		return h(step)
	}
	return nil
}

// Target is the copy: a MySQL server that receives the changes.
type Target struct {
	db       *sql.DB
	sourceID string
	tables   map[string]*targetTable
	tx       *sql.Tx
	mode     ApplyMode
	epoch    int64
}

type targetTable struct {
	ins, upd, del string // strict
	upsert, delPK string // converge
	pk            []int
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
	db.SetMaxOpenConns(4)
	return db, nil
}

// newTarget prepares the statements for the followed tables, keyed by their source "db.table",
// writing into dataDB. Its writes check epoch.
func newTarget(db *sql.DB, sourceID, dataDB string, defs map[string]*tableDef, epoch int64) *Target {
	t := &Target{db: db, sourceID: sourceID, tables: map[string]*targetTable{}, epoch: epoch}
	for n, d := range defs {
		_, table, _ := strings.Cut(n, ".")
		name := quoteName(dataDB) + "." + quoteName(table)
		var cols, marks, sets, match, news, keyMatch []string
		for _, c := range d.Columns {
			q := quoteName(c.Name)
			cols, marks, sets, news = append(cols, q), append(marks, "?"), append(sets, q+" = ?"), append(news, q+" = new."+q)
			// strings compare by bytes: the column collation ignores case and trailing spaces
			if c.DataType == "varchar" || c.DataType == "enum" {
				match = append(match, "BINARY "+q+" <=> BINARY ?")
			} else {
				match = append(match, q+" <=> ?")
			}
		}
		for _, i := range d.PK {
			keyMatch = append(keyMatch, quoteName(d.Columns[i].Name)+" = ?")
		}
		where := strings.Join(match, " AND ")
		ins := "INSERT INTO " + name + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
		t.tables[n] = &targetTable{
			ins:    ins,
			upd:    "UPDATE " + name + " SET " + strings.Join(sets, ", ") + " WHERE " + where,
			del:    "DELETE FROM " + name + " WHERE " + where,
			upsert: ins + " AS new ON DUPLICATE KEY UPDATE " + strings.Join(news, ", "),
			delPK:  "DELETE FROM " + name + " WHERE " + strings.Join(keyMatch, " AND "),
			pk:     d.PK,
		}
	}
	return t
}

func quoteName(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// savedState is the stored row for a source.
type savedState struct {
	Position, Tables, SchemaFP string
	Phase, DataDB, InitID      string
	P, P2                      string // positions around the init snapshot
	FirstInstall               bool
}

// added columns, by table; a row stored before they existed gets the default
var stateColumns = []struct{ table, name, def string }{
	{"position", "phase", "varchar(16) NOT NULL DEFAULT '" + phaseLegacy + "'"},
	{"position", "data_db", "varchar(64) NOT NULL DEFAULT ''"},
	{"position", "init_id", "varchar(32) NOT NULL DEFAULT ''"},
	{"position", "p", "varchar(512) NOT NULL DEFAULT ''"},
	{"position", "p2", "varchar(512) NOT NULL DEFAULT ''"},
	{"position", "first_install", "tinyint NOT NULL DEFAULT 0"},
	{"staging_owner", "table_id", "bigint NOT NULL DEFAULT 0"},
}

// ensureState creates the position and owner tables, adding columns a row from an older version lacks.
// It runs on the lock's connection when the lock is taken.
func ensureState(ctx context.Context, db *sql.Conn) error {
	for _, q := range []string{
		"CREATE DATABASE IF NOT EXISTS growth_lab_cdc",
		"CREATE TABLE IF NOT EXISTS " + positionTable + ` (
			source_id varchar(64) NOT NULL PRIMARY KEY,
			position varchar(512) NOT NULL,
			tables text NOT NULL,
			schema_fp char(64) NOT NULL,
			updated_at datetime(6) NOT NULL) ENGINE=InnoDB`,
		"CREATE TABLE IF NOT EXISTS " + ownerTable + ` (
			table_name varchar(64) NOT NULL PRIMARY KEY,
			init_id varchar(32) NOT NULL) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, c := range stateColumns {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = 'growth_lab_cdc' AND TABLE_NAME = ? AND COLUMN_NAME = ?`, c.table, c.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.ExecContext(ctx, "ALTER TABLE growth_lab_cdc."+c.table+" ADD COLUMN "+c.name+" "+c.def); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadState returns the stored state, if any. A stored row for another source stops the run.
func loadState(ctx context.Context, db *sql.DB, sourceID, sourceDB string) (*savedState, error) {
	rows, err := db.QueryContext(ctx, "SELECT source_id, position, tables, schema_fp, phase, data_db, init_id, p, p2, first_install FROM "+positionTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var st *savedState
	for rows.Next() {
		var id string
		var s savedState
		if err := rows.Scan(&id, &s.Position, &s.Tables, &s.SchemaFP, &s.Phase, &s.DataDB, &s.InitID, &s.P, &s.P2, &s.FirstInstall); err != nil {
			return nil, err
		}
		if id != sourceID {
			return nil, &StopError{"the copy follows another source (server uuid " + id + ")"}
		}
		if s.DataDB == "" {
			s.DataDB = sourceDB
		}
		st = &s
	}
	return st, rows.Err()
}

func initState(ctx context.Context, tx *sql.Tx, sourceID string, s savedState) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO `+positionTable+` (source_id, position, tables, schema_fp, phase, data_db, init_id, p, p2, first_install, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6))`,
		sourceID, s.Position, s.Tables, s.SchemaFP, s.Phase, s.DataDB, s.InitID, s.P, s.P2, s.FirstInstall)
	return err
}

// setState rewrites the stored row's fields that init moves between phases.
func setState(ctx context.Context, tx *sql.Tx, sourceID string, s savedState) error {
	res, err := tx.ExecContext(ctx, `UPDATE `+positionTable+` SET position = ?, phase = ?, data_db = ?, init_id = ?, p = ?, p2 = ?,
		first_install = ?, updated_at = NOW(6) WHERE source_id = ?`, s.Position, s.Phase, s.DataDB, s.InitID, s.P, s.P2, s.FirstInstall, sourceID)
	if err != nil {
		return err
	}
	return oneRow(res)
}

func oneRow(res sql.Result) error {
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return &StopError{"the position row for this source is missing in the copy"}
	}
	return nil
}

func (t *Target) Begin(ctx context.Context, mode ApplyMode) error {
	if t.tx != nil {
		return &Error{"a copy transaction is already open"}
	}
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	t.tx, t.mode = tx, mode
	return nil
}

func (t *Target) Apply(ctx context.Context, c *Change) error {
	key := c.Schema + "." + c.Table
	tt := t.tables[key]
	if tt == nil || t.tx == nil {
		return &Error{key + ": not a followed table or no open transaction"}
	}
	if t.mode == Converge {
		return t.converge(ctx, tt, c)
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

// converge overwrites by primary key: the row ends as the change left it, whatever the copy had.
func (t *Target) converge(ctx context.Context, tt *targetTable, c *Change) error {
	key := func(row []any) []any {
		out := make([]any, len(tt.pk))
		for i, p := range tt.pk {
			out[i] = row[p]
		}
		return out
	}
	var err error
	switch c.Kind {
	case Insert:
		_, err = t.tx.ExecContext(ctx, tt.upsert, c.After...)
	case Update:
		if before, after := key(c.Before), key(c.After); !reflect.DeepEqual(before, after) {
			if _, err = t.tx.ExecContext(ctx, tt.delPK, before...); err != nil {
				return err
			}
		}
		_, err = t.tx.ExecContext(ctx, tt.upsert, c.After...)
	case Delete:
		_, err = t.tx.ExecContext(ctx, tt.delPK, key(c.Before)...)
	}
	if err != nil {
		return err
	}
	if h := testHooks.afterApply; h != nil {
		return h()
	}
	return nil
}

func (t *Target) Commit(ctx context.Context, position, phase string) error {
	if t.tx == nil {
		return &Error{"no open copy transaction"}
	}
	tx := t.tx
	t.tx = nil
	if err := fence(ctx, tx, t.epoch); err != nil {
		tx.Rollback()
		return err
	}
	if err := savePosition(ctx, tx, t.sourceID, position, phase); err != nil {
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

func (t *Target) Save(ctx context.Context, position, phase string) error {
	if t.tx != nil {
		return &Error{"cannot save a position while a copy transaction is open"}
	}
	return fenced(ctx, t.db, t.epoch, func(tx *sql.Tx) error {
		return savePosition(ctx, tx, t.sourceID, position, phase)
	})
}

func savePosition(ctx context.Context, tx *sql.Tx, sourceID, position, phase string) error {
	res, err := tx.ExecContext(ctx, "UPDATE "+positionTable+" SET position = ?, phase = IF(? = '', phase, ?), updated_at = NOW(6) WHERE source_id = ?",
		position, phase, phase, sourceID)
	if err != nil {
		return err
	}
	return oneRow(res)
}
