package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"growth-lab/internal/civiltime"
	"growth-lab/internal/collect"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/sensitive"
	"growth-lab/internal/sqlitec"
)

// FinalizeError is a snapshot finalize error.
type FinalizeError struct{ Msg string }

func (e *FinalizeError) Error() string { return e.Msg }

// GA4Inputs are the GA4 config hash, the column roles of the GA4 tables and the planned table names.
type GA4Inputs struct {
	SpecHash string
	Roles    *collect.Roles
	Tables   []string
}

// BuildInputs are the workspace definitions a snapshot is built from.
type BuildInputs struct {
	Specs        []collect.TableSpec
	DerivedSQL   string
	DerivedRoles *collect.Roles
	Params       jsjson.Object
	GA4          *GA4Inputs
}

// Meta are the snapshot times.
type Meta struct{ Cutoff, StartedAt, FinishedAt string }

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ApplyCutoff deletes rows after the cutoff; nullAfterCutoff columns become NULL.
func ApplyCutoff(db *sqlitec.DB, specs []collect.TableSpec, cutoff string) error {
	for _, t := range specs {
		dropped, err := db.RunOnce("DELETE FROM "+t.Target+" WHERE "+t.CutoffColumn+" > ?", cutoff)
		if err != nil {
			return err
		}
		nulled := 0
		for _, c := range t.Columns {
			if !c.NullAfterCutoff {
				continue
			}
			n, err := db.RunOnce("UPDATE "+t.Target+" SET "+c.As+" = NULL WHERE "+c.As+" > ?", cutoff)
			if err != nil {
				return err
			}
			nulled += n
		}
		if _, err := db.RunOnce("UPDATE r_collect_log SET dropped_after_cutoff = ?, nulled_after_cutoff = ? WHERE table_name = ?", float64(dropped), float64(nulled), t.Target); err != nil {
			return err
		}
	}
	return nil
}

func validate(db *sqlitec.DB, specs []collect.TableSpec) error {
	for _, t := range specs {
		cols, err := tableInfo(db, t.Target)
		if err != nil {
			return err
		}
		have := make([]string, len(cols))
		for i, c := range cols {
			have[i] = c.name
		}
		want := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			want[i] = c.As
		}
		if strings.Join(have, ",") != strings.Join(want, ",") {
			return &FinalizeError{t.Target + ": columns differ from the spec"}
		}
		n, err := db.Query("SELECT count(*) n FROM " + t.Target)
		if err != nil {
			return err
		}
		if n[0][0].Int == 0 {
			return &FinalizeError{t.Target + ": no rows"}
		}
		// fail if a public text column holds a secret-looking value (the value is not logged)
		for _, c := range t.Columns {
			if c.Kind != "text" || c.Role == collect.Private {
				continue
			}
			st, err := db.Prepare("SELECT " + c.As + " AS v FROM " + t.Target + " WHERE " + c.As + " IS NOT NULL")
			if err != nil {
				return err
			}
			for {
				ok, err := st.Step()
				if err != nil {
					st.Finalize()
					return err
				}
				if !ok {
					break
				}
				v := st.Value(0)
				if v.Type != sqlitec.Text {
					continue
				}
				if kind := sensitive.SecretShape(v.Text); kind != "" {
					st.Finalize()
					return &FinalizeError{fmt.Sprintf("%s.%s: has a secret-looking value (%s), not collected. Declare it private or check the source", t.Target, c.As, kind)}
				}
			}
			st.Finalize()
		}
	}
	return nil
}

// hashRows adds JSON.stringify(row) + "\n" per row of sql to h.
func hashRows(db *sqlitec.DB, sql string, add func(string)) error {
	st, err := db.Prepare(sql)
	if err != nil {
		return err
	}
	defer st.Finalize()
	n := len(st.Columns())
	for {
		ok, err := st.Step()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		row := make([]any, n)
		for i := range row {
			if row[i], err = st.Value(i).JS(); err != nil {
				return err
			}
		}
		add(jsjson.MustStringify(row) + "\n")
	}
}

// RawHash hashes the r_* content (and the GA4 tables' schema and content).
func RawHash(db *sqlitec.DB, specs []collect.TableSpec, ga4Tables []string) (string, error) {
	var parts []string
	sorted := append([]collect.TableSpec(nil), specs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Target < sorted[j].Target })
	for _, t := range sorted {
		h := sha256.New()
		h.Write([]byte(t.Target + "\n"))
		cols := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			cols[i] = c.As
		}
		if err := hashRows(db, "SELECT "+strings.Join(cols, ", ")+" FROM "+t.Target+" ORDER BY "+strings.Join(t.Key, ", "), func(s string) { h.Write([]byte(s)) }); err != nil {
			return "", err
		}
		parts = append(parts, hex.EncodeToString(h.Sum(nil)))
	}
	// GA4 tables: the actual set must match the plan
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'r\_ga4\_%' ESCAPE '\' AND name <> 'r_ga4_collect_log' ORDER BY name`)
	if err != nil {
		return "", err
	}
	actual := make([]string, len(rows))
	for i, r := range rows {
		actual[i] = r[0].Text
	}
	planned := append([]string(nil), ga4Tables...)
	sort.Strings(planned)
	if jsjson.MustStringify(actual) != jsjson.MustStringify(planned) {
		a, p := strings.Join(actual, ", "), strings.Join(planned, ", ")
		if a == "" {
			a = "none"
		}
		if p == "" {
			p = "none"
		}
		return "", &FinalizeError{"GA4 tables differ from the plan (actual " + a + " / planned " + p + ")"}
	}
	for _, t := range planned {
		info, err := db.Query("PRAGMA table_info(" + t + ")")
		if err != nil {
			return "", err
		}
		desc := make([]any, len(info))
		names := make([]string, len(info))
		for i, c := range info {
			desc[i] = []any{c[1].Text, c[2].Text, float64(c[3].Int), float64(c[5].Int)}
			names[i] = c[1].Text
		}
		h := sha256.New()
		h.Write([]byte(t + "\n" + jsjson.MustStringify(desc) + "\n"))
		if err := hashRows(db, "SELECT * FROM "+t+" ORDER BY "+strings.Join(names, ", "), func(s string) { h.Write([]byte(s)) }); err != nil {
			return "", err
		}
		parts = append(parts, hex.EncodeToString(h.Sum(nil)))
	}
	return sha(strings.Join(parts, "\n")), nil
}

// BuildCalendar creates d_calendar_week: weeks from the calendar_start week up to the cutoff.
func BuildCalendar(db *sqlitec.DB, params jsjson.Object, asOf string) error {
	if err := db.Exec("CREATE TABLE d_calendar_week (week_start TEXT PRIMARY KEY, week_end TEXT NOT NULL)"); err != nil {
		return err
	}
	v, _ := params.Get("calendar_start")
	start, ok := v.(string)
	if !ok {
		return nil
	}
	ns, err := civiltime.Normalize(start)
	if err != nil {
		return err
	}
	w, err := civiltime.WeekStart(ns)
	if err != nil {
		return err
	}
	ins, err := db.Prepare("INSERT INTO d_calendar_week VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer ins.Finalize()
	for w < asOf {
		next, err := civiltime.AddDays(w, 7)
		if err != nil {
			return err
		}
		if _, err := ins.Run(w, next); err != nil {
			return err
		}
		w = next
	}
	return nil
}

// IndexDerivedIdentifiers indexes identifier columns of d_* tables unless an index already starts with that column.
func IndexDerivedIdentifiers(db *sqlitec.DB, roles *collect.Roles) error {
	for _, key := range roles.Keys() {
		role, _ := roles.Get(key)
		if !role.IsIdentifier() || !strings.HasPrefix(key, "d_") {
			continue
		}
		parts := strings.Split(key, ".")
		table, column := parts[0], parts[1]
		list, err := db.Query(`PRAGMA index_list("` + table + `")`)
		if err != nil {
			return err
		}
		leads := false
		for _, ix := range list {
			info, err := db.Query(`PRAGMA index_info("` + ix[1].Text + `")`)
			if err != nil {
				return err
			}
			for _, c := range info {
				if c[0].Int == 0 {
					if c[2].Type == sqlitec.Text && c[2].Text == column {
						leads = true
					}
					break
				}
			}
		}
		if !leads {
			if err := db.Exec(`CREATE INDEX "ix_` + table + `_` + column + `" ON "` + table + `"("` + column + `")`); err != nil {
				return err
			}
		}
	}
	return nil
}

// InputHashes are the hashes of the workspace definitions (stored in snapshot_meta).
type InputHashes struct{ Derived, Roles, Params, Spec, GA4Spec string }

func allRoles(inp BuildInputs) (*collect.Roles, error) {
	roles, err := collect.AllRoles(inp.Specs, inp.DerivedRoles)
	if err != nil {
		return nil, err
	}
	if inp.GA4 != nil {
		for _, k := range inp.GA4.Roles.Keys() {
			v, _ := inp.GA4.Roles.Get(k)
			roles.Set(k, v)
		}
	}
	return roles, nil
}

// HashInputs computes the definition hashes.
func HashInputs(inp BuildInputs) (InputHashes, error) {
	roles, err := allRoles(inp)
	if err != nil {
		return InputHashes{}, err
	}
	h := InputHashes{Derived: sha(inp.DerivedSQL), Roles: collect.RolesHash(roles), Params: collect.ParamsHash(inp.Params), Spec: collect.SpecHash(inp.Specs)}
	if inp.GA4 != nil {
		h.GA4Spec = inp.GA4.SpecHash
	}
	return h, nil
}

func build(db *sqlitec.DB, inp BuildInputs, meta Meta) (string, *collect.Roles, error) {
	roles, err := allRoles(inp)
	if err != nil {
		return "", nil, err
	}
	defs, err := HashInputs(inp)
	if err != nil {
		return "", nil, err
	}
	var ga4Tables []string
	if inp.GA4 != nil {
		ga4Tables = inp.GA4.Tables
	}
	raw, err := RawHash(db, inp.Specs, ga4Tables)
	if err != nil {
		return "", nil, err
	}
	if err := db.Exec("DELETE FROM snapshot_meta; DELETE FROM snapshot_params"); err != nil {
		return "", nil, err
	}
	if _, err := db.RunOnce(`INSERT INTO snapshot_meta (source_cutoff_at, collection_started_at, collection_finished_at, raw_hash, derived_hash, roles_hash, params_hash, spec_hash, ga4_spec_hash)
              VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, meta.Cutoff, meta.StartedAt, meta.FinishedAt, raw, defs.Derived, defs.Roles, defs.Params, defs.Spec, defs.GA4Spec); err != nil {
		return "", nil, err
	}
	insParam, err := db.Prepare("INSERT INTO snapshot_params VALUES (?, ?)")
	if err != nil {
		return "", nil, err
	}
	for _, m := range inp.Params {
		vals := []any{m.Value}
		if arr, ok := m.Value.([]string); ok {
			vals = vals[:0]
			for _, x := range arr {
				vals = append(vals, x)
			}
		}
		for _, x := range vals {
			s, ok := x.(string)
			if !ok {
				s = jsjson.Number(x.(float64))
			}
			if _, err := insParam.Run(m.Key, s); err != nil {
				insParam.Finalize()
				return "", nil, err
			}
		}
	}
	insParam.Finalize()

	if err := ExecDerivedSQL(db, inp.DerivedSQL, roles); err != nil {
		return "", nil, &FinalizeError{"derived.sql failed: " + err.Error()}
	}
	if err := BuildCalendar(db, inp.Params, meta.Cutoff); err != nil {
		return "", nil, err
	}
	for _, t := range inp.Specs {
		for _, c := range t.Columns {
			if c.Role.IsIdentifier() && c.As != t.Key[0] {
				if err := db.Exec("CREATE INDEX IF NOT EXISTS ix_" + t.Target + "_" + c.As + " ON " + t.Target + "(" + c.As + ")"); err != nil {
					return "", nil, err
				}
			}
		}
	}
	if err := IndexDerivedIdentifiers(db, roles); err != nil {
		return "", nil, err
	}
	last := defs.Params
	if defs.GA4Spec != "" {
		last = sha(defs.Params + "\n" + defs.GA4Spec)
	}
	id := strings.ReplaceAll(meta.Cutoff[:10], "-", "") + "-" + strings.ReplaceAll(meta.Cutoff[11:19], ":", "") + "-" + raw[:8] + "-" + defs.Derived[:8] + "-" + defs.Roles[:8] + "-" + last[:8]
	if _, err := db.RunOnce("UPDATE snapshot_meta SET snapshot_id = ?", id); err != nil {
		return "", nil, err
	}
	return id, roles, nil
}

// AcquireLock takes the snapshots/ lock and returns its release function. A leftover lock is reported, not removed.
func AcquireLock(dir string) (func(), error) {
	lock := filepath.Join(dir, ".lock")
	var f *os.File
	for attempt := 0; f == nil; attempt++ {
		var err error
		f, err = os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		text, rerr := os.ReadFile(lock)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) && attempt < 3 {
				continue
			}
			return nil, rerr
		}
		t := strings.TrimSpace(string(text))
		owner, perr := strconv.Atoi(t)
		alive := perr == nil && owner > 0 && syscall.Kill(owner, 0) == nil
		if alive {
			return nil, &FinalizeError{fmt.Sprintf("another collect or derive is running (pid %d). Run again after it finishes", owner)}
		}
		if t == "" {
			t = "?"
		}
		return nil, &FinalizeError{"stale lock left behind (pid " + t + " is gone). Make sure nothing else is running, delete " + lock + " and run again"}
	}
	_, werr := f.WriteString(strconv.Itoa(os.Getpid()))
	f.Close()
	if werr != nil {
		return nil, werr
	}
	return func() { os.Remove(lock) }, nil
}

// MetaHashes are the hashes stored in snapshot_meta.
type MetaHashes struct{ Raw, Derived, Roles, Params, Spec, GA4Spec string }

// ReadMetaHashes reads them, or nil.
func ReadMetaHashes(path string) *MetaHashes {
	db, err := sqlitec.Open(path, true)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, _, err := db.QueryJS("SELECT raw_hash, derived_hash, roles_hash, params_hash, spec_hash, ga4_spec_hash FROM snapshot_meta")
	if err != nil || len(rows) == 0 {
		return nil
	}
	s := func(v any) string { x, _ := v.(string); return x }
	r := rows[0]
	return &MetaHashes{s(r[0]), s(r[1]), s(r[2]), s(r[3]), s(r[4]), s(r[5])}
}

// Result is a finalized snapshot.
type Result struct {
	SnapshotID string
	Reused     bool
	Files      Files
}

// FinalizeOptions: FailAt forces a failure at a stage (tests).
type FinalizeOptions struct {
	TmpPath          string
	SnapshotsDir     string
	Inputs           BuildInputs
	Meta             Meta
	ApplyCutoffFirst bool
	LockHeld         bool
	FailAt           string
}

// Finalize builds and swaps in a snapshot. On failure it deletes the temporary files and leaves current.json as is.
func Finalize(o FinalizeOptions) (res Result, err error) {
	tmpAgent := o.TmpPath + ".agent"
	tmpMap := o.TmpPath + ".map"
	cleanup := func() {
		for _, p := range []string{o.TmpPath, tmpAgent, tmpMap} {
			os.Remove(p)
			os.Remove(p + "-journal")
		}
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	var id string
	var roles *collect.Roles
	err = func() error {
		db, err := sqlitec.Open(o.TmpPath, false)
		if err != nil {
			return err
		}
		defer db.Close()
		if err := db.Exec("PRAGMA journal_mode = DELETE"); err != nil {
			return err
		}
		if o.ApplyCutoffFirst {
			if o.FailAt == "cutoff" {
				return &FinalizeError{"forced failure (cutoff)"}
			}
			if err := ApplyCutoff(db, o.Inputs.Specs, o.Meta.Cutoff); err != nil {
				return err
			}
			if err := validate(db, o.Inputs.Specs); err != nil {
				return err
			}
		}
		if o.FailAt == "build" {
			return &FinalizeError{"forced failure (build)"}
		}
		id, roles, err = build(db, o.Inputs, o.Meta)
		return err
	}()
	if err != nil {
		return res, err
	}
	if o.FailAt == "pseudonymize" {
		return res, &FinalizeError{"forced failure (pseudonymize)"}
	}
	if _, err = Pseudonymize(o.TmpPath, tmpAgent, tmpMap, roles); err != nil {
		return res, err
	}
	for _, p := range []string{o.TmpPath, tmpAgent, tmpMap} {
		if err = FsyncPath(p); err != nil {
			return res, err
		}
	}
	if o.LockHeld {
		return place(o, id, tmpAgent, tmpMap, cleanup)
	}
	release, err := AcquireLock(o.SnapshotsDir)
	if err != nil {
		return res, err
	}
	defer release()
	return place(o, id, tmpAgent, tmpMap, cleanup)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// place reuses an identical set; otherwise moves the three files in and updates the pointer.
func place(o FinalizeOptions, id, tmpAgent, tmpMap string, cleanup func()) (Result, error) {
	files := SnapshotFiles(o.SnapshotsDir, id)
	existing := 0
	for _, p := range []string{files.Real, files.Agent, files.Map} {
		if exists(p) {
			existing++
		}
	}
	if existing > 0 {
		mine, theirs := ReadMetaHashes(o.TmpPath), ReadMetaHashes(files.Real)
		if existing == 3 && mine != nil && theirs != nil && *mine == *theirs {
			cleanup()
			if err := WriteCurrent(o.SnapshotsDir, id); err != nil {
				return Result{}, err
			}
			return Result{SnapshotID: id, Reused: true, Files: files}, nil
		}
		return Result{}, &FinalizeError{"files for snapshot_id " + id + " already exist but some are missing or differ. Needs a manual check"}
	}
	if o.FailAt == "rename" {
		return Result{}, &FinalizeError{"forced failure (rename)"}
	}
	var moved []string
	err := func() error {
		for _, m := range [][2]string{{tmpMap, files.Map}, {tmpAgent, files.Agent}, {o.TmpPath, files.Real}} {
			if err := os.Rename(m[0], m[1]); err != nil {
				return err
			}
			moved = append(moved, m[1])
			if o.FailAt == fmt.Sprintf("rename-%d", len(moved)) {
				return &FinalizeError{fmt.Sprintf("forced failure (rename-%d)", len(moved))}
			}
		}
		return FsyncPath(o.SnapshotsDir)
	}()
	if err != nil {
		for _, p := range moved {
			os.Remove(p)
		}
		return Result{}, err
	}
	if err := WriteCurrent(o.SnapshotsDir, id); err != nil {
		return Result{}, err
	}
	return Result{SnapshotID: id, Files: files}, nil
}
