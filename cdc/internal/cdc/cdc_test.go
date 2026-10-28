package cdc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	gosql "github.com/go-sql-driver/mysql"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "client.cnf")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSource(t *testing.T) {
	s, err := LoadSource(writeFile(t, "[mysqld]\nhost=wrong\n[client]\nhost=\"db.example\"\nuser='reader'\nport=2016\npassword=p@ss=1\nssl-mode=REQUIRED\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "db.example" || s.User != "reader" || s.Port != 2016 || s.Password != "p@ss=1" {
		t.Fatalf("got %+v", s)
	}
	for name, body := range map[string]string{
		"include":        "!include other.cnf\n[client]\nhost=h\nuser=u\n",
		"inline comment": "[client]\nhost=h # note\nuser=u\n",
		"escape":         "[client]\nhost=h\nuser=u\npassword=\"a\\\"b\"\n",
		"unbalanced":     "[client]\nhost=h\nuser=u\npassword=\"ab\n",
		"bad port":       "[client]\nhost=h\nuser=u\nport=x\n",
		"no user":        "[client]\nhost=h\n",
	} {
		if _, err := LoadSource(writeFile(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func newTestTally(t *testing.T) (*tally, *Report) {
	t.Helper()
	f, err := NewFilter([]string{"app", "shop"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sel := map[string]TableInfo{
		"app.user":        {PK: []int{0}, Columns: 2},
		"app.team_member": {PK: []int{0, 1}, Columns: 3},
		"shop.user":       {PK: []int{0}, Columns: 1},
	}
	rep := &Report{}
	return newTally(f, sel, time.UTC, 80408, rep), rep
}

func pos(n uint32) mysql.Position { return mysql.Position{Name: "bin.000001", Pos: n} }

const (
	ins = replication.EnumRowsEventTypeInsert
	upd = replication.EnumRowsEventTypeUpdate
	del = replication.EnumRowsEventTypeDelete
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestTallyCountsCommittedRows(t *testing.T) {
	tl, rep := newTestTally(t)
	must(t, tl.begin())
	must(t, tl.rows("app", "user", ins, [][]any{{int64(1), "a"}, {int64(2), "b"}}, true, 100))
	must(t, tl.rows("app", "user", upd, [][]any{{int64(1), "a"}, {int64(1), "c"}}, true, 50))
	must(t, tl.rows("app", "team_member", upd, [][]any{{int64(7), int64(1), 0}, {int64(7), int64(2), 0}}, true, 40))
	must(t, tl.rows("other", "user", ins, [][]any{{int64(3)}}, false, 10))
	must(t, tl.rows("shop", "user", ins, [][]any{{int64(3)}}, true, 10))
	tl.commit(3600, pos(500))

	must(t, tl.begin())
	must(t, tl.rows("app", "user", del, [][]any{{int64(2), "b"}}, true, 30))
	tl.rollback(pos(520))
	if rep.Counted != pos(520) {
		t.Fatalf("rollback should move counted, got %v", rep.Counted)
	}

	must(t, tl.begin())
	must(t, tl.rows("app", "user", del, [][]any{{int64(1), "c"}}, true, 30))
	tl.finish()

	if u := rep.Tables["app.user"]; u.Inserts != 2 || u.Updates != 1 || u.Deletes != 0 || u.RowBytes != 150 {
		t.Fatalf("app.user %+v", u)
	}
	if tm := rep.Tables["app.team_member"]; tm.Updates != 1 || tm.KeyChanges != 1 {
		t.Fatalf("app.team_member %+v", tm)
	}
	if su := rep.Tables["shop.user"]; su.Inserts != 1 {
		t.Fatalf("shop.user %+v", su)
	}
	if _, ok := rep.Tables["other.user"]; ok {
		t.Fatal("other.user should be ignored")
	}
	if rep.Commits != 1 || rep.TargetCommits != 1 || rep.Rollbacks != 1 || !rep.DroppedOpen {
		t.Fatalf("commits %d/%d rollbacks %d dropped %v", rep.Commits, rep.TargetCommits, rep.Rollbacks, rep.DroppedOpen)
	}
	if rep.Hours["1970-01-01 01:00"] != 5 {
		t.Fatalf("counted %v hours %v", rep.Counted, rep.Hours)
	}
	if strings.Join(rep.TablesSelected, ",") != "app.team_member,app.user,shop.user" {
		t.Fatalf("selected %v", rep.TablesSelected)
	}
	if strings.Join(rep.NotObserved, ",") != "delete" {
		t.Fatalf("not observed %v", rep.NotObserved)
	}
}

func TestTallyStatements(t *testing.T) {
	tl, rep := newTestTally(t)
	must(t, tl.statement("app", "UPDATE X SET Y = 1", pos(100)))
	must(t, tl.statement("mysql", "INSERT INTO RDS_HEARTBEAT2 VALUES (1)", pos(200)))
	must(t, tl.statement("", "INSERT INTO APP.USER VALUES (1)", pos(300)))
	must(t, tl.statement("mysql", "ALTER TABLE X ADD C INT", pos(310)))
	if rep.Statements["app UPDATE"] != 1 || len(rep.Statements) != 1 || rep.Counted != pos(310) || rep.Commits != 0 {
		t.Fatalf("outside: %v counted %v commits %d", rep.Statements, rep.Counted, rep.Commits)
	}

	must(t, tl.begin())
	must(t, tl.statement("app", "INSERT INTO X VALUES (1)", pos(400)))
	must(t, tl.statement("app", "SAVEPOINT S1", pos(410)))
	must(t, tl.statement("app", "ROLLBACK TO SAVEPOINT S1", pos(420)))
	must(t, tl.statement("app", "RELEASE SAVEPOINT S1", pos(430)))
	must(t, tl.statement("mysql", "INSERT INTO RDS_HEARTBEAT2 VALUES (1)", pos(440)))
	tl.commit(10, pos(450))
	if rep.Statements["app INSERT"] != 1 || len(rep.Statements) != 2 || rep.Commits != 1 || rep.TargetCommits != 0 {
		t.Fatalf("committed: %v commits %d/%d", rep.Statements, rep.Commits, rep.TargetCommits)
	}

	must(t, tl.begin())
	must(t, tl.statement("app", "DELETE FROM X", pos(500)))
	tl.rollback(pos(550))
	must(t, tl.begin())
	must(t, tl.statement("app", "UPDATE X SET Y = 2", pos(600)))
	tl.finish()
	if rep.Statements["app DELETE"] != 0 || rep.Statements["app UPDATE"] != 1 {
		t.Fatalf("rolled back or open statements were kept: %v", rep.Statements)
	}

	for _, q := range []string{"ALTER TABLE X ADD C INT", "CREATE TABLE Y (ID INT)", "DROP TABLE X", "RENAME TABLE X TO Y", "TRUNCATE TABLE X"} {
		tl2, _ := newTestTally(t)
		if err := tl2.statement("app", q, pos(700)); err == nil {
			t.Errorf("%s in a followed database should stop", q)
		}
	}
	tl3, rep3 := newTestTally(t)
	if err := tl3.statement("app", "/* MIGRATION */ -- NOTE\nALTER TABLE X ADD C INT", pos(800)); err == nil {
		t.Error("a schema change after comments should stop")
	}
	if err := tl3.statement("app", "/*!80000 ALTER TABLE X ADD C INT */", pos(805)); err == nil {
		t.Error("a schema change in an executable comment should stop")
	}
	if err := tl3.statement("app", "/*!99999 SET @X = 1 */ ALTER TABLE X ADD C INT", pos(806)); err == nil {
		t.Error("a schema change after a comment the server skips should stop")
	}
	if err := tl3.statement("app", "/*! ALTER TABLE X ADD C INT */", pos(806)); err == nil {
		t.Error("a schema change in an unversioned executable comment should stop")
	}
	must(t, tl3.statement("app", "/*!40101 SET NAMES UTF8 */", pos(807)))
	must(t, tl3.statement("app", "RELEASE ALL", pos(810)))
	must(t, tl3.statement("app", "ROLLBACK WORK TO S1", pos(820)))
	if rep3.Statements["app RELEASE"] != 1 || rep3.Statements["app SET"] != 1 || len(rep3.Statements) != 2 {
		t.Errorf("only RELEASE SAVEPOINT is skipped: %v", rep3.Statements)
	}
	if keyword(skipComments("/* X", 80408)) != "?" {
		t.Error("unreadable keyword should be ?")
	}
}

func TestVersionNumber(t *testing.T) {
	for v, want := range map[string]int{"8.4.8": 80408, "8.0.36-log": 80036, "5.7.44-rds": 50744} {
		if got, err := versionNumber(v); err != nil || got != want {
			t.Errorf("%s: %d %v", v, got, err)
		}
	}
	if _, err := versionNumber("x"); err == nil {
		t.Error("bad version should fail")
	}
}

func TestTallyRowErrors(t *testing.T) {
	tl, _ := newTestTally(t)
	if err := tl.rows("mysql", "user", ins, [][]any{{int64(1)}}, false, 1); err != nil {
		t.Errorf("rows outside the filter should be ignored first: %v", err)
	}
	if err := tl.rows("app", "user", ins, [][]any{{int64(1), "a"}}, true, 1); err == nil {
		t.Error("row outside a transaction should fail")
	}
	must(t, tl.begin())
	for name, err := range map[string]error{
		"new table":     tl.rows("app", "orders", ins, [][]any{{int64(1)}}, true, 1),
		"partial image": tl.rows("app", "user", ins, [][]any{{int64(1), "a"}}, false, 1),
		"column count":  tl.rows("app", "user", ins, [][]any{{int64(1), "a", "extra"}}, true, 1),
		"odd update":    tl.rows("app", "user", upd, [][]any{{int64(1), "a"}}, true, 1),
		"unknown kind":  tl.rows("app", "user", replication.EnumRowsEventTypeUnknown, [][]any{{int64(1), "a"}}, true, 1),
	} {
		if err == nil {
			t.Errorf("%s should fail", name)
		}
	}
	if err := tl.begin(); err == nil {
		t.Error("nested BEGIN should fail")
	}
}

func TestFilter(t *testing.T) {
	got, err := ParseList("--x", " app , shop,app ")
	if err != nil || strings.Join(got, ",") != "app,shop" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := ParseList("--x", "app,,shop"); err == nil {
		t.Error("empty item should fail")
	}
	if _, err := NewFilter(nil, nil); err == nil {
		t.Error("database list is required")
	}
	if _, err := NewFilter([]string{"app("}, nil); err == nil {
		t.Error("invalid pattern should fail")
	}
	f, err := NewFilter([]string{"app", ".*"}, []string{`app\.user`, `shop\..*`})
	must(t, err)
	for _, c := range []struct {
		db, table string
		want      bool
	}{
		{"app", "user", true},
		{"app", "users", false},
		{"app", "team_member", false},
		{"shop", "orders", true},
		{"App", "user", false},
		{"mysql", "user", false},
		{"information_schema", "TABLES", false},
	} {
		if f.Table(c.db, c.table) != c.want {
			t.Errorf("%s.%s: want %v", c.db, c.table, c.want)
		}
	}
	all, err := NewFilter([]string{"app"}, nil)
	must(t, err)
	if !all.Table("app", "anything") || all.Table("application", "user") {
		t.Error("without a table list every table of a matched database is followed, names match whole")
	}
}

func TestSafeErrorKeepsOnlyFixedInformation(t *testing.T) {
	raw := &replication.EventError{Header: &replication.EventHeader{EventType: replication.WRITE_ROWS_EVENTv2, LogPos: 900}, Err: "bad", Data: []byte("secret-row-value")}
	for _, err := range []error{raw, fmt.Errorf("wrapped: %w", raw)} {
		msg := SafeError(err).Error()
		if strings.Contains(msg, "secret") || !strings.Contains(msg, "WriteRowsEventV2") || !strings.Contains(msg, "900") {
			t.Errorf("got %q", msg)
		}
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{&gosql.MySQLError{Number: 1062, Message: "Duplicate entry 'secret' for key"}, "MySQL error 1062"},
		{&mysql.MyError{Code: 1045, Message: "Access denied for user 'secret'"}, "MySQL error 1045: access denied (user or password)"},
		{fmt.Errorf("query: %w", context.DeadlineExceeded), "time limit reached"},
		{errors.New("value 'secret' is short"), "error of type *errors.errorString"},
		{&Error{"own message"}, "own message"},
	} {
		if got := SafeError(c.err).Error(); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestPickServerID(t *testing.T) {
	st := Status{ServerID: 5, ReplicaIDs: []uint32{7}}
	if _, err := pickServerID(5, st); err == nil {
		t.Error("source id should be rejected")
	}
	if _, err := pickServerID(7, st); err == nil {
		t.Error("replica id should be rejected")
	}
	if id, err := pickServerID(9, st); err != nil || id != 9 {
		t.Errorf("got %d %v", id, err)
	}
	for range 100 {
		id, err := pickServerID(0, st)
		if err != nil || id < 1<<31 {
			t.Fatalf("got %d %v", id, err)
		}
	}
}
