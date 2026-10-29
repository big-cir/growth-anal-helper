package cdc

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestHashPartPerType(t *testing.T) {
	for typ, want := range map[string]string{
		"bigint":   "CAST(`c` AS CHAR)",
		"datetime": "CAST(`c` AS CHAR)",
		"varchar":  "CAST(`c` AS BINARY)",
		"enum":     "CAST(`c` + 0 AS CHAR)",
	} {
		got := hashPart(column{Name: "c", DataType: typ})
		v := "IFNULL(" + want + ", _binary'')"
		if got != "ISNULL(`c`), ':', LENGTH("+v+"), ':', "+v {
			t.Errorf("%s: %s", typ, got)
		}
	}
}

func TestCheckSQL(t *testing.T) {
	d := &tableDef{Columns: []column{{Name: "team", DataType: "bigint"}, {Name: "day", DataType: "datetime"},
		{Name: "note", DataType: "varchar"}}, PK: []int{1, 0}}
	ct, err := checkSQL(d, "`app`.`t`", "`copy`.`t`")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ct.keys, []string{"`day`", "`team`"}) || !reflect.DeepEqual(ct.ints, []bool{false, true}) {
		t.Fatalf("keys %v ints %v", ct.keys, ct.ints)
	}
	// every column is hashed, the key in key order
	if !strings.Contains(ct.rows, "SELECT CONCAT(ISNULL(`day`)") || strings.Count(ct.chunk, "ISNULL(") != 3 {
		t.Fatalf("rows %s\nchunk %s", ct.rows, ct.chunk)
	}
	if !strings.Contains(ct.bound, "ORDER BY `day` ASC, `team` ASC LIMIT 1 OFFSET ?") || !strings.Contains(ct.bound, "FROM `app`.`t`") {
		t.Fatalf("bound %s", ct.bound)
	}

	for _, c := range []struct {
		s    span
		dst  bool
		want string
		args []any
	}{
		{span{}, false, "FROM `app`.`t` WHERE TRUE)", nil},
		{span{hi: []any{"d", 1}}, true, "FROM `copy`.`t` WHERE (`day`, `team`) < (?, ?))", []any{"d", 1}},
		{span{lo: []any{"a", 2}}, false, "WHERE (`day`, `team`) >= (?, ?))", []any{"a", 2}},
		{span{lo: []any{"a", 2}, hi: []any{"d", 1}}, false, "WHERE (`day`, `team`) >= (?, ?) AND (`day`, `team`) < (?, ?))", []any{"a", 2, "d", 1}},
	} {
		q, args := ct.sql(ct.chunk, c.s, c.dst)
		if !strings.HasSuffix(q, c.want+" x") || !reflect.DeepEqual(args, c.args) {
			t.Errorf("%+v: %s %v", c.s, q, args)
		}
	}

	d.Columns[0].DataType = "enum"
	d.PK = []int{0}
	if _, err := checkSQL(d, "a", "b"); err == nil {
		t.Fatal("an enum key should stop")
	}
}

func TestDiffRows(t *testing.T) {
	src := [][]string{{"k1", "h1"}, {"k2", "h2"}, {"k3", "h3"}}
	dst := [][]string{{"k1", "h1"}, {"k2", "hX"}, {"k4", "h4"}}
	got := diffRows(src, dst)
	want := map[string]byte{"k2": 'd', "k3": 'm', "k4": 'e'}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if len(diffRows(src, src)) != 0 {
		t.Fatal("same rows should not differ")
	}
}

func TestCheckReportResult(t *testing.T) {
	r := &CheckReport{Tables: map[string]*TableCheck{"a.x": {}, "a.y": {}}}
	if r.Differs() || r.Undecided() {
		t.Fatal("empty report")
	}
	r.Tables["a.y"].Extra = 1
	if !r.Differs() || r.Undecided() {
		t.Fatal("extra row")
	}
	r.Tables["a.x"].Unchecked = true
	if !r.Undecided() {
		t.Fatal("unchecked table")
	}
	for _, c := range []struct {
		stopped, differs, undecided bool
		want                        int
	}{
		{false, false, false, 0}, {true, false, false, 0},
		{true, true, false, 3}, {true, true, true, 3}, {true, false, true, 4},
		{false, true, false, 4}, {false, false, true, 4},
	} {
		r := &CheckReport{WritesStopped: c.stopped, Tables: map[string]*TableCheck{"a.x": {}}}
		if c.differs {
			r.Tables["a.x"].Differ = 1
		}
		if c.undecided {
			r.Tables["a.x"].Undecided = 1
		}
		if got := r.ExitCode(); got != c.want {
			t.Errorf("%+v: exit %d", c, got)
		}
	}
	// differences the end could not confirm are not mismatches
	r = &CheckReport{WritesStopped: true, Provisional: true, Tables: map[string]*TableCheck{"a.x": {Differ: 1}}}
	if got := r.ExitCode(); got != 4 {
		t.Fatalf("provisional: exit %d", got)
	}
	r.Tables["a.x"].Differ = 0
	if got := r.ExitCode(); got != 4 {
		t.Fatalf("provisional without differences: exit %d", got)
	}
}

func TestRecheck(t *testing.T) {
	sp := func(id int) suspect {
		return suspect{s: span{lo: []any{id}}, keys: map[string]byte{"a": 'd', "b": 'm'}, decided: true}
	}
	// chunk 1 differs in a only, chunk 2 becomes the same, chunk 3's last look does not finish
	looks := map[int][]func() (map[string]byte, bool){
		1: {func() (map[string]byte, bool) { return map[string]byte{"a": 'e', "c": 'd'}, true }},
		2: {func() (map[string]byte, bool) { return map[string]byte{}, true }},
		3: {func() (map[string]byte, bool) { return map[string]byte{"a": 'd'}, true }},
	}
	calls := map[int]int{}
	look := func(s span) (map[string]byte, bool, error) {
		id := s.lo[0].(int)
		calls[id]++
		if id == 3 && calls[id] == 2 {
			return nil, false, nil
		}
		f := looks[id][0]
		k, ok := f()
		return k, ok, nil
	}
	got, err := recheck(context.Background(), []suspect{sp(1), sp(2), sp(3)}, 2, 0, look)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d chunks left", len(got))
	}
	// a row new in a later look is not added; a row's kind is the last one seen
	if !got[0].decided || !reflect.DeepEqual(got[0].keys, map[string]byte{"a": 'e'}) {
		t.Fatalf("chunk 1: %+v", got[0])
	}
	if got[1].decided || calls[3] != 2 {
		t.Fatalf("chunk 3 should be undecided after its last look: %+v", got[1])
	}
}
