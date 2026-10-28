package cdc

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	gosql "github.com/go-sql-driver/mysql"
	"github.com/pingcap/tidb/pkg/parser"
)

// RunOptions configures a run that applies the source binlog to the copy.
type RunOptions struct {
	Source   Source
	Target   Source
	Database string
	Tables   []string        // exact table names in Database
	From     *mysql.Position // first run only: where the copy's data was taken
	ServerID uint32
	Logf     func(format string, a ...any)
	// SaveEvery is how often a position is saved when no followed row has changed.
	SaveEvery time.Duration
	// StopWait is how long an open source transaction may take to finish after a stop request.
	StopWait time.Duration
	Retries  int
}

// RunSummary is what a run applied before it returned.
type RunSummary struct {
	Saved   string // last stored position
	Applied map[string]*Counts
	Skipped int64 // transactions with no followed rows
}

// binlogPos is the position run stores: the end of the last finished source transaction,
// and the start and checksum of the event that ended it, to recognise the same binlog on return.
type binlogPos struct {
	File       string `json:"file"`
	Pos        uint32 `json:"pos"`
	EventStart uint32 `json:"event_start,omitempty"`
	EventCRC   uint32 `json:"event_crc,omitempty"`
}

func (p binlogPos) position() mysql.Position { return mysql.Position{Name: p.File, Pos: p.Pos} }

func (p binlogPos) String() string {
	b, _ := json.Marshal(p)
	return string(b)
}

func parsePos(s string) (binlogPos, error) {
	var p binlogPos
	if err := json.Unmarshal([]byte(s), &p); err != nil || p.File == "" || p.Pos < 4 {
		return p, &StopError{"the stored position is not understood"}
	}
	return p, nil
}

func (o *RunOptions) names() []string {
	out := make([]string, len(o.Tables))
	for i, t := range o.Tables {
		out[i] = o.Database + "." + t
	}
	sort.Strings(out)
	return out
}

// Run applies source changes to the copy until ctx is cancelled or a stop condition.
// Connection failures and uncertain commits restart from the position stored in the copy.
func Run(ctx context.Context, o RunOptions) (*RunSummary, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.SaveEvery == 0 {
		o.SaveEvery = 10 * time.Second
	}
	if o.StopWait == 0 {
		o.StopWait = 30 * time.Second
	}
	if o.Retries == 0 {
		o.Retries = 8
	}
	sum := &RunSummary{Applied: map[string]*Counts{}}
	for _, n := range o.names() {
		sum.Applied[n] = &Counts{}
	}
	from := o.From // kept until the copy has stored it
	for attempt := 0; ; attempt++ {
		started := time.Now()
		err := session(ctx, &o, &from, sum)
		if err == nil || errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return sum, nil
		}
		if ctx.Err() != nil {
			return sum, err // a failure while stopping is reported, not retried
		}
		if !retryable(err) {
			return sum, err
		}
		if time.Since(started) > time.Minute {
			attempt = 0
		}
		if attempt >= o.Retries {
			return sum, fmt.Errorf("giving up after %d retries: %w", attempt, err)
		}
		wait := min(time.Duration(1<<attempt)*time.Second, time.Minute)
		o.Logf("reconnecting in %s after: %s", wait, SafeError(err))
		select {
		case <-ctx.Done():
			return sum, nil
		case <-time.After(wait):
		}
	}
}

func isStop(err error) bool {
	var s *StopError
	var e *Error
	return errors.As(err, &s) || errors.As(err, &e)
}

// retryable reports failures of a connection, a lock or a commit outcome; anything else stops.
func retryable(err error) bool {
	if isStop(err) {
		return false
	}
	var u *UncertainError
	var me *gosql.MySQLError
	var re *mysql.MyError
	var ne net.Error
	switch {
	case errors.As(err, &u):
		return true
	case errors.As(err, &me):
		return me.Number == 1205 || me.Number == 1213 || me.Number == 1927 || me.Number == 2006 || me.Number == 2013
	case errors.As(err, &re):
		return re.Code == 1927 || re.Code == 2006 || re.Code == 2013
	case errors.Is(err, driver.ErrBadConn), errors.Is(err, gosql.ErrInvalidConn), errors.Is(err, mysql.ErrBadConn),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.As(err, &ne):
		return true
	case errors.Is(err, context.DeadlineExceeded):
		return true
	}
	return false
}

// session checks both sides, opens the stream at the stored position and applies until it ends.
func session(parent context.Context, o *RunOptions, fromp **mysql.Position, sum *RunSummary) error {
	from := *fromp
	ctx := parent
	st, err := o.Source.ReadStatus(ctx)
	if err != nil {
		return err
	}
	src, err := o.Source.open()
	if err != nil {
		return err
	}
	defer src.Close()
	var format, image string
	if err := src.QueryRowContext(ctx, "SELECT @@binlog_format, @@binlog_row_image").Scan(&format, &image); err != nil {
		return err
	}
	if format != "ROW" || image != "FULL" {
		return &StopError{"the source must use binlog_format=ROW and binlog_row_image=FULL"}
	}
	dst, err := openTarget(o.Target)
	if err != nil {
		return err
	}
	defer dst.Close()
	names := o.names()
	srcDefs, err := readDefs(ctx, src, names)
	if err != nil {
		return err
	}
	dstDefs, err := readDefs(ctx, dst, names)
	if err != nil {
		return err
	}
	fp, err := sameDefs(srcDefs, dstDefs)
	if err != nil {
		return err
	}
	if err := noTriggersOrForeignKeys(ctx, dst, names); err != nil {
		return err
	}
	saved, err := loadState(ctx, dst, st.ServerUUID)
	if err != nil {
		return err
	}
	tables := strings.Join(names, ",")
	var pos binlogPos
	switch {
	case saved == nil && from == nil:
		return &StopError{"the copy has no stored position; give --from with the position its data was taken at"}
	case saved == nil:
		pos = binlogPos{File: from.Name, Pos: from.Pos}
		if err := initState(ctx, dst, st.ServerUUID, savedState{Position: pos.String(), Tables: tables, SchemaFP: fp}); err != nil {
			return err
		}
		*fromp = nil
		o.Logf("starting at %s:%d (trusted as given until the first transaction is stored with its event checksum)", pos.File, pos.Pos)
	case from != nil && saved.Position == (binlogPos{File: from.Name, Pos: from.Pos}).String() && saved.Tables == tables && saved.SchemaFP == fp:
		// the first run stored it but lost the reply
		pos = binlogPos{File: from.Name, Pos: from.Pos}
		*fromp = nil
	case from != nil:
		return &StopError{"the copy already has a stored position; --from is only for the first run"}
	case saved.Tables != tables:
		return &StopError{"the table list differs from the one stored in the copy (" + saved.Tables + ")"}
	default:
		if saved.SchemaFP != fp {
			return &StopError{"table definitions changed since the copy started following"}
		}
		if pos, err = parsePos(saved.Position); err != nil {
			return err
		}
	}
	if err := continuity(st, pos); err != nil {
		return err
	}
	id, err := pickServerID(o.ServerID, st)
	if err != nil {
		return err
	}
	r := &reader{
		o: o, sink: newTarget(dst, st.ServerUUID, dstDefs), defs: srcDefs, parser: parser.New(),
		version: st.Version, saved: pos, last: pos, sum: sum, open: map[string]*Counts{},
	}
	return r.stream(parent, id)
}

// continuity checks that the stored position is still in a kept file and that no file is missing after it.
func continuity(st Status, p binlogPos) error {
	idx := -1
	for i, f := range st.Files {
		if f.Name == p.File {
			idx = i
			if uint64(p.Pos) > f.Size {
				return &StopError{fmt.Sprintf("stored position %s:%d is past the end of that file; the binlog differs, a fresh copy is needed", p.File, p.Pos)}
			}
		}
	}
	if idx < 0 {
		return &StopError{"binlog file " + p.File + " is no longer kept by the source; a fresh copy is needed"}
	}
	for i := idx + 1; i < len(st.Files); i++ {
		if st.Files[i].Name != nextFile(st.Files[i-1].Name) {
			return &StopError{"binlog files are not consecutive after " + st.Files[i-1].Name + "; a fresh copy is needed"}
		}
	}
	return nil
}

// nextFile is the name MySQL gives the file after name: the numeric suffix plus one, same width.
func nextFile(name string) string {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return ""
	}
	n, err := strconv.ParseUint(name[dot+1:], 10, 64)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s.%0*d", name[:dot], len(name)-dot-1, n+1)
}

// reader turns binlog events into changes for the sink and decides where positions may be stored.
type reader struct {
	o       *RunOptions
	sink    Sink
	defs    map[string]*tableDef
	parser  *parser.Parser
	version int
	sum     *RunSummary

	file     string
	verify   bool      // the first event must match the stored boundary event
	saved    binlogPos // stored in the copy
	last     binlogPos // end of the last finished transaction
	lastSave time.Time
	inTx     atomic.Bool
	sinkOpen bool
	open     map[string]*Counts
	stopping atomic.Bool
}

func (r *reader) stream(parent context.Context, id uint32) error {
	start := r.saved.position()
	r.verify = r.saved.EventStart > 0
	if r.verify {
		start.Pos = r.saved.EventStart
	}
	r.file = start.Name
	r.lastSave = time.Now()
	syncer := replication.NewBinlogSyncer(replication.BinlogSyncerConfig{
		ServerID: id, Flavor: mysql.MySQLFlavor,
		Host: r.o.Source.Host, Port: uint16(r.o.Source.Port), User: r.o.Source.User, Password: r.o.Source.Password,
		TLSConfig: tlsConfig(), DisableRetrySync: true, EventCacheCount: 256,
		HeartbeatPeriod: 30 * time.Second, ReadTimeout: 90 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer func() { go syncer.Close() }()
	streamer, err := syncer.StartSync(start)
	if err != nil {
		return err
	}
	// A stop request ends the stream at once between transactions, or after StopWait inside one.
	// Copy writes and commits run on their own context so a stop does not cut one short,
	// until StopWait plus a grace period, after which they are cancelled too.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	copyCtx, copyCancel := context.WithCancel(context.WithoutCancel(parent))
	defer copyCancel()
	var stopBy time.Time
	stopped, finished := make(chan struct{}), make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-parent.Done():
		case <-finished:
			return
		}
		stopBy = time.Now().Add(r.o.StopWait)
		r.stopping.Store(true)
		close(stopped)
		hard := time.NewTimer(r.o.StopWait + 5*time.Second)
		defer hard.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-finished:
				return
			case <-hard.C:
				cancel()
				copyCancel()
				return
			case <-tick.C:
				if !r.inTx.Load() || time.Now().After(stopBy) {
					cancel()
				}
			}
		}
	}()
	for {
		ev, err := streamer.GetEvent(ctx)
		if err != nil {
			r.sink.Rollback()
			r.sinkOpen = false
			if r.stopping.Load() && errors.Is(err, context.Canceled) {
				<-stopped
				// an unfinished source transaction is dropped; the copy keeps the last finished one
				r.inTx.Store(false)
				clear(r.open)
				saveCtx, done := context.WithDeadline(copyCtx, stopBy.Add(5*time.Second))
				defer done()
				return r.saveLast(saveCtx)
			}
			return err
		}
		done, err := r.handle(copyCtx, ev, ev)
		if err != nil {
			r.sink.Rollback()
			r.sinkOpen = false
			return err
		}
		if done {
			return nil
		}
	}
}

// saveLast stores the end of the last finished transaction if it is past the stored one.
func (r *reader) saveLast(ctx context.Context) error {
	if r.sinkOpen || r.inTx.Load() || r.last.position().Compare(r.saved.position()) <= 0 {
		return nil
	}
	if err := r.sink.Save(ctx, r.last.String()); err != nil {
		return err
	}
	r.saved, r.lastSave = r.last, time.Now()
	r.sum.Saved = r.saved.String()
	return nil
}

// handle applies one event. outer is the event as read from the stream; for events inside a
// compressed transaction it is the payload event, whose end is the position.
func (r *reader) handle(ctx context.Context, ev, outer *replication.BinlogEvent) (bool, error) {
	h := ev.Header
	if rot, ok := ev.Event.(*replication.RotateEvent); ok {
		next := string(rot.NextLogName)
		if h.Timestamp == 0 || h.LogPos == 0 {
			if next != r.file {
				return false, &StopError{"the source started the stream in " + next + " instead of " + r.file}
			}
			return false, nil
		}
		if r.inTx.Load() {
			return false, &StopError{"binlog rotated inside a transaction"}
		}
		if next != nextFile(r.file) || rot.Position != 4 {
			return false, &StopError{fmt.Sprintf("binlog rotated from %s to %s:%d, not the start of the next file", r.file, next, rot.Position)}
		}
		r.file = next
		return false, nil
	}
	switch h.EventType {
	case replication.FORMAT_DESCRIPTION_EVENT, replication.PREVIOUS_GTIDS_EVENT, replication.HEARTBEAT_EVENT, replication.HEARTBEAT_LOG_EVENT_V2,
		replication.ANONYMOUS_GTID_EVENT, replication.GTID_EVENT, replication.TABLE_MAP_EVENT, replication.ROWS_QUERY_EVENT,
		replication.INTVAR_EVENT, replication.RAND_EVENT, replication.USER_VAR_EVENT, replication.STOP_EVENT, replication.IGNORABLE_EVENT:
		return false, nil // no row change of their own; a statement they belong to is judged by its query event
	case replication.QUERY_EVENT, replication.XID_EVENT, replication.TRANSACTION_PAYLOAD_EVENT, replication.PARTIAL_UPDATE_ROWS_EVENT,
		replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2, replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2,
		replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
	default:
		return false, &StopError{fmt.Sprintf("unsupported event %s at %s:%d", h.EventType, r.file, outer.Header.LogPos)}
	}
	if outer.Header.LogPos == 0 {
		return false, &StopError{fmt.Sprintf("%s event without a position in %s", h.EventType, r.file)}
	}
	oh := outer.Header
	pos := binlogPos{File: r.file, Pos: oh.LogPos, EventStart: oh.LogPos - oh.EventSize, EventCRC: crc32.ChecksumIEEE(outer.RawData)}
	if r.verify && ev == outer {
		r.verify = false
		if pos.Pos != r.saved.Pos || pos.EventStart != r.saved.EventStart || pos.EventCRC != r.saved.EventCRC {
			return false, &StopError{"the binlog at the stored position is not the one the copy was built from; a fresh copy is needed"}
		}
		return false, nil
	}
	if pos.position().Compare(r.saved.position()) <= 0 {
		return false, nil // already in the copy
	}
	switch e := ev.Event.(type) {
	case *replication.TransactionPayloadEvent:
		for _, inner := range e.Events {
			if done, err := r.handle(ctx, inner, outer); err != nil || done {
				return done, err
			}
		}
		return false, nil
	case *replication.XIDEvent:
		return r.boundary(ctx, pos, true)
	case *replication.QueryEvent:
		return r.query(ctx, string(e.Schema), string(e.Query), pos)
	case *replication.RowsEvent:
		if h.EventType == replication.PARTIAL_UPDATE_ROWS_EVENT {
			return false, &StopError{"partial update row event (binlog_row_value_options=PARTIAL_JSON is not supported)"}
		}
		return false, r.rows(ctx, e, h)
	}
	return false, nil
}

// query handles BEGIN, COMMIT, ROLLBACK, savepoints and statement-format events.
func (r *reader) query(ctx context.Context, schema, text string, pos binlogPos) (bool, error) {
	q := strings.ToUpper(strings.TrimSpace(strings.TrimRight(strings.TrimSpace(skipComments(text, r.version)), ";")))
	switch {
	case q == "BEGIN":
		if r.inTx.Load() {
			return false, &StopError{"BEGIN inside an open transaction"}
		}
		r.inTx.Store(true)
		return false, nil
	case q == "COMMIT":
		return r.boundary(ctx, pos, true)
	case q == "ROLLBACK":
		if r.sinkOpen {
			r.sink.Rollback()
			r.sinkOpen = false
		}
		clear(r.open)
		return r.boundary(ctx, pos, false)
	case savepoint(q):
		return false, nil
	case strings.HasPrefix(q, "XA "):
		return false, &StopError{"XA transactions are not supported"}
	}
	if err := judgeStatement(r.parser, schema, text); err != nil {
		return false, &StopError{fmt.Sprintf("%s at %s:%d", err, pos.File, pos.Pos)}
	}
	if r.inTx.Load() {
		return false, nil
	}
	return r.boundary(ctx, pos, false)
}

// boundary ends a source transaction at pos: commits the copy transaction with the position, or just
// remembers the position and saves it now and then.
func (r *reader) boundary(ctx context.Context, pos binlogPos, committed bool) (bool, error) {
	if r.sinkOpen {
		r.sinkOpen = false
		err := r.sink.Commit(ctx, pos.String())
		r.inTx.Store(false)
		if err != nil {
			clear(r.open)
			return false, err
		}
		for n, c := range r.open {
			r.sum.Applied[n].add(c)
		}
		clear(r.open)
		r.saved, r.last, r.lastSave = pos, pos, time.Now()
		r.sum.Saved = pos.String()
	} else {
		r.inTx.Store(false)
		r.last = pos
		if committed {
			r.sum.Skipped++
		}
		// the first stored position gets its event checksum as soon as possible
		if r.saved.EventStart == 0 || time.Since(r.lastSave) >= r.o.SaveEvery {
			if err := r.saveLast(ctx); err != nil {
				return false, err
			}
		}
	}
	if r.stopping.Load() {
		return true, r.saveLast(ctx)
	}
	return false, nil
}

// rowKind maps a MySQL row event type to the change it carries; 0 for anything else.
func rowKind(t replication.EventType) ChangeKind {
	switch t {
	case replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2:
		return Insert
	case replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2:
		return Update
	case replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		return Delete
	}
	return 0
}

// rows turns a row event of a followed table into changes and applies them.
func (r *reader) rows(ctx context.Context, e *replication.RowsEvent, h *replication.EventHeader) error {
	key := string(e.Table.Schema) + "." + string(e.Table.Table)
	d := r.defs[key]
	if d == nil {
		return nil
	}
	at := fmt.Sprintf(" at %s:%d", r.file, h.LogPos)
	if !r.inTx.Load() {
		return &StopError{key + ": row event outside a transaction" + at}
	}
	for _, s := range e.SkippedColumns {
		if len(s) > 0 {
			return &StopError{key + ": row image is not full (binlog_row_image must be FULL)" + at}
		}
	}
	rows := make([][]any, len(e.Rows))
	for i, row := range e.Rows {
		if len(row) != len(d.Columns) {
			return &StopError{fmt.Sprintf("%s: row has %d columns but the table has %d%s", key, len(row), len(d.Columns), at)}
		}
		out := make([]any, len(row))
		for j, v := range row {
			c := d.Columns[j]
			if c.DataType == "enum" && v != nil {
				n, ok := v.(int64)
				if !ok || n < 1 || int(n) > len(c.Enum) {
					return &StopError{fmt.Sprintf("%s.%s: enum value number out of range%s", key, c.Name, at)}
				}
				v = c.Enum[n-1]
			}
			out[j] = v
		}
		rows[i] = out
	}
	if !r.sinkOpen {
		if err := r.sink.Begin(ctx); err != nil {
			return err
		}
		r.sinkOpen = true
	}
	cnt := r.open[key]
	if cnt == nil {
		cnt = &Counts{}
		r.open[key] = cnt
	}
	schema, table := string(e.Table.Schema), string(e.Table.Table)
	switch rowKind(h.EventType) {
	case Insert:
		for _, row := range rows {
			if err := r.sink.Apply(ctx, &Change{Schema: schema, Table: table, Kind: Insert, After: row}); err != nil {
				return err
			}
			cnt.Inserts++
		}
	case Delete:
		for _, row := range rows {
			if err := r.sink.Apply(ctx, &Change{Schema: schema, Table: table, Kind: Delete, Before: row}); err != nil {
				return err
			}
			cnt.Deletes++
		}
	case Update:
		if len(rows)%2 != 0 {
			return &StopError{key + ": update event has an odd number of row images" + at}
		}
		for i := 0; i < len(rows); i += 2 {
			if err := r.sink.Apply(ctx, &Change{Schema: schema, Table: table, Kind: Update, Before: rows[i], After: rows[i+1]}); err != nil {
				return err
			}
			cnt.Updates++
		}
	default:
		return &StopError{key + ": unsupported row event" + at}
	}
	return nil
}
