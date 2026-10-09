package snapshot

import (
	"growth-lab/internal/collect"
	"growth-lab/internal/collect/ga4"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/workspace"
)

type ga4Hook struct{}

func init() { GA4 = ga4Hook{} }

func (ga4Hook) plan(ws *workspace.Workspace) (*ga4.Plan, error) {
	return ga4.LoadPlan(ws.Dir, *ws.Config.GA4)
}

func (h ga4Hook) Inputs(ws *workspace.Workspace) (*GA4Inputs, error) {
	p, err := h.plan(ws)
	if err != nil {
		return nil, err
	}
	roles := collect.NewRoles()
	for _, k := range ga4.RoleColumns(p) {
		roles.Set(k, collect.Ordinary)
	}
	return &GA4Inputs{SpecHash: p.SpecHash, Roles: roles, Tables: ga4.Tables(p)}, nil
}

// execDB runs GA4 statements on the temporary snapshot.
type execDB struct{ db *sqlitec.DB }

func (e execDB) Exec(sql string, args ...any) error {
	if len(args) == 0 {
		return e.db.Exec(sql)
	}
	_, err := e.db.RunOnce(sql, args...)
	return err
}

func (h ga4Hook) Import(db *sqlitec.DB, ws *workspace.Workspace, log func(string)) error {
	p, err := h.plan(ws)
	if err != nil {
		return err
	}
	return ga4.Import(execDB{db}, p, ga4.ImportOptions{Log: log})
}
