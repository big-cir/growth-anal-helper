// Command growth-lab is the engine: snapshot commands, the web server and the per-query worker.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"growth-lab/internal/auth"
	"growth-lab/internal/collect"
	"growth-lab/internal/demo"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/publiccheck"
	"growth-lab/internal/query"
	"growth-lab/internal/server"
	"growth-lab/internal/snapshot"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/trace"
	"growth-lab/internal/verify"
	"growth-lab/internal/workspace"
)

var usage = `Usage: growth-lab <command>
  serve [--port N]        Web server
  collect                 Collect from the source → finalize a snapshot
  derive                  Rebuild derived tables and the pseudonymized copy of the current snapshot
  test-derived            Derived-rule fixtures
  eval [--runs N] [--blind] [--case ID|--tag T] [--model M] [--max-cost-usd X] [--seed N]
                          Run the eval cases (workspace eval/cases) and save a report
  eval check | eval freeze [--case ID] | eval compare <before.json> <after.json>
  trace [--days N]        Step timings, cache hits and failures of web requests (default: last 7 days)
  verify [--week DATE]    Cross-check with the source (workspace verify.json; default: the Monday week 5 weeks before the cutoff)
  public-check [--history] [--require-denylist]
  demo                    Example workspace: seed → collect → serve
  ` + auth.AccountUsage + `
                          (for the example workspace, prefix with GROWTH_LAB_WORKSPACE=examples/demo)
  sqlite-info             SQLite version and compile options (JSON)
  query-worker            Run one query: JSON on stdin, JSON on stdout`

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
	switch cmd {
	case "query-worker":
		os.Stdout.WriteString(jsjson.MustStringify(queryWorker(os.Stdin)))
		return 0
	case "public-check":
		cwd, _ := os.Getwd()
		return publiccheck.Command(rest, cwd, os.Getenv, os.Stdout, os.Stderr)
	case "sqlite-info":
		info, err := sqlitec.ReadInfo()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(info)
		return 0
	}
	var code int
	var err error
	switch cmd {
	case "collect", "derive", "test-derived":
		code, err = pipelineCmd(cmd)
	case "serve":
		var ws *workspace.Workspace
		if ws, err = loadWorkspace(); err == nil {
			code, err = serve(ws, rest)
		}
	case "demo":
		code, err = demoCmd(rest)
	case "eval":
		code, err = evalCmd(rest)
	case "trace":
		code, err = traceCmd(rest)
	case "verify":
		code, err = verifyCmd(rest)
	case "account":
		var ws *workspace.Workspace
		if ws, err = loadWorkspace(); err == nil {
			code = auth.AccountCommand(ws.Config.OutDir, rest, os.Stdout, os.Stderr, nil)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n%s\n", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s failed: %s\n", cmd, err.Error())
		return 1
	}
	return code
}

func queryWorker(r io.Reader) jsjson.Object {
	buf, err := io.ReadAll(io.LimitReader(r, query.InputLimit+1))
	if err != nil {
		return jsjson.Object{{Key: "ok", Value: false}, {Key: "kind", Value: "input"}, {Key: "message", Value: err.Error()}}
	}
	if len(buf) > query.InputLimit {
		return jsjson.Object{{Key: "ok", Value: false}, {Key: "kind", Value: "input"}, {Key: "message", Value: "input exceeds 64KB"}}
	}
	in, err := query.ParseInput(buf)
	if err != nil {
		return jsjson.Object{{Key: "ok", Value: false}, {Key: "kind", Value: "input"}, {Key: "message", Value: err.Error()}}
	}
	return query.Run(in)
}

func loadWorkspace() (*workspace.Workspace, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return workspace.Load(workspace.FindDir(os.Getenv, cwd))
}

func reusedWord(reused bool) string {
	if reused {
		return "reused"
	}
	return "finalized"
}

func pipelineCmd(cmd string) (int, error) {
	ws, err := loadWorkspace()
	if err != nil {
		return 1, err
	}
	switch cmd {
	case "collect":
		var src collect.Source
		stop := onInterrupt(func() {
			if src != nil {
				src.Abort()
			}
		})
		defer stop()
		r, err := snapshot.RunCollect(ws, func(m string) { fmt.Println(m) }, func(s collect.Source) { src = s })
		if err != nil {
			return 1, err
		}
		fmt.Printf("Snapshot %s: %s\n", reusedWord(r.Reused), r.SnapshotID)
	case "derive":
		r, err := snapshot.RunDerive(ws)
		if err != nil {
			return 1, err
		}
		fmt.Printf("Snapshot %s: %s\n", reusedWord(r.Reused), r.SnapshotID)
	case "test-derived":
		results, err := snapshot.RunTestDerived(ws)
		if err != nil {
			return 1, err
		}
		failed := 0
		for _, r := range results {
			status := "pass"
			if !r.OK {
				status = "FAIL"
				failed++
			}
			line := status + "  " + r.Name
			if r.Message != "" {
				line += "\n  " + r.Message
			}
			fmt.Println(line)
		}
		fmt.Printf("%d of %d passed\n", len(results)-failed, len(results))
		if failed > 0 || len(results) == 0 {
			return 1, nil
		}
	}
	return 0, nil
}

// onInterrupt calls fn on Ctrl-C; the returned function stops listening.
func onInterrupt(fn func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			fn()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

func flagValue(args []string, name string) (string, bool, error) {
	for i, a := range args {
		if a == name {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", true, fmt.Errorf("%s needs a value", name)
			}
			return args[i+1], true, nil
		}
	}
	return "", false, nil
}

func serve(ws *workspace.Workspace, args []string) (int, error) {
	if err := workspace.AssertOutputsIgnored(workspace.GuardedPaths(ws), ws.Dir); err != nil {
		return 1, err
	}
	if auth.PendingInputPasswords(ws.Config.OutDir) {
		return 1, errors.New("accounts.input.json still has passwords. Run `growth-lab account import` first")
	}
	port := ws.Config.Server.Port
	if v, ok, _ := flagValue(args, "--port"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			port = -1
		} else {
			port = n
		}
	}
	if port < 1 || port > 65535 {
		return 1, errors.New("--port must be 1-65535")
	}
	server.SetupLogger(os.Stderr)
	app, err := server.NewApp(ws)
	if err != nil {
		return 1, err
	}
	s, err := server.Start(app, port)
	if err != nil {
		return 1, err
	}
	fmt.Printf("growth-lab: http://127.0.0.1:%d  (workspace %s)\n", s.Port, ws.Config.Name)
	if !ws.Config.Server.Auth {
		fmt.Println("Auth is off: anyone can use it as admin without signing in. Set server.auth to true in workspace.json before exposing it")
	}
	if app.Snapshot() == nil {
		fmt.Println("No snapshot yet. Run collect first")
	}
	app.StartIsolationCheck()
	go func() {
		app.WaitAgent()
		fmt.Printf("Agent: %s\n", app.AgentStatus().Message)
	}()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	fmt.Println("Stopping the server…")
	s.Close()
	return 0, nil
}

// exampleDir is the repository's examples/demo workspace.
func exampleDir() string {
	cwd, _ := os.Getwd()
	for _, base := range []string{cwd, workspace.GitRoot(cwd)} {
		if base == "" {
			continue
		}
		d := filepath.Join(base, "examples", "demo")
		if _, err := os.Stat(filepath.Join(d, "workspace.json")); err == nil {
			return d
		}
	}
	return filepath.Join(cwd, "examples", "demo")
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// demoCmd creates example data, collects it and starts the server.
func demoCmd(args []string) (int, error) {
	ws, err := workspace.Load(exampleDir())
	if err != nil {
		return 1, err
	}
	if ws.Config.Server.Auth {
		accounts, err := auth.ReadAccounts(ws.Config.OutDir)
		if err != nil {
			return 1, err
		}
		if len(accounts) == 0 {
			if !isTerminal(os.Stdin) {
				return 1, errors.New("No accounts. Run `GROWTH_LAB_WORKSPACE=examples/demo growth-lab account add <name> --role admin` first")
			}
			fmt.Println("Creating an admin account for the example server.")
			fmt.Print("Name: ")
			name, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if code := auth.AccountCommand(ws.Config.OutDir, []string{"add", strings.TrimSpace(name), "--role", "admin"}, os.Stdout, os.Stderr, nil); code != 0 {
				return code, nil
			}
		}
	}
	if ws.Config.Datasource.Kind != "sqlite" {
		return 1, errors.New("The example workspace source must be sqlite")
	}
	now := collect.LocalNow()[:19]
	fmt.Printf("Creating example data (as of %s)\n", now)
	sum, err := demo.Seed(ws.Config.Datasource.Path, demo.Options{Anchor: now})
	if err != nil {
		return 1, err
	}
	fmt.Println(sum.JSON())
	r, err := snapshot.RunCollect(ws, func(m string) { fmt.Println(m) }, nil)
	if err != nil {
		return 1, err
	}
	fmt.Printf("Snapshot %s: %s\n", reusedWord(r.Reused), r.SnapshotID)
	return serve(ws, args)
}

func traceCmd(args []string) (int, error) {
	days := 7
	if v, ok, err := flagValue(args, "--days"); err != nil {
		return 1, err
	} else if ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return 1, errors.New("--days must be a positive integer")
		}
		days = n
	}
	ws, err := loadWorkspace()
	if err != nil {
		return 1, err
	}
	for _, l := range trace.Report(ws.Config.OutDir, days, time.Now().UnixMilli()) {
		fmt.Println(l)
	}
	return 0, nil
}

func verifyCmd(args []string) (int, error) {
	var week *string
	for i, a := range args {
		if a == "--week" {
			if i+1 >= len(args) {
				return 1, errors.New("--week needs a date (YYYY-MM-DD)")
			}
			w := args[i+1]
			week = &w
		}
	}
	ws, err := loadWorkspace()
	if err != nil {
		return 1, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var src collect.Source
	stop := onInterrupt(func() {
		cancel()
		if src != nil {
			src.Abort()
		}
	})
	defer stop()
	r, err := verify.Run(ws, verify.Options{Week: week, Ctx: ctx, OnSource: func(s collect.Source) { src = s }})
	if errors.Is(err, verify.ErrCancelled) {
		fmt.Fprintln(os.Stderr, "Stopped")
		return 130, nil
	}
	if err != nil {
		return 1, err
	}
	fmt.Printf("Snapshot %s, week %s ~ %s\n", r.SnapshotID, r.WeekStart[:10], r.WeekEnd[:10])
	matched := 0
	for _, it := range r.Items {
		status := "MISMATCH"
		if it.OK {
			status = "match   "
			matched++
		}
		fmt.Printf("%s  %s (%s)\n", status, it.ID, it.Title)
		if it.Source != nil {
			fmt.Printf("  source    %s\n", jsjson.MustStringify(it.Source))
		}
		if it.Snapshot != nil {
			fmt.Printf("  snapshot  %s\n", jsjson.MustStringify(it.Snapshot))
		}
		if it.Err != nil {
			fmt.Printf("  %s\n", *it.Err)
		}
	}
	fmt.Printf("%d of %d match\n", matched, len(r.Items))
	if r.OK {
		return 0, nil
	}
	return 1, nil
}
