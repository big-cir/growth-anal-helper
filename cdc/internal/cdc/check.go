package cdc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// CheckOptions configures comparing the followed tables of the source and the copy.
type CheckOptions struct {
	Source   Source
	Target   Source
	Database string
	Tables   []string
	// WritesStopped: no source transaction is open or starts until the check ends, so a
	// difference is a mismatch; otherwise it is only a suspect.
	WritesStopped bool
	ChunkRows     int           // rows per chunk at most
	Wait          time.Duration // how long the copy may take to reach the source position
	Rounds        int           // times a differing row is read again while the source writes
	RoundGap      time.Duration
	MaxTime       time.Duration
	Logf          func(format string, a ...any)
}

// TableCheck counts what the check found in one table. Rows are counted, never shown.
type TableCheck struct {
	Chunks    int64 `json:"chunks"`
	Rows      int64 `json:"rows"`      // source rows in the compared chunks
	Missing   int64 `json:"missing"`   // on the source, not in the copy
	Extra     int64 `json:"extra"`     // in the copy only
	Differ    int64 `json:"differ"`    // in both with other values
	Undecided int64 `json:"undecided"` // chunks not decided: the copy lagged or too many rows differed
	Unchecked bool  `json:"unchecked"` // the time limit stopped the check in this table
}

func (t *TableCheck) differs() bool { return t.Missing+t.Extra+t.Differ > 0 }

// CheckReport is the result: with WritesStopped differences are mismatches, otherwise suspects.
type CheckReport struct {
	WritesStopped bool                   `json:"writes_stopped"`
	Provisional   bool                   `json:"provisional,omitempty"` // the source moved during a writes-stopped check
	Position      string                 `json:"position,omitempty"`    // the source position the copy reached, writes stopped only
	Tables        map[string]*TableCheck `json:"tables"`
}

// Differs reports rows that differed: mismatches with WritesStopped, suspects otherwise.
func (r *CheckReport) Differs() bool {
	for _, t := range r.Tables {
		if t.differs() {
			return true
		}
	}
	return false
}

// Undecided reports chunks or tables the check could not decide.
func (r *CheckReport) Undecided() bool {
	for _, t := range r.Tables {
		if t.Undecided > 0 || t.Unchecked {
			return true
		}
	}
	return false
}

// ExitCode is 0 when every chunk was the same, 3 with a mismatch (writes stopped and final),
// 4 with suspects or undecided chunks and no mismatch, and 4 for any provisional result.
func (r *CheckReport) ExitCode() int {
	switch {
	case r.Provisional:
		return 4
	case r.Differs() && r.WritesStopped && !r.Provisional:
		return 3
	case r.Differs(), r.Undecided():
		return 4
	}
	return 0
}

const maxSuspects = 1000 // differing rows per chunk kept for another look

// eventPage is how many binlog events one SHOW BINLOG EVENTS reads.
var eventPage = 1000

type checker struct {
	o        CheckOptions
	src, dst *sql.Conn
	uuid     string
	pt       binlogPos     // writes stopped: the copy position every read must see
	srcTook  time.Duration // the last source query of read
	why      string        // why the last read could not decide
	// the binlog from quietFrom up to quietTo holds no transaction
	quietFrom, quietTo mysql.Position
}

// checkTable is the SQL for one table: its name, key, chunk hash and row hashes.
type checkTable struct {
	srcName, dstName string // quoted db.table
	keys             []string
	ints             []bool // key column is an integer
	chunk            string // SELECT count, hashed count, xor of row hashes FROM ... WHERE %s
	rows             string // SELECT key bytes, row hash FROM ... WHERE %s
	bound            string // SELECT key columns FROM ... WHERE %s ORDER BY key LIMIT 1 OFFSET ?
}

// Check compares the followed tables chunk by chunk and counts differing rows.
func Check(ctx context.Context, o CheckOptions) (*CheckReport, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.ChunkRows == 0 {
		o.ChunkRows = 10000
	}
	if o.Wait == 0 {
		o.Wait = time.Minute
	}
	if o.Rounds == 0 {
		o.Rounds = 3
	}
	if o.RoundGap == 0 {
		o.RoundGap = 10 * time.Second
	}
	if o.MaxTime == 0 {
		o.MaxTime = time.Hour
	}
	ro := RunOptions{Database: o.Database, Tables: o.Tables}
	names := ro.names()
	rep := &CheckReport{WritesStopped: o.WritesStopped, Tables: map[string]*TableCheck{}}
	// past MaxTime the tables left are reported unchecked, not as an error
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, o.MaxTime)
	defer cancel()
	timedOut := func(err error) bool { return err != nil && ctx.Err() != nil && parent.Err() == nil }
	unchecked := func() (*CheckReport, error) {
		for _, n := range names {
			if rep.Tables[n] == nil {
				rep.Tables[n] = &TableCheck{}
			}
			if rep.Tables[n].Chunks == 0 {
				rep.Tables[n].Unchecked = true
			}
		}
		o.Logf("stopped by the time limit")
		return rep, nil
	}
	out, err := check(ctx, o, names, rep)
	if timedOut(err) && !rep.Provisional { // a provisional result says why it is not final
		return unchecked()
	}
	return out, err
}

func check(ctx context.Context, o CheckOptions, names []string, rep *CheckReport) (*CheckReport, error) {
	st, err := o.Source.ReadStatus(ctx)
	if err != nil {
		return nil, err
	}
	srcDB, err := o.Source.open()
	if err != nil {
		return nil, err
	}
	defer srcDB.Close()
	dstDB, err := openTarget(o.Target)
	if err != nil {
		return nil, err
	}
	defer dstDB.Close()

	saved, err := loadState(ctx, dstDB, st.ServerUUID, o.Database)
	if err != nil {
		return nil, err
	}
	switch {
	case saved == nil:
		return nil, &StopError{"the copy has no stored position for this source"}
	case saved.Phase != phaseReady:
		return nil, &StopError{"the copy is not ready (" + saved.Phase + "); finish init first"}
	case saved.Tables != strings.Join(names, ","):
		return nil, &StopError{"the table list differs from the one stored in the copy (" + saved.Tables + ")"}
	}
	srcDefs, err := readDefs(ctx, srcDB, "", names)
	if err != nil {
		return nil, err
	}
	if fingerprint(srcDefs) != saved.SchemaFP {
		return nil, &StopError{"table definitions changed since the copy started following"}
	}
	dstDefs, err := readDefs(ctx, dstDB, saved.DataDB, names)
	if err != nil {
		return nil, err
	}
	if err := sameDefs(srcDefs, dstDefs, true); err != nil {
		return nil, err
	}

	c := &checker{o: o, uuid: st.ServerUUID}
	if c.src, err = srcDB.Conn(ctx); err != nil {
		return nil, err
	}
	defer c.src.Close()
	if c.dst, err = dstDB.Conn(ctx); err != nil {
		return nil, err
	}
	defer c.dst.Close()
	for _, conn := range []*sql.Conn{c.src, c.dst} {
		if _, err := conn.ExecContext(ctx, "SET NAMES utf8mb4"); err != nil {
			return nil, err
		}
	}
	if _, err := c.dst.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		return nil, err
	}
	var iso string
	if err := c.dst.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&iso); err != nil {
		return nil, err
	}
	if iso != "REPEATABLE-READ" {
		return nil, &StopError{"the copy connection is not REPEATABLE READ (" + iso + ")"}
	}

	var q mysql.Position
	if o.WritesStopped {
		if q, err = c.sourcePos(ctx); err != nil {
			return nil, err
		}
		if c.pt, err = c.reach(ctx, q); err != nil {
			return nil, err
		}
		rep.Position = q.Name + ":" + strconv.FormatUint(uint64(q.Pos), 10)
		o.Logf("the copy reached %s", rep.Position)
	}
	var stopped error // the time limit, after which the tables left are unchecked
	for _, n := range names {
		tc := &TableCheck{}
		rep.Tables[n] = tc
		if stopped != nil {
			tc.Unchecked = true
			continue
		}
		_, table, _ := strings.Cut(n, ".")
		ct, err := checkSQL(srcDefs[n], quoteName(o.Database)+"."+quoteName(table), quoteName(saved.DataDB)+"."+quoteName(table))
		if err != nil {
			return nil, err
		}
		if err := c.table(ctx, ct, tc); err != nil {
			if ctx.Err() == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, err
			}
			tc.Unchecked, stopped = true, err
			o.Logf("%s: stopped by the time limit", n)
			continue
		}
		o.Logf("%s: %d chunks, %d rows", n, tc.Chunks, tc.Rows)
	}
	if o.WritesStopped {
		// after the time limit the connections may be gone: a new one with its own short limit
		fctx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		var again mysql.Position
		conn, err := srcDB.Conn(fctx)
		if err == nil {
			again, err = sourcePos(fctx, conn)
			conn.Close()
		}
		switch {
		case err != nil:
			rep.Provisional = true
			if stopped == nil {
				return rep, err
			}
		case again.Compare(q) != 0:
			rep.Provisional = true
			if stopped == nil {
				return rep, &StopError{"the source was written during the check; the result is not final"}
			}
		}
	}
	return rep, nil
}

// checkSQL builds the queries for a table whose definition is the same on both sides.
func checkSQL(d *tableDef, srcName, dstName string) (*checkTable, error) {
	ct := &checkTable{srcName: srcName, dstName: dstName}
	var parts, keyParts, order []string
	for _, c := range d.Columns {
		parts = append(parts, hashPart(c))
	}
	for _, i := range d.PK {
		c := d.Columns[i]
		if c.DataType == "enum" {
			return nil, &StopError{"check does not support an enum column in a primary key"}
		}
		q := quoteName(c.Name)
		ct.keys = append(ct.keys, q)
		ct.ints = append(ct.ints, c.DataType != "datetime" && c.DataType != "varchar")
		keyParts = append(keyParts, hashPart(c))
		order = append(order, q+" ASC")
	}
	hash := "MD5(CONCAT(" + strings.Join(parts, ", ") + "))"
	key := "CONCAT(" + strings.Join(keyParts, ", ") + ")"
	// %t is the table on either side, %w the chunk condition
	ct.chunk = "SELECT COUNT(*), COUNT(h), IFNULL(BIT_XOR(CAST(CONV(LEFT(h, 16), 16, 10) AS UNSIGNED)), 0) FROM (SELECT " + hash + " AS h FROM %t WHERE %w) x"
	ct.rows = "SELECT " + key + ", " + hash + " FROM %t WHERE %w"
	ct.bound = "SELECT " + strings.Join(ct.keys, ", ") + " FROM " + srcName + " WHERE %w ORDER BY " + strings.Join(order, ", ") + " LIMIT 1 OFFSET ?"
	return ct, nil
}

// hashPart encodes one column so that different values never give the same bytes:
// a null flag, the byte length and the bytes.
func hashPart(c column) string {
	q := quoteName(c.Name)
	conv := "CAST(" + q + " AS CHAR)"
	switch c.DataType {
	case "varchar":
		conv = "CAST(" + q + " AS BINARY)" // the column's bytes, not converted
	case "enum":
		conv = "CAST(" + q + " + 0 AS CHAR)" // the index: tells the error value 0 from an empty item
	}
	v := "IFNULL(" + conv + ", _binary'')"
	return "ISNULL(" + q + "), ':', LENGTH(" + v + "), ':', " + v
}

// span is a chunk: from lo (none: no lower bound) up to hi (none: no upper bound), hi excluded.
type span struct{ lo, hi []any }

func (ct *checkTable) where(s span) (string, []any) {
	var conds []string
	var args []any
	tuple := "(" + strings.Join(ct.keys, ", ") + ")"
	marks := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(ct.keys)), ", ") + ")"
	if s.lo != nil {
		conds, args = append(conds, tuple+" >= "+marks), append(args, s.lo...)
	}
	if s.hi != nil {
		conds, args = append(conds, tuple+" < "+marks), append(args, s.hi...)
	}
	if len(conds) == 0 {
		return "TRUE", nil
	}
	return strings.Join(conds, " AND "), args
}

func (ct *checkTable) sql(tmpl string, s span, dst bool) (string, []any) {
	w, args := ct.where(s)
	name := ct.srcName
	if dst {
		name = ct.dstName
	}
	return strings.NewReplacer("%t", name, "%w", w).Replace(tmpl), args
}

// nextBound is the key chunkRows rows past lo on the source, or nil when the rest fits in one chunk.
func (c *checker) nextBound(ctx context.Context, ct *checkTable, lo []any, n int) ([]any, error) {
	q, args := ct.sql(ct.bound, span{lo: lo}, false)
	vals := make([]any, len(ct.keys))
	ptrs := make([]any, len(ct.keys))
	ints := make([]int64, len(ct.keys))
	strs := make([]string, len(ct.keys))
	for i := range ptrs {
		if ct.ints[i] {
			ptrs[i] = &ints[i]
		} else {
			ptrs[i] = &strs[i]
		}
	}
	err := c.src.QueryRowContext(ctx, q, append(args, n)...).Scan(ptrs...)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for i := range vals {
		if ct.ints[i] {
			vals[i] = ints[i]
		} else {
			vals[i] = strs[i]
		}
	}
	return vals, nil
}

// rowLimit is how many rows a chunk may hold on either side to be read row by row.
func (c *checker) rowLimit() int { return 4*c.o.ChunkRows + maxSuspects }

// suspect is a chunk whose differing rows are read again later.
type suspect struct {
	s       span
	keys    map[string]byte // key bytes → 'm' missing, 'e' extra, 'd' differ
	decided bool            // the last look at the chunk finished
	chunk   int64
}

func (c *checker) table(ctx context.Context, ct *checkTable, tc *TableCheck) error {
	var lo []any
	n := c.o.ChunkRows
	var suspects []suspect
	limit := int64(c.rowLimit())
	for {
		hi, err := c.nextBound(ctx, ct, lo, n)
		if err != nil {
			return err
		}
		s := span{lo: lo, hi: hi}
		src, dst, ok, err := c.read(ctx, ct, ct.chunk, s, 0)
		took := c.srcTook // the chunk hash query only; a row diff below reads again
		if err != nil {
			return err
		}
		tc.Chunks++
		undecided := func(why string) {
			tc.Undecided++
			c.o.Logf("%s chunk %d undecided: %s", ct.srcName, tc.Chunks, why)
		}
		switch {
		case !ok:
			undecided(c.why)
		default:
			if len(src) != 1 || len(dst) != 1 {
				return &Error{"chunk hash: expected one row"}
			}
			if src[0][0] != src[0][1] || dst[0][0] != dst[0][1] {
				return &StopError{"MD5() returned NULL; check cannot hash rows on this server"}
			}
			rows, _ := strconv.ParseInt(src[0][0], 10, 64)
			copyRows, _ := strconv.ParseInt(dst[0][0], 10, 64)
			tc.Rows += rows
			if src[0][0] == dst[0][0] && src[0][2] == dst[0][2] {
				break
			}
			if rows > limit || copyRows > limit {
				undecided(fmt.Sprintf("the hashes differ and the chunk has more than %d rows", limit))
				break
			}
			keys, ok, err := c.diff(ctx, ct, s)
			if err != nil {
				return err
			}
			switch {
			case !ok:
				undecided(c.why)
			case c.o.WritesStopped:
				count(tc, keys)
			case len(keys) > maxSuspects:
				undecided(fmt.Sprintf("%d rows differ, more than %d", len(keys), maxSuspects))
			case len(keys) > 0:
				suspects = append(suspects, suspect{s: s, keys: keys, decided: true, chunk: tc.Chunks})
			}
		}
		// keep one source query near half a second
		if took > 500*time.Millisecond && n > 100 {
			n /= 2
		} else if took < 100*time.Millisecond && n < c.o.ChunkRows {
			n = min(n*2, c.o.ChunkRows)
		}
		if hi == nil {
			break
		}
		lo = hi
	}
	suspects, err := recheck(ctx, suspects, c.o.Rounds, c.o.RoundGap, func(s span) (map[string]byte, bool, error) {
		return c.diff(ctx, ct, s)
	})
	if err != nil {
		return err
	}
	for _, sp := range suspects {
		if sp.decided {
			count(tc, sp.keys)
		} else {
			tc.Undecided++
			c.o.Logf("%s chunk %d undecided: its rows still differed or could not be read after %d more looks", ct.srcName, sp.chunk, c.o.Rounds)
		}
	}
	return nil
}

// recheck looks at the suspect chunks again rounds times, gap apart. A row stays a suspect while
// every finished look finds it differing; a chunk whose last look did not finish is not decided.
func recheck(ctx context.Context, suspects []suspect, rounds int, gap time.Duration, look func(span) (map[string]byte, bool, error)) ([]suspect, error) {
	for round := 0; round < rounds && len(suspects) > 0; round++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(gap):
		}
		var still []suspect
		for _, sp := range suspects {
			keys, ok, err := look(sp.s)
			if err != nil {
				return nil, err
			}
			sp.decided = ok
			if !ok {
				still = append(still, sp)
				continue
			}
			for k := range sp.keys {
				if kind, ok := keys[k]; ok {
					sp.keys[k] = kind
				} else {
					delete(sp.keys, k)
				}
			}
			if len(sp.keys) > 0 {
				still = append(still, sp)
			}
		}
		suspects = still
	}
	return suspects, nil
}

func count(tc *TableCheck, keys map[string]byte) {
	for _, kind := range keys {
		switch kind {
		case 'm':
			tc.Missing++
		case 'e':
			tc.Extra++
		default:
			tc.Differ++
		}
	}
}

// diff reads the rows of a chunk on both sides and returns the keys that differ, by kind.
func (c *checker) diff(ctx context.Context, ct *checkTable, s span) (map[string]byte, bool, error) {
	src, dst, ok, err := c.read(ctx, ct, ct.rows, s, c.rowLimit())
	if err != nil || !ok {
		return nil, ok, err
	}
	return diffRows(src, dst), true, nil
}

// diffRows compares rows of key bytes and row hash; it keys a map instead of merging in order,
// since Go orders strings by bytes and MySQL by the column collation.
func diffRows(src, dst [][]string) map[string]byte {
	in := make(map[string]string, len(src))
	for _, r := range src {
		in[r[0]] = r[1]
	}
	out := map[string]byte{}
	seen := make(map[string]bool, len(dst))
	for _, r := range dst {
		seen[r[0]] = true
		h, ok := in[r[0]]
		switch {
		case !ok:
			out[r[0]] = 'e'
		case h != r[1]:
			out[r[0]] = 'd'
		}
	}
	for k := range in {
		if !seen[k] {
			out[k] = 'm'
		}
	}
	return out
}

// read runs a query on both sides so that the copy is read at a known position: with writes
// stopped, the one it reached; otherwise one that has applied every transaction before the source
// position after the source read, since a row the source read showed was written to the binlog before it.
// ok is false when the copy did not get there in time or a side has more than max rows.
func (c *checker) read(ctx context.Context, ct *checkTable, tmpl string, s span, max int) (src, dst [][]string, ok bool, err error) {
	q, args := ct.sql(tmpl, s, false)
	started := time.Now()
	if src, err = connRows(ctx, c.src, q, max, args...); err == errTooManyRows {
		c.why = fmt.Sprintf("the source has more than %d rows here", max)
		return nil, nil, false, nil
	} else if err != nil {
		return nil, nil, false, err
	}
	c.srcTook = time.Since(started)
	want := c.pt.position()
	if !c.o.WritesStopped {
		if want, err = c.sourcePos(ctx); err != nil {
			return nil, nil, false, err
		}
		if ok, err = c.waitCopy(ctx, want); err != nil || !ok {
			return nil, nil, ok, err
		}
	}
	q, args = ct.sql(tmpl, s, true)
	pt, dst, err := c.copyRead(ctx, q, args, max)
	if err == errTooManyRows {
		c.why = fmt.Sprintf("the copy has more than %d rows here", max)
		return nil, nil, false, nil
	} else if err != nil {
		return nil, nil, false, err
	}
	if c.o.WritesStopped && pt.position().Compare(want) != 0 {
		return nil, nil, false, &StopError{"the copy moved during the check; the source was written"}
	}
	return src, dst, true, nil
}

// copyRead reads the stored position and the query's rows from one snapshot of the copy.
func (c *checker) copyRead(ctx context.Context, q string, args []any, max int) (binlogPos, [][]string, error) {
	if _, err := c.dst.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY"); err != nil {
		return binlogPos{}, nil, err
	}
	defer c.dst.ExecContext(context.Background(), "ROLLBACK")
	pt, err := c.copyPos(ctx)
	if err != nil {
		return binlogPos{}, nil, err
	}
	rows, err := connRows(ctx, c.dst, q, max, args...)
	if err != nil {
		return binlogPos{}, nil, err
	}
	_, err = c.dst.ExecContext(ctx, "COMMIT")
	return pt, rows, err
}

func (c *checker) copyPos(ctx context.Context) (binlogPos, error) {
	var s string
	if err := c.dst.QueryRowContext(ctx, "SELECT position FROM "+positionTable+" WHERE source_id = ?", c.uuid).Scan(&s); err != nil {
		return binlogPos{}, err
	}
	return parsePos(s)
}

// waitCopy waits until the copy has applied every transaction before want: its stored position
// is at or past want, or only events without a transaction lie between the two. run stores the
// end of the last finished transaction, so after a rotation or a quiet spell it can stay behind
// want with nothing left to apply. The binlog is read once the copy has lagged 200ms, from
// where the last quiet stretch ended, and again only after the copy's position moved.
func (c *checker) waitCopy(ctx context.Context, want mysql.Position) (bool, error) {
	end := time.Now().Add(c.o.Wait)
	var looked mysql.Position // the copy position with transactions after it, read from the binlog
	lookAfter := time.Now().Add(200 * time.Millisecond)
	for {
		pt, err := c.copyPos(ctx)
		if err != nil {
			return false, err
		}
		p := pt.position()
		if p.Compare(want) >= 0 {
			return true, nil
		}
		if p != looked && time.Now().After(lookAfter) {
			from := p
			if p == c.quietFrom && c.quietTo.Compare(want) <= 0 {
				from = c.quietTo
			}
			done, err := c.noTransactions(ctx, from, want)
			if err != nil {
				return false, err
			}
			if done {
				c.quietFrom, c.quietTo = p, want
				return true, nil
			}
			looked, lookAfter = p, time.Now().Add(time.Second)
		}
		if time.Now().After(end) {
			c.why = fmt.Sprintf("the copy stayed at %s:%d, behind the source position %s:%d, for %s", p.Name, p.Pos, want.Name, want.Pos, c.o.Wait)
			if looked == p {
				c.why += ", with transactions between them"
			} else {
				c.why += "; the binlog between them was not read"
			}
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c *checker) sourcePos(ctx context.Context) (mysql.Position, error) {
	return sourcePos(ctx, c.src)
}

// reach waits until the copy has applied every transaction before q and returns its position.
// run stores the end of the last finished transaction, so after a rotation with no new
// transaction the stored position stays behind q; the events in between show whether any
// transaction is left.
func (c *checker) reach(ctx context.Context, q mysql.Position) (binlogPos, error) {
	end := time.Now().Add(c.o.Wait)
	for {
		pt, err := c.copyPos(ctx)
		if err != nil {
			return binlogPos{}, err
		}
		switch d := pt.position().Compare(q); {
		case d > 0:
			return binlogPos{}, &StopError{"the copy is past the source position; the source was written"}
		case d == 0:
			return pt, nil
		}
		done, err := c.noTransactions(ctx, pt.position(), q)
		if err != nil {
			return binlogPos{}, err
		}
		if done {
			return pt, nil
		}
		if time.Now().After(end) {
			return binlogPos{}, &StopError{"the copy did not reach the source position in time; is run running?"}
		}
		select {
		case <-ctx.Done():
			return binlogPos{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// eventsWithoutTransaction may lie between the copy position and the source position.
var eventsWithoutTransaction = map[string]bool{"Rotate": true, "Format_desc": true, "Previous_gtids": true, "Stop": true}

// noTransactions reports whether the binlog from `from` up to `to` holds no transaction.
// It reads only the event types.
func (c *checker) noTransactions(ctx context.Context, from, to mysql.Position) (bool, error) {
	files, err := connRows(ctx, c.src, "SHOW BINARY LOGS", 0)
	if err != nil {
		return false, err
	}
	started := false
	for _, f := range files {
		name := f[0]
		if name == from.Name {
			started = true
		}
		if !started {
			continue
		}
		pos := uint32(4)
		if name == from.Name {
			pos = from.Pos
		}
		page := eventPage
		for {
			if name == to.Name && pos >= to.Pos {
				return true, nil
			}
			rows, err := c.src.QueryContext(ctx, "SHOW BINLOG EVENTS IN '"+sqlString(name)+"' FROM "+strconv.FormatUint(uint64(pos), 10)+" LIMIT "+strconv.Itoa(page))
			if err != nil {
				return false, err
			}
			n, next, ok, err := eventTypes(rows, name, to)
			if err != nil || !ok {
				return false, err
			}
			if n < page || next == 0 {
				break // end of this file
			}
			pos = next
		}
		if name == to.Name {
			return true, nil
		}
	}
	return false, &StopError{"binlog file " + from.Name + " is no longer kept on the source"}
}

// eventTypes reads one page of SHOW BINLOG EVENTS: the event count, where the next page starts,
// and whether every event before `to` carries no transaction. The Info column is not kept.
func eventTypes(rows *sql.Rows, file string, to mysql.Position) (int, uint32, bool, error) {
	defer rows.Close()
	var n int
	var next uint32
	for rows.Next() {
		var logName, typ string
		var pos, end uint32
		var serverID uint64
		var info sql.RawBytes
		if err := rows.Scan(&logName, &pos, &typ, &serverID, &end, &info); err != nil {
			return 0, 0, false, err
		}
		n++
		if file == to.Name && pos >= to.Pos {
			return n, 0, true, nil
		}
		if !eventsWithoutTransaction[typ] {
			return n, 0, false, nil
		}
		next = end
	}
	return n, next, true, rows.Err()
}
