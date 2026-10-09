// Package verify cross-checks the snapshot with the source: each query pair from verify.json runs on the source DB
// and on the current snapshot, and the values are compared.
package verify

import (
	"context"
	"errors"
	"fmt"
	"growth-lab/internal/jsstr"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"growth-lab/internal/civiltime"
	"growth-lab/internal/collect"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/query"
	"growth-lab/internal/snapshot"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/workspace"
)

// Error is a verify setup error.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// ErrCancelled is returned when the run is cancelled.
var ErrCancelled = errors.New("cancelled")

// Check is one query pair.
type Check struct{ ID, Title, SourceSQL, SnapshotSQL string }

// Row is a one-row result as column → text (nil = NULL), in column order.
type Row = jsjson.Object

// Item is one check result.
type Item struct {
	ID, Title        string
	OK               bool
	Source, Snapshot Row
	Err              *string
}

// Report is a verify run.
type Report struct {
	SnapshotID, AsOf, WeekStart, WeekEnd string
	Items                                []*Item
	OK                                   bool
}

// Byte limit for one row (column names + values), column count limit, check count limit.
const (
	rowLimit   = 4096
	maxColumns = 20
	maxChecks  = 20
)

var checkIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// LoadChecks reads verify.json.
func LoadChecks(wsDir string) ([]Check, error) {
	b, err := os.ReadFile(filepath.Join(wsDir, "verify.json"))
	if err != nil {
		return nil, &Error{"verify.json not found"}
	}
	raw, err := jsjson.Parse(string(b))
	if err != nil {
		return nil, &Error{"verify.json: invalid JSON"}
	}
	var checks []any
	if o, ok := raw.(jsjson.Object); ok {
		v, _ := o.Get("checks")
		checks, _ = v.([]any)
	}
	if len(checks) == 0 || len(checks) > maxChecks {
		return nil, &Error{fmt.Sprintf("verify.json: checks must be an array of 1-%d items", maxChecks)}
	}
	ids := map[string]bool{}
	out := make([]Check, len(checks))
	for i, c := range checks {
		o, ok := c.(jsjson.Object)
		if !ok {
			return nil, &Error{fmt.Sprintf("verify.json: checks[%d] must be an object", i)}
		}
		for _, m := range o {
			switch m.Key {
			case "id", "title", "source_sql", "snapshot_sql":
			default:
				return nil, &Error{fmt.Sprintf("verify.json: checks[%d] has unknown key %s", i, m.Key)}
			}
		}
		idv, _ := o.Get("id")
		id, ok := idv.(string)
		if !ok || !checkIDRE.MatchString(id) || ids[id] {
			return nil, &Error{fmt.Sprintf("verify.json: checks[%d].id must be a unique lowercase identifier", i)}
		}
		ids[id] = true
		tv, _ := o.Get("title")
		title, ok := tv.(string)
		if !ok || title == "" || jsstr.U16Len(title) > 80 {
			return nil, &Error{"verify.json: " + id + ".title must be 1-80 characters"}
		}
		var sqls [2]string
		for j, k := range []string{"source_sql", "snapshot_sql"} {
			v, _ := o.Get(k)
			s, ok := v.(string)
			if !ok || jsstr.Trim(s) == "" {
				return nil, &Error{"verify.json: " + id + "." + k + " is empty"}
			}
			sqls[j] = s
		}
		out[i] = Check{id, title, sqls[0], sqls[1]}
	}
	return out, nil
}

var weekRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Week is the given Monday, or the Monday five weeks before the cutoff's week. The week must end before the cutoff.
func Week(asOf string, week *string) (start, end string, err error) {
	if week != nil {
		w := *week
		if !weekRE.MatchString(w) {
			return "", "", &Error{"--week must be a real date YYYY-MM-DD"}
		}
		y, _ := strconv.Atoi(w[:4])
		mo, _ := strconv.Atoi(w[5:7])
		d, _ := strconv.Atoi(w[8:10])
		if mo < 1 || mo > 12 || d < 1 || d > 31 {
			return "", "", errors.New("Invalid time value")
		}
		if time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC).Format("2006-01-02") != w {
			return "", "", &Error{"--week must be a real date YYYY-MM-DD"}
		}
		start = w + " 00:00:00.000000"
		ws, err := civiltime.WeekStart(start)
		if err != nil {
			return "", "", err
		}
		if ws != start {
			return "", "", &Error{"--week must be a Monday"}
		}
	} else {
		ws, err := civiltime.WeekStart(asOf)
		if err != nil {
			return "", "", err
		}
		if start, err = civiltime.AddDays(ws, -35); err != nil {
			return "", "", err
		}
	}
	if end, err = civiltime.AddDays(start, 7); err != nil {
		return "", "", err
	}
	if end > asOf {
		return "", "", &Error{"target week (" + start[:10] + ") does not end before the cutoff"}
	}
	return start, end, nil
}

var decimalRE = regexp.MustCompile(`(?i)^([+-]?)(\d+)(?:\.(\d+))?(?:e([+-]?\d+))?$`)

// decimalKey normalizes a decimal string to sign, digits and exponent; "" when it is not a number.
func decimalKey(v string) string {
	m := decimalRE.FindStringSubmatch(jsstr.Trim(v))
	if m == nil {
		return ""
	}
	frac := m[3]
	digits := strings.TrimLeft(m[2]+frac, "0")
	if digits == "" {
		return "0"
	}
	exp := new(big.Int)
	if m[4] != "" {
		exp.SetString(strings.TrimPrefix(m[4], "+"), 10)
	}
	exp.Sub(exp, big.NewInt(int64(len(frac))))
	trimmed := strings.TrimRight(digits, "0")
	exp.Add(exp, big.NewInt(int64(len(digits)-len(trimmed))))
	sign := ""
	if m[1] == "-" {
		sign = "-"
	}
	return sign + trimmed + "e" + exp.String()
}

// SameValue: both NULL, exact numeric match if both are decimals, otherwise exact text.
func SameValue(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ka, kb := decimalKey(*a), decimalKey(*b)
	if ka != "" && kb != "" {
		return ka == kb
	}
	return *a == *b
}

func text(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func compare(src, snap Row) *string {
	if len(src) != len(snap) || !keysIn(src, snap) {
		s := "column names differ (source " + strings.Join(keys(src), ", ") + " / snapshot " + strings.Join(keys(snap), ", ") + ")"
		return &s
	}
	var diff []string
	for _, m := range src {
		v, _ := snap.Get(m.Key)
		if !SameValue(text(m.Value), text(v)) {
			diff = append(diff, m.Key)
		}
	}
	if len(diff) > 0 {
		s := "different values: " + strings.Join(diff, ", ")
		return &s
	}
	return nil
}

func keys(o Row) []string {
	out := make([]string, len(o))
	for i, m := range o {
		out[i] = m.Key
	}
	return out
}

func keysIn(a, b Row) bool {
	for _, m := range a {
		if _, ok := b.Get(m.Key); !ok {
			return false
		}
	}
	return true
}

// rowProblem checks column count, duplicate names and size of a one-row result.
func rowProblem(columns []string, values []*string) error {
	if len(columns) > maxColumns {
		return fmt.Errorf("more than %d columns", maxColumns)
	}
	seen := map[string]bool{}
	for _, c := range columns {
		if seen[c] {
			return errors.New("duplicate column names")
		}
		seen[c] = true
	}
	n := 0
	for _, c := range columns {
		n += len(c)
	}
	for _, v := range values {
		if v != nil {
			n += len(*v)
		}
	}
	if n > rowLimit {
		return fmt.Errorf("the row exceeds %d bytes", rowLimit)
	}
	return nil
}

// toRow builds the column → value object (later duplicate names win, like Object.fromEntries).
func toRow(columns []string, values []*string) Row {
	o := Row{}
	for i, c := range columns {
		var v any
		if values[i] != nil {
			v = *values[i]
		}
		replaced := false
		for j := range o {
			if o[j].Key == c {
				o[j].Value = v
				replaced = true
			}
		}
		if !replaced {
			o = append(o, jsjson.Member{Key: c, Value: v})
		}
	}
	return jsjson.Ordered(o)
}

func sourceRow(src collect.Source, sql string) (Row, error) {
	var columns []string
	var rows [][]*string
	_, err := src.SelectStream(sql, func(v []collect.RawValue) error {
		if len(rows) >= 1 {
			return errors.New("more than one row")
		}
		if err := rowProblem(columns, v); err != nil {
			return err
		}
		rows = append(rows, v)
		return nil
	}, func(c []string) error {
		if len(c) > maxColumns {
			return fmt.Errorf("more than %d columns", maxColumns)
		}
		columns = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, errors.New("the result must be exactly one row")
	}
	return toRow(columns, rows[0]), nil
}

// Options: Week picks the target week; Source replaces the configured source; Now names the log file (tests).
type Options struct {
	Week     *string
	Source   collect.Source
	OnSource func(collect.Source)
	Ctx      context.Context
	Now      func() time.Time
}

// Run runs every check and writes the report to logs/verify-<time>.json.
func Run(ws *workspace.Workspace, o Options) (*Report, error) {
	ctx := o.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	checks, err := LoadChecks(ws.Dir)
	if err != nil {
		return nil, err
	}
	cur, err := snapshot.ReadCurrent(snapshot.Dir(ws.Config.OutDir))
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, &Error{"no snapshot: run collect first"}
	}
	if _, err := os.Stat(cur.File); err != nil {
		return nil, &Error{"no snapshot: run collect first"}
	}
	db, err := sqlitec.Open(cur.File, true)
	if err != nil {
		return nil, err
	}
	metaRows, _, err := db.QueryJS("SELECT snapshot_id, source_cutoff_at FROM snapshot_meta")
	db.Close()
	if err != nil {
		return nil, err
	}
	snapshotID, _ := metaRows[0][0].(string)
	asOf, _ := metaRows[0][1].(string)
	start, end, err := Week(asOf, o.Week)
	if err != nil {
		return nil, err
	}
	values := map[string]string{"week_start": start, "week_end": end, "as_of": asOf}

	inputs, err := snapshot.LoadBuildInputs(ws)
	if err != nil {
		return nil, err
	}
	roles, err := collect.AllRoles(inputs.Specs, inputs.DerivedRoles)
	if err != nil {
		return nil, err
	}
	blocked := query.BlockedList{Columns: []string{}, Tables: []string{}}
	for _, k := range roles.Keys() {
		if r, _ := roles.Get(k); r == collect.Private {
			blocked.Columns = append(blocked.Columns, k)
		}
	}
	cfg := ws.Config
	slots := query.NewExecutionSlots(1, 1)
	src := o.Source
	if src == nil {
		if src, err = snapshot.MakeSource(ws); err != nil {
			return nil, err
		}
	}
	if o.OnSource != nil {
		o.OnSource(src)
	}
	prefixes := append([]string(nil), cfg.ReadablePrefixes...)
	for _, p := range []string{"r_", "snapshot_"} {
		if !contains(prefixes, p) {
			prefixes = append(prefixes, p)
		}
	}
	params := append(jsjson.Object(nil), cfg.Params...)
	params = setParam(setParam(params, "week_start", start), "week_end", end)

	var items []*Item
	for _, c := range checks {
		if ctx.Err() != nil {
			return nil, ErrCancelled
		}
		item := &Item{ID: c.ID, Title: c.Title}
		items = append(items, item)
		setErr := func(s string) { item.Err = &s }
		bound, err := BindSourceSQL(c.SourceSQL, values)
		if err != nil {
			setErr("source SQL: " + err.Error())
			continue
		}
		if item.Source, err = sourceRow(src, bound); err != nil {
			if ctx.Err() != nil {
				return nil, ErrCancelled
			}
			setErr("source: " + err.Error())
			continue
		}
		var r query.Result
		if err := slots.Run(ctx, query.Interactive, func(l *query.SlotLease) error {
			r = query.RunQuery(query.Request{Lease: l, SQL: c.SnapshotSQL, Path: cur.File, Mode: query.Panel, AsOf: asOf, Params: params, ReadablePrefixes: prefixes, Blocked: &blocked, HeapLimitMb: float64(cfg.HeapLimitMb), Ctx: ctx})
			return nil
		}); err != nil {
			return nil, ErrCancelled
		}
		if !r.OK && (r.Kind == "cancelled" || ctx.Err() != nil) {
			return nil, ErrCancelled
		}
		if !r.OK {
			setErr("snapshot: " + r.Message)
			continue
		}
		if len(r.Rows) != 1 {
			setErr("snapshot: the result must be exactly one row")
			continue
		}
		names := make([]string, len(r.Columns))
		for i, x := range r.Columns {
			names[i] = x.Name
		}
		vals := make([]*string, len(r.Rows[0]))
		for i, v := range r.Rows[0] {
			switch x := v.Value.(type) {
			case string:
				vals[i] = &x
			case float64:
				s := jsjson.Number(x)
				vals[i] = &s
			}
		}
		if err := rowProblem(names, vals); err != nil {
			setErr("snapshot: " + err.Error())
			continue
		}
		item.Snapshot = toRow(names, vals)
		item.Err = compare(item.Source, item.Snapshot)
		item.OK = item.Err == nil
	}
	rep := &Report{SnapshotID: snapshotID, AsOf: asOf, WeekStart: start, WeekEnd: end, Items: items, OK: true}
	for _, it := range items {
		if !it.OK {
			rep.OK = false
		}
	}
	logDir := filepath.Join(cfg.OutDir, "logs")
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return nil, err
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(now().UTC().Format("2006-01-02T15:04:05.000Z"))
	if err := os.WriteFile(filepath.Join(logDir, "verify-"+stamp+".json"), []byte(ReportText(rep)), 0o600); err != nil {
		return nil, err
	}
	return rep, nil
}

func setParam(o jsjson.Object, k string, v any) jsjson.Object {
	for i := range o {
		if o[i].Key == k {
			o[i].Value = v
			return o
		}
	}
	return append(o, jsjson.Member{Key: k, Value: v})
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// JS returns the report in its stored JSON form.
func (r *Report) JS() jsjson.Object {
	items := make([]any, len(r.Items))
	for i, it := range r.Items {
		var src, snap, e any
		if it.Source != nil {
			src = it.Source
		}
		if it.Snapshot != nil {
			snap = it.Snapshot
		}
		if it.Err != nil {
			e = *it.Err
		}
		items[i] = jsjson.Object{{Key: "id", Value: it.ID}, {Key: "title", Value: it.Title}, {Key: "ok", Value: it.OK}, {Key: "source", Value: src}, {Key: "snapshot", Value: snap}, {Key: "error", Value: e}}
	}
	return jsjson.Object{{Key: "snapshot_id", Value: r.SnapshotID}, {Key: "as_of", Value: r.AsOf}, {Key: "week_start", Value: r.WeekStart}, {Key: "week_end", Value: r.WeekEnd}, {Key: "items", Value: items}, {Key: "ok", Value: r.OK}}
}

// ReportText is the log file content: JSON.stringify(report, null, 2) + "\n".
func ReportText(r *Report) string { return stringifyIndent(r.JS(), "  ", "") + "\n" }

func stringifyIndent(v any, step, cur string) string {
	switch x := v.(type) {
	case jsjson.Object:
		if len(x) == 0 {
			return "{}"
		}
		next := cur + step
		parts := make([]string, len(x))
		for i, m := range x {
			parts[i] = next + jsjson.QuoteString(m.Key) + ": " + stringifyIndent(m.Value, step, next)
		}
		return "{\n" + strings.Join(parts, ",\n") + "\n" + cur + "}"
	case []any:
		if len(x) == 0 {
			return "[]"
		}
		next := cur + step
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = next + stringifyIndent(e, step, next)
		}
		return "[\n" + strings.Join(parts, ",\n") + "\n" + cur + "]"
	}
	return jsjson.MustStringify(v)
}
