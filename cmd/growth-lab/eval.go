package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"growth-lab/internal/eval"
)

// evalNumber reads a number like JavaScript's Number(); nil when the flag is absent.
func evalNumber(args []string, name string) (*float64, error) {
	v, ok, err := flagValue(args, name)
	if err != nil {
		return nil, &eval.Error{Msg: err.Error()}
	}
	if !ok {
		return nil, nil
	}
	t := strings.TrimSpace(v)
	n := 0.0
	if t != "" {
		if i, err := strconv.ParseInt(t, 0, 64); err == nil && (strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0o") || strings.HasPrefix(t, "0b")) {
			n = float64(i)
		} else if n, err = strconv.ParseFloat(t, 64); err != nil || strings.ContainsAny(t, "_") {
			n = math.NaN()
		}
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil, &eval.Error{Msg: name + " must be a number"}
	}
	return &n, nil
}

func evalFlag(args []string, name string) (string, error) {
	v, _, err := flagValue(args, name)
	if err != nil {
		return "", &eval.Error{Msg: err.Error()}
	}
	return v, nil
}

func evalCmd(args []string) (int, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "compare" {
		if len(args) != 3 {
			return 1, &eval.Error{Msg: "usage: eval compare <before.json> <after.json>"}
		}
		var reps [2]*evalReport
		for i, p := range args[1:] {
			b, err := os.ReadFile(p)
			if err != nil {
				return 1, err
			}
			r, err := eval.ParseStoredReport(b)
			if err != nil {
				return 1, err
			}
			reps[i] = r
		}
		lines, verdict, err := eval.CompareReports(reps[0], reps[1])
		if err != nil {
			return 1, err
		}
		for _, l := range lines {
			fmt.Println(l)
		}
		if verdict == "regression" {
			return 1, nil
		}
		return 0, nil
	}
	ws, err := loadWorkspace()
	if err != nil {
		return 1, err
	}
	if sub == "check" || sub == "freeze" {
		app, err := eval.NewApp(ws, "")
		if err != nil {
			return 1, err
		}
		ids, err := eval.MetricIDs(app)
		if err != nil {
			return 1, err
		}
		cases, problems, err := eval.LoadCases(ws.Dir, ids)
		if err != nil {
			return 1, err
		}
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, p)
		}
		if sub == "check" {
			fmt.Printf("%d case(s) valid, %d problem(s)\n", len(cases), len(problems))
			if _, _, err := eval.CheckEnv(app); err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				return 1, nil
			}
			fmt.Println("snapshot matches the workspace definitions")
			if len(problems) > 0 {
				return 1, nil
			}
			return 0, nil
		}
		if len(problems) > 0 {
			return 1, nil
		}
		id, err := evalFlag(args, "--case")
		if err != nil {
			return 1, err
		}
		var pick []*eval.Case
		for _, c := range cases {
			if id == "" || c.ID == id {
				pick = append(pick, c)
			}
		}
		results, err := eval.FreezeCases(app, pick)
		if err != nil {
			return 1, err
		}
		code := 0
		for _, r := range results {
			word := "frozen"
			if !r.OK {
				word, code = "failed", 1
			}
			fmt.Printf("%s  %s: %s\n", word, r.ID, r.Message)
		}
		return code, nil
	}
	o := eval.Options{Blind: contains(args, "--blind"), Log: func(l string) { fmt.Println(l) }}
	if o.Runs, err = evalNumber(args, "--runs"); err != nil {
		return 1, err
	}
	if o.CaseID, err = evalFlag(args, "--case"); err != nil {
		return 1, err
	}
	if o.Tag, err = evalFlag(args, "--tag"); err != nil {
		return 1, err
	}
	if o.Model, err = evalFlag(args, "--model"); err != nil {
		return 1, err
	}
	if o.MaxCostUSD, err = evalNumber(args, "--max-cost-usd"); err != nil {
		return 1, err
	}
	if o.Seed, err = evalNumber(args, "--seed"); err != nil {
		return 1, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.Ctx = ctx
	stop := onInterrupt(func() {
		fmt.Fprintln(os.Stderr, "Stopping… (saving the runs done so far)")
		cancel()
	})
	defer stop()
	rep, path, err := eval.Run(ws, o)
	if err != nil {
		return 1, err
	}
	for _, l := range eval.ConsoleSummary(rep) {
		fmt.Println(l)
	}
	fmt.Println("report: " + path)
	if rep.Meta.Aborted == "interrupted" {
		return 130, nil
	}
	return 0, nil
}

type evalReport = eval.StoredReport

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
