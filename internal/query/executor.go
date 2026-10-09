package query

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/sqllint"
)

// Mode is probe (exploration) or panel.
type Mode string

const (
	Probe Mode = "probe"
	Panel Mode = "panel"
)

type modeLimits struct {
	MaxRows       int
	Overflow      string
	TruncateCells bool
}

// Limits per mode, cell size and total output size.
var Limits = struct {
	Probe, Panel modeLimits
	CellBytes    int
	OutputBytes  int
}{
	Probe:       modeLimits{MaxRows: 50, Overflow: "more", TruncateCells: true},
	Panel:       modeLimits{MaxRows: 5000, Overflow: "error", TruncateCells: false},
	CellBytes:   4 * 1024,
	OutputBytes: 4 * 1024 * 1024,
}

// BlockedList are columns ("table.column") and tables to deny as sensitive.
type BlockedList struct{ Columns, Tables []string }

// Request is one query.
type Request struct {
	Lease *SlotLease
	SQL   string
	Path  string
	Mode  Mode
	AsOf  string
	// Params: workspace params (float64, string or []string); only scalars bind
	Params           jsjson.Object
	ReadablePrefixes []string
	Blocked          *BlockedList
	HeapLimitMb      float64
	Ctx              context.Context
}

// ResultColumn is a result column; Table and Column are nil for expressions.
type ResultColumn struct {
	Name   string
	Table  *string
	Column *string
}

// Tagged is a typed cell: n (null), s (string), i (integer), f (real).
type Tagged struct {
	Tag   string
	Value any
}

// Result is a query result; on failure Kind is lint, sqlite, type, limit, input, cancelled, crash or sensitive.
type Result struct {
	OK             bool
	Columns        []ResultColumn
	Rows           [][]Tagged
	More           bool
	TruncatedCells int
	Ms             float64
	Tables         []string
	Kind, Message  string
}

func failed(kind, msg string) Result { return Result{Kind: kind, Message: msg} }

// ScalarParams keeps the non-array params.
func ScalarParams(params jsjson.Object) jsjson.Object {
	out := jsjson.Object{}
	for _, m := range params {
		if _, arr := m.Value.([]string); arr {
			continue
		}
		if _, arr := m.Value.([]any); arr {
			continue
		}
		out = append(out, m)
	}
	return out
}

// WorkerCommand returns the program that runs one query (this binary's query-worker by default).
var WorkerCommand = func() (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	return exe, []string{"query-worker"}, nil
}

// RunQuery runs a query on its lease: static checks, then a child process.
func RunQuery(req Request) Result {
	if req.Lease == nil || !req.Lease.begin() {
		return failed("input", "cannot run a query without an execution slot (or two at once on the same slot)")
	}
	defer req.Lease.end()
	return runWithLease(req)
}

var oomRE = regexp.MustCompile(`(?i)out of memory|heap`)

func runWithLease(req Request) Result {
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	scalars := ScalarParams(req.Params)
	keys := make([]string, len(scalars))
	for i, m := range scalars {
		keys[i] = m.Key
	}
	lint := sqllint.Lint(req.SQL, keys)
	if !lint.OK {
		return failed("lint", lint.Message)
	}
	if ctx.Err() != nil {
		return failed("cancelled", "cancelled")
	}
	bound := jsjson.Object{}
	for _, p := range lint.Params {
		if p == "as_of" {
			bound = append(bound, jsjson.Member{Key: p, Value: req.AsOf})
		} else {
			v, _ := scalars.Get(p)
			bound = append(bound, jsjson.Member{Key: p, Value: v})
		}
	}
	lim := Limits.Probe
	if req.Mode == Panel {
		lim = Limits.Panel
	}
	input := jsjson.Object{{Key: "path", Value: req.Path}, {Key: "sql", Value: req.SQL}, {Key: "params", Value: bound}, {Key: "readablePrefixes", Value: req.ReadablePrefixes}}
	if req.Blocked != nil {
		input = append(input, jsjson.Member{Key: "blockedColumns", Value: req.Blocked.Columns}, jsjson.Member{Key: "blockedTables", Value: req.Blocked.Tables})
	} else {
		input = append(input, jsjson.Member{Key: "blockedColumns", Value: jsjson.Undefined}, jsjson.Member{Key: "blockedTables", Value: jsjson.Undefined})
	}
	input = append(input,
		jsjson.Member{Key: "heapLimitMb", Value: req.HeapLimitMb}, jsjson.Member{Key: "maxRows", Value: lim.MaxRows}, jsjson.Member{Key: "overflow", Value: lim.Overflow},
		jsjson.Member{Key: "cellLimit", Value: Limits.CellBytes}, jsjson.Member{Key: "truncateCells", Value: lim.TruncateCells}, jsjson.Member{Key: "outputLimit", Value: Limits.OutputBytes - 64*1024})
	payload := jsjson.MustStringify(input)
	if len(payload) > InputLimit {
		return failed("input", "SQL and path exceed 64KB")
	}
	name, args, err := WorkerCommand()
	if err != nil {
		return failed("crash", err.Error())
	}
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = bytes.NewReader([]byte(payload))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failed("crash", err.Error())
	}
	var errText limitedText
	cmd.Stderr = &errText
	if err := cmd.Start(); err != nil {
		return failed("crash", err.Error())
	}
	var once sync.Once
	killGroup := func() { once.Do(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }) }
	stop := context.AfterFunc(ctx, killGroup)
	defer stop()

	var out bytes.Buffer
	var pending *Result
	buf := make([]byte, 64*1024)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 && pending == nil {
			if out.Len()+n > Limits.OutputBytes {
				r := failed("limit", "result output exceeds the limit")
				pending = &r
				out.Reset()
				killGroup()
			} else {
				out.Write(buf[:n])
			}
		}
		if rerr != nil {
			break
		}
	}
	werr := cmd.Wait()
	if pending != nil {
		return *pending
	}
	if ctx.Err() != nil {
		return failed("cancelled", "cancelled")
	}
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			if oomRE.MatchString(errText.String()) {
				return failed("limit", fmt.Sprintf("exceeded the memory limit (%sMB)", jsjson.Number(req.HeapLimitMb)))
			}
			return failed("crash", fmt.Sprintf("worker exited abnormally (%s)", exitDesc(ee)))
		}
		return failed("crash", werr.Error())
	}
	var raw struct {
		OK      bool `json:"ok"`
		Columns []struct {
			Name   string  `json:"name"`
			Table  *string `json:"table"`
			Column *string `json:"column"`
		} `json:"columns"`
		Rows           [][][2]json.RawMessage `json:"rows"`
		More           bool                   `json:"more"`
		TruncatedCells int                    `json:"truncatedCells"`
		Ms             float64                `json:"ms"`
		Tables         []string               `json:"tables"`
		Kind           string                 `json:"kind"`
		Message        string                 `json:"message"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return failed("crash", "cannot read worker output")
	}
	if !raw.OK {
		return failed(raw.Kind, raw.Message)
	}
	r := Result{OK: true, More: raw.More, TruncatedCells: raw.TruncatedCells, Ms: raw.Ms, Tables: raw.Tables}
	for _, c := range raw.Columns {
		r.Columns = append(r.Columns, ResultColumn{c.Name, c.Table, c.Column})
	}
	r.Rows = make([][]Tagged, len(raw.Rows))
	for i, row := range raw.Rows {
		cells := make([]Tagged, len(row))
		for j, c := range row {
			var tag string
			_ = json.Unmarshal(c[0], &tag)
			var v any
			switch tag {
			case "s":
				v = jsString(c[1])
			case "i", "f":
				f, _ := strconv.ParseFloat(string(c[1]), 64)
				v = f
			}
			cells[j] = Tagged{tag, v}
		}
		r.Rows[i] = cells
	}
	if r.Tables == nil {
		r.Tables = []string{}
	}
	return r
}

// limitedText keeps the first 4096 characters of the worker's stderr.
type limitedText struct{ b bytes.Buffer }

func (l *limitedText) Write(p []byte) (int, error) {
	if l.b.Len() < 4096 {
		l.b.Write(p)
	}
	return len(p), nil
}

func (l *limitedText) String() string { return l.b.String() }

var signalNames = map[syscall.Signal]string{
	syscall.SIGKILL: "SIGKILL", syscall.SIGTERM: "SIGTERM", syscall.SIGSEGV: "SIGSEGV", syscall.SIGABRT: "SIGABRT",
	syscall.SIGBUS: "SIGBUS", syscall.SIGINT: "SIGINT", syscall.SIGHUP: "SIGHUP", syscall.SIGPIPE: "SIGPIPE", syscall.SIGILL: "SIGILL", syscall.SIGFPE: "SIGFPE",
}

// exitDesc is the exit code, or the signal name when killed by a signal.
func exitDesc(ee *exec.ExitError) string {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		if n, ok := signalNames[ws.Signal()]; ok {
			return n
		}
		return ws.Signal().String()
	}
	return strconv.Itoa(ee.ExitCode())
}

// Untag returns a tagged cell's value.
func Untag(t Tagged) any { return t.Value }

// JS returns the result in its stored JSON form.
func (r Result) JS() jsjson.Object {
	if !r.OK {
		return jsjson.Object{{Key: "ok", Value: false}, {Key: "kind", Value: r.Kind}, {Key: "message", Value: r.Message}}
	}
	cols := make([]any, len(r.Columns))
	for i, c := range r.Columns {
		cols[i] = jsjson.Object{{Key: "name", Value: c.Name}, {Key: "table", Value: nullable(c.Table)}, {Key: "column", Value: nullable(c.Column)}}
	}
	rows := make([]any, len(r.Rows))
	for i, row := range r.Rows {
		cells := make([]any, len(row))
		for j, c := range row {
			cells[j] = []any{c.Tag, c.Value}
		}
		rows[i] = cells
	}
	return jsjson.Object{{Key: "ok", Value: true}, {Key: "columns", Value: cols}, {Key: "rows", Value: rows}, {Key: "more", Value: r.More}, {Key: "truncatedCells", Value: r.TruncatedCells}, {Key: "ms", Value: r.Ms}, {Key: "tables", Value: r.Tables}}
}

// jsString decodes a JSON string literal; a value with a lone surrogate (a cell cut inside a pair) stays as UTF-16 code units.
func jsString(lit []byte) any {
	var s string
	if err := json.Unmarshal(lit, &s); err != nil || !bytes.Contains(lit, []byte(`\u`)) {
		return s
	}
	var u jsjson.UTF16
	t := string(lit[1 : len(lit)-1])
	for i := 0; i < len(t); {
		if t[i] == '\\' && i+1 < len(t) {
			if t[i+1] == 'u' && i+6 <= len(t) {
				n, _ := strconv.ParseUint(t[i+2:i+6], 16, 16)
				u = append(u, uint16(n))
				i += 6
				continue
			}
			var one string
			_ = json.Unmarshal([]byte(`"`+t[i:i+2]+`"`), &one)
			u = append(u, utf16.Encode([]rune(one))...)
			i += 2
			continue
		}
		r, size := utf8.DecodeRuneInString(t[i:])
		u = append(u, utf16.Encode([]rune{r})...)
		i += size
	}
	for i := 0; i < len(u); i++ {
		c := u[i]
		if c >= 0xd800 && c <= 0xdbff && i+1 < len(u) && u[i+1] >= 0xdc00 && u[i+1] <= 0xdfff {
			i++
			continue
		}
		if c >= 0xd800 && c <= 0xdfff {
			return u
		}
	}
	return s
}
