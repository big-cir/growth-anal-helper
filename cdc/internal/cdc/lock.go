package cdc

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

const (
	lockName = "growth_lab_cdc"
	// epochTable holds a number raised by every process that takes the lock; writes check it.
	epochTable = "growth_lab_cdc.lock_epoch"
	lockWait   = 5 * time.Second
)

// errLockLost means the copy-wide lock was lost; work stops and resumes only after taking it again.
var errLockLost = &lockLostError{}

type lockLostError struct{}

func (*lockLostError) Error() string { return "lost the copy lock; stopped writing" }

// copyLock keeps run and init from writing to the same copy at once. It is a MySQL named lock held
// on its own connection; the server frees it when that connection ends, so the connection is checked
// and losing it cancels the work. Taking it raises the epoch, so writes of a process that lost it
// fail even before it notices. DDL runs on the lock's own connection: the server frees the lock only
// when that session ends, which a running statement holds off.
type copyLock struct {
	conn   *sql.Conn
	mu     sync.Mutex // held while a statement runs on conn
	epoch  int64
	lost   chan struct{}
	once   sync.Once
	cancel context.CancelFunc
}

// takeLock takes the lock or stops when another process holds it.
func takeLock(ctx context.Context, db *sql.DB) (*copyLock, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", lockName).Scan(&got); err != nil {
		conn.Close()
		return nil, err
	}
	if !got.Valid || got.Int64 != 1 {
		conn.Close()
		return nil, &StopError{"another growth-lab-cdc process is writing to this copy"}
	}
	l := &copyLock{conn: conn, lost: make(chan struct{})}
	if err := l.raiseEpoch(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	if err := ensureState(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}
	watch, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	go l.watch(watch)
	return l, nil
}

// raiseEpoch waits for writes of an earlier holder still in flight, then makes them fail.
func (l *copyLock) raiseEpoch(ctx context.Context) error {
	for _, q := range []string{
		"CREATE DATABASE IF NOT EXISTS growth_lab_cdc",
		"CREATE TABLE IF NOT EXISTS " + epochTable + " (id tinyint NOT NULL PRIMARY KEY, epoch bigint NOT NULL) ENGINE=InnoDB",
		"INSERT INTO " + epochTable + " (id, epoch) VALUES (1, 1) ON DUPLICATE KEY UPDATE epoch = epoch + 1",
	} {
		if _, err := l.conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return l.conn.QueryRowContext(ctx, "SELECT epoch FROM "+epochTable+" WHERE id = 1").Scan(&l.epoch)
}

func (l *copyLock) watch(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !l.mu.TryLock() {
				continue // a statement on the connection shows it is alive
			}
			var mine sql.NullInt64
			qctx, done := context.WithTimeout(ctx, lockWait)
			err := l.conn.QueryRowContext(qctx, "SELECT IS_USED_LOCK(?) = CONNECTION_ID()", lockName).Scan(&mine)
			done()
			l.mu.Unlock()
			if ctx.Err() != nil {
				return
			}
			if err != nil || !mine.Valid || mine.Int64 != 1 {
				l.once.Do(func() { close(l.lost) })
				return
			}
		}
	}
}

// Lost is closed when the lock is no longer held.
func (l *copyLock) Lost() <-chan struct{} { return l.lost }

// ok reports whether the lock is still held.
func (l *copyLock) ok() bool {
	select {
	case <-l.lost:
		return false
	default:
		return true
	}
}

// check is for writes outside a position transaction (DDL): the lock and the epoch are still this process's.
func (l *copyLock) check(ctx context.Context, db *sql.DB) error {
	if !l.ok() {
		return errLockLost
	}
	var cur int64
	if err := db.QueryRowContext(ctx, "SELECT epoch FROM "+epochTable+" WHERE id = 1").Scan(&cur); err != nil {
		return err
	}
	if cur != l.epoch {
		return errLockLost
	}
	return nil
}

func (l *copyLock) release() {
	l.cancel()
	ctx, done := context.WithTimeout(context.Background(), lockWait)
	defer done()
	l.conn.ExecContext(ctx, "DO RELEASE_LOCK(?)", lockName)
	l.conn.Close()
}

// withLock returns a context cancelled when the lock is lost.
func (l *copyLock) withLock(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-l.lost:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// fence stops a write whose process no longer holds the copy. The shared lock on the epoch row makes a
// newer process wait for this transaction to end before it takes over.
func fence(ctx context.Context, tx *sql.Tx, epoch int64) error {
	var cur int64
	if err := tx.QueryRowContext(ctx, "SELECT epoch FROM "+epochTable+" WHERE id = 1 FOR SHARE").Scan(&cur); err != nil {
		return err
	}
	if cur != epoch {
		return errLockLost
	}
	return nil
}

// do runs f with the lock's connection, for DDL and writes outside a position transaction. f only
// runs statements on that connection: while it does, the watch skips its check.
func (l *copyLock) do(f func(c *sql.Conn) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ok() {
		return errLockLost
	}
	return f(l.conn)
}

// fenced runs f in a transaction that first checks the epoch.
func fenced(ctx context.Context, db *sql.DB, epoch int64, f func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fence(ctx, tx, epoch); err != nil {
		return err
	}
	if err := f(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return &UncertainError{err}
	}
	return nil
}
