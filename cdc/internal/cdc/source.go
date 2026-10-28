package cdc

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/go-mysql-org/go-mysql/mysql"
	gosql "github.com/go-sql-driver/mysql"
)

// Source is the MySQL server whose binlog is read.
type Source struct {
	Host     string
	Port     int
	User     string
	Password string
}

// Error is a CDC setup or protocol error.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// LoadSource reads host, port, user and password from the [client] section of a MySQL option file.
// Includes, inline comments and escapes are rejected rather than guessed.
func LoadSource(path string) (Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return Source{}, err
	}
	defer f.Close()
	s := Source{Port: 3306}
	section := ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		where := fmt.Sprintf("%s line %d", path, n)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '!' {
			return Source{}, &Error{where + ": includes are not supported"}
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "client" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.ReplaceAll(strings.TrimSpace(k), "_", "-")
		if k != "host" && k != "port" && k != "user" && k != "password" {
			continue
		}
		v, err := optionValue(strings.TrimSpace(v))
		if err != nil {
			return Source{}, &Error{where + ": " + err.Error()}
		}
		switch k {
		case "host":
			s.Host = v
		case "port":
			p, err := strconv.Atoi(v)
			if err != nil || p < 1 || p > 65535 {
				return Source{}, &Error{where + ": port must be a number"}
			}
			s.Port = p
		case "user":
			s.User = v
		case "password":
			s.Password = v
		}
	}
	if err := sc.Err(); err != nil {
		return Source{}, err
	}
	if s.Host == "" || s.User == "" {
		return Source{}, &Error{path + ": [client] needs host and user"}
	}
	return s, nil
}

func optionValue(v string) (string, error) {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		inner := v[1 : len(v)-1]
		if strings.ContainsAny(inner, "\\") || strings.ContainsRune(inner, rune(v[0])) {
			return "", &Error{"escapes in quoted values are not supported"}
		}
		return inner, nil
	}
	if strings.ContainsAny(v, "\"'") {
		return "", &Error{"unbalanced quotes"}
	}
	if strings.Contains(v, "#") {
		return "", &Error{"inline comments are not supported"}
	}
	return v, nil
}

// tlsConfig requires TLS without checking the certificate, since a tunnel address does not match its name.
func tlsConfig() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

var registerTLS sync.Once

// open returns a regular client connection for status and schema queries.
func (s Source) open() (*sql.DB, error) {
	var regErr error
	registerTLS.Do(func() { regErr = gosql.RegisterTLSConfig("cdc", tlsConfig()) })
	if regErr != nil {
		return nil, regErr
	}
	cfg := gosql.NewConfig()
	cfg.User, cfg.Passwd = s.User, s.Password
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	cfg.TLSConfig = "cdc"
	cfg.AllowNativePasswords = true
	c, err := gosql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}

// LogFile is a binlog file the server still keeps.
type LogFile struct {
	Name string
	Size uint64
}

// Status is the server's identity, kept binlog files and current write position.
type Status struct {
	ServerUUID string
	ServerID   uint32
	Version    int // 8.4.8 → 80408, as in /*!80408 … */
	Files      []LogFile
	Current    mysql.Position
	ReplicaIDs []uint32
}

// Has reports whether the server still keeps the named file.
func (st Status) Has(name string) bool {
	for _, f := range st.Files {
		if f.Name == name {
			return true
		}
	}
	return false
}

// versionNumber turns "8.4.8-log" into 80408.
func versionNumber(v string) (int, error) {
	var major, minor, patch int
	if n, _ := fmt.Sscanf(v, "%d.%d.%d", &major, &minor, &patch); n != 3 || minor > 99 || patch > 99 {
		return 0, &Error{"VERSION(): not in major.minor.patch form"}
	}
	return major*10000 + minor*100 + patch, nil
}

// ReadStatus reads the server identity, its binlog files, current position and connected replica IDs.
func (s Source) ReadStatus(ctx context.Context) (Status, error) {
	db, err := s.open()
	if err != nil {
		return Status{}, err
	}
	defer db.Close()
	var st Status
	var id uint64
	var version string
	if err := db.QueryRowContext(ctx, "SELECT @@server_uuid, @@server_id, VERSION()").Scan(&st.ServerUUID, &id, &version); err != nil {
		return Status{}, err
	}
	st.ServerID = uint32(id)
	if st.Version, err = versionNumber(version); err != nil {
		return Status{}, err
	}
	files, err := queryRows(ctx, db, "SHOW BINARY LOGS", 2)
	if err != nil {
		return Status{}, err
	}
	for _, r := range files {
		size, err := strconv.ParseUint(r[1], 10, 64)
		if err != nil {
			return Status{}, &Error{"SHOW BINARY LOGS: file size is not a number"}
		}
		st.Files = append(st.Files, LogFile{Name: r[0], Size: size})
	}
	cur, err := queryRows(ctx, db, "SHOW BINARY LOG STATUS", 2)
	if err != nil {
		return Status{}, err
	}
	if len(cur) != 1 || cur[0][0] == "" {
		return Status{}, &Error{"binary logging is off on the source"}
	}
	pos, err := strconv.ParseUint(cur[0][1], 10, 64)
	if err != nil || pos > math.MaxUint32 {
		return Status{}, &Error{"SHOW BINARY LOG STATUS: position is not a number"}
	}
	st.Current = mysql.Position{Name: cur[0][0], Pos: uint32(pos)}
	if !st.Has(st.Current.Name) {
		return Status{}, &Error{"current binlog file " + st.Current.Name + " is not in SHOW BINARY LOGS; try again"}
	}
	reps, err := queryRows(ctx, db, "SHOW REPLICAS", 1)
	if err != nil {
		return Status{}, err
	}
	for _, r := range reps {
		n, err := strconv.ParseUint(r[0], 10, 32)
		if err != nil {
			return Status{}, &Error{"SHOW REPLICAS: server id is not a number"}
		}
		st.ReplicaIDs = append(st.ReplicaIDs, uint32(n))
	}
	return st, nil
}

// queryRows returns all rows as text; it fails when a row has fewer than min columns.
func queryRows(ctx context.Context, db *sql.DB, q string, min int) ([][]string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if len(cols) < min {
		return nil, &Error{fmt.Sprintf("%s: expected at least %d columns, got %d", q, min, len(cols))}
	}
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := make([]string, len(cols))
		for i, v := range vals {
			r[i] = v.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SelectTables lists the base tables this account can see, keeps those the filter follows,
// and reads their primary key positions and column counts.
func (s Source) SelectTables(ctx context.Context, f Filter) (map[string]TableInfo, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT c.TABLE_SCHEMA, c.TABLE_NAME, c.ORDINAL_POSITION, s.COLUMN_NAME IS NOT NULL
		FROM information_schema.COLUMNS c
		JOIN information_schema.TABLES t
		  ON BINARY t.TABLE_SCHEMA = BINARY c.TABLE_SCHEMA AND BINARY t.TABLE_NAME = BINARY c.TABLE_NAME
		LEFT JOIN information_schema.STATISTICS s
		  ON BINARY s.TABLE_SCHEMA = BINARY c.TABLE_SCHEMA AND BINARY s.TABLE_NAME = BINARY c.TABLE_NAME
		 AND BINARY s.COLUMN_NAME = BINARY c.COLUMN_NAME AND s.INDEX_NAME = 'PRIMARY'
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND c.TABLE_SCHEMA NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
		ORDER BY c.TABLE_SCHEMA, c.TABLE_NAME, c.ORDINAL_POSITION`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TableInfo{}
	for rows.Next() {
		var schema, table string
		var pos int
		var pri bool
		if err := rows.Scan(&schema, &table, &pos, &pri); err != nil {
			return nil, err
		}
		if !f.Table(schema, table) {
			continue
		}
		key := schema + "." + table
		info := out[key]
		info.Columns++
		if pri {
			info.PK = append(info.PK, pos-1)
		}
		out[key] = info
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, &Error{"no table matches the include lists (or this account cannot see them)"}
	}
	var noKey []string
	for k, info := range out {
		if len(info.PK) == 0 {
			noKey = append(noKey, k)
		}
	}
	if len(noKey) > 0 {
		sort.Strings(noKey)
		return nil, &Error{"tables without a primary key: " + strings.Join(noKey, ", ") + "; leave them out with --table-include-list"}
	}
	return out, nil
}
