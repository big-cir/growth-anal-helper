package cdc

import (
	"context"
	"errors"
	"hash/crc32"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/pingcap/tidb/pkg/parser"
)

// fakeSink records what the reader asks of the copy.
type fakeSink struct {
	open    bool
	applied []*Change
	pending []*Change
	commits []string
	saves   []string
	phases  []string // phase passed with each commit or save
	modes   []ApplyMode
	failOn  ChangeKind
}

func (f *fakeSink) Begin(_ context.Context, mode ApplyMode) error {
	if f.open {
		return errors.New("already open")
	}
	f.open = true
	f.modes = append(f.modes, mode)
	return nil
}

func (f *fakeSink) Apply(_ context.Context, c *Change) error {
	if c.Kind == f.failOn {
		return &StopError{"copy differs"}
	}
	f.pending = append(f.pending, c)
	return nil
}

func (f *fakeSink) Commit(_ context.Context, p, phase string) error {
	f.open = false
	f.phases = append(f.phases, phase)
	f.applied = append(f.applied, f.pending...)
	f.pending = nil
	f.commits = append(f.commits, p)
	return nil
}

func (f *fakeSink) Rollback() { f.open, f.pending = false, nil }

func (f *fakeSink) Save(_ context.Context, p, phase string) error {
	if f.open {
		return errors.New("save inside a transaction")
	}
	f.phases = append(f.phases, phase)
	f.saves = append(f.saves, p)
	return nil
}

func testReader(sink Sink, saved binlogPos) *reader {
	return &reader{
		o:    &RunOptions{SaveEvery: time.Hour},
		sink: sink,
		defs: map[string]*tableDef{"app.user": {Columns: []column{
			{Name: "id", DataType: "bigint"}, {Name: "locale", DataType: "enum", Enum: []string{"EN", "KO"}},
		}}},
		parser: parser.New(), version: 80408, saved: saved, last: saved, file: saved.File,
		sum: &RunSummary{Applied: map[string]*Counts{"app.user": {}}}, open: map[string]*Counts{}, lastSave: time.Now(),
	}
}

// ev builds an event ending at end with the given size; raw bytes make its checksum.
func ev(t replication.EventType, end, size uint32, e replication.Event) *replication.BinlogEvent {
	return &replication.BinlogEvent{
		Header:  &replication.EventHeader{EventType: t, LogPos: end, EventSize: size, Timestamp: 1},
		Event:   e,
		RawData: []byte{byte(t), byte(end), byte(end >> 8), byte(size)},
	}
}

func query(end uint32, schema, q string) *replication.BinlogEvent {
	return ev(replication.QUERY_EVENT, end, 20, &replication.QueryEvent{Schema: []byte(schema), Query: []byte(q)})
}

func rowsEv(t replication.EventType, end uint32, table string, rows ...[]any) *replication.BinlogEvent {
	return ev(t, end, 30, &replication.RowsEvent{Table: &replication.TableMapEvent{Schema: []byte("app"), Table: []byte(table)}, Rows: rows})
}

func xid(end uint32) *replication.BinlogEvent {
	return ev(replication.XID_EVENT, end, 31, &replication.XIDEvent{})
}

func feed(t *testing.T, r *reader, evs ...*replication.BinlogEvent) error {
	t.Helper()
	for _, e := range evs {
		if _, err := r.handle(context.Background(), e, e); err != nil {
			return err
		}
	}
	return nil
}

func TestReaderCommitsWithPosition(t *testing.T) {
	sink := &fakeSink{}
	r := testReader(sink, binlogPos{File: "bin.000001", Pos: 100})
	err := feed(t, r,
		query(150, "app", "BEGIN"),
		rowsEv(replication.WRITE_ROWS_EVENTv2, 200, "user", []any{int64(1), int64(2)}),
		rowsEv(replication.UPDATE_ROWS_EVENTv2, 250, "user", []any{int64(1), int64(2)}, []any{int64(1), int64(1)}),
		rowsEv(replication.DELETE_ROWS_EVENTv2, 300, "other", []any{int64(9)}),
		xid(331),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.commits) != 1 || len(sink.applied) != 2 {
		t.Fatalf("commits %v applied %d", sink.commits, len(sink.applied))
	}
	p, _ := parsePos(sink.commits[0])
	if p.Pos != 331 || p.EventStart != 300 || p.EventCRC == 0 {
		t.Errorf("position %+v", p)
	}
	if sink.applied[0].After[1] != "KO" || sink.applied[1].Before[1] != "KO" || sink.applied[1].After[1] != "EN" {
		t.Errorf("enum values not converted: %v", sink.applied)
	}
	if c := r.sum.Applied["app.user"]; c.Inserts != 1 || c.Updates != 1 {
		t.Errorf("summary %+v", c)
	}
}

func TestReaderStopsOnBadEnumAndPartialUpdate(t *testing.T) {
	for _, n := range []int64{0, 3} {
		r := testReader(&fakeSink{}, binlogPos{File: "bin.000001", Pos: 100})
		err := feed(t, r, query(150, "app", "BEGIN"), rowsEv(replication.WRITE_ROWS_EVENTv2, 200, "user", []any{int64(1), n}))
		var s *StopError
		if !errors.As(err, &s) {
			t.Errorf("enum %d: %v", n, err)
		}
	}
	r := testReader(&fakeSink{}, binlogPos{File: "bin.000001", Pos: 100})
	err := feed(t, r, query(150, "app", "BEGIN"), rowsEv(replication.PARTIAL_UPDATE_ROWS_EVENT, 200, "other", []any{int64(1)}, []any{int64(1)}))
	var s *StopError
	if !errors.As(err, &s) {
		t.Errorf("partial update on any table should stop: %v", err)
	}
}

func TestReaderRollsBackOnStop(t *testing.T) {
	sink := &fakeSink{failOn: Delete}
	r := testReader(sink, binlogPos{File: "bin.000001", Pos: 100})
	err := feed(t, r, query(150, "app", "BEGIN"), rowsEv(replication.DELETE_ROWS_EVENTv2, 200, "user", []any{int64(1), int64(1)}))
	var s *StopError
	if !errors.As(err, &s) || len(sink.commits) != 0 {
		t.Fatalf("err %v commits %v", err, sink.commits)
	}
}

func TestReaderSavesOnlyFinishedTransactions(t *testing.T) {
	sink := &fakeSink{}
	r := testReader(sink, binlogPos{File: "bin.000001", Pos: 100})
	r.o.SaveEvery = 0
	// a transaction with no followed rows: its end may be saved
	if err := feed(t, r, query(150, "app", "BEGIN"), rowsEv(replication.WRITE_ROWS_EVENTv2, 200, "other", []any{int64(1)}), xid(231)); err != nil {
		t.Fatal(err)
	}
	if len(sink.saves) != 1 || len(sink.commits) != 0 || r.sum.Skipped != 1 {
		t.Fatalf("saves %v commits %v skipped %d", sink.saves, sink.commits, r.sum.Skipped)
	}
	// stopping inside a followed transaction keeps the last finished end
	if err := feed(t, r, query(280, "app", "BEGIN"), rowsEv(replication.WRITE_ROWS_EVENTv2, 330, "user", []any{int64(2), int64(1)})); err != nil {
		t.Fatal(err)
	}
	if err := r.saveLast(context.Background(), ""); err != nil || len(sink.saves) != 1 {
		t.Fatalf("saved inside an open transaction: %v %v", err, sink.saves)
	}
}

func TestReaderRotateAndVerify(t *testing.T) {
	boundary := xid(331)
	saved := binlogPos{File: "bin.000001", Pos: 331, EventStart: 300, EventCRC: crc32.ChecksumIEEE(boundary.RawData)}
	fake := &replication.BinlogEvent{Header: &replication.EventHeader{EventType: replication.ROTATE_EVENT}, Event: &replication.RotateEvent{NextLogName: []byte("bin.000001"), Position: 300}}
	real := ev(replication.ROTATE_EVENT, 400, 40, &replication.RotateEvent{NextLogName: []byte("bin.000002"), Position: 4})

	sink := &fakeSink{}
	r := testReader(sink, saved)
	r.verify = true
	if err := feed(t, r, fake, boundary, real, query(200, "app", "BEGIN"), rowsEv(replication.WRITE_ROWS_EVENTv2, 250, "user", []any{int64(3), int64(1)}), xid(281)); err != nil {
		t.Fatal(err)
	}
	p, _ := parsePos(sink.commits[0])
	if r.file != "bin.000002" || p.File != "bin.000002" || p.Pos != 281 || len(sink.applied) != 1 {
		t.Fatalf("after rotate: file %s commit %+v", r.file, p)
	}

	other := xid(331)
	other.RawData = []byte("different")
	r = testReader(&fakeSink{}, saved)
	r.verify = true
	var s *StopError
	if err := feed(t, r, fake, other); !errors.As(err, &s) {
		t.Errorf("a different boundary event should stop: %v", err)
	}
	r = testReader(&fakeSink{}, saved)
	jump := ev(replication.ROTATE_EVENT, 400, 40, &replication.RotateEvent{NextLogName: []byte("bin.000003"), Position: 4})
	if err := feed(t, r, jump); !errors.As(err, &s) {
		t.Errorf("a skipped file should stop: %v", err)
	}
}

func TestReaderSkipsWhatTheCopyHas(t *testing.T) {
	sink := &fakeSink{}
	r := testReader(sink, binlogPos{File: "bin.000001", Pos: 500})
	if err := feed(t, r, query(150, "app", "BEGIN"), rowsEv(replication.WRITE_ROWS_EVENTv2, 200, "user", []any{int64(1), int64(1)}), xid(231)); err != nil {
		t.Fatal(err)
	}
	if len(sink.applied) != 0 || len(sink.commits) != 0 {
		t.Errorf("applied a transaction at or before the stored position: %v", sink.applied)
	}
}

func TestJudgeStatement(t *testing.T) {
	p := parser.New()
	pass := []struct{ db, q string }{
		{"mysql", "INSERT INTO mysql.rds_heartbeat2(id, value) VALUES (1, 2) ON DUPLICATE KEY UPDATE value = 2"},
		{"mysql", "INSERT INTO rds_history (action) VALUES ('x')"},
		{"app", "GRANT REPLICATION SLAVE ON *.* TO 'u'@'%'"},
		{"app", "CREATE USER 'u'@'%' IDENTIFIED BY 'p'"},
		{"", "FLUSH PRIVILEGES"},
	}
	for _, c := range pass {
		if err := judgeStatement(p, c.db, c.q); err != nil {
			t.Errorf("%q should pass: %v", c.q, err)
		}
	}
	stop := []struct{ db, q string }{
		{"app", "INSERT INTO user VALUES (1)"},
		{"mysql", "INSERT INTO app.user VALUES (1)"},
		{"mysql", "UPDATE rds_x SET v = (SELECT MAX(id) FROM app.user)"},
		{"mysql", "DELETE FROM rds_x WHERE id IN (SELECT id FROM app.user)"},
		{"other", "INSERT INTO t VALUES (1)"},
		{"app", "ALTER TABLE user ADD c INT"},
		{"mysql", "DROP DATABASE app"},
		{"app", "SET @x = 1"},
		{"app", "THIS IS NOT SQL"},
	}
	for _, c := range stop {
		var s *StopError
		if err := judgeStatement(p, c.db, c.q); !errors.As(err, &s) {
			t.Errorf("%q in %s should stop: %v", c.q, c.db, err)
		}
	}
}

func TestReaderStatementInTransaction(t *testing.T) {
	r := testReader(&fakeSink{}, binlogPos{File: "bin.000001", Pos: 100})
	if err := feed(t, r, query(150, "app", "BEGIN"), query(200, "mysql", "INSERT INTO rds_heartbeat2 VALUES (1)"), xid(231)); err != nil {
		t.Errorf("system statement in a transaction should pass: %v", err)
	}
	if err := feed(t, r, query(250, "app", "COMMIT;"), query(300, "app", "XA START 'x'")); err == nil || !strings.Contains(err.Error(), "XA") {
		t.Errorf("XA should stop: %v", err)
	}
}

func TestEnumValuesAndFiles(t *testing.T) {
	v, err := enumValues("enum('DE','it''s','KO')")
	if err != nil || len(v) != 3 || v[1] != "it's" {
		t.Errorf("enum %v %v", v, err)
	}
	if nextFile("mysql-bin-changelog.143338") != "mysql-bin-changelog.143339" || nextFile("bin.000009") != "bin.000010" {
		t.Error("next file")
	}
	st := Status{Files: []LogFile{{"bin.000001", 500}, {"bin.000002", 300}, {"bin.000004", 10}}}
	var s *StopError
	if err := continuity(st, binlogPos{File: "bin.000001", Pos: 400}); !errors.As(err, &s) {
		t.Errorf("a missing file should stop: %v", err)
	}
	st.Files[2].Name = "bin.000003"
	if err := continuity(st, binlogPos{File: "bin.000001", Pos: 400}); err != nil {
		t.Errorf("consecutive files: %v", err)
	}
	if err := continuity(st, binlogPos{File: "bin.000001", Pos: 600}); !errors.As(err, &s) {
		t.Errorf("a position past the file end should stop: %v", err)
	}
}

func TestReaderStopsOnUnknownEventsAndBadPositions(t *testing.T) {
	var s *StopError
	r := testReader(&fakeSink{}, binlogPos{File: "bin.000001", Pos: 100})
	if err := feed(t, r, ev(replication.VIEW_CHANGE_EVENT, 150, 20, nil)); !errors.As(err, &s) {
		t.Errorf("an unknown event should stop: %v", err)
	}
	r = testReader(&fakeSink{}, binlogPos{File: "bin.000001", Pos: 100})
	if err := feed(t, r, query(0, "app", "BEGIN")); !errors.As(err, &s) {
		t.Errorf("a query event without a position should stop: %v", err)
	}
	r = testReader(&fakeSink{}, binlogPos{File: "bin.000001", Pos: 100})
	if err := feed(t, r, ev(replication.ROTATE_EVENT, 400, 40, &replication.RotateEvent{NextLogName: []byte("bin.000002"), Position: 120})); !errors.As(err, &s) {
		t.Errorf("a rotate into the middle of the next file should stop: %v", err)
	}
	r = testReader(&fakeSink{}, binlogPos{File: "bin.000001", Pos: 100})
	if err := feed(t, r, ev(replication.TABLE_MAP_EVENT, 120, 20, nil), ev(replication.ANONYMOUS_GTID_EVENT, 140, 20, nil)); err != nil {
		t.Errorf("table map and gtid events should pass: %v", err)
	}
}

func TestReaderDoesNotSaveInsideASourceTransaction(t *testing.T) {
	sink := &fakeSink{}
	r := testReader(sink, binlogPos{File: "bin.000001", Pos: 100, EventStart: 90, EventCRC: 1})
	if err := feed(t, r, query(150, "app", "BEGIN"), xid(181), query(220, "app", "BEGIN"), rowsEv(replication.WRITE_ROWS_EVENTv2, 260, "other", []any{int64(1)})); err != nil {
		t.Fatal(err)
	}
	if err := r.saveLast(context.Background(), ""); err != nil || len(sink.saves) != 0 {
		t.Fatalf("saved while a source transaction was open: %v %v", err, sink.saves)
	}
	if err := feed(t, r, xid(291)); err != nil {
		t.Fatal(err)
	}
	if err := r.saveLast(context.Background(), ""); err != nil || len(sink.saves) != 1 {
		t.Fatalf("expected one save after the transaction ended: %v %v", err, sink.saves)
	}
}

func TestReaderStoresTheFirstBoundaryAtOnce(t *testing.T) {
	sink := &fakeSink{}
	r := testReader(sink, binlogPos{File: "bin.000001", Pos: 100}) // a --from start has no event checksum
	if err := feed(t, r, query(150, "app", "BEGIN"), xid(181)); err != nil {
		t.Fatal(err)
	}
	if len(sink.saves) != 1 {
		t.Fatalf("the first boundary should be stored with its checksum at once: %v", sink.saves)
	}
	if p, _ := parsePos(sink.saves[0]); p.EventStart == 0 || p.EventCRC == 0 {
		t.Errorf("stored without a checksum: %+v", p)
	}
}

func TestReaderConvergesUntilTheFirstEndAfterP2(t *testing.T) {
	sink := &fakeSink{}
	r := testReader(sink, binlogPos{File: "bin.000001", Pos: 100, EventStart: 90, EventCRC: 1})
	r.converging, r.p2 = true, binlogPos{File: "bin.000001", Pos: 300}
	tx := func(begin, row, end uint32) []*replication.BinlogEvent {
		return []*replication.BinlogEvent{query(begin, "app", "BEGIN"), rowsEv(replication.WRITE_ROWS_EVENTv2, row, "user", []any{int64(row), int64(1)}), xid(end)}
	}
	var evs []*replication.BinlogEvent
	evs = append(evs, tx(150, 180, 211)...) // ends before p2: converge
	evs = append(evs, tx(250, 280, 331)...) // starts before, ends after p2: still converge, and ends converging
	evs = append(evs, tx(380, 410, 441)...) // strict
	if err := feed(t, r, evs...); err != nil {
		t.Fatal(err)
	}
	if want := []ApplyMode{Converge, Converge, Strict}; !reflect.DeepEqual(sink.modes, want) {
		t.Errorf("modes %v, want %v", sink.modes, want)
	}
	if want := []string{"", phaseCatchingUp, ""}; !reflect.DeepEqual(sink.phases, want) {
		t.Errorf("phases %v, want %v", sink.phases, want)
	}
	if r.converging {
		t.Error("still converging")
	}
}

func TestReaderEndsConvergingOnAnUnfollowedTransaction(t *testing.T) {
	sink := &fakeSink{}
	r := testReader(sink, binlogPos{File: "bin.000001", Pos: 100, EventStart: 90, EventCRC: 1})
	r.converging, r.p2 = true, binlogPos{File: "bin.000001", Pos: 200}
	r.o.SaveEvery = time.Hour
	if err := feed(t, r, query(150, "app", "BEGIN"), rowsEv(replication.WRITE_ROWS_EVENTv2, 180, "other", []any{int64(1)}), xid(211)); err != nil {
		t.Fatal(err)
	}
	if len(sink.saves) != 1 || sink.phases[0] != phaseCatchingUp || r.converging {
		t.Fatalf("the phase change should be saved at once with that end: saves %v phases %v", sink.saves, sink.phases)
	}
}

func TestAddIndexSQL(t *testing.T) {
	got := addIndexSQL("stage", "t", indexDef{Name: "k", Unique: true, Visible: false, Parts: []indexPart{{Column: "a", SubPart: "10"}, {Column: "b", Desc: true}}})
	if want := "ALTER TABLE `stage`.`t` ADD UNIQUE INDEX `k` (`a`(10), `b` DESC) INVISIBLE"; got != want {
		t.Errorf("got %s", got)
	}
}
