package server

import (
	"crypto/sha256"
	"fmt"
	"log/slog"

	"growth-lab/internal/coverage"
	"growth-lab/internal/jsjson"
)

// Coverage is the snapshot's data coverage when agent.coverageCheck is on (the default); nil when off.
// A result whose gap copies failed is not kept, so the next request tries again.
func (a *App) Coverage(snap *Snapshot) (*coverage.Resolved, error) {
	if !a.WS.Config.Agent.CoverageCheck || snap == nil {
		return nil, nil
	}
	key := snap.ID + "|" + a.CoverageKey()
	a.covMu.Lock()
	defer a.covMu.Unlock()
	if a.covCache != nil && a.covCache.key == key {
		return a.covCache.r, nil
	}
	cfg, _, err := coverage.LoadConfig(a.WS.Dir)
	if err != nil {
		return nil, err
	}
	r, err := coverage.Resolve(cfg, snap.Real, snap.AsOf, a.WS.Config.Params)
	if err != nil {
		return nil, err
	}
	if err := r.EnsureGapCopies(snap.Real); err != nil {
		slog.Error("coverage gap copy; the gap check is off for this request", "err", err)
		r.GapPaths, r.GapError = nil, err.Error()
		return r, nil
	}
	a.covCache = &struct {
		key string
		r   *coverage.Resolved
	}{key, r}
	return r, nil
}

// CoverageKey identifies the coverage settings and coverage.json in context versions and cache keys ("" when off).
func (a *App) CoverageKey() string {
	if !a.WS.Config.Agent.CoverageCheck {
		return ""
	}
	gaps, _ := a.WS.Config.Params.Get("quality_gap_ranges")
	_, raw, err := coverage.LoadConfig(a.WS.Dir)
	if err != nil {
		raw = []byte("error: " + err.Error())
	}
	return fmt.Sprintf("coverage:%s:%x", jsjson.MustStringify(gaps), sha256.Sum256(raw))
}

// QueryParams are the workspace params plus the coverage params.
func (a *App) QueryParams(snap *Snapshot) jsjson.Object {
	if r, _ := a.Coverage(snap); r != nil {
		return withParams(a.WS.Config.Params, r.Params())
	}
	return a.WS.Config.Params
}

// withParams adds extra to params; an extra value replaces a param of the same name.
func withParams(params, extra jsjson.Object) jsjson.Object {
	out := jsjson.Object{}
	for _, m := range params {
		if _, dup := extra.Get(m.Key); !dup {
			out = append(out, m)
		}
	}
	return append(out, extra...)
}
