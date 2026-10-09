package collect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ServerDatasource is a MySQL or PostgreSQL connection.
type ServerDatasource struct {
	Kind, Host, User, Password, Database string
	Port                                 int
}

// WireError is a source connection or server error.
type WireError struct{ Msg string }

func (e *WireError) Error() string { return e.Msg }

// cancellable runs one query at a time and stops it on Abort.
type cancellable struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	aborted bool
}

func (c *cancellable) start() (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.aborted {
		return nil, &WireError{"cancelled"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	return ctx, nil
}

func (c *cancellable) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
}

func (c *cancellable) Abort() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.aborted = true
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *cancellable) wrap(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return &WireError{"cancelled"}
	}
	return err
}

// MysqlSource reads MySQL over a new read-only session per query (TLS when the server offers it, certificate not verified).
type MysqlSource struct {
	DS ServerDatasource
	cancellable
}

func (s *MysqlSource) Dialect() Dialect { return "mysql" }

func (s *MysqlSource) Now() (string, error) { return SingleValue(s, "SELECT NOW(6) AS now") }

func (s *MysqlSource) SelectStream(query string, onRow func([]RawValue) error, onColumns func([]string) error) (SelectResult, error) {
	t0 := time.Now()
	ctx, err := s.start()
	if err != nil {
		return SelectResult{}, err
	}
	defer s.done()
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.DBName = s.DS.User, s.DS.Password, s.DS.Database
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(s.DS.Host, strconv.Itoa(s.DS.Port))
	cfg.TLSConfig = "preferred"
	cfg.Collation = "utf8mb4_general_ci"
	cfg.AllowNativePasswords = true
	cfg.AllowCleartextPasswords = false
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return SelectResult{}, &WireError{err.Error()}
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return SelectResult{}, s.wrap(ctx, mysqlErr(err))
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
		return SelectResult{}, s.wrap(ctx, mysqlErr(err))
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return SelectResult{}, s.wrap(ctx, mysqlErr(err))
	}
	n, cols, err := stream(rows, onRow, onColumns)
	if err != nil {
		return SelectResult{}, s.wrap(ctx, mysqlErr(err))
	}
	return SelectResult{Columns: cols, Rows: n, Ms: msSince(t0)}, nil
}

func mysqlErr(err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return &WireError{fmt.Sprintf("MySQL error %d: %s", me.Number, me.Message)}
	}
	return err
}

// stream passes text values row by row.
func stream(rows *sql.Rows, onRow func([]RawValue) error, onColumns func([]string) error) (int, []string, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, nil, err
	}
	if onColumns != nil {
		if err := onColumns(cols); err != nil {
			return 0, nil, err
		}
	}
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return 0, nil, err
		}
		vals := make([]RawValue, len(cols))
		for i, b := range raw {
			if b != nil {
				s := string(b)
				vals[i] = &s
			}
		}
		if err := onRow(vals); err != nil {
			return 0, nil, err
		}
		n++
	}
	return n, cols, rows.Err()
}

// PostgresSource reads PostgreSQL with a read-only default transaction (TLS when the server offers it, certificate not verified).
type PostgresSource struct {
	DS ServerDatasource
	cancellable
}

func (s *PostgresSource) Dialect() Dialect { return "postgres" }

func (s *PostgresSource) Now() (string, error) {
	return SingleValue(s, "SELECT to_char(LOCALTIMESTAMP, 'YYYY-MM-DD HH24:MI:SS.US') AS now")
}

func (s *PostgresSource) SelectStream(query string, onRow func([]RawValue) error, onColumns func([]string) error) (SelectResult, error) {
	t0 := time.Now()
	ctx, err := s.start()
	if err != nil {
		return SelectResult{}, err
	}
	defer s.done()
	cfg, err := pgx.ParseConfig("sslmode=prefer")
	if err != nil {
		return SelectResult{}, err
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = s.DS.Host, uint16(s.DS.Port), s.DS.User, s.DS.Password, s.DS.Database
	cfg.RuntimeParams = map[string]string{"client_encoding": "UTF8", "DateStyle": "ISO", "default_transaction_read_only": "on", "application_name": "growth-lab"}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return SelectResult{}, s.wrap(ctx, pgErr(err))
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return SelectResult{}, s.wrap(ctx, pgErr(err))
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = f.Name
	}
	if onColumns != nil {
		if err := onColumns(cols); err != nil {
			return SelectResult{}, err
		}
	}
	n := 0
	for rows.Next() {
		raw := rows.RawValues()
		vals := make([]RawValue, len(raw))
		for i, b := range raw {
			if b != nil {
				v := string(b)
				vals[i] = &v
			}
		}
		if err := onRow(vals); err != nil {
			return SelectResult{}, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return SelectResult{}, s.wrap(ctx, pgErr(err))
	}
	return SelectResult{Columns: cols, Rows: n, Ms: msSince(t0)}, nil
}

func pgErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		msg := pe.Message
		if msg == "" {
			msg = "(no message)"
		}
		return &WireError{"PostgreSQL error " + pe.Code + ": " + msg}
	}
	return err
}
