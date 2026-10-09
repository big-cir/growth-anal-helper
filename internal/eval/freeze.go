package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/query"
	"growth-lab/internal/server"
	"growth-lab/internal/snapshot"
)

// Fixture is a reference panel's result on one snapshot, as role rows.
type Fixture struct {
	CaseID        string
	SnapshotID    string
	Env           jsjson.Object
	ReferenceHash string
	XGrain        *string
	RoleRows
}

// OutDir is <outDir>/eval.
func OutDir(a *server.App) string { return filepath.Join(a.WS.Config.OutDir, "eval") }

// FixturesDir holds the fixtures of one snapshot.
func FixturesDir(a *server.App, snapshotID string) string {
	return filepath.Join(OutDir(a), "fixtures", snapshotID)
}

// CheckEnv: the current snapshot must be built from exactly the current workspace definitions (full hashes, not the id).
func CheckEnv(a *server.App) (*server.Snapshot, jsjson.Object, error) {
	snap := a.Snapshot()
	if snap == nil {
		return nil, nil, &Error{"no snapshot: run collect first"}
	}
	stored := snapshot.ReadMetaHashes(snap.Real)
	if stored == nil {
		return nil, nil, &Error{"cannot read snapshot_meta of the current snapshot"}
	}
	inputs, err := snapshot.LoadBuildInputs(a.WS)
	if err != nil {
		return nil, nil, err
	}
	now, err := snapshot.HashInputs(inputs)
	if err != nil {
		return nil, nil, err
	}
	pairs := [][3]string{{"derived_hash", stored.Derived, now.Derived}, {"roles_hash", stored.Roles, now.Roles}, {"params_hash", stored.Params, now.Params}, {"spec_hash", stored.Spec, now.Spec}, {"ga4_spec_hash", stored.GA4Spec, now.GA4Spec}}
	var bad []string
	env := jsjson.Object{}
	for _, p := range pairs {
		if p[1] != p[2] {
			bad = append(bad, p[0])
		}
		env = append(env, jsjson.Member{Key: p[0], Value: p[2]})
	}
	if len(bad) > 0 {
		return nil, nil, &Error{"snapshot and workspace definitions differ (" + strings.Join(bad, ", ") + "): run collect or derive, then eval freeze again"}
	}
	cfg := a.WS.Config
	env = append(env, jsjson.Member{Key: "contract_version", Value: float64(panels.PatternContractVersion)}, jsjson.Member{Key: "policy_version", Value: panels.PolicyVersion(cfg.ReadablePrefixes, cfg.PanelPrefixes)})
	return snap, env, nil
}

// StaleReason says why a fixture can't be used for this case now ("" when usable).
func StaleReason(c *Case, f *Fixture, env jsjson.Object) string {
	if f == nil {
		return "no fixture for this snapshot: run eval freeze"
	}
	if f.ReferenceHash != c.Reference.Hash {
		return "reference changed since freeze"
	}
	var changed []string
	for _, m := range env {
		v, ok := f.Env.Get(m.Key)
		if !ok || jsjson.MustStringify(v) != jsjson.MustStringify(m.Value) {
			changed = append(changed, m.Key)
		}
	}
	if len(changed) > 0 {
		return "environment changed since freeze (" + strings.Join(changed, ", ") + ")"
	}
	fg := ""
	if f.XGrain != nil {
		fg = *f.XGrain
	}
	if fg != c.XGrain || (f.XGrain == nil) != (c.XGrain == "") {
		return "x_grain changed since freeze"
	}
	return ""
}

// ReadFixture returns the fixture of a case on a snapshot, or nil.
func ReadFixture(a *server.App, snapshotID, caseID string) (*Fixture, error) {
	b, err := os.ReadFile(filepath.Join(FixturesDir(a, snapshotID), caseID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := jsjson.Parse(string(b))
	if err != nil {
		return nil, err
	}
	o, _ := v.(jsjson.Object)
	f := &Fixture{RoleRows: roleRowsFrom(o)}
	s := func(k string) string { x, _ := o.Get(k); r, _ := x.(string); return r }
	f.CaseID, f.SnapshotID, f.ReferenceHash = s("case_id"), s("snapshot_id"), s("reference_hash")
	if e, _ := o.Get("env"); e != nil {
		f.Env, _ = e.(jsjson.Object)
	}
	if g, _ := o.Get("x_grain"); g != nil {
		x, _ := g.(string)
		f.XGrain = &x
	}
	return f, nil
}

func (f *Fixture) js() jsjson.Object {
	var g any
	if f.XGrain != nil {
		g = *f.XGrain
	}
	o := jsjson.Object{{Key: "format", Value: 1}, {Key: "case_id", Value: f.CaseID}, {Key: "snapshot_id", Value: f.SnapshotID}, {Key: "env", Value: f.Env}, {Key: "reference_hash", Value: f.ReferenceHash}, {Key: "x_grain", Value: g}}
	return append(o, f.RoleRows.JS()...)
}

// FreezeResult is one case's freeze outcome.
type FreezeResult struct {
	ID      string
	OK      bool
	Message string
}

// FreezeCases runs each reference with the same panel checks as the web app. Existing fixtures are never overwritten.
func FreezeCases(a *server.App, cases []*Case) ([]FreezeResult, error) {
	snap, env, err := CheckEnv(a)
	if err != nil {
		return nil, err
	}
	dir := FixturesDir(a, snap.ID)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	cfg := a.WS.Config
	dict, _, err := a.Metrics()
	if err != nil {
		return nil, err
	}
	var out []FreezeResult
	for _, c := range cases {
		if c.Reference == nil {
			continue
		}
		file := filepath.Join(dir, c.ID+".json")
		if _, err := os.Stat(file); err == nil {
			out = append(out, FreezeResult{c.ID, false, "fixture exists (delete it to freeze again)"})
			continue
		}
		spec := c.Reference.Spec
		var r panels.RunResult
		err := a.Slots.Run(context.Background(), query.Interactive, func(l *query.SlotLease) error {
			r = panels.Run(panels.RunInput{Spec: spec, RealPath: snap.Real, AgentPath: snap.Agent, AsOf: snap.AsOf, Params: cfg.Params, PanelPrefix: cfg.PanelPrefixes,
				Metrics: dict, Blocked: a.Blocked(), HeapLimitMb: float64(cfg.HeapLimitMb), Roles: a.Roles, Lease: l, Ctx: context.Background()})
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !r.OK {
			out = append(out, FreezeResult{c.ID, false, fmt.Sprintf("reference failed (%s): %s", r.Stage, r.Message)})
			continue
		}
		rows, err := NormalizeResult(spec, r.Columns, r.Real, c.XGrain)
		if err != nil {
			var nf *NormalizeFailure
			if errors.As(err, &nf) {
				out = append(out, FreezeResult{c.ID, false, nf.Code + ": " + nf.Msg})
				continue
			}
			return nil, err
		}
		f := &Fixture{CaseID: c.ID, SnapshotID: snap.ID, Env: env, ReferenceHash: c.Reference.Hash, RoleRows: rows}
		if c.XGrain != "" {
			f.XGrain = ptr(c.XGrain)
		}
		if err := writeAtomic(file, jsjson.Indent(f.js(), 2)+"\n"); err != nil {
			return nil, err
		}
		out = append(out, FreezeResult{c.ID, true, fmt.Sprintf("%d rows (%s)", len(rows.Rows), spec.Display.Type)})
	}
	return out, nil
}

func writeAtomic(file, text string) error {
	tmp := fmt.Sprintf("%s.tmp-%d", file, os.Getpid())
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
