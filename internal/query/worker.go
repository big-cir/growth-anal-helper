package query

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/sqllint"
)

// InputLimit is the largest worker input.
const InputLimit = 64 * 1024

// SensitiveMessage does not say what was blocked.
const SensitiveMessage = "This tool can't work with that data"

// Input is one query request (JSON on stdin).
type Input struct {
	Path             string   `json:"path"`
	SQL              string   `json:"sql"`
	ReadablePrefixes []string `json:"readablePrefixes"`
	BlockedColumns   []string `json:"blockedColumns"`
	BlockedTables    []string `json:"blockedTables"`
	HeapLimitMb      float64  `json:"heapLimitMb"`
	MaxRows          int      `json:"maxRows"`
	Overflow         string   `json:"overflow"`
	CellLimit        int      `json:"cellLimit"`
	TruncateCells    bool     `json:"truncateCells"`
	OutputLimit      int      `json:"outputLimit"`
	// Params keep their key order (they bind in that order)
	Params jsjson.Object `json:"-"`
}

// ParseInput decodes the worker input.
func ParseInput(text []byte) (*Input, error) {
	var in Input
	if err := json.Unmarshal(text, &in); err != nil {
		return nil, err
	}
	v, err := jsjson.Parse(string(text))
	if err != nil {
		return nil, err
	}
	if obj, ok := v.(jsjson.Object); ok {
		if p, ok := obj.Get("params"); ok {
			if po, ok := p.(jsjson.Object); ok {
				in.Params = po
			}
		}
	}
	return &in, nil
}

type fail struct{ kind, msg string }

func (f *fail) Error() string { return f.msg }

// errOut is a failed result.
func errOut(kind, msg string) jsjson.Object {
	return jsjson.Object{{"ok", false}, {"kind", kind}, {"message", msg}}
}

const maxSafe = 1<<53 - 1

// cell converts one value to its tagged form ([type, value]).
func cell(v sqlitec.Value, col string, in *Input, truncated *int) ([]any, error) {
	switch v.Type {
	case sqlitec.Null:
		return []any{"n", nil}, nil
	case sqlitec.Text:
		u := toUTF16(v.Text)
		if utf8Len(u) <= in.CellLimit {
			return []any{"s", u}, nil
		}
		if !in.TruncateCells {
			return nil, &fail{"limit", fmt.Sprintf("value of column %s exceeds %d bytes", col, in.CellLimit)}
		}
		*truncated++
		cut := u
		if len(cut) > in.CellLimit {
			cut = cut[:in.CellLimit]
		}
		for utf8Len(cut) > in.CellLimit {
			cut = cut[:len(cut)-1]
		}
		return []any{"s", cut}, nil
	case sqlitec.Integer:
		return []any{"i", float64(v.Int)}, nil
	case sqlitec.Float:
		f := v.Float
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, &fail{"type", fmt.Sprintf("column %s: non-finite number", col)}
		}
		if math.Trunc(f) == f {
			if math.Abs(f) > maxSafe {
				return nil, &fail{"type", fmt.Sprintf("column %s: integer outside the safe range", col)}
			}
			return []any{"i", f}, nil
		}
		return []any{"f", f}, nil
	}
	return nil, &fail{"type", fmt.Sprintf("column %s: BLOB is not supported", col)}
}

// toUTF16 decodes UTF-8 into JavaScript string code units.
func toUTF16(s string) jsjson.UTF16 {
	return jsjson.UTF16(utf16.Encode([]rune(s)))
}

// utf8Len is the length TextEncoder gives (lone surrogates become U+FFFD, 3 bytes).
func utf8Len(u jsjson.UTF16) int {
	n := 0
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		case c < 0x80:
			n++
		case c < 0x800:
			n += 2
		case c >= 0xd800 && c <= 0xdbff && i+1 < len(u) && u[i+1] >= 0xdc00 && u[i+1] <= 0xdfff:
			n += 4
			i++
		default:
			n += 3
		}
	}
	return n
}

// Run executes one query and returns the worker output object.
func Run(in *Input) jsjson.Object {
	t0 := time.Now()
	db, err := sqlitec.Open(in.Path, true)
	if err != nil {
		return errOut("sqlite", err.Error())
	}
	defer db.Close()
	seen := NewSeen()
	blocked := Blocked{Columns: set(in.BlockedColumns), Tables: set(in.BlockedTables)}
	out, err := run(db, in, seen, blocked, t0)
	if err == nil {
		return out
	}
	if f, ok := err.(*fail); ok {
		return errOut(f.kind, f.msg)
	}
	if strings.Contains(strings.ToLower(err.Error()), "too large to be represented") {
		return errOut("type", "integer outside the safe range")
	}
	if len(seen.Sensitive) > 0 {
		return errOut("sensitive", SensitiveMessage)
	}
	if len(seen.Denied) > 0 {
		return errOut("sqlite", "tables not readable: "+strings.Join(sorted(seen.Denied), ", "))
	}
	return errOut("sqlite", err.Error())
}

func run(db *sqlitec.DB, in *Input, seen *Seen, blocked Blocked, t0 time.Time) (jsjson.Object, error) {
	if err := db.Exec("PRAGMA hard_heap_limit = " + jsjson.Number(math.Floor(in.HeapLimitMb*1024*1024))); err != nil {
		return nil, err
	}
	// A WITH name that matches a real schema object or a blocked table gets no exception
	names, err := db.Query("SELECT lower(name) AS n FROM sqlite_schema")
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, r := range names {
		taken[r[0].Text] = true
	}
	for t := range blocked.Tables {
		taken[strings.ToLower(t)] = true
	}
	cteOnly := map[string]bool{}
	for _, n := range sqllint.CTENames(in.SQL) {
		if !taken[n] {
			cteOnly[n] = true
		}
	}
	db.SetAuthorizer(Authorizer(in.ReadablePrefixes, seen, blocked, cteOnly))
	st, err := db.Prepare(in.SQL)
	if err != nil {
		return nil, err
	}
	defer st.Finalize()
	// Do not run if any read was denied
	if len(seen.Sensitive) > 0 {
		return errOut("sensitive", SensitiveMessage), nil
	}
	if len(seen.Denied) > 0 {
		return errOut("sqlite", "tables not readable: "+strings.Join(sorted(seen.Denied), ", ")), nil
	}
	cols := st.Columns()
	columns := make([]any, len(cols))
	for i, c := range cols {
		columns[i] = jsjson.Object{{"name", c.Name}, {"table", nullable(c.Table)}, {"column", nullable(c.Origin)}}
	}
	tables := sorted(seen.Reads)
	header := jsjson.MustStringify(jsjson.Object{{"ok", true}, {"columns", columns}, {"rows", []any{}}, {"more", false}, {"truncatedCells", 0}, {"ms", 0}, {"tables", tables}})
	size := len(header) + 64
	more := false
	truncated := 0
	rows := []any{}
	if len(in.Params) > 0 {
		params := map[string]any{}
		order := make([]string, 0, len(in.Params))
		for _, m := range in.Params {
			params[m.Key] = m.Value
			order = append(order, m.Key)
		}
		if err := st.BindNamed(params, order); err != nil {
			return nil, err
		}
	}
	for {
		ok, err := st.Step()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		vals := make([]sqlitec.Value, len(cols))
		for i := range vals {
			vals[i] = st.Value(i)
			if vals[i].Type == sqlitec.Integer && (vals[i].Int > maxSafe || vals[i].Int < -maxSafe) {
				return nil, &fail{"type", "integer outside the safe range"}
			}
		}
		if len(rows) == in.MaxRows {
			if in.Overflow == "error" {
				return nil, &fail{"limit", fmt.Sprintf("result exceeds %d rows", in.MaxRows)}
			}
			more = true
			break
		}
		tagged := make([]any, len(vals))
		for i, v := range vals {
			c, err := cell(v, cols[i].Name, in, &truncated)
			if err != nil {
				return nil, err
			}
			tagged[i] = c
		}
		size += len(jsjson.MustStringify(tagged)) + 1
		if size > in.OutputLimit {
			return nil, &fail{"limit", fmt.Sprintf("result exceeds %d bytes", in.OutputLimit)}
		}
		rows = append(rows, tagged)
	}
	ms := math.Floor(float64(time.Since(t0).Microseconds())/1000 + 0.5)
	return jsjson.Object{{"ok", true}, {"columns", columns}, {"rows", rows}, {"more", more}, {"truncatedCells", truncated}, {"ms", ms}, {"tables", tables}}, nil
}

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func set(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
