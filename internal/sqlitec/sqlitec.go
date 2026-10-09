// Package sqlitec is a thin binding to the SQLite C API with fixed connection defaults.
// It links the SQLite build compiled by mattn/go-sqlite3 (see scripts/go) and calls the C functions directly,
// because the engine needs NULL arguments in the authorizer and column origin names.
package sqlitec

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;
typedef long long sqlite3_int64;

int sqlite3_open_v2(const char*, sqlite3**, int, const char*);
int sqlite3_close_v2(sqlite3*);
const char *sqlite3_errmsg(sqlite3*);
int sqlite3_exec(sqlite3*, const char*, void*, void*, char**);
int sqlite3_db_config(sqlite3*, int, ...);
int sqlite3_busy_timeout(sqlite3*, int);
int sqlite3_set_authorizer(sqlite3*, int(*)(void*, int, const char*, const char*, const char*, const char*), void*);
int sqlite3_prepare_v2(sqlite3*, const char*, int, sqlite3_stmt**, const char**);
int sqlite3_finalize(sqlite3_stmt*);
int sqlite3_step(sqlite3_stmt*);
int sqlite3_reset(sqlite3_stmt*);
int sqlite3_column_count(sqlite3_stmt*);
const char *sqlite3_column_name(sqlite3_stmt*, int);
const char *sqlite3_column_table_name(sqlite3_stmt*, int);
const char *sqlite3_column_origin_name(sqlite3_stmt*, int);
const char *sqlite3_column_database_name(sqlite3_stmt*, int);
int sqlite3_column_type(sqlite3_stmt*, int);
sqlite3_int64 sqlite3_column_int64(sqlite3_stmt*, int);
double sqlite3_column_double(sqlite3_stmt*, int);
const unsigned char *sqlite3_column_text(sqlite3_stmt*, int);
int sqlite3_column_bytes(sqlite3_stmt*, int);
int sqlite3_bind_parameter_count(sqlite3_stmt*);
const char *sqlite3_bind_parameter_name(sqlite3_stmt*, int);
int sqlite3_bind_parameter_index(sqlite3_stmt*, const char*);
int sqlite3_bind_double(sqlite3_stmt*, int, double);
int sqlite3_bind_int64(sqlite3_stmt*, int, sqlite3_int64);
int sqlite3_bind_null(sqlite3_stmt*, int);
int sqlite3_bind_text(sqlite3_stmt*, int, const char*, int, void(*)(void*));
int sqlite3_clear_bindings(sqlite3_stmt*);
int sqlite3_changes(sqlite3*);
int sqlite3_get_autocommit(sqlite3*);

extern int goAuthorizer(uintptr_t, int, char*, char*, char*, char*);

static int gl_auth(void *h, int op, const char *a, const char *b, const char *c, const char *d) {
	return goAuthorizer((uintptr_t)h, op, (char*)a, (char*)b, (char*)c, (char*)d);
}
static int gl_set_authorizer(sqlite3 *db, uintptr_t h) {
	return sqlite3_set_authorizer(db, h ? gl_auth : 0, (void*)h);
}
static int gl_db_config_int(sqlite3 *db, int op, int v) {
	return sqlite3_db_config(db, op, v, (int*)0);
}
static int gl_bind_text(sqlite3_stmt *s, int i, const char *p, int n) {
	return sqlite3_bind_text(s, i, p, n, (void(*)(void*))-1);
}
*/
import "C"

import (
	"runtime/cgo"
	"strconv"
	"unsafe"

	_ "github.com/mattn/go-sqlite3"
)

// Result and type codes.
const (
	OK        = 0
	Deny      = 1
	Ignore    = 2
	rowCode   = 100
	doneCode  = 101
	Integer   = 1
	Float     = 2
	Text      = 3
	Blob      = 4
	Null      = 5
	openRO    = 0x1
	openRW    = 0x2
	openCreat = 0x4

	dbconfigEnableFkey    = 1002
	dbconfigLoadExtension = 1005
	dbconfigDefensive     = 1010
	dbconfigDQSDML        = 1013
	dbconfigDQSDDL        = 1014
)

// Authorizer action codes used by the engine.
const (
	CreateIndex  = 1
	CreateTable  = 2
	Delete       = 9
	DropIndex    = 10
	DropTable    = 11
	Insert       = 18
	Pragma       = 19
	Read         = 20
	Select       = 21
	Transaction  = 22
	Update       = 23
	Attach       = 24
	Detach       = 25
	AlterTable   = 26
	Reindex      = 27
	Analyze      = 28
	CreateVtable = 29
	DropVtable   = 30
	Function     = 31
	Savepoint    = 32
	Recursive    = 33
)

// Error is an SQLite error message.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// AuthFunc receives the action code and the four string arguments (nil = NULL).
type AuthFunc func(code int, a1, a2, db, trigger *string) int

// DB is one connection.
type DB struct {
	p    *C.sqlite3
	auth cgo.Handle
}

// Open opens a database with the engine's defaults: foreign keys on, double-quoted strings off,
// defensive mode on, extension loading off, no busy timeout.
func Open(path string, readOnly bool) (*DB, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	flags := C.int(openRW | openCreat)
	if readOnly {
		flags = openRO
	}
	var p *C.sqlite3
	rc := C.sqlite3_open_v2(cpath, &p, flags, nil)
	if rc != OK {
		msg := "unable to open database file"
		if p != nil {
			msg = C.GoString(C.sqlite3_errmsg(p))
			C.sqlite3_close_v2(p)
		}
		return nil, &Error{msg}
	}
	db := &DB{p: p}
	for _, c := range [][2]int{{dbconfigDQSDML, 0}, {dbconfigDQSDDL, 0}, {dbconfigEnableFkey, 1}, {dbconfigDefensive, 1}, {dbconfigLoadExtension, 0}} {
		C.gl_db_config_int(p, C.int(c[0]), C.int(c[1]))
	}
	C.sqlite3_busy_timeout(p, 0)
	return db, nil
}

// Close closes the connection.
func (db *DB) Close() {
	if db.p == nil {
		return
	}
	C.gl_set_authorizer(db.p, 0)
	C.sqlite3_close_v2(db.p)
	db.p = nil
	if db.auth != 0 {
		db.auth.Delete()
		db.auth = 0
	}
}

func (db *DB) err() error { return &Error{C.GoString(C.sqlite3_errmsg(db.p))} }

// Exec runs SQL without results.
func (db *DB) Exec(sql string) error {
	c := C.CString(sql)
	defer C.free(unsafe.Pointer(c))
	var msg *C.char
	if C.sqlite3_exec(db.p, c, nil, nil, &msg) != OK {
		return db.err()
	}
	return nil
}

// SetAuthorizer installs f (nil removes it).
func (db *DB) SetAuthorizer(f AuthFunc) {
	old := db.auth
	if f == nil {
		db.auth = 0
		C.gl_set_authorizer(db.p, 0)
	} else {
		db.auth = cgo.NewHandle(f)
		C.gl_set_authorizer(db.p, C.uintptr_t(db.auth))
	}
	if old != 0 {
		old.Delete()
	}
}

//export goAuthorizer
func goAuthorizer(h C.uintptr_t, op C.int, a, b, c, d *C.char) C.int {
	f := cgo.Handle(h).Value().(AuthFunc)
	return C.int(f(int(op), str(a), str(b), str(c), str(d)))
}

func str(p *C.char) *string {
	if p == nil {
		return nil
	}
	s := C.GoString(p)
	return &s
}

// Stmt is a prepared statement.
type Stmt struct {
	db *DB
	p  *C.sqlite3_stmt
}

// Prepare compiles the first statement of sql.
func (db *DB) Prepare(sql string) (*Stmt, error) {
	c := C.CString(sql)
	defer C.free(unsafe.Pointer(c))
	var p *C.sqlite3_stmt
	if C.sqlite3_prepare_v2(db.p, c, C.int(len(sql)), &p, nil) != OK {
		return nil, db.err()
	}
	if p == nil {
		return nil, &Error{"empty statement"}
	}
	return &Stmt{db: db, p: p}, nil
}

// Finalize releases the statement.
func (s *Stmt) Finalize() {
	if s.p != nil {
		C.sqlite3_finalize(s.p)
		s.p = nil
	}
}

// Column describes one result column; Table and Origin are nil for expressions.
type Column struct {
	Name   string
	Table  *string
	Origin *string
}

// Columns returns the result columns.
func (s *Stmt) Columns() []Column {
	n := int(C.sqlite3_column_count(s.p))
	out := make([]Column, n)
	for i := range n {
		out[i] = Column{
			Name:   C.GoString(C.sqlite3_column_name(s.p, C.int(i))),
			Table:  str(C.sqlite3_column_table_name(s.p, C.int(i))),
			Origin: str(C.sqlite3_column_origin_name(s.p, C.int(i))),
		}
	}
	return out
}

// BindNamed binds an object of named parameters like JavaScript named parameters: a key matches `:key`, `$key` or `@key`;
// numbers bind as REAL, strings as TEXT, nil as NULL. Unknown keys are an error.
func (s *Stmt) BindNamed(params map[string]any, order []string) error {
	bare := map[string]int{}
	for i := 1; i <= int(C.sqlite3_bind_parameter_count(s.p)); i++ {
		name := C.sqlite3_bind_parameter_name(s.p, C.int(i))
		if name == nil {
			continue
		}
		full := C.GoString(name)
		key := full[1:]
		if prev, ok := bare[key]; ok && prev != i {
			prevName := C.GoString(C.sqlite3_bind_parameter_name(s.p, C.int(prev)))
			return &Error{"Cannot create bare named parameter '" + key + "' because of conflicting names '" + prevName + "' and '" + full + "'."}
		}
		bare[key] = i
	}
	for _, k := range order {
		ck := C.CString(k)
		idx := int(C.sqlite3_bind_parameter_index(s.p, ck))
		C.free(unsafe.Pointer(ck))
		if idx == 0 {
			idx = bare[k]
		}
		if idx == 0 {
			return &Error{"Unknown named parameter '" + k + "'"}
		}
		var rc C.int
		switch v := params[k].(type) {
		case nil:
			rc = C.sqlite3_bind_null(s.p, C.int(idx))
		case float64:
			rc = C.sqlite3_bind_double(s.p, C.int(idx), C.double(v))
		case string:
			cv := C.CString(v)
			rc = C.gl_bind_text(s.p, C.int(idx), cv, C.int(len(v)))
			C.free(unsafe.Pointer(cv))
		default:
			return &Error{"Provided value cannot be bound to SQLite parameter " + strconv.Itoa(idx) + "."}
		}
		if rc != OK {
			return s.db.err()
		}
	}
	return nil
}

// Step advances; false at the end.
func (s *Stmt) Step() (bool, error) {
	switch C.sqlite3_step(s.p) {
	case rowCode:
		return true, nil
	case doneCode:
		return false, nil
	}
	return false, s.db.err()
}

// Value is one cell.
type Value struct {
	Type  int
	Int   int64
	Float float64
	Text  string
}

// Value reads column i of the current row.
func (s *Stmt) Value(i int) Value {
	ci := C.int(i)
	switch t := int(C.sqlite3_column_type(s.p, ci)); t {
	case Integer:
		return Value{Type: t, Int: int64(C.sqlite3_column_int64(s.p, ci))}
	case Float:
		return Value{Type: t, Float: float64(C.sqlite3_column_double(s.p, ci))}
	case Text:
		p := C.sqlite3_column_text(s.p, ci)
		n := C.sqlite3_column_bytes(s.p, ci)
		return Value{Type: t, Text: C.GoStringN((*C.char)(unsafe.Pointer(p)), n)}
	case Blob:
		return Value{Type: t}
	default:
		return Value{Type: Null}
	}
}

// Query runs SQL and returns all rows as values (helper for small internal reads).
func (db *DB) Query(sql string) ([][]Value, error) {
	st, err := db.Prepare(sql)
	if err != nil {
		return nil, err
	}
	defer st.Finalize()
	n := len(st.Columns())
	var rows [][]Value
	for {
		ok, err := st.Step()
		if err != nil {
			return nil, err
		}
		if !ok {
			return rows, nil
		}
		row := make([]Value, n)
		for i := range row {
			row[i] = st.Value(i)
		}
		rows = append(rows, row)
	}
}

// Bind binds positional values: float64 (JavaScript numbers) as REAL,
// int64 as INTEGER, strings as TEXT, nil as NULL.
func (s *Stmt) Bind(vals ...any) error {
	C.sqlite3_reset(s.p)
	C.sqlite3_clear_bindings(s.p)
	for i, v := range vals {
		idx := C.int(i + 1)
		var rc C.int
		switch x := v.(type) {
		case nil:
			rc = C.sqlite3_bind_null(s.p, idx)
		case float64:
			rc = C.sqlite3_bind_double(s.p, idx, C.double(x))
		case int:
			rc = C.sqlite3_bind_double(s.p, idx, C.double(float64(x)))
		case int64:
			rc = C.sqlite3_bind_int64(s.p, idx, C.sqlite3_int64(x))
		case string:
			cv := C.CString(x)
			rc = C.gl_bind_text(s.p, idx, cv, C.int(len(x)))
			C.free(unsafe.Pointer(cv))
		default:
			return &Error{"Provided value cannot be bound to SQLite parameter " + strconv.Itoa(i+1) + "."}
		}
		if rc != OK {
			return s.db.err()
		}
	}
	return nil
}

// Run binds vals, steps to the end and returns the number of changed rows.
func (s *Stmt) Run(vals ...any) (int, error) {
	if err := s.Bind(vals...); err != nil {
		return 0, err
	}
	for {
		ok, err := s.Step()
		if err != nil {
			C.sqlite3_reset(s.p)
			return 0, err
		}
		if !ok {
			break
		}
	}
	n := int(C.sqlite3_changes(s.db.p))
	C.sqlite3_reset(s.p)
	return n, nil
}

// RunOnce prepares, runs and finalizes one statement.
func (db *DB) RunOnce(sql string, vals ...any) (int, error) {
	st, err := db.Prepare(sql)
	if err != nil {
		return 0, err
	}
	defer st.Finalize()
	return st.Run(vals...)
}

// InTransaction reports an open transaction.
func (db *DB) InTransaction() bool { return C.sqlite3_get_autocommit(db.p) == 0 }

// MaxSafe is Number.MAX_SAFE_INTEGER.
const MaxSafe = 1<<53 - 1

// JS returns a cell as a JavaScript value: numbers as float64, text as string.
// Integers outside the safe range are an error.
func (v Value) JS() (any, error) {
	switch v.Type {
	case Integer:
		if v.Int > MaxSafe || v.Int < -MaxSafe {
			return nil, &Error{"Value is too large to be represented as a JavaScript number: " + strconv.FormatInt(v.Int, 10)}
		}
		return float64(v.Int), nil
	case Float:
		return v.Float, nil
	case Text:
		return v.Text, nil
	case Null:
		return nil, nil
	}
	return nil, &Error{"BLOB values are not supported"}
}

// QueryJS runs SQL with positional values and returns rows as JavaScript values.
func (db *DB) QueryJS(sql string, vals ...any) ([][]any, []string, error) {
	st, err := db.Prepare(sql)
	if err != nil {
		return nil, nil, err
	}
	defer st.Finalize()
	if err := st.Bind(vals...); err != nil {
		return nil, nil, err
	}
	cols := st.Columns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	var rows [][]any
	for {
		ok, err := st.Step()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return rows, names, nil
		}
		row := make([]any, len(cols))
		for i := range row {
			if row[i], err = st.Value(i).JS(); err != nil {
				return nil, nil, err
			}
		}
		rows = append(rows, row)
	}
}
