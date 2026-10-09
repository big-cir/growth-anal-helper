// Package collect holds the collection spec (tables.json) and column roles. Columns not in the spec are never collected.
package collect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"growth-lab/internal/jsstr"
	"math"
	"os"
	"regexp"
	"strings"

	"growth-lab/internal/jsjson"
)

// Kind is a column type.
type Kind string

// Role is "ordinary", "private" or an identifier domain. private: only in the real snapshot.
type Role struct {
	Name       string // "ordinary" or "private"; empty for identifiers
	Identifier string
}

// IsIdentifier reports an identifier role.
func (r Role) IsIdentifier() bool { return r.Name == "" }

// JS is the role as it appears in JSON.
func (r Role) JS() any {
	if r.IsIdentifier() {
		return jsjson.Object{{Key: "identifier", Value: r.Identifier}}
	}
	return r.Name
}

var (
	Ordinary = Role{Name: "ordinary"}
	Private  = Role{Name: "private"}
)

// ColumnSpec is one collected column.
type ColumnSpec struct {
	Expr            string // identifier or `<identifier> IS [NOT] NULL`
	As              string
	Kind            Kind
	Role            Role
	MaxLength       int  // text only
	NullAfterCutoff bool // ts only: values after the cutoff become NULL
}

// TableSpec is one collected table.
type TableSpec struct {
	Source       string
	Target       string
	Key          []string
	CutoffColumn string
	Columns      []ColumnSpec
}

// SpecError is a tables.json validation error.
type SpecError struct{ Msg string }

func (e *SpecError) Error() string { return e.Msg }

var (
	identRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	exprRE   = regexp.MustCompile(`(?i)^([A-Za-z_][A-Za-z0-9_]*)(?:` + jsstr.Space + `+IS` + jsstr.Space + `+(NOT` + jsstr.Space + `+)?NULL)?$`)
	isRE     = regexp.MustCompile(`(?i)` + jsstr.Space + `+IS` + jsstr.Space + `+`)
	spacesRE = regexp.MustCompile(jsstr.Space + `+`)
	kinds    = []string{"int", "ts", "text", "bool"}
)

type failure struct{ err error }

func fail(where, msg string) { panic(failure{&SpecError{"tables.json " + where + ": " + msg}}) }

func catch(err *error) {
	if r := recover(); r != nil {
		f, ok := r.(failure)
		if !ok {
			panic(r)
		}
		*err = f.err
	}
}

func ident(v any, where string) string {
	s, ok := v.(string)
	if !ok || !identRE.MatchString(s) {
		fail(where, "must be an identifier (^[A-Za-z_][A-Za-z0-9_]*$)")
	}
	return s
}

func parseRole(v any, where string) Role {
	if s, ok := v.(string); ok && (s == "ordinary" || s == "private") {
		return Role{Name: s}
	}
	if o, ok := v.(jsjson.Object); ok && len(o) == 1 {
		if dom, ok := o[0].Value.(string); ok && o[0].Key == "identifier" && identRE.MatchString(dom) {
			return Role{Identifier: dom}
		}
	}
	fail(where, `role must be "ordinary" | "private" | { "identifier": "<domain>" } (required)`)
	return Role{}
}

func onlyKeys(o jsjson.Object, allowed []string, where func(k string) string) {
	for _, m := range o {
		if !has(allowed, m.Key) {
			fail(where(m.Key), "unknown key")
		}
	}
}

func get(o jsjson.Object, k string) any {
	v, _ := o.Get(k)
	return v
}

func has(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ParseSpec validates tables.json content (as decoded by jsjson.Parse).
func ParseSpec(raw any) (specs []TableSpec, err error) {
	defer catch(&err)
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		fail("(root)", "must be an array of table specs")
	}
	targets := map[string]bool{}
	for ti, t := range arr {
		w := fmt.Sprintf("[%d]", ti)
		o, ok := t.(jsjson.Object)
		if !ok {
			fail(w, "must be an object")
		}
		onlyKeys(o, []string{"source", "target", "key", "cutoffColumn", "columns"}, func(k string) string { return w + "." + k })
		source := ident(get(o, "source"), w+".source")
		target := ident(get(o, "target"), w+".target")
		if !strings.HasPrefix(target, "r_") {
			fail(w+".target", "must start with r_")
		}
		if strings.HasPrefix(target, "r_ga4_") {
			fail(w+".target", "r_ga4_ is reserved for GA4 tables")
		}
		if targets[target] {
			fail(w+".target", "duplicate: "+target)
		}
		targets[target] = true

		colsRaw, ok := get(o, "columns").([]any)
		if !ok || len(colsRaw) == 0 {
			fail(w+".columns", "must be a non-empty array")
		}
		seen := map[string]bool{}
		columns := make([]ColumnSpec, len(colsRaw))
		for ci, c := range colsRaw {
			cw := fmt.Sprintf("%s.columns[%d]", w, ci)
			co, ok := c.(jsjson.Object)
			if !ok {
				fail(cw, "must be an object")
			}
			onlyKeys(co, []string{"expr", "as", "kind", "role", "maxLength", "nullAfterCutoff"}, func(k string) string { return cw + "." + k })
			expr, ok := get(co, "expr").(string)
			if !ok || !exprRE.MatchString(jsstr.Trim(expr)) {
				fail(cw+".expr", "only an identifier or `<identifier> IS [NOT] NULL`")
			}
			as := ident(get(co, "as"), cw+".as")
			if seen[as] {
				fail(cw+".as", "duplicate: "+as)
			}
			seen[as] = true
			kindStr, _ := get(co, "kind").(string)
			if !has(kinds, kindStr) {
				fail(cw+".kind", "one of "+strings.Join(kinds, "|"))
			}
			kind := Kind(kindStr)
			if isRE.MatchString(expr) && kind != "bool" {
				fail(cw+".kind", "IS [NOT] NULL expressions must be bool")
			}
			role := parseRole(get(co, "role"), cw+".role")
			if role.IsIdentifier() && kind != "int" {
				fail(cw+".role", `identifier is only allowed for kind = "int"`)
			}
			hit := SensitiveColumnName(expr)
			if hit == "" {
				hit = SensitiveColumnName(as)
			}
			if hit == "" {
				hit = SensitiveColumnName(source)
			}
			if hit != "" && role != Private {
				fail(cw+".role", "looks sensitive ("+hit+`), so it can only be collected as "private"`)
			}
			maxLength := 64
			if v, present := co.Get("maxLength"); present {
				if kind != "text" {
					fail(cw+".maxLength", "only for text")
				}
				f, ok := v.(float64)
				if !ok || f != math.Trunc(f) || f < 1 || f > 4096 {
					fail(cw+".maxLength", "integer 1-4096")
				}
				maxLength = int(f)
			}
			nullAfter := false
			if v, present := co.Get("nullAfterCutoff"); present {
				b, ok := v.(bool)
				if !ok {
					fail(cw+".nullAfterCutoff", "true/false")
				}
				nullAfter = b
			}
			if nullAfter && kind != "ts" {
				fail(cw+".nullAfterCutoff", "only for ts")
			}
			columns[ci] = ColumnSpec{Expr: spacesRE.ReplaceAllString(jsstr.Trim(expr), " "), As: as, Kind: kind, Role: role, MaxLength: maxLength, NullAfterCutoff: nullAfter}
		}

		keyRaw, ok := get(o, "key").([]any)
		if !ok || len(keyRaw) == 0 {
			fail(w+".key", "must be a non-empty array")
		}
		key := make([]string, len(keyRaw))
		for ki, k := range keyRaw {
			name := ident(k, fmt.Sprintf("%s.key[%d]", w, ki))
			col := findColumn(columns, name)
			if col == nil {
				fail(fmt.Sprintf("%s.key[%d]", w, ki), "not among columns[].as: "+name)
			}
			if col.Kind != "int" {
				fail(fmt.Sprintf("%s.key[%d]", w, ki), "keys must be int")
			}
			key[ki] = name
		}
		if len(uniq(key)) != len(key) {
			fail(w+".key", "duplicate")
		}
		cutoff := ident(get(o, "cutoffColumn"), w+".cutoffColumn")
		cc := findColumn(columns, cutoff)
		if cc == nil {
			fail(w+".cutoffColumn", "not among columns[].as: "+cutoff)
		}
		if cc.Kind != "ts" {
			fail(w+".cutoffColumn", "must be ts")
		}
		if cc.NullAfterCutoff {
			fail(w+".cutoffColumn", "cannot be combined with nullAfterCutoff")
		}
		specs = append(specs, TableSpec{Source: source, Target: target, Key: key, CutoffColumn: cutoff, Columns: columns})
	}
	return specs, nil
}

func findColumn(cols []ColumnSpec, as string) *ColumnSpec {
	for i := range cols {
		if cols[i].As == as {
			return &cols[i]
		}
	}
	return nil
}

func uniq(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !has(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// LoadSpec reads and validates tables.json.
func LoadSpec(file string) ([]TableSpec, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, &SpecError{"cannot read tables.json: " + err.Error()}
	}
	raw, err := jsjson.Parse(string(b))
	if err != nil {
		return nil, &SpecError{"cannot read tables.json: " + err.Error()}
	}
	return ParseSpec(raw)
}

// JS returns the spec in its stored JSON form.
func (t TableSpec) JS(withRoles bool) jsjson.Object {
	cols := make([]any, len(t.Columns))
	for i, c := range t.Columns {
		o := jsjson.Object{{Key: "expr", Value: c.Expr}, {Key: "as", Value: c.As}, {Key: "kind", Value: string(c.Kind)}}
		if withRoles {
			o = append(o, jsjson.Member{Key: "role", Value: c.Role.JS()})
		}
		o = append(o, jsjson.Member{Key: "maxLength", Value: c.MaxLength}, jsjson.Member{Key: "nullAfterCutoff", Value: c.NullAfterCutoff})
		cols[i] = o
	}
	return jsjson.Object{{Key: "source", Value: t.Source}, {Key: "target", Value: t.Target}, {Key: "key", Value: t.Key}, {Key: "cutoffColumn", Value: t.CutoffColumn}, {Key: "columns", Value: cols}}
}

// SpecHash is the spec hash without roles.
func SpecHash(specs []TableSpec) string {
	arr := make([]any, len(specs))
	for i, t := range specs {
		arr[i] = t.JS(false)
	}
	return sha(jsjson.MustStringify(arr))
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Dialect is the source database type.
type Dialect string

// SelectSQL is the collection SELECT. Timestamps and bools are cast to the engine format, since their text form differs by database.
func SelectSQL(t TableSpec, dialect Dialect) string {
	cols := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = columnExpr(c, dialect) + " AS " + c.As
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + t.Source + " ORDER BY " + strings.Join(t.Key, ", ")
}

func columnExpr(c ColumnSpec, dialect Dialect) string {
	if dialect != "postgres" {
		return c.Expr
	}
	switch c.Kind {
	case "ts":
		return "to_char(" + c.Expr + ", 'YYYY-MM-DD HH24:MI:SS.US')"
	case "bool":
		return "CAST(" + c.Expr + " AS int)"
	}
	return c.Expr
}
