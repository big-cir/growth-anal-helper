package cdc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// column is one column of a followed table.
type column struct {
	Name     string
	DataType string   // information_schema DATA_TYPE
	Enum     []string // values in order, for enum columns
}

// tableDef is what run checks and uses about a followed table on either side.
type tableDef struct {
	Columns []column
	text    []string // canonical definition lines, compared between the two sides
}

var supportedTypes = map[string]bool{
	"tinyint": true, "smallint": true, "mediumint": true, "int": true, "bigint": true,
	"datetime": true, "varchar": true, "enum": true,
}

// readDefs reads the definitions of the named tables ("db.table"). It stops on a missing table,
// an engine other than InnoDB, or a column type run does not convert.
func readDefs(ctx context.Context, db *sql.DB, names []string) (map[string]*tableDef, error) {
	defs := map[string]*tableDef{}
	for _, n := range names {
		schema, table, _ := strings.Cut(n, ".")
		var engine sql.NullString
		err := db.QueryRowContext(ctx, `SELECT ENGINE FROM information_schema.TABLES
			WHERE BINARY TABLE_SCHEMA = BINARY ? AND BINARY TABLE_NAME = BINARY ? AND TABLE_TYPE = 'BASE TABLE'`, schema, table).Scan(&engine)
		if err == sql.ErrNoRows {
			return nil, &StopError{n + ": table not found"}
		} else if err != nil {
			return nil, err
		}
		if !strings.EqualFold(engine.String, "InnoDB") {
			return nil, &StopError{n + ": not an InnoDB table"}
		}
		d := &tableDef{text: []string{"engine InnoDB"}}
		rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE,
				IFNULL(CHARACTER_SET_NAME, ''), IFNULL(COLLATION_NAME, '')
			FROM information_schema.COLUMNS
			WHERE BINARY TABLE_SCHEMA = BINARY ? AND BINARY TABLE_NAME = BINARY ?
			ORDER BY ORDINAL_POSITION`, schema, table)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name, dataType, colType, nullable, charset, collation string
			if err := rows.Scan(&name, &dataType, &colType, &nullable, &charset, &collation); err != nil {
				rows.Close()
				return nil, err
			}
			dataType = strings.ToLower(dataType)
			if !supportedTypes[dataType] || strings.Contains(strings.ToLower(colType), "unsigned") {
				rows.Close()
				return nil, &StopError{fmt.Sprintf("%s.%s: column type %s is not supported", n, name, colType)}
			}
			c := column{Name: name, DataType: dataType}
			if dataType == "enum" {
				if c.Enum, err = enumValues(colType); err != nil {
					rows.Close()
					return nil, &StopError{fmt.Sprintf("%s.%s: %s", n, name, err)}
				}
			}
			d.Columns = append(d.Columns, c)
			d.text = append(d.text, strings.Join([]string{"column", name, colType, nullable, charset, collation}, " "))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		keys, err := queryRows(ctx, db, fmt.Sprintf(`SELECT INDEX_NAME, SEQ_IN_INDEX, IFNULL(COLUMN_NAME, ''), IFNULL(SUB_PART, ''),
				IFNULL(COLLATION, ''), IFNULL(EXPRESSION, ''), INDEX_TYPE FROM information_schema.STATISTICS
			WHERE BINARY TABLE_SCHEMA = BINARY '%s' AND BINARY TABLE_NAME = BINARY '%s' AND NON_UNIQUE = 0
			ORDER BY INDEX_NAME, SEQ_IN_INDEX`, sqlString(schema), sqlString(table)), 7)
		if err != nil {
			return nil, err
		}
		hasPK := false
		for _, k := range keys {
			hasPK = hasPK || k[0] == "PRIMARY"
			d.text = append(d.text, "unique "+strings.Join(k, " "))
		}
		if !hasPK {
			return nil, &StopError{n + ": no primary key"}
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

// sameDefs reports the first table whose definition differs, and a fingerprint of the source side.
func sameDefs(src, dst map[string]*tableDef) (string, error) {
	names := make([]string, 0, len(src))
	for n := range src {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		a, b := strings.Join(src[n].text, "\n"), ""
		if d := dst[n]; d != nil {
			b = strings.Join(d.text, "\n")
		}
		if a != b {
			return "", &StopError{n + ": table definition differs between the source and the copy"}
		}
		fmt.Fprintf(h, "%s\n%s\n", n, a)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// noTriggersOrForeignKeys stops when the copy has triggers on, or foreign keys from or to, a followed table:
// applying a row change there would do more than the source did.
func noTriggersOrForeignKeys(ctx context.Context, db *sql.DB, names []string) error {
	for _, n := range names {
		schema, table, _ := strings.Cut(n, ".")
		var c int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TRIGGERS
			WHERE BINARY EVENT_OBJECT_SCHEMA = BINARY ? AND BINARY EVENT_OBJECT_TABLE = BINARY ?`, schema, table).Scan(&c); err != nil {
			return err
		}
		if c > 0 {
			return &StopError{n + ": the copy has triggers on this table"}
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE
			WHERE REFERENCED_TABLE_NAME IS NOT NULL AND (
			  (BINARY TABLE_SCHEMA = BINARY ? AND BINARY TABLE_NAME = BINARY ?) OR
			  (BINARY REFERENCED_TABLE_SCHEMA = BINARY ? AND BINARY REFERENCED_TABLE_NAME = BINARY ?))`, schema, table, schema, table).Scan(&c); err != nil {
			return err
		}
		if c > 0 {
			return &StopError{n + ": the copy has foreign keys on this table"}
		}
	}
	return nil
}
