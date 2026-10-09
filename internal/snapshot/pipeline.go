package snapshot

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"syscall"
	"time"

	"growth-lab/internal/collect"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/workspace"
)

// PipelineError is a collect / derive error.
type PipelineError struct{ Msg string }

func (e *PipelineError) Error() string { return e.Msg }

// GA4 connects the GA4 import (nil until wired in).
var GA4 interface {
	Inputs(ws *workspace.Workspace) (*GA4Inputs, error)
	Import(db *sqlitec.DB, ws *workspace.Workspace, log func(string)) error
}

func ga4Inputs(ws *workspace.Workspace) (*GA4Inputs, error) {
	if ws.Config.GA4 == nil {
		return nil, nil
	}
	if GA4 == nil {
		return nil, &PipelineError{"GA4 is not supported by the Go engine yet"}
	}
	return GA4.Inputs(ws)
}

// LoadBuildInputs reads the workspace definitions.
func LoadBuildInputs(ws *workspace.Workspace) (BuildInputs, error) {
	g, err := ga4Inputs(ws)
	if err != nil {
		return BuildInputs{}, err
	}
	derivedFile := filepath.Join(ws.Dir, "derived.sql")
	derived, err := os.ReadFile(derivedFile)
	if err != nil {
		return BuildInputs{}, &PipelineError{"derived SQL not found: " + derivedFile}
	}
	specs, err := collect.LoadSpec(filepath.Join(ws.Dir, "tables.json"))
	if err != nil {
		return BuildInputs{}, err
	}
	roles, err := collect.LoadDerivedRoles(filepath.Join(ws.Dir, "derived-columns.json"))
	if err != nil {
		return BuildInputs{}, err
	}
	return BuildInputs{Specs: specs, DerivedSQL: string(derived), DerivedRoles: roles, Params: ws.Config.Params, GA4: g}, nil
}

// MakeSource opens the configured source.
func MakeSource(ws *workspace.Workspace) (collect.Source, error) {
	s := ws.Config.Datasource
	var src collect.Source
	switch s.Kind {
	case "mysql":
		src = &collect.MysqlSource{DS: collect.ServerDatasource{Kind: s.Kind, Host: s.Host, Port: s.Port, User: s.User, Password: s.Password, Database: s.Database}}
	case "postgres":
		src = &collect.PostgresSource{DS: collect.ServerDatasource{Kind: s.Kind, Host: s.Host, Port: s.Port, User: s.User, Password: s.Password, Database: s.Database}}
	default:
		if _, err := os.Stat(s.Path); err != nil {
			return nil, &PipelineError{"source SQLite file not found: " + s.Path}
		}
		src = &collect.SqliteSource{Path: s.Path}
	}
	if now := os.Getenv("GROWTH_LAB_TEST_SOURCE_NOW"); now != "" {
		return fixedNow{src, now}, nil
	}
	return src, nil
}

// fixedNow replaces the source clock (GROWTH_LAB_TEST_SOURCE_NOW, for tests).
type fixedNow struct {
	collect.Source
	now string
}

func (f fixedNow) Now() (string, error) { return f.now, nil }

var tmpRE = regexp.MustCompile(`^tmp-(\d+)-`)

// removeStaleTemps deletes temporary files left by processes that have exited.
func removeStaleTemps(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		m := tmpRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		if syscall.Kill(pid, 0) != nil {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func prepareOut(ws *workspace.Workspace) (string, error) {
	if err := workspace.AssertOutputsIgnored(workspace.GuardedPaths(ws), ws.Dir); err != nil {
		return "", err
	}
	dir := Dir(ws.Config.OutDir)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	removeStaleTemps(dir)
	return dir, nil
}

// RunCollect holds the lock throughout: DB collect → GA4 import → finalize. On failure only the temporary files are deleted.
func RunCollect(ws *workspace.Workspace, log func(string), onSource func(collect.Source)) (Result, error) {
	inputs, err := LoadBuildInputs(ws)
	if err != nil {
		return Result{}, err
	}
	dir, err := prepareOut(ws)
	if err != nil {
		return Result{}, err
	}
	release, err := AcquireLock(dir)
	if err != nil {
		return Result{}, err
	}
	defer release()
	source, err := MakeSource(ws)
	if err != nil {
		return Result{}, err
	}
	if onSource != nil {
		onSource(source)
	}
	c, err := collect.Collect(inputs.Specs, source, dir, log)
	if err != nil {
		return Result{}, err
	}
	if inputs.GA4 != nil {
		db, err := sqlitec.Open(c.TmpPath, false)
		if err == nil {
			err = GA4.Import(db, ws, log)
			db.Close()
		}
		if err != nil {
			os.Remove(c.TmpPath)
			os.Remove(c.TmpPath + "-journal")
			return Result{}, err
		}
	}
	return Finalize(FinalizeOptions{TmpPath: c.TmpPath, SnapshotsDir: dir, Inputs: inputs, Meta: Meta{c.Cutoff, c.StartedAt, c.FinishedAt}, ApplyCutoffFirst: true, LockHeld: true})
}

// RunDerive rebuilds derived tables and the pseudonymized copy from the current snapshot's raw tables (the source is not read).
func RunDerive(ws *workspace.Workspace) (Result, error) {
	inputs, err := LoadBuildInputs(ws)
	if err != nil {
		return Result{}, err
	}
	dir, err := prepareOut(ws)
	if err != nil {
		return Result{}, err
	}
	release, err := AcquireLock(dir)
	if err != nil {
		return Result{}, err
	}
	defer release()
	cur, err := ReadCurrent(dir)
	if err != nil {
		return Result{}, err
	}
	if cur == nil {
		return Result{}, &PipelineError{"no current snapshot: run collect first"}
	}
	tmpPath := filepath.Join(dir, fmt.Sprintf("tmp-%d-%d.sqlite", os.Getpid(), time.Now().UnixMilli()))
	if err := copyFile(cur.File, tmpPath); err != nil {
		return Result{}, err
	}
	meta, err := func() (Meta, error) {
		db, err := sqlitec.Open(tmpPath, false)
		if err != nil {
			return Meta{}, err
		}
		defer db.Close()
		rows, _, err := db.QueryJS("SELECT source_cutoff_at, collection_started_at, collection_finished_at, spec_hash, ga4_spec_hash FROM snapshot_meta")
		if err != nil {
			return Meta{}, err
		}
		now, err := HashInputs(inputs)
		if err != nil {
			return Meta{}, err
		}
		m := rows[0]
		if m[3] != now.Spec {
			return Meta{}, &PipelineError{"tables.json changed beyond roles: run collect, not derive"}
		}
		if m[4] != now.GA4Spec {
			return Meta{}, &PipelineError{"GA4 settings (workspace.json ga4, ga4-reports.json) changed: run collect, not derive"}
		}
		names, err := db.Query(`SELECT name FROM sqlite_master WHERE type IN ('table', 'view') AND name LIKE 'd\_%' ESCAPE '\'`)
		if err != nil {
			return Meta{}, err
		}
		for _, n := range names {
			if err := db.Exec(`DROP TABLE IF EXISTS "` + n[0].Text + `"`); err != nil {
				return Meta{}, err
			}
		}
		return Meta{m[0].(string), m[1].(string), m[2].(string)}, nil
	}()
	if err != nil {
		os.Remove(tmpPath)
		return Result{}, err
	}
	return Finalize(FinalizeOptions{TmpPath: tmpPath, SnapshotsDir: dir, Inputs: inputs, Meta: meta, LockHeld: true})
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// CaseResult is one derived-rule fixture result.
type CaseResult struct {
	Name    string
	OK      bool
	Message string
}

// RunTestDerived loads tests/<case>/input.sql, runs derived.sql and compares with expected.json.
func RunTestDerived(ws *workspace.Workspace) ([]CaseResult, error) {
	testsDir := filepath.Join(ws.Dir, "tests")
	if _, err := os.Stat(testsDir); err != nil {
		return nil, &PipelineError{"fixtures folder not found: " + testsDir}
	}
	specs, err := collect.LoadSpec(filepath.Join(ws.Dir, "tables.json"))
	if err != nil {
		return nil, err
	}
	derived, err := os.ReadFile(filepath.Join(ws.Dir, "derived.sql"))
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(testsDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]CaseResult, len(names))
	for i, n := range names {
		out[i] = runCase(n, filepath.Join(testsDir, n), specs, string(derived))
	}
	return out, nil
}

func runCase(name, dir string, specs []collect.TableSpec, derivedSQL string) CaseResult {
	fail := func(msg string) CaseResult { return CaseResult{Name: name, Message: msg} }
	db, err := sqlitec.Open(":memory:", false)
	if err != nil {
		return fail(err.Error())
	}
	defer db.Close()
	if err := db.Exec(collect.EngineTablesSQL); err != nil {
		return fail(err.Error())
	}
	for _, t := range specs {
		if err := db.Exec(collect.RawDDL(t)); err != nil {
			return fail(err.Error())
		}
	}
	input, err := os.ReadFile(filepath.Join(dir, "input.sql"))
	if err != nil {
		return fail(err.Error())
	}
	if err := db.Exec(string(input)); err != nil {
		return fail(err.Error())
	}
	if err := ExecDerivedSQL(db, derivedSQL, nil); err != nil {
		return fail(err.Error())
	}
	params := jsjson.Object{}
	prow, _, err := db.QueryJS("SELECT key, value FROM snapshot_params")
	if err != nil {
		return fail(err.Error())
	}
	for _, r := range prow {
		k := r[0].(string)
		replaced := false
		for i := range params {
			if params[i].Key == k {
				params[i].Value = r[1]
				replaced = true
			}
		}
		if !replaced {
			params = append(params, jsjson.Member{Key: k, Value: r[1]})
		}
	}
	asRows, _, err := db.QueryJS("SELECT source_cutoff_at a FROM snapshot_meta")
	if err != nil {
		return fail(err.Error())
	}
	asOf, _ := func() (string, bool) {
		if len(asRows) == 0 {
			return "", false
		}
		s, ok := asRows[0][0].(string)
		return s, ok
	}()
	if asOf == "" {
		return fail("input.sql has no snapshot_meta row")
	}
	if err := BuildCalendar(db, params, asOf); err != nil {
		return fail(err.Error())
	}
	expectedText, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		return fail(err.Error())
	}
	var expected []struct {
		Query string          `json:"query"`
		Rows  json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(expectedText, &expected); err != nil {
		return fail(err.Error())
	}
	for i, e := range expected {
		rows, names, err := db.QueryJS(e.Query)
		if err != nil {
			return fail(err.Error())
		}
		actual := make([]any, len(rows))
		for r, row := range rows {
			o := jsjson.Object{}
			for c, n := range names {
				o = append(o, jsjson.Member{Key: n, Value: row[c]})
			}
			actual[r] = o
		}
		want, err := jsjson.Parse(string(e.Rows))
		if err != nil {
			return fail(err.Error())
		}
		if !deepStrictEqual(actual, want) {
			return fail(fmt.Sprintf("query %d mismatch\n  query:    %s\n  expected: %s\n  actual:   %s", i+1, e.Query, jsjson.MustStringify(want), jsjson.MustStringify(actual)))
		}
	}
	return CaseResult{Name: name, OK: true}
}

// deepStrictEqual compares like util.isDeepStrictEqual: object key order does not matter, numbers compare with Object.is.
func deepStrictEqual(a, b any) bool {
	switch x := a.(type) {
	case jsjson.Object:
		y, ok := b.(jsjson.Object)
		if !ok || len(x) != len(y) {
			return false
		}
		for _, m := range x {
			v, ok := y.Get(m.Key)
			if !ok || !deepStrictEqual(m.Value, v) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !deepStrictEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case float64:
		y, ok := b.(float64)
		return ok && (x == y && math.Signbit(x) == math.Signbit(y) || math.IsNaN(x) && math.IsNaN(y))
	}
	return reflect.DeepEqual(a, b)
}
