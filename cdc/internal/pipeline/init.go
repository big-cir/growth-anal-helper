package pipeline

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	gosql "github.com/go-sql-driver/mysql"

	"growth-lab-cdc/internal/cdc"
	"growth-lab-cdc/internal/schema"
	"growth-lab-cdc/internal/source"
	"growth-lab-cdc/internal/target"
)

// InitOptions configures building a fresh copy of the followed tables and swapping it in.
type InitOptions struct {
	Source   source.Source
	Target   source.Source
	Database string
	Tables   []string
	ServerID uint32
	Logf     func(format string, a ...any)
	// MaxCopy bounds the snapshot copy; 0 means half the source's binlog retention.
	MaxCopy         time.Duration
	SaveEvery       time.Duration
	StopWait        time.Duration
	HeartbeatPeriod time.Duration
	Retries         int
}

// Init copies the followed tables from one snapshot into a staging database on the target,
// replays the binlog from just before the snapshot until the copy has caught up, rebuilds the
// secondary indexes and swaps the staging tables in with one RENAME TABLE. It resumes from the
// phase stored in the target after a crash or a lost connection.
func Init(ctx context.Context, o InitOptions) error {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Retries == 0 {
		o.Retries = 8
	}
	for attempt := 0; ; attempt++ {
		started := time.Now()
		err := initOnce(ctx, o)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !retryable(err) {
			return err
		}
		if time.Since(started) > time.Minute {
			attempt = 0
		}
		if attempt >= o.Retries {
			return fmt.Errorf("giving up after %d retries: %w", attempt, err)
		}
		wait := min(time.Duration(1<<attempt)*time.Second, time.Minute)
		o.Logf("init resumes in %s after: %s", wait, cdc.SafeError(err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

type initRun struct {
	o        InitOptions
	ro       RunOptions
	names    []string
	src, dst *sql.DB
	lock     *target.Lock
	uuid     string
	srcDefs  map[string]*schema.TableDef
	fp       string
}

func initOnce(parent context.Context, o InitOptions) error {
	st, err := o.Source.ReadStatus(parent)
	if err != nil {
		return err
	}
	in := &initRun{o: o, uuid: st.ServerUUID}
	in.ro = RunOptions{Source: o.Source, Target: o.Target, Database: o.Database, Tables: o.Tables, ServerID: o.ServerID,
		Logf: o.Logf, SaveEvery: o.SaveEvery, StopWait: o.StopWait, HeartbeatPeriod: o.HeartbeatPeriod, Retries: o.Retries}
	in.names = in.ro.names()
	if in.src, err = o.Source.Open(); err != nil {
		return err
	}
	defer in.src.Close()
	var format, image string
	if err := in.src.QueryRowContext(parent, "SELECT @@binlog_format, @@binlog_row_image").Scan(&format, &image); err != nil {
		return err
	}
	if format != "ROW" || image != "FULL" {
		return &cdc.StopError{Msg: "the source must use binlog_format=ROW and binlog_row_image=FULL"}
	}
	if in.dst, err = target.Open(o.Target); err != nil {
		return err
	}
	defer in.dst.Close()
	if in.lock, err = target.TakeLock(parent, in.dst); err != nil {
		return err
	}
	defer in.lock.Release()
	in.ro.lock = in.lock
	ctx, cancel := in.lock.WithLock(parent)
	defer cancel()
	if in.srcDefs, err = schema.ReadDefs(ctx, in.src, "", in.names); err != nil {
		return err
	}
	if err := schema.NoTriggersOrCascades(ctx, in.src, in.names); err != nil {
		return err
	}
	in.fp = schema.Fingerprint(in.srcDefs)
	err = in.phases(ctx)
	if err != nil && !in.lock.OK() {
		return target.ErrLockLost
	}
	return err
}

// phases runs from the stored phase to the end.
func (in *initRun) phases(ctx context.Context) error {
	for {
		saved, err := target.LoadState(ctx, in.dst, in.uuid, in.o.Database)
		if err != nil {
			return err
		}
		if saved == nil {
			if err := in.start(ctx); err != nil {
				return err
			}
			continue
		}
		if saved.Phase == target.PhaseLegacy {
			return &cdc.StopError{Msg: "the stored position is from before init existed; remove its row in " + target.PositionTable + " and run init"}
		}
		if saved.Tables != strings.Join(in.names, ",") {
			return &cdc.StopError{Msg: "the table list differs from the one stored in the copy (" + saved.Tables + ")"}
		}
		if saved.SchemaFP != in.fp {
			return &cdc.StopError{Msg: "table definitions changed since this init started; start again from a removed position row"}
		}
		switch saved.Phase {
		case target.PhaseConverging, target.PhaseCatchingUp, target.PhaseSwapping:
			if !saved.SnapshotLocked {
				return &cdc.StopError{Msg: "this init took its snapshot without locking the source tables (an earlier version) and the copy may miss rows; drop " + target.StagingDB + ", remove the row in " + target.PositionTable + " and run init again"}
			}
		}
		switch saved.Phase {
		case target.PhaseReady:
			if saved.InitID == "" && saved.DataDB == in.o.Database {
				in.o.Logf("the copy is ready; continue with run")
				return nil
			}
			return &cdc.StopError{Msg: "the copy is ready but its stored row is not what init leaves"}
		case target.PhaseCopying:
			err = in.copy(ctx, saved)
		case target.PhaseConverging:
			in.o.Logf("replaying from before the snapshot, overwriting by key")
			err = in.replay(ctx, target.PhaseConverging, true)
		case target.PhaseCatchingUp:
			err = in.catchUp(ctx, saved)
		case target.PhaseSwapping:
			err = in.swap(ctx, saved)
		default:
			return &cdc.StopError{Msg: "unknown stored phase " + saved.Phase}
		}
		if err != nil {
			return err
		}
	}
}

// start records a new init: its id, and whether the copy already has the followed tables.
func (in *initRun) start(ctx context.Context) error {
	live, err := in.where(ctx, in.o.Database)
	if err != nil {
		return err
	}
	if live > 0 && live < len(in.names) {
		return &cdc.StopError{Msg: fmt.Sprintf("the copy has %d of the %d followed tables in %s; fix that before init", live, len(in.names), in.o.Database)}
	}
	if old, err := in.where(ctx, target.OldDB); err != nil {
		return err
	} else if old > 0 {
		return &cdc.StopError{Msg: "tables from an earlier swap are still in " + target.OldDB + "; remove them before init"}
	}
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	in.o.Logf("init %s: %s", id, map[bool]string{true: "first install", false: "replaces the current tables"}[live == 0])
	s := target.State{Position: (cdc.Pos{File: "-", Pos: 4}).String(), Tables: strings.Join(in.names, ","),
		SchemaFP: in.fp, Phase: target.PhaseCopying, DataDB: target.StagingDB, InitID: id, FirstInstall: live == 0}
	return in.fenced(ctx, func(tx *sql.Tx) error { return target.InitState(ctx, tx, in.uuid, s) })
}

// fenced runs f in a transaction that stops when another process has taken the copy.
func (in *initRun) fenced(ctx context.Context, f func(tx *sql.Tx) error) error {
	return target.Fenced(ctx, in.dst, in.lock.Epoch(), f)
}

// where counts the followed tables present in a database of the copy.
func (in *initRun) where(ctx context.Context, db string) (int, error) {
	n := 0
	for _, name := range in.names {
		_, table, _ := strings.Cut(name, ".")
		var c int
		if err := in.dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TABLES
			WHERE BINARY TABLE_SCHEMA = BINARY ? AND BINARY TABLE_NAME = BINARY ?`, db, table).Scan(&c); err != nil {
			return 0, err
		}
		n += c
	}
	return n, nil
}

// copy creates the staging tables and fills them from one snapshot taken at position P.
func (in *initRun) copy(ctx context.Context, saved *target.State) error {
	in.o.Logf("copying from a snapshot into %s", target.StagingDB)
	if err := in.createStaging(ctx, saved); err != nil {
		return err
	}
	created, err := schema.ReadDefs(ctx, in.dst, target.StagingDB, in.names)
	if err != nil {
		return err
	}
	if err := schema.SameDefs(in.srcDefs, created, false); err != nil {
		return err
	}
	return in.snapshot(ctx, saved)
}

// createStaging replaces what an earlier attempt of this init left with empty staging tables.
// Everything is read first; the lock's connection then only runs the writes.
func (in *initRun) createStaging(ctx context.Context, saved *target.State) error {
	// remove what an earlier attempt of this init left; anything else stops
	owners, err := source.QueryRows(ctx, in.dst, "SELECT table_name, init_id FROM "+target.OwnerTable, 2)
	if err != nil {
		return err
	}
	owner := map[string]string{}
	for _, r := range owners {
		owner[r[0]] = r[1]
	}
	staged, err := source.QueryRows(ctx, in.dst, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = '"+target.StagingDB+"'", 1)
	if err != nil {
		return err
	}
	type stmt struct {
		q    string
		args []any
	}
	stmts := []stmt{{q: "CREATE DATABASE IF NOT EXISTS " + cdc.QuoteName(target.StagingDB)}}
	for _, r := range staged {
		if owner[r[0]] != saved.InitID {
			return &cdc.StopError{Msg: target.StagingDB + "." + r[0] + " was not created by this init; remove it first"}
		}
		stmts = append(stmts, stmt{q: "DROP TABLE " + cdc.QuoteName(target.StagingDB) + "." + cdc.QuoteName(r[0])})
	}
	for _, name := range in.names {
		_, table, _ := strings.Cut(name, ".")
		ddl, err := schema.StagingDDL(ctx, in.src, name, target.StagingDB)
		if err != nil {
			return err
		}
		stmts = append(stmts, stmt{"REPLACE INTO " + target.OwnerTable + " (table_name, init_id) VALUES (?, ?)", []any{table, saved.InitID}}, stmt{q: ddl})
	}
	return in.lock.Do(func(c *sql.Conn) error {
		for _, s := range stmts {
			if _, err := c.ExecContext(ctx, s.q, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// snapshot fills the staging tables from one snapshot taken at position P.
func (in *initRun) snapshot(ctx context.Context, saved *target.State) error {
	limit, err := in.copyLimit(ctx)
	if err != nil {
		return err
	}

	if err := hook("before-snapshot"); err != nil {
		return err
	}
	conn, err := in.src.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		return err
	}
	var iso string
	if err := conn.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&iso); err != nil {
		return err
	}
	if iso != "REPEATABLE-READ" {
		return &cdc.StopError{Msg: "the copy connection is not REPEATABLE READ (" + iso + ")"}
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	// the limit counts from P, where the replay starts
	p, started, err := in.lockedSnapshot(ctx, conn)
	if err != nil {
		return err
	}
	for i, name := range in.names {
		if err := in.copyTable(ctx, conn, name, started, limit); err != nil {
			return err
		}
		if i == 0 {
			if err := hook("copied-first-table"); err != nil {
				return err
			}
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	again, err := schema.ReadDefs(ctx, in.src, "", in.names)
	if err != nil {
		return err
	}
	if schema.Fingerprint(again) != in.fp {
		return &cdc.StopError{Msg: "source table definitions changed during the copy"}
	}
	if err := copyTime(started, limit); err != nil {
		return err
	}
	// the snapshot holds exactly the followed tables' changes up to P, so the replay applies strictly
	start := cdc.Pos{File: p.Name, Pos: p.Pos}
	in.o.Logf("copied in %s; replay starts at the snapshot position %s:%d", time.Since(started).Round(time.Second), start.File, start.Pos)
	s := *saved
	s.Position, s.P, s.P2, s.Phase, s.SnapshotLocked = start.String(), start.String(), start.String(), target.PhaseCatchingUp, true
	return in.fenced(ctx, func(tx *sql.Tx) error { return target.SetState(ctx, tx, in.uuid, s) })
}

const (
	snapshotLockWait  = 5 // seconds one attempt waits for the source table lock
	snapshotLockTries = 3
)

// lockedSnapshot starts the snapshot on conn and reads the binlog position while another
// connection holds the followed tables with LOCK TABLES ... READ. A transaction is in the binlog
// before its engine commit, so a position read without the lock can include a change the
// snapshot does not show yet; the lock is granted only once every transaction writing those
// tables has committed, and keeps new writes out until the position is read.
func (in *initRun) lockedSnapshot(ctx context.Context, conn *sql.Conn) (mysql.Position, time.Time, error) {
	var parts []string
	for _, n := range in.names {
		schema, table, _ := strings.Cut(n, ".")
		parts = append(parts, cdc.QuoteName(schema)+"."+cdc.QuoteName(table)+" READ")
	}
	lockSQL := "LOCK TABLES " + strings.Join(parts, ", ")
	for try := 1; ; try++ {
		p, at, waited, err := in.tryLockedSnapshot(ctx, conn, lockSQL)
		if err == nil {
			return p, at, nil
		}
		var me *gosql.MySQLError
		switch {
		case errors.Is(err, errSourceBusy):
			if try >= snapshotLockTries {
				return mysql.Position{}, time.Time{}, &cdc.StopError{Msg: fmt.Sprintf("could not lock the followed tables on the source in %d tries of %d seconds: write transactions kept them busy; run init again later", try, snapshotLockWait)}
			}
			in.o.Logf("try %d: the source tables stayed busy for %s; trying again", try, waited.Round(time.Millisecond))
			select {
			case <-ctx.Done():
				return mysql.Position{}, time.Time{}, ctx.Err()
			case <-time.After(snapshotLockWait * time.Second):
			}
		case errors.As(err, &me) && (me.Number == 1044 || me.Number == 1142 || me.Number == 1227):
			return mysql.Position{}, time.Time{}, &cdc.StopError{Msg: fmt.Sprintf("the source account needs the LOCK TABLES and SELECT privileges on the followed tables (MySQL error %d)", me.Number)}
		default:
			return mysql.Position{}, time.Time{}, err
		}
	}
}

// errSourceBusy means one attempt could not lock the source tables in time.
var errSourceBusy = errors.New("the source tables stayed busy")

// tryLockedSnapshot makes one attempt. It returns the position, when it was read, and how long
// the lock statement waited.
func (in *initRun) tryLockedSnapshot(ctx context.Context, conn *sql.Conn, lockSQL string) (mysql.Position, time.Time, time.Duration, error) {
	fail := func(waited time.Duration, err error) (mysql.Position, time.Time, time.Duration, error) {
		return mysql.Position{}, time.Time{}, waited, err
	}
	lock, err := in.src.Conn(ctx)
	if err != nil {
		return fail(0, err)
	}
	// the server frees the lock when the session ends, and a pooled connection keeps its session
	defer func() {
		uctx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		lock.ExecContext(uctx, "UNLOCK TABLES")
		lock.Raw(func(any) error { return driver.ErrBadConn })
		lock.Close()
	}()
	var id int64
	if err := lock.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		return fail(0, err)
	}
	// lock_wait_timeout applies to each table's lock in turn, so the statement as a whole is
	// stopped from another connection at the limit
	if _, err := lock.ExecContext(ctx, fmt.Sprintf("SET SESSION lock_wait_timeout = %d", snapshotLockWait)); err != nil {
		return fail(0, err)
	}
	var mu sync.Mutex
	finished, fired := false, false
	var killErr error
	killed := make(chan struct{})
	asked := time.Now()
	stop := time.AfterFunc(snapshotLockWait*time.Second, func() {
		defer close(killed)
		mu.Lock()
		if finished {
			mu.Unlock()
			return
		}
		fired = true
		mu.Unlock()
		kctx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, killErr = in.src.ExecContext(kctx, fmt.Sprintf("KILL QUERY %d", id))
	})
	// past this the driver drops the connection, should the KILL QUERY fail
	lctx, done := context.WithTimeout(ctx, (snapshotLockWait+5)*time.Second)
	_, err = lock.ExecContext(lctx, lockSQL)
	overran := errors.Is(lctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	done()
	mu.Lock()
	finished = true
	timedOut := fired
	mu.Unlock()
	waited := time.Since(asked)
	if !stop.Stop() {
		<-killed // a KILL QUERY on its way must not reach a later statement
	}
	var me *gosql.MySQLError
	switch {
	case timedOut && killErr != nil: // its number only, so a privilege error is not taken for the lock's
		return fail(waited, &cdc.Error{Msg: fmt.Sprintf("could not stop the lock statement at its time limit (%s)", cdc.SafeError(killErr))})
	case overran:
		return fail(waited, &cdc.Error{Msg: "the lock statement did not end at its time limit"})
	case timedOut:
		return fail(waited, errSourceBusy) // even if the lock came: the kill may be on its way
	case errors.As(err, &me) && me.Number == 1205:
		return fail(waited, errSourceBusy)
	case err != nil:
		return fail(waited, err)
	}
	locked := time.Now()
	if err := hook("source-locked"); err != nil {
		return fail(0, err)
	}
	if _, err := conn.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY"); err != nil {
		return fail(0, err)
	}
	p, err := source.Position(ctx, lock)
	if err != nil {
		return fail(0, err)
	}
	at := time.Now()
	if err := hook("source-position-read"); err != nil {
		return fail(0, err)
	}
	if _, err := lock.ExecContext(ctx, "UNLOCK TABLES"); err != nil {
		return fail(0, err)
	}
	in.o.Logf("locked the source tables after waiting %s, held them %s", waited.Round(time.Millisecond), time.Since(locked).Round(time.Millisecond))
	return p, at, waited, nil
}

func copyTime(started time.Time, limit time.Duration) error {
	if time.Since(started) > limit {
		return &cdc.StopError{Msg: fmt.Sprintf("the copy took longer than %s; the binlog it needs may be removed before the replay", limit)}
	}
	return nil
}

// copyLimit is MaxCopy, or half the source's binlog retention: past it, the replay might find
// the start of its range already removed.
func (in *initRun) copyLimit(ctx context.Context) (time.Duration, error) {
	if in.o.MaxCopy > 0 {
		return in.o.MaxCopy, nil
	}
	var retention time.Duration
	// a managed server keeps its own setting; reading it may need privileges the account lacks
	if rows, err := in.src.QueryContext(ctx, "CALL mysql.rds_show_configuration"); err == nil {
		for rows.Next() {
			var name string
			var value, desc sql.NullString
			if rows.Scan(&name, &value, &desc) == nil && name == "binlog retention hours" {
				if h, err := strconv.Atoi(value.String); err == nil {
					retention = time.Duration(h) * time.Hour
				}
			}
		}
		rows.Close()
	}
	if retention == 0 {
		var secs int64
		if err := in.src.QueryRowContext(ctx, "SELECT @@binlog_expire_logs_seconds").Scan(&secs); err != nil {
			return 0, err
		}
		retention = time.Duration(secs) * time.Second
	}
	if retention <= 0 {
		in.o.Logf("binlog retention unknown; the copy may take at most 6h")
		return 6 * time.Hour, nil
	}
	in.o.Logf("binlog retention %s; the copy may take at most %s", retention, retention/2)
	return retention / 2, nil
}

// copyTable reads one table inside the snapshot in key order and inserts it in batches.
func (in *initRun) copyTable(ctx context.Context, conn *sql.Conn, name string, started time.Time, limit time.Duration) error {
	schema, table, _ := strings.Cut(name, ".")
	d := in.srcDefs[name]
	var cols, order []string
	for _, c := range d.Columns {
		cols = append(cols, cdc.QuoteName(c.Name))
	}
	for _, i := range d.PK {
		order = append(order, cdc.QuoteName(d.Columns[i].Name))
	}
	rows, err := conn.QueryContext(ctx, "SELECT "+strings.Join(cols, ", ")+" FROM "+cdc.QuoteName(schema)+"."+cdc.QuoteName(table)+" ORDER BY "+strings.Join(order, ", "))
	if err != nil {
		return err
	}
	defer rows.Close()
	const batch = 500
	row := "(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"
	insert := "INSERT INTO " + cdc.QuoteName(target.StagingDB) + "." + cdc.QuoteName(table) + " (" + strings.Join(cols, ", ") + ") VALUES "
	var args []any
	n, total := 0, 0
	flush := func() error {
		if n == 0 {
			return nil
		}
		err := in.fenced(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, insert+strings.TrimSuffix(strings.Repeat(row+",", n), ","), args...)
			return err
		})
		args, n = args[:0], 0
		if err != nil {
			return err
		}
		return copyTime(started, limit)
	}
	raw := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for _, v := range raw {
			if v == nil {
				args = append(args, nil)
			} else {
				args = append(args, string(v))
			}
		}
		n++
		total++
		if n == batch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if err := copyTime(started, limit); err != nil {
		return err
	}
	in.o.Logf("copied %s: %d rows", name, total)
	return nil
}

// replay applies the binlog to the staging tables until it has caught up with the source.
func (in *initRun) replay(ctx context.Context, phase string, baseOnly bool) error {
	ro := in.ro
	ro.catchUp, ro.baseOnly, ro.phases = true, baseOnly, []string{phase}
	_, err := Run(ctx, ro)
	return err
}

// catchUp replays with the primary key only, rebuilds the secondary indexes, and replays again
// with the full definition. Where it resumes depends on which indexes the staging tables have.
func (in *initRun) catchUp(ctx context.Context, saved *target.State) error {
	staged, err := schema.ReadDefs(ctx, in.dst, target.StagingDB, in.names)
	if err != nil {
		return err
	}
	have, missing := 0, 0
	for _, name := range in.names {
		want := map[string]string{}
		for _, ix := range in.srcDefs[name].Indexes {
			want[ix.Name] = ix.Text()
		}
		for _, ix := range staged[name].Indexes {
			if want[ix.Name] != ix.Text() {
				return &cdc.StopError{Msg: fmt.Sprintf("%s.%s: index %s differs from the source", target.StagingDB, name, ix.Name)}
			}
			have++
			delete(want, ix.Name)
		}
		missing += len(want)
	}
	if have == 0 {
		in.o.Logf("catching up with the primary key only")
		if err := in.replay(ctx, target.PhaseCatchingUp, true); err != nil {
			return err
		}
	}
	if missing > 0 {
		if err := hook("before-restore"); err != nil {
			return err
		}
		if err := in.restoreIndexes(ctx, staged); err != nil {
			return err
		}
	}
	in.o.Logf("catching up with the full table definitions")
	if err := in.replay(ctx, target.PhaseCatchingUp, false); err != nil {
		return err
	}
	cur, err := target.LoadState(ctx, in.dst, in.uuid, in.o.Database)
	if err != nil {
		return err
	}
	if err := in.swapChecks(ctx, cur); err != nil {
		return err
	}
	s := *cur
	s.Phase = target.PhaseSwapping
	return in.fenced(ctx, func(tx *sql.Tx) error {
		// InnoDB keeps a table's id through RENAME TABLE: the swap finds this init's tables by it
		for _, name := range in.names {
			_, table, _ := strings.Cut(name, ".")
			id, err := tableID(ctx, tx, target.StagingDB, table)
			if err != nil {
				return err
			}
			if id == 0 {
				return &cdc.StopError{Msg: target.StagingDB + "." + table + " is missing before the swap"}
			}
			res, err := tx.ExecContext(ctx, "UPDATE "+target.OwnerTable+" SET table_id = ? WHERE table_name = ? AND init_id = ?", id, table, s.InitID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n != 1 {
				return &cdc.StopError{Msg: target.StagingDB + "." + table + " is not listed as this init's"}
			}
		}
		return target.SetState(ctx, tx, in.uuid, s)
	})
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// tableID is the InnoDB id of db.table, 0 when there is none.
func tableID(ctx context.Context, q queryer, db, table string) (int64, error) {
	name, err := innodbName(db, table)
	if err != nil {
		return 0, err
	}
	var id int64
	err = q.QueryRowContext(ctx, "SELECT TABLE_ID FROM information_schema.INNODB_TABLES WHERE NAME = ?", name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// innodbName is how InnoDB lists db.table; names with characters it would encode are not handled.
func innodbName(db, table string) (string, error) {
	for _, n := range []string{db, table} {
		for _, c := range n {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				return "", &cdc.StopError{Msg: "init handles only database and table names of letters, digits and _ (" + n + ")"}
			}
		}
	}
	return db + "/" + table, nil
}

// restoreIndexes adds the source's secondary indexes the staging tables lack, one at a time.
func (in *initRun) restoreIndexes(ctx context.Context, staged map[string]*schema.TableDef) error {
	added := 0
	for _, name := range in.names {
		_, table, _ := strings.Cut(name, ".")
		have := map[string]bool{}
		for _, ix := range staged[name].Indexes {
			have[ix.Name] = true
		}
		for _, ix := range in.srcDefs[name].Indexes {
			if have[ix.Name] {
				continue
			}
			in.o.Logf("adding index %s.%s", name, ix.Name)
			err := in.lock.Do(func(c *sql.Conn) error {
				_, err := c.ExecContext(ctx, schema.AddIndexSQL(target.StagingDB, table, ix))
				return err
			})
			if err != nil {
				var me *gosql.MySQLError
				if errors.As(err, &me) && me.Number == 1062 {
					return &cdc.StopError{Msg: fmt.Sprintf("%s: unique index %s has duplicates in the staging copy", name, ix.Name)}
				}
				return err
			}
			added++
			if added == 1 {
				if err := hook("added-first-index"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// swapChecks stops before a swap that would move tables over others.
func (in *initRun) swapChecks(ctx context.Context, s *target.State) error {
	live, err := in.where(ctx, in.o.Database)
	if err != nil {
		return err
	}
	old, err := in.where(ctx, target.OldDB)
	if err != nil {
		return err
	}
	staged, err := in.where(ctx, target.StagingDB)
	if err != nil {
		return err
	}
	all := len(in.names)
	wantLive := all
	if s.FirstInstall {
		wantLive = 0
	}
	if staged != all || live != wantLive || old != 0 {
		return &cdc.StopError{Msg: fmt.Sprintf("cannot swap: %d staged, %d in use, %d in %s (expected %d, %d, 0)", staged, live, old, target.OldDB, all, wantLive)}
	}
	return nil
}

// swap moves the staging tables into place with one RENAME TABLE, or, after a crash, decides from
// where this init's tables are, found by their InnoDB ids, whether that already happened.
func (in *initRun) swap(ctx context.Context, s *target.State) error {
	ids, err := source.QueryRows(ctx, in.dst, "SELECT table_name, table_id FROM "+target.OwnerTable+" WHERE init_id = '"+s.InitID+"'", 2)
	if err != nil {
		return err
	}
	own := map[string]int64{}
	for _, r := range ids {
		id, err := strconv.ParseInt(r[1], 10, 64)
		if err != nil {
			return err
		}
		own[r[0]] = id
	}
	inStaging, inLive := 0, 0
	for _, name := range in.names {
		_, table, _ := strings.Cut(name, ".")
		staged, err := tableID(ctx, in.dst, target.StagingDB, table)
		if err != nil {
			return err
		}
		live, err := tableID(ctx, in.dst, in.o.Database, table)
		if err != nil {
			return err
		}
		switch id := own[table]; {
		case id == 0:
			return &cdc.StopError{Msg: table + ": this init's table is not recorded; cannot tell whether the swap happened"}
		case staged == id:
			inStaging++
		case live == id:
			inLive++
		}
	}
	live, err := in.where(ctx, in.o.Database)
	if err != nil {
		return err
	}
	old, err := in.where(ctx, target.OldDB)
	if err != nil {
		return err
	}
	staged, err := in.where(ctx, target.StagingDB)
	if err != nil {
		return err
	}
	all := len(in.names)
	liveBefore, oldAfter := all, all
	if s.FirstInstall {
		liveBefore, oldAfter = 0, 0
	}
	switch {
	case inStaging == all && staged == all && live == liveBefore && old == 0:
		var parts []string
		for _, name := range in.names {
			_, table, _ := strings.Cut(name, ".")
			if !s.FirstInstall {
				parts = append(parts, cdc.QuoteName(in.o.Database)+"."+cdc.QuoteName(table)+" TO "+cdc.QuoteName(target.OldDB)+"."+cdc.QuoteName(table))
			}
			parts = append(parts, cdc.QuoteName(target.StagingDB)+"."+cdc.QuoteName(table)+" TO "+cdc.QuoteName(in.o.Database)+"."+cdc.QuoteName(table))
		}
		if err := hook("before-rename"); err != nil {
			return err
		}
		in.o.Logf("swapping the staging tables in")
		err := in.lock.Do(func(c *sql.Conn) error {
			for _, db := range []string{in.o.Database, target.OldDB} {
				if _, err := c.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+cdc.QuoteName(db)); err != nil {
					return err
				}
			}
			_, err := c.ExecContext(ctx, "RENAME TABLE "+strings.Join(parts, ", "))
			return err
		})
		if err != nil {
			return err
		}
		if err := hook("after-rename"); err != nil {
			return err
		}
	case inLive == all && staged == 0 && old == oldAfter:
		in.o.Logf("the swap had already happened")
	default:
		return &cdc.StopError{Msg: fmt.Sprintf("cannot tell whether the swap happened: of this init's %d tables %d are staged and %d in use; %d staged, %d in use, %d in %s",
			all, inStaging, inLive, staged, live, old, target.OldDB)}
	}
	done := *s
	done.Phase, done.DataDB, done.InitID, done.P, done.P2, done.FirstInstall = target.PhaseReady, in.o.Database, "", "", "", false
	err = in.fenced(ctx, func(tx *sql.Tx) error {
		if err := target.SetState(ctx, tx, in.uuid, done); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM "+target.OwnerTable+" WHERE init_id = ?", s.InitID)
		return err
	})
	if err != nil {
		return err
	}
	in.o.Logf("init done; the previous tables are in %s", target.OldDB)
	return nil
}
