package cdc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"syscall"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	gosql "github.com/go-sql-driver/mysql"
)

// InspectOptions bounds one read of the binlog.
type InspectOptions struct {
	Source      Source
	Filter      Filter
	FromFile    string         // empty: oldest kept file
	To          mysql.Position // zero: current position when the read starts; not included
	MaxBytes    int64
	MaxDuration time.Duration
	ServerID    uint32 // 0: random, checked against the source and its replicas
	Location    *time.Location
}

// Counts are the changed rows of one table in committed transactions.
type Counts struct {
	Inserts    int64 `json:"inserts"`
	Updates    int64 `json:"updates"`
	Deletes    int64 `json:"deletes"`
	KeyChanges int64 `json:"key_changes"`
	RowBytes   int64 `json:"row_bytes"`
}

func (c *Counts) add(o *Counts) {
	c.Inserts += o.Inserts
	c.Updates += o.Updates
	c.Deletes += o.Deletes
	c.KeyChanges += o.KeyChanges
	c.RowBytes += o.RowBytes
}

func (c *Counts) rows() int64 { return c.Inserts + c.Updates + c.Deletes }

// Report is the result of Inspect. Only committed transactions are counted.
type Report struct {
	SourceUUID      string             `json:"source_uuid"`
	SourceServerID  uint32             `json:"source_server_id"`
	ServerID        uint32             `json:"server_id"`
	DatabaseInclude []string           `json:"database_include_list"`
	TableInclude    []string           `json:"table_include_list,omitempty"`
	TablesSelected  []string           `json:"tables_selected"`
	From            mysql.Position     `json:"from"`
	To              mysql.Position     `json:"to"`
	Counted         mysql.Position     `json:"counted"`
	Read            mysql.Position     `json:"read"`
	Complete        bool               `json:"complete"`
	Stop            string             `json:"stop"`
	Error           string             `json:"error,omitempty"`
	Zone            string             `json:"zone"`
	FirstCommit     *time.Time         `json:"first_commit,omitempty"`
	LastCommit      *time.Time         `json:"last_commit,omitempty"`
	Commits         int64              `json:"commits"`
	TargetCommits   int64              `json:"target_commits"`
	Rollbacks       int64              `json:"rollbacks"`
	DroppedOpen     bool               `json:"dropped_open_transaction"`
	Compressed      int64              `json:"compressed_transactions"`
	StreamBytes     int64              `json:"stream_bytes"`
	Tables          map[string]*Counts `json:"tables"`
	Statements      map[string]int64   `json:"statements"`
	StatementNote   string             `json:"statement_note"`
	Hours           map[string]int64   `json:"hours"`
	NotObserved     []string           `json:"not_observed"`
}

// StatementNote explains what statement records mean and what they miss.
const StatementNote = "Statement records are statement-format changes in followed databases; their rows are not in the table counts. " +
	"A statement is matched by the database selected when it ran, so one that names a followed table while another database is selected is not seen."

// Inspect reads the binlog between two positions and counts changed rows per table.
// It applies nothing; the report holds counts only.
func Inspect(parent context.Context, o InspectOptions) (*Report, error) {
	ctx, cancel := parent, context.CancelFunc(func() {})
	if o.MaxDuration > 0 {
		ctx, cancel = context.WithTimeout(parent, o.MaxDuration)
	}
	defer cancel()
	setupErr := func(stage string, err error) error {
		if parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &Error{"time limit reached before reading, while " + stage}
		}
		return SafeError(err)
	}
	st, err := o.Source.ReadStatus(ctx)
	if err != nil {
		return nil, setupErr("reading the source status", err)
	}
	if len(st.Files) == 0 {
		return nil, &Error{"the source keeps no binlog files"}
	}
	from := o.FromFile
	if from == "" {
		from = st.Files[0].Name
	}
	to := o.To
	if to.Name == "" {
		to = st.Current
	}
	start := mysql.Position{Name: from, Pos: 4}
	switch {
	case !st.Has(from):
		return nil, &Error{"binlog file " + from + " is no longer kept by the source"}
	case !st.Has(to.Name):
		return nil, &Error{"binlog file " + to.Name + " is not kept by the source"}
	case start.Compare(to) >= 0:
		return nil, &Error{fmt.Sprintf("nothing to read: %s is not before %s", start, to)}
	}
	selected, err := o.Source.SelectTables(ctx, o.Filter)
	if err != nil {
		return nil, setupErr("listing the tables to follow", err)
	}
	id, err := pickServerID(o.ServerID, st)
	if err != nil {
		return nil, err
	}
	loc := o.Location
	if loc == nil {
		loc = time.Local
	}
	rep := &Report{SourceUUID: st.ServerUUID, SourceServerID: st.ServerID, ServerID: id,
		DatabaseInclude: o.Filter.Databases, TableInclude: o.Filter.Tables, From: start, To: to, Zone: loc.String()}
	rep.StatementNote = StatementNote
	t := newTally(o.Filter, selected, loc, st.Version, rep)
	syncer := replication.NewBinlogSyncer(replication.BinlogSyncerConfig{
		ServerID: id, Flavor: mysql.MySQLFlavor,
		Host: o.Source.Host, Port: uint16(o.Source.Port), User: o.Source.User, Password: o.Source.Password,
		TLSConfig: tlsConfig(), DisableRetrySync: true,
		HeartbeatPeriod: 30 * time.Second, ReadTimeout: 90 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// Closing waits for a start in progress, so it never holds up the return.
	defer func() { go syncer.Close() }()
	type started struct {
		s   *replication.BinlogStreamer
		err error
	}
	ch := make(chan started, 1)
	go func() {
		s, err := syncer.StartSync(start)
		ch <- started{s, err}
	}()
	var streamer *replication.BinlogStreamer
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, setupErr("starting the binlog stream", r.err)
		}
		streamer = r.s
	case <-ctx.Done():
		return nil, setupErr("starting the binlog stream", ctx.Err())
	}
	file := from
	stop := func(reason string, err error) {
		rep.Stop = reason
		if err != nil {
			rep.Error = err.Error()
		}
	}
	for rep.Stop == "" {
		ev, err := streamer.GetEvent(ctx)
		if err != nil {
			switch {
			case parent.Err() != nil:
				stop("cancelled", nil)
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				stop("max-seconds", nil)
			default:
				stop("error", SafeError(err))
			}
			continue
		}
		h := ev.Header
		if rot, ok := ev.Event.(*replication.RotateEvent); ok {
			file = string(rot.NextLogName)
			continue
		}
		switch h.EventType {
		case replication.FORMAT_DESCRIPTION_EVENT, replication.HEARTBEAT_EVENT, replication.HEARTBEAT_LOG_EVENT_V2:
			continue
		}
		if h.LogPos == 0 {
			continue
		}
		pos := mysql.Position{Name: file, Pos: h.LogPos}
		if pos.Compare(to) > 0 {
			stop("error", &Error{fmt.Sprintf("passed the end %s at %s without reaching it", to, pos)})
			break
		}
		rep.StreamBytes += int64(len(ev.RawData))
		if err := t.handle(ev, h.Timestamp, pos); err != nil {
			stop("error", err)
			break
		}
		rep.Read = pos
		switch {
		case pos.Compare(to) == 0:
			stop("to", nil)
		case o.MaxBytes > 0 && rep.StreamBytes >= o.MaxBytes:
			stop("max-bytes", nil)
		}
	}
	t.finish()
	rep.Complete = rep.Stop == "to"
	return rep, nil
}

// SafeError keeps only fixed information about an error: our own messages, MySQL error numbers,
// event types and positions, and the kind of network failure. Driver text and event bodies are dropped.
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	var own *Error
	if errors.As(err, &own) {
		return own
	}
	var ee *replication.EventError
	if errors.As(err, &ee) {
		if ee.Header == nil {
			return &Error{"could not parse a binlog event"}
		}
		return &Error{fmt.Sprintf("could not parse a %s event ending at %d", ee.Header.EventType, ee.Header.LogPos)}
	}
	var me *gosql.MySQLError
	if errors.As(err, &me) {
		return mysqlError(me.Number)
	}
	var re *mysql.MyError
	if errors.As(err, &re) {
		return mysqlError(re.Code)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return &Error{fmt.Sprintf("%s %s: %s", pe.Op, pe.Path, pe.Err)}
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{"time limit reached"}
	case errors.Is(err, context.Canceled):
		return &Error{"cancelled"}
	case errors.Is(err, syscall.ECONNREFUSED):
		return &Error{"connection refused"}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return &Error{"connection closed by the server"}
	case errors.As(err, &ne) && ne.Timeout():
		return &Error{"network timeout"}
	case errors.As(err, &ne):
		return &Error{"network error"}
	}
	return &Error{fmt.Sprintf("error of type %T", err)}
}

func mysqlError(code uint16) error {
	hint := map[uint16]string{
		1044: "access denied to the database", 1045: "access denied (user or password)",
		1142: "missing table privilege", 1227: "missing privilege",
		1236: "binlog position is not available on the source",
	}[code]
	if hint == "" {
		return &Error{fmt.Sprintf("MySQL error %d", code)}
	}
	return &Error{fmt.Sprintf("MySQL error %d: %s", code, hint)}
}

// pickServerID returns the given ID, or a random one at or above 2^31, if it clashes with neither the source nor its replicas.
func pickServerID(want uint32, st Status) (uint32, error) {
	taken := map[uint32]bool{0: true, st.ServerID: true}
	for _, id := range st.ReplicaIDs {
		taken[id] = true
	}
	if want != 0 {
		if taken[want] {
			return 0, &Error{fmt.Sprintf("server id %d is already used by the source or a replica", want)}
		}
		return want, nil
	}
	for {
		id := uint32(1<<31) + rand.Uint32N(math.MaxUint32-(1<<31))
		if !taken[id] {
			return id, nil
		}
	}
}
