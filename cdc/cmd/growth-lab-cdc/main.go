// Command growth-lab-cdc follows a MySQL binlog and keeps a local copy of selected tables up to date.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"

	"growth-lab-cdc/internal/cdc"
)

var usage = `Usage: growth-lab-cdc <command>
  inspect --defaults-file FILE --database-include-list DBS [--table-include-list TABLES]
          [--host H] [--port N] [--from FILE] [--to FILE:POS] [--max-mb N] [--max-seconds N]
          [--server-id N] [--tz ZONE] [--json]
                          Read the binlog in a range and count changed rows per table (applies nothing)
                          DBS: comma-separated regular expressions for database names (required)
                          TABLES: comma-separated regular expressions for db.table names; without it,
                          every table of the matched databases. Patterns match whole names, e.g. app\.orders
                          Defaults: from the oldest kept file to the current position, 200 MB, 600 seconds
  init --defaults-file FILE --target-defaults-file FILE --database DB --tables T1,T2,...
       [--host H] [--port N] [--server-id N] [--max-copy-minutes N]
                          Build a fresh copy of the tables from one snapshot, catch up with the binlog and
                          swap it in; the previous tables move to growth_lab_cdc_old. Resumes after a stop
  run --defaults-file FILE --target-defaults-file FILE --database DB --tables T1,T2,...
      [--host H] [--port N] [--from FILE:POS] [--server-id N]
                          Apply source changes to the same tables on the target and keep the position there
                          --from: first run only, the position the target's data was taken at
                          Stops on anything it cannot apply exactly; Ctrl+C finishes the current transaction`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		fmt.Println(usage)
		if len(argv) == 0 {
			return 2
		}
		return 0
	}
	cmd, rest := argv[0], argv[1:]
	var code int
	var err error
	switch cmd {
	case "inspect":
		code, err = inspectCmd(rest)
	case "run":
		code, err = runCmd(rest)
	case "init":
		code, err = initCmd(rest)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n%s\n", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s failed: %s\n", cmd, cdc.SafeError(err))
		return 1
	}
	return code
}

func cdcErrorf(format string, a ...any) error { return &cdc.Error{Msg: fmt.Sprintf(format, a...)} }

func flagValue(args []string, name string) (string, bool, error) {
	for i, a := range args {
		if a == name {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", true, cdcErrorf("%s needs a value", name)
			}
			return args[i+1], true, nil
		}
	}
	return "", false, nil
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func intFlag(args []string, name string, def, min, max int64) (int64, error) {
	v, ok, err := flagValue(args, name)
	if err != nil || !ok {
		return def, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < min || n > max {
		return 0, cdcErrorf("%s must be an integer from %d to %d", name, min, max)
	}
	return n, nil
}

// inspectFlags are the flags inspect takes, and whether each takes a value.
var inspectFlags = map[string]bool{
	"--defaults-file": true, "--host": true, "--port": true, "--database-include-list": true, "--table-include-list": true,
	"--from": true, "--to": true, "--max-mb": true, "--max-seconds": true, "--server-id": true, "--tz": true, "--json": false,
	"--schema": true, "--tables": true, "--include": true,
}

// checkFlags rejects unknown, repeated and stray arguments.
func checkFlags(args []string, known map[string]bool) error {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		takesValue, ok := known[a]
		if !ok {
			return cdcErrorf("unknown argument %s", a)
		}
		if seen[a] {
			return cdcErrorf("%s may be given once; separate items with commas", a)
		}
		seen[a] = true
		if takesValue {
			i++
		}
	}
	return nil
}

func inspectCmd(args []string) (int, error) {
	for _, old := range []string{"--schema", "--tables", "--include"} {
		if hasFlag(args, old) {
			return 1, &cdc.Error{Msg: old + " is no longer supported; use --database-include-list and --table-include-list"}
		}
	}
	if err := checkFlags(args, inspectFlags); err != nil {
		return 1, err
	}
	file, ok, err := flagValue(args, "--defaults-file")
	if err != nil {
		return 1, err
	}
	if !ok {
		return 1, &cdc.Error{Msg: "--defaults-file is required"}
	}
	src, err := cdc.LoadSource(file)
	if err != nil {
		return 1, err
	}
	if v, ok, err := flagValue(args, "--host"); err != nil {
		return 1, err
	} else if ok {
		src.Host = v
	}
	port, err := intFlag(args, "--port", int64(src.Port), 1, 65535)
	if err != nil {
		return 1, err
	}
	src.Port = int(port)
	o := cdc.InspectOptions{Source: src}
	if o.Filter, err = filterFlags(args); err != nil {
		return 1, err
	}
	if v, ok, err := flagValue(args, "--from"); err != nil {
		return 1, err
	} else if ok {
		if strings.Contains(v, ":") {
			return 1, &cdc.Error{Msg: "--from takes a file name only; reading starts at the beginning of that file"}
		}
		o.FromFile = v
	}
	if v, ok, err := flagValue(args, "--to"); err != nil {
		return 1, err
	} else if ok {
		name, p, found := strings.Cut(v, ":")
		n, perr := strconv.ParseUint(p, 10, 32)
		if !found || name == "" || perr != nil {
			return 1, &cdc.Error{Msg: "--to must be FILE:POS"}
		}
		o.To = mysql.Position{Name: name, Pos: uint32(n)}
	}
	mb, err := intFlag(args, "--max-mb", 200, 1, 1<<20)
	if err != nil {
		return 1, err
	}
	o.MaxBytes = mb << 20
	secs, err := intFlag(args, "--max-seconds", 600, 1, 86400)
	if err != nil {
		return 1, err
	}
	o.MaxDuration = time.Duration(secs) * time.Second
	id, err := intFlag(args, "--server-id", 0, 1, math.MaxUint32)
	if err != nil {
		return 1, err
	}
	o.ServerID = uint32(id)
	if v, ok, err := flagValue(args, "--tz"); err != nil {
		return 1, err
	} else if ok {
		if o.Location, err = time.LoadLocation(v); err != nil {
			return 1, &cdc.Error{Msg: "--tz: unknown time zone " + v}
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	rep, err := cdc.Inspect(ctx, o)
	if err != nil {
		return 1, err
	}
	if hasFlag(args, "--json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return 1, err
		}
	} else {
		printReport(rep)
	}
	if !rep.Complete {
		return 1, nil
	}
	return 0, nil
}

// filterFlags reads the include lists; each flag may appear once.
func filterFlags(args []string) (cdc.Filter, error) {
	for _, old := range []string{"--schema", "--tables", "--include"} {
		if hasFlag(args, old) {
			return cdc.Filter{}, &cdc.Error{Msg: old + " is no longer supported; use --database-include-list and --table-include-list"}
		}
	}
	list := func(flag string) ([]string, error) {
		n := 0
		for _, a := range args {
			if a == flag {
				n++
			}
		}
		if n > 1 {
			return nil, cdcErrorf("%s may be given once; separate items with commas", flag)
		}
		v, ok, err := flagValue(args, flag)
		if err != nil || !ok {
			return nil, err
		}
		return cdc.ParseList(flag, v)
	}
	dbs, err := list("--database-include-list")
	if err != nil {
		return cdc.Filter{}, err
	}
	tables, err := list("--table-include-list")
	if err != nil {
		return cdc.Filter{}, err
	}
	return cdc.NewFilter(dbs, tables)
}

func pos(p mysql.Position) string {
	if p.Name == "" {
		return "-"
	}
	return fmt.Sprintf("%s:%d", p.Name, p.Pos)
}

func printReport(r *cdc.Report) {
	state := "complete"
	if !r.Complete {
		state = "partial (" + r.Stop + ")"
	}
	fmt.Printf("Source     %s (server id %d), read as server id %d\n", r.SourceUUID, r.SourceServerID, r.ServerID)
	fmt.Printf("Range      %s to %s (end not included)\n", pos(r.From), pos(r.To))
	fmt.Printf("Result     %s; counted up to %s, read up to %s\n", state, pos(r.Counted), pos(r.Read))
	if r.Error != "" {
		fmt.Printf("Error      %s\n", r.Error)
	}
	if r.DroppedOpen {
		fmt.Println("Note       a transaction still open at the stop was not counted")
	}
	if r.FirstCommit != nil {
		fmt.Printf("Commits    %s ~ %s (%s, event header time in seconds)\n", r.FirstCommit.Format("2006-01-02 15:04:05"), r.LastCommit.Format("2006-01-02 15:04:05"), r.Zone)
	}
	fmt.Printf("           %d commits, %d with target rows, %d rollbacks, %d compressed\n", r.Commits, r.TargetCommits, r.Rollbacks, r.Compressed)
	fmt.Printf("Bytes      %.1f MB read\n", float64(r.StreamBytes)/(1<<20))
	fmt.Println()
	fmt.Printf("Following  databases %s", strings.Join(r.DatabaseInclude, ","))
	if len(r.TableInclude) > 0 {
		fmt.Printf(", tables %s", strings.Join(r.TableInclude, ","))
	}
	fmt.Printf(" → %d tables\n\n", len(r.TablesSelected))
	fmt.Printf("%-32s %10s %10s %10s %10s %12s\n", "table", "insert", "update", "delete", "key change", "row bytes")
	names := make([]string, 0, len(r.Tables))
	for n := range r.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	var sum cdc.Counts
	for _, n := range names {
		c := r.Tables[n]
		fmt.Printf("%-32s %10d %10d %10d %10d %12d\n", n, c.Inserts, c.Updates, c.Deletes, c.KeyChanges, c.RowBytes)
		sum.Inserts, sum.Updates, sum.Deletes, sum.KeyChanges, sum.RowBytes = sum.Inserts+c.Inserts, sum.Updates+c.Updates, sum.Deletes+c.Deletes, sum.KeyChanges+c.KeyChanges, sum.RowBytes+c.RowBytes
	}
	fmt.Printf("%-32s %10d %10d %10d %10d %12d\n", "total", sum.Inserts, sum.Updates, sum.Deletes, sum.KeyChanges, sum.RowBytes)
	if len(r.NotObserved) > 0 {
		fmt.Printf("Not observed in this range: %s\n", strings.Join(r.NotObserved, ", "))
	}
	var stmts int64
	for _, n := range r.Statements {
		stmts += n
	}
	fmt.Printf("\nStatement records in followed databases: %d — their changes are not in the counts above\n", stmts)
	printCounts(r.Statements)
	fmt.Println("Note: a statement is matched by the database selected when it ran; one that names a followed table")
	fmt.Println("      while another database is selected is not seen.")
	if len(r.Hours) > 0 {
		fmt.Printf("\nTarget rows by hour (%s)\n", r.Zone)
		printCounts(r.Hours)
	}
}

func printCounts(m map[string]int64) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-40s %10d\n", k, m[k])
	}
}

var runFlags = map[string]bool{
	"--defaults-file": true, "--target-defaults-file": true, "--host": true, "--port": true,
	"--database": true, "--tables": true, "--from": true, "--server-id": true,
}

func runCmd(args []string) (int, error) {
	if err := checkFlags(args, runFlags); err != nil {
		return 1, err
	}
	o, err := copyOptions(args)
	if err != nil {
		return 1, err
	}
	if v, ok, err := flagValue(args, "--from"); err != nil {
		return 1, err
	} else if ok {
		name, p, found := strings.Cut(v, ":")
		n, perr := strconv.ParseUint(p, 10, 32)
		if !found || name == "" || perr != nil || n < 4 {
			return 1, &cdc.Error{Msg: "--from must be FILE:POS"}
		}
		o.From = &mysql.Position{Name: name, Pos: uint32(n)}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	sum, err := cdc.Run(ctx, o)
	printSummary(sum)
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func printSummary(s *cdc.RunSummary) {
	if s == nil {
		return
	}
	saved := s.Saved
	if saved == "" {
		saved = "(unchanged)"
	}
	fmt.Printf("Saved position  %s\n", saved)
	fmt.Printf("Skipped         %d transactions without followed rows\n", s.Skipped)
	fmt.Printf("%-32s %10s %10s %10s\n", "applied", "insert", "update", "delete")
	names := make([]string, 0, len(s.Applied))
	for n := range s.Applied {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := s.Applied[n]
		fmt.Printf("%-32s %10d %10d %10d\n", n, c.Inserts, c.Updates, c.Deletes)
	}
}

var initFlags = map[string]bool{
	"--defaults-file": true, "--target-defaults-file": true, "--host": true, "--port": true,
	"--database": true, "--tables": true, "--server-id": true, "--max-copy-minutes": true,
}

func initCmd(args []string) (int, error) {
	if err := checkFlags(args, initFlags); err != nil {
		return 1, err
	}
	ro, err := copyOptions(args)
	if err != nil {
		return 1, err
	}
	o := cdc.InitOptions{Source: ro.Source, Target: ro.Target, Database: ro.Database, Tables: ro.Tables, ServerID: ro.ServerID, Logf: ro.Logf}
	mins, err := intFlag(args, "--max-copy-minutes", 0, 1, 7*24*60)
	if err != nil {
		return 1, err
	}
	o.MaxCopy = time.Duration(mins) * time.Minute
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := cdc.Init(ctx, o); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "init stopped; run it again to resume")
			return 1, nil
		}
		return 1, err
	}
	return 0, nil
}

// copyOptions reads the flags run and init share.
func copyOptions(args []string) (cdc.RunOptions, error) {
	load := func(flag string) (cdc.Source, error) {
		file, ok, err := flagValue(args, flag)
		if err != nil {
			return cdc.Source{}, err
		}
		if !ok {
			return cdc.Source{}, cdcErrorf("%s is required", flag)
		}
		return cdc.LoadSource(file)
	}
	var o cdc.RunOptions
	src, err := load("--defaults-file")
	if err != nil {
		return o, err
	}
	if v, ok, err := flagValue(args, "--host"); err != nil {
		return o, err
	} else if ok {
		src.Host = v
	}
	port, err := intFlag(args, "--port", int64(src.Port), 1, 65535)
	if err != nil {
		return o, err
	}
	src.Port = int(port)
	dst, err := load("--target-defaults-file")
	if err != nil {
		return o, err
	}
	o = cdc.RunOptions{Source: src, Target: dst, Logf: func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(f, a...))
	}}
	db, ok, err := flagValue(args, "--database")
	if err != nil {
		return o, err
	}
	if !ok || db == "" {
		return o, &cdc.Error{Msg: "--database is required"}
	}
	o.Database = db
	v, ok, err := flagValue(args, "--tables")
	if err != nil {
		return o, err
	}
	if !ok {
		return o, &cdc.Error{Msg: "--tables is required: the exact table names"}
	}
	if o.Tables, err = cdc.ParseList("--tables", v); err != nil {
		return o, err
	}
	id, err := intFlag(args, "--server-id", 0, 1, math.MaxUint32)
	if err != nil {
		return o, err
	}
	o.ServerID = uint32(id)
	return o, nil
}
