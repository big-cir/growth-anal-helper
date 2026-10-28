package cdc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// column is one column of a followed table.
type column struct {
	Name     string
	DataType string   // information_schema DATA_TYPE
	Enum     []string // values in order, for enum columns
}

// indexPart is one column of an index.
type indexPart struct {
	Column  string
	SubPart string // prefix length, empty for the whole column
	Desc    bool
}

// indexDef is a secondary index, rebuilt on a staging table after the copy.
type indexDef struct {
	Name    string
	Unique  bool
	Visible bool
	Parts   []indexPart
}

func (d indexDef) text() string {
	var parts []string
	for _, p := range d.Parts {
		parts = append(parts, fmt.Sprintf("%s(%s)%v", p.Column, p.SubPart, p.Desc))
	}
	return fmt.Sprintf("index %s unique=%v visible=%v %s", d.Name, d.Unique, d.Visible, strings.Join(parts, ","))
}

// tableDef is what run checks and uses about a followed table on either side.
type tableDef struct {
	Columns []column
	PK      []int // column positions of the primary key, in key order
	Indexes []indexDef
	base    []string // engine, columns and primary key
	full    []string // base plus every secondary index
}

var supportedTypes = map[string]bool{
	"tinyint": true, "smallint": true, "mediumint": true, "int": true, "bigint": true,
	"datetime": true, "varchar": true, "enum": true,
}

// readDefs reads the definitions of the followed tables, named by their source "db.table".
// With physDB set the tables are read from that database instead (a staging copy).
// It stops on a missing table, an engine other than InnoDB, a column type run does not convert,
// or an index it cannot rebuild.
func readDefs(ctx context.Context, db *sql.DB, physDB string, names []string) (map[string]*tableDef, error) {
	defs := map[string]*tableDef{}
	for _, n := range names {
		schema, table, _ := strings.Cut(n, ".")
		if physDB != "" {
			schema = physDB
		}
		where := schema + "." + table
		var engine sql.NullString
		err := db.QueryRowContext(ctx, `SELECT ENGINE FROM information_schema.TABLES
			WHERE BINARY TABLE_SCHEMA = BINARY ? AND BINARY TABLE_NAME = BINARY ? AND TABLE_TYPE = 'BASE TABLE'`, schema, table).Scan(&engine)
		if err == sql.ErrNoRows {
			return nil, &StopError{where + ": table not found"}
		} else if err != nil {
			return nil, err
		}
		if !strings.EqualFold(engine.String, "InnoDB") {
			return nil, &StopError{where + ": not an InnoDB table"}
		}
		d := &tableDef{base: []string{"engine InnoDB"}}
		rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE,
				IFNULL(CHARACTER_SET_NAME, ''), IFNULL(COLLATION_NAME, '')
			FROM information_schema.COLUMNS
			WHERE BINARY TABLE_SCHEMA = BINARY ? AND BINARY TABLE_NAME = BINARY ?
			ORDER BY ORDINAL_POSITION`, schema, table)
		if err != nil {
			return nil, err
		}
		pos := map[string]int{}
		for rows.Next() {
			var name, dataType, colType, nullable, charset, collation string
			if err := rows.Scan(&name, &dataType, &colType, &nullable, &charset, &collation); err != nil {
				rows.Close()
				return nil, err
			}
			dataType = strings.ToLower(dataType)
			if !supportedTypes[dataType] || strings.Contains(strings.ToLower(colType), "unsigned") {
				rows.Close()
				return nil, &StopError{fmt.Sprintf("%s.%s: column type %s is not supported", where, name, colType)}
			}
			c := column{Name: name, DataType: dataType}
			if dataType == "enum" {
				if c.Enum, err = enumValues(colType); err != nil {
					rows.Close()
					return nil, &StopError{fmt.Sprintf("%s.%s: %s", where, name, err)}
				}
			}
			pos[name] = len(d.Columns)
			d.Columns = append(d.Columns, c)
			d.base = append(d.base, strings.Join([]string{"column", name, colType, nullable, charset, collation}, " "))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		keys, err := queryRows(ctx, db, fmt.Sprintf(`SELECT INDEX_NAME, NON_UNIQUE, IFNULL(COLUMN_NAME, ''), IFNULL(SUB_PART, ''),
				IFNULL(COLLATION, ''), IFNULL(EXPRESSION, ''), INDEX_TYPE, IS_VISIBLE FROM information_schema.STATISTICS
			WHERE BINARY TABLE_SCHEMA = BINARY '%s' AND BINARY TABLE_NAME = BINARY '%s'
			ORDER BY INDEX_NAME, SEQ_IN_INDEX`, sqlString(schema), sqlString(table)), 8)
		if err != nil {
			return nil, err
		}
		var pk []string
		byName := map[string]*indexDef{}
		var order []string
		for _, k := range keys {
			name, nonUnique, col, sub, coll, expr, typ, visible := k[0], k[1], k[2], k[3], k[4], k[5], k[6], k[7]
			if expr != "" || col == "" || !strings.EqualFold(typ, "BTREE") {
				return nil, &StopError{fmt.Sprintf("%s: index %s is not a plain column index", where, name)}
			}
			if name == "PRIMARY" {
				pk = append(pk, fmt.Sprintf("%s(%s)%s", col, sub, coll))
				d.PK = append(d.PK, pos[col])
				continue
			}
			ix := byName[name]
			if ix == nil {
				ix = &indexDef{Name: name, Unique: nonUnique == "0", Visible: visible == "YES"}
				byName[name] = ix
				order = append(order, name)
			}
			ix.Parts = append(ix.Parts, indexPart{Column: col, SubPart: sub, Desc: coll == "D"})
		}
		if len(pk) == 0 {
			return nil, &StopError{where + ": no primary key"}
		}
		d.base = append(d.base, "primary "+strings.Join(pk, ","))
		d.full = append([]string(nil), d.base...)
		for _, name := range order {
			d.Indexes = append(d.Indexes, *byName[name])
			d.full = append(d.full, byName[name].text())
		}
		defs[n] = d
	}
	return defs, nil
}

// sqlString escapes a name for a quoted literal; names come from the command line.
func sqlString(s string) string { return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) }

// enumValues parses enum('a','b') as stored in COLUMN_TYPE.
func enumValues(colType string) ([]string, error) {
	s := strings.TrimSpace(colType)
	if !strings.HasPrefix(strings.ToLower(s), "enum(") || !strings.HasSuffix(s, ")") {
		return nil, fmt.Errorf("enum definition not understood")
	}
	s = s[5 : len(s)-1]
	var out []string
	for len(s) > 0 {
		if s[0] != '\'' {
			return nil, fmt.Errorf("enum definition not understood")
		}
		var b strings.Builder
		i := 1
		for ; i < len(s); i++ {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				break
			}
			b.WriteByte(s[i])
		}
		if i >= len(s) {
			return nil, fmt.Errorf("enum definition not understood")
		}
		out = append(out, b.String())
		s = s[i+1:]
		if strings.HasPrefix(s, ",") {
			s = s[1:]
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("enum has no values")
	}
	return out, nil
}

// fingerprint is the hash of the full definitions.
func fingerprint(defs map[string]*tableDef) string {
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\n%s\n", n, strings.Join(defs[n].full, "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sameDefs reports the first table whose definition differs: everything with full set,
// otherwise engine, columns and primary key only (a staging table before its indexes are rebuilt).
func sameDefs(src, dst map[string]*tableDef, full bool) error {
	names := make([]string, 0, len(src))
	for n := range src {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		d := dst[n]
		if d == nil {
			return &StopError{n + ": missing in the copy"}
		}
		a, b := src[n].base, d.base
		if full {
			a, b = src[n].full, d.full
		}
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			return &StopError{n + ": table definition differs between the source and the copy"}
		}
	}
	return nil
}

// noTriggersOrForeignKeys stops when side has triggers on, or foreign keys from or to, a followed table.
// On the copy, applying a row change would do more than the source did; on the source, a cascaded
// change may not reach the binlog, and locking the table would lock the related ones too.
// It sees only what the account may see: triggers need the TRIGGER privilege, and a foreign key
// from another schema needs access to that table.
func noTriggersOrForeignKeys(ctx context.Context, db *sql.DB, physDB string, names []string, side string) error {
	for _, n := range names {
		schema, table, _ := strings.Cut(n, ".")
		if physDB != "" {
			schema = physDB
		}
		var c int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TRIGGERS
			WHERE BINARY EVENT_OBJECT_SCHEMA = BINARY ? AND BINARY EVENT_OBJECT_TABLE = BINARY ?`, schema, table).Scan(&c); err != nil {
			return err
		}
		if c > 0 {
			return &StopError{schema + "." + table + ": " + side + " has triggers on this table"}
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE
			WHERE REFERENCED_TABLE_NAME IS NOT NULL AND (
			  (BINARY TABLE_SCHEMA = BINARY ? AND BINARY TABLE_NAME = BINARY ?) OR
			  (BINARY REFERENCED_TABLE_SCHEMA = BINARY ? AND BINARY REFERENCED_TABLE_NAME = BINARY ?))`, schema, table, schema, table).Scan(&c); err != nil {
			return err
		}
		if c > 0 {
			return &StopError{schema + "." + table + ": " + side + " has foreign keys on this table"}
		}
	}
	return nil
}

// stagingDDL is the source table's CREATE TABLE for physDB, keeping columns, primary key,
// engine, charset and comment, and leaving out secondary indexes, foreign keys and the auto-increment counter.
func stagingDDL(ctx context.Context, src *sql.DB, name, physDB string) (string, error) {
	schema, table, _ := strings.Cut(name, ".")
	var tn, ddl string
	if err := src.QueryRowContext(ctx, "SHOW CREATE TABLE "+quoteName(schema)+"."+quoteName(table)).Scan(&tn, &ddl); err != nil {
		return "", err
	}
	lines := strings.Split(ddl, "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "CREATE TABLE ") {
		return "", &StopError{name + ": CREATE TABLE not understood"}
	}
	out := []string{"CREATE TABLE " + quoteName(physDB) + "." + quoteName(table) + " ("}
	for _, l := range lines[1 : len(lines)-1] {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "KEY "), strings.HasPrefix(t, "UNIQUE KEY "), strings.HasPrefix(t, "FULLTEXT "),
			strings.HasPrefix(t, "SPATIAL "), strings.HasPrefix(t, "CONSTRAINT "):
			continue
		}
		out = append(out, "  "+strings.TrimSuffix(t, ","))
	}
	for i := 1; i < len(out)-1; i++ {
		out[i] += ","
	}
	tail := autoIncrement.ReplaceAllString(strings.TrimPrefix(lines[len(lines)-1], ")"), "")
	return strings.Join(out, "\n") + "\n)" + tail, nil
}

var autoIncrement = regexp.MustCompile(` AUTO_INCREMENT=[0-9]+`)

// addIndexSQL rebuilds one secondary index on physDB.table.
func addIndexSQL(physDB, table string, ix indexDef) string {
	var parts []string
	for _, p := range ix.Parts {
		s := quoteName(p.Column)
		if p.SubPart != "" {
			s += "(" + p.SubPart + ")"
		}
		if p.Desc {
			s += " DESC"
		}
		parts = append(parts, s)
	}
	kind := "INDEX"
	if ix.Unique {
		kind = "UNIQUE INDEX"
	}
	q := fmt.Sprintf("ALTER TABLE %s.%s ADD %s %s (%s)", quoteName(physDB), quoteName(table), kind, quoteName(ix.Name), strings.Join(parts, ", "))
	if !ix.Visible {
		q += " INVISIBLE"
	}
	return q
}
