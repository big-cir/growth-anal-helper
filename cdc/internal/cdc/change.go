package cdc

import "context"

// ChangeKind is the row operation of a Change.
type ChangeKind int

const (
	Insert ChangeKind = iota + 1
	Update
	Delete
)

// Change is one row change. Before is set for updates and deletes, After for inserts and updates.
// Values are int64-like integers or strings (datetime text, varchar, enum names).
type Change struct {
	Schema, Table string
	Kind          ChangeKind
	Before, After []any
}

// ApplyMode is how a source transaction is applied.
type ApplyMode int

const (
	// Strict applies what happened on the source and stops when the copy's previous values differ.
	Strict ApplyMode = iota
	// Converge overwrites by primary key, for replaying changes the copy may already have
	// (a staging copy between the position before its snapshot and the first one after it).
	Converge
)

// Sink applies changes to the copy and keeps the position with them. It knows nothing of binlogs:
// a position is an opaque string handed back by Load.
type Sink interface {
	// Begin opens a copy transaction for one source transaction.
	Begin(ctx context.Context, mode ApplyMode) error
	Apply(ctx context.Context, c *Change) error
	// Commit stores the position, and the phase when not empty, in the open transaction and commits.
	// An error after the commit was sent is an *UncertainError.
	Commit(ctx context.Context, position, phase string) error
	Rollback()
	// Save stores the position, and the phase when not empty, when no transaction is open.
	Save(ctx context.Context, position, phase string) error
}

// StopError is a condition run must not retry: the copy or the stream is not what it expects.
type StopError struct{ Msg string }

func (e *StopError) Error() string { return e.Msg }

// UncertainError means a commit was sent but its outcome is unknown.
type UncertainError struct{ Err error }

func (e *UncertainError) Error() string { return "commit outcome unknown: " + SafeError(e.Err).Error() }
func (e *UncertainError) Unwrap() error { return e.Err }
