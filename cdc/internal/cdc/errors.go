package cdc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"syscall"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	gosql "github.com/go-sql-driver/mysql"
)

// Error is a CDC setup or protocol error.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// SafeError keeps only fixed information about an error: our own messages, MySQL error numbers,
// event types and positions, and the kind of network failure. Driver text and event bodies are dropped.
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	var own *Error
	if errors.As(err, &own) {
		return own
	}
	var stop *StopError
	if errors.As(err, &stop) {
		return &Error{stop.Msg}
	}
	var unsure *UncertainError
	if errors.As(err, &unsure) {
		return &Error{unsure.Error()}
	}
	var ee *replication.EventError
	if errors.As(err, &ee) {
		if ee.Header == nil {
			return &Error{"could not parse a binlog event"}
		}
		return &Error{fmt.Sprintf("could not parse a %s event ending at %d", ee.Header.EventType, ee.Header.LogPos)}
	}
	var me *gosql.MySQLError
	if errors.As(err, &me) {
		return mysqlError(me.Number)
	}
	var re *mysql.MyError
	if errors.As(err, &re) {
		return mysqlError(re.Code)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return &Error{fmt.Sprintf("%s %s: %s", pe.Op, pe.Path, pe.Err)}
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{"time limit reached"}
	case errors.Is(err, context.Canceled):
		return &Error{"cancelled"}
	case errors.Is(err, syscall.ECONNREFUSED):
		return &Error{"connection refused"}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, mysql.ErrBadConn), errors.Is(err, gosql.ErrInvalidConn):
		return &Error{"connection closed by the server"}
	case errors.As(err, &ne) && ne.Timeout():
		return &Error{"network timeout"}
	case errors.As(err, &ne):
		return &Error{"network error"}
	}
	return &Error{fmt.Sprintf("error of type %T", err)}
}

func mysqlError(code uint16) error {
	hint := map[uint16]string{
		1044: "access denied to the database", 1045: "access denied (user or password)",
		1142: "missing table privilege", 1227: "missing privilege",
		1236: "binlog position is not available on the source",
	}[code]
	if hint == "" {
		return &Error{fmt.Sprintf("MySQL error %d", code)}
	}
	return &Error{fmt.Sprintf("MySQL error %d: %s", code, hint)}
}
