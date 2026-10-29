package coverage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/sqlitec"
)

var twoGaps = [][2]string{{"2010-01-01 00:00:00.000000", "2011-01-01 00:00:00.000000"}, {"2014-03-01 00:00:00.000000", "2016-06-01 00:00:00.000000"}}

const config = `{"events": [{"table": "e_events", "time": "at"}, {"table": "e_posts", "time": "created_at"}],
 "kinds": {"table": "e_events", "column": "kind", "time": "at"},
 "ga4": {"table": "g_daily", "date": "date", "prefix": "g_"}}`

func rules(ss []Signal) string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Rule)
	}
	return strings.Join(out, ",")
}

func TestGapDependentPerGap(t *testing.T) {
	r := &Resolved{Gaps: twoGaps, GapPaths: []string{"g1", "g2"}, Events: []Column{{"e_posts", "created_at"}, {"e_events", "at"}}}
	changes := map[string]bool{"g1": true}
	same := func(p string) (bool, error) { return !changes[p], nil }
	got := r.Check(CheckInput{Tables: []string{"e_posts"}, SameOnGapCopy: same, Lang: "ko"})
	if rules(got) != "gap_dependent" || !got[0].Ask || !strings.Contains(got[0].Caveat, "2010-01-01 ~ 2011-01-01") || strings.Contains(got[0].Caveat, "2014") {
		t.Fatalf("got %+v", got)
	}
	changes = map[string]bool{}
	if got := r.Check(CheckInput{Tables: []string{"e_events"}, SameOnGapCopy: same}); len(got) != 0 {
		t.Fatalf("unchanged result flagged: %+v", got)
	}
	if got := r.Check(CheckInput{Tables: []string{"e_users"}, SameOnGapCopy: func(string) (bool, error) { return false, nil }}); len(got) != 0 {
		t.Fatalf("non-event table checked: %+v", got)
	}
}

func TestGapCheckFailureIsShown(t *testing.T) {
	r := &Resolved{Gaps: twoGaps, Events: []Column{{"e_posts", "created_at"}}, GapError: "disk full"}
	if got := rules(r.Check(CheckInput{Tables: []string{"e_posts"}})); got != "gap_check_failed" {
		t.Fatalf("got %q", got)
	}
	if got := rules((&Resolved{Events: r.Events}).Check(CheckInput{Tables: []string{"e_posts"}})); got != "" {
		t.Fatalf("no gaps but got %q", got)
	}
}

func snapshot(t *testing.T, stmts ...string) string {
	path := filepath.Join(t.TempDir(), "snap.sqlite")
	db, err := sqlitec.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.RunOnce(s); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func loadConfig(t *testing.T, text string) *Config {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "coverage.json"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResolveAndGapCopies(t *testing.T) {
	path := snapshot(t,
		"CREATE TABLE e_events (kind TEXT, at TEXT)",
		"CREATE TABLE e_posts (created_at TEXT)",
		"CREATE TABLE g_daily (date TEXT)",
		"INSERT INTO e_events VALUES ('a', '2009-05-01 10:00:00.000000'), ('a', '2012-01-01 10:00:00.000000'), ('a', '2016-06-01 00:00:01.000000'), ('b', '2017-04-03 08:00:00.000000')",
		"INSERT INTO e_posts VALUES ('2012-01-01 10:00:00.000000'), ('2016-08-01 10:00:00.000000')",
		"INSERT INTO g_daily VALUES ('2016-06-01'), ('2018-09-20')")
	params := jsjson.Object{{Key: "quality_gap_ranges", Value: []any{"2010-01-01 00:00:00.000000~2011-01-01 00:00:00.000000", "2014-03-01 00:00:00.000000~2016-06-01 00:00:00.000000"}}}
	r, err := Resolve(loadConfig(t, config), path, "2018-09-22 10:00:00.000000", params)
	if err != nil {
		t.Fatal(err)
	}
	if r.AppEnd != "2018-09-21" || r.GA4Start != "2016-06-01" || r.GA4End != "2018-09-20" || len(r.Kinds) != 1 || r.Kinds["b"] != "2017-04-03" {
		t.Fatalf("resolved %+v", r)
	}
	if err := r.EnsureGapCopies(path); err != nil || len(r.GapPaths) != 2 {
		t.Fatalf("copies %v %v", err, r.GapPaths)
	}
	count := func(p, table string) int64 {
		d, err := sqlitec.Open(p, true)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		rows, err := d.Query("SELECT count(*) FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		return rows[0][0].Int
	}
	if n := count(r.GapPaths[0], "e_events"); n != 3 {
		t.Fatalf("first gap copy keeps %d event rows", n)
	}
	if n := count(r.GapPaths[1], "e_events") + count(r.GapPaths[1], "e_posts"); n != 3 {
		t.Fatalf("second gap copy keeps %d rows", n)
	}
	if n := count(path, "e_events"); n != 4 {
		t.Fatalf("source changed: %d", n)
	}
	st, _ := os.Stat(r.GapPaths[1])
	if err := r.EnsureGapCopies(path); err != nil {
		t.Fatal(err)
	}
	if st2, _ := os.Stat(r.GapPaths[1]); !st2.ModTime().Equal(st.ModTime()) {
		t.Fatal("copy not reused")
	}
	g := r.Guide()
	if !strings.Contains(g, ":data_end") || !strings.Contains(g, "GA4 tables (g_*)") || !strings.Contains(g, "2014-03-01") || !strings.Contains(g, "e_events.kind = 'b': first record in this snapshot on 2017-04-03") {
		t.Fatalf("guide %s", g)
	}
}

func TestNoConfig(t *testing.T) {
	c, raw, err := LoadConfig(t.TempDir())
	if err != nil || raw != nil || len(c.Events) != 0 || c.Kinds != nil || c.GA4 != nil {
		t.Fatalf("%v %+v", err, c)
	}
	path := snapshot(t)
	params := jsjson.Object{{Key: "quality_gap_ranges", Value: []any{"2014-03-01 00:00:00.000000~2016-06-01 00:00:00.000000"}}}
	r, err := Resolve(c, path, "2018-09-22 10:00:00.000000", params)
	if err != nil || r.GA4End != "" || len(r.Kinds) != 0 {
		t.Fatalf("%v %+v", err, r)
	}
	if err := r.EnsureGapCopies(path); err != nil || len(r.GapPaths) != 0 {
		t.Fatalf("%v %v", err, r.GapPaths)
	}
	if g := r.Guide(); strings.Contains(g, "GA4") || strings.Contains(g, "reruns") || !strings.Contains(g, "Data gap") {
		t.Fatalf("guide %s", g)
	}
}

func TestBadConfig(t *testing.T) {
	for _, text := range []string{
		`{"events": [{"table": "e_events; DROP", "time": "at"}]}`,
		`{"ga4": {"table": "g_daily", "date": "date", "prefix": "g-"}}`,
		`{"events": [{"table": "e_events", "time": "at"}], "extra": 1}`,
		`{"events": `,
		`{"events": []} {"events": []}`,
	} {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "coverage.json"), []byte(text), 0o644)
		if _, _, err := LoadConfig(dir); err == nil {
			t.Errorf("accepted %s", text)
		}
	}
	path := snapshot(t, "CREATE TABLE e_events (kind TEXT)")
	if _, err := Resolve(loadConfig(t, `{"events": [{"table": "e_events", "time": "at"}]}`), path, "2018-09-22 10:00:00.000000", nil); err == nil || !strings.Contains(err.Error(), "e_events.at") {
		t.Fatalf("missing column accepted: %v", err)
	}
}
