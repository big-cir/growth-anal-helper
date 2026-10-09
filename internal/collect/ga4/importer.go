package ga4

// GA4 import: fetches the configured reports into r_ga4_* tables and r_ga4_collect_log of the temporary snapshot.

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"growth-lab/internal/jsjson"
)

// Plan is the connection, the reports and their hash.
type Plan struct {
	Conn     Connection
	Reports  *Reports
	SpecHash string
}

// LoadPlan reads ga4-reports.json for a connection.
func LoadPlan(wsDir string, conn Connection) (*Plan, error) {
	r, err := LoadReports(wsDir)
	if err != nil {
		return nil, err
	}
	return &Plan{Conn: conn, Reports: r, SpecHash: SpecHash(conn, r)}, nil
}

// PlanDefs are the reports in this plan (built-in + custom).
func PlanDefs(p *Plan) []ReportDef {
	var defs []ReportDef
	for _, k := range p.Reports.Reports {
		defs = append(defs, ReportKinds[k])
	}
	for _, c := range p.Reports.Custom {
		defs = append(defs, CustomDef(c))
	}
	return defs
}

// Tables are the GA4 table names in this plan, sorted.
func Tables(p *Plan) []string {
	out := []string{}
	if p == nil {
		return out
	}
	for _, d := range PlanDefs(p) {
		out = append(out, d.Table)
	}
	sort.Strings(out)
	return out
}

// RoleColumns are the "table.column" names of GA4 tables; all are ordinary (no user IDs or free text).
func RoleColumns(p *Plan) []string {
	var out []string
	if p == nil {
		return out
	}
	for _, d := range PlanDefs(p) {
		for _, c := range TableColumns(d) {
			out = append(out, d.Table+"."+c.Name)
		}
	}
	return out
}

// DB runs statements on the temporary snapshot. Arguments are string, float64 or nil.
type DB interface {
	Exec(sql string, args ...any) error
}

// ImportOptions replace the transport, sleep and clock (for tests).
type ImportOptions struct {
	Transport Transport
	Sleep     func(ms int)
	Now       func() int64
	Jitter    func(n int) int
	Log       func(string)
}

type quota struct{ day, hour float64 }

func finite(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0
}

func checkQuota(j jsjson.Object, where string) (quota, error) {
	q, ok := prop(j, "propertyQuota").(jsjson.Object)
	if !ok {
		return quota{}, NewError("invalid_response", 0, where+" missing quota info")
	}
	share := func(k string) (float64, error) {
		x, ok := prop(q, k).(jsjson.Object)
		remaining, rOK := finite(prop(x, "remaining"))
		consumed, cOK := finite(prop(x, "consumed"))
		if !ok || !rOK || !cOK {
			return 0, NewError("invalid_response", 0, where+" quota info format")
		}
		total := remaining + consumed
		if total > 0 && remaining/total < ClientLimits.QuotaMinShare {
			return 0, NewError("quota", 0, where)
		}
		return remaining, nil
	}
	day, err := share("tokensPerDay")
	if err != nil {
		return quota{}, err
	}
	hour, err := share("tokensPerHour")
	if err != nil {
		return quota{}, err
	}
	return quota{day, hour}, nil
}

// names maps a header array to its name properties (missing ones read as undefined).
func names(v any) []any {
	arr, ok := v.([]any)
	out := []any{}
	if !ok {
		return out
	}
	for _, x := range arr {
		n := any(jsjson.Undefined)
		if o, isO := x.(jsjson.Object); isO {
			n = jsUndef(prop(o, "name"))
		}
		out = append(out, n)
	}
	return out
}

func jsUndef(v any) any {
	if v == undef {
		return jsjson.Undefined
	}
	return v
}

func truthy(v any) int {
	switch x := v.(type) {
	case nil, undefinedT:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		if x == 0 || math.IsNaN(x) {
			return 0
		}
	case string:
		if x == "" {
			return 0
		}
	}
	return 1
}

var safeDigitsRE = regexp.MustCompile(`^\d{1,20}$`)

func countText(v any) string {
	if s, ok := v.(string); ok && safeDigitsRE.MatchString(s) {
		return s
	}
	if f, ok := v.(float64); ok && f == math.Trunc(f) && math.Abs(f) <= 1<<53-1 {
		return jsjson.Number(f)
	}
	return "?"
}

func apiNames(def ReportDef, dims bool) []any {
	var out []any
	if dims {
		for _, d := range def.Dims {
			out = append(out, d.API)
		}
	} else {
		for _, m := range def.Metrics {
			out = append(out, m.API)
		}
	}
	return out
}

// Import builds the GA4 tables in the temporary snapshot DB. On failure the caller discards the temporary file.
func Import(db DB, plan *Plan, o ImportOptions) error {
	cred, err := LoadCredentials(plan.Conn.KeyFile)
	if err != nil {
		return err
	}
	client, err := NewClient(cred, plan.Conn.PropertyID, ClientOptions{Transport: o.Transport, Sleep: o.Sleep, Now: o.Now, Jitter: o.Jitter})
	if err != nil {
		return err
	}
	now := client.o.Now
	through := DataThrough(now(), plan.Conn.TimeZone)
	const insLog = `INSERT INTO r_ga4_collect_log (report, table_name, rows, row_count, dropped_small, dropped_unobserved, dropped_unmapped, dropped_collision, suppressed_cells, subject_to_thresholding, data_loss_from_other_row, sampled, sampling_summary, truncated, schema_restricted, empty_reason, time_zone, data_through, calls, quota_day_remaining, quota_hour_remaining, collected_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	for _, def := range PlanDefs(plan) {
		kind := def.Key
		cols := TableColumns(def)
		defsSQL := make([]string, len(cols))
		var keys []string
		for i, c := range cols {
			nn := " NOT NULL"
			if c.Nullable {
				nn = ""
			}
			defsSQL[i] = c.Name + " " + c.Type + nn
			if c.Key {
				keys = append(keys, c.Name)
			}
		}
		if err := db.Exec("CREATE TABLE " + def.Table + " (" + strings.Join(defsSQL, ", ") + ", PRIMARY KEY (" + strings.Join(keys, ", ") + "))"); err != nil {
			return err
		}
		callsBefore := client.Calls
		opt := RequestOptions{Start: plan.Reports.Start, Through: through, Events: plan.Reports.Events}
		first := BuildRequest(def, opt, 0)
		if first == nil {
			if err := db.Exec(insLog, kind, def.Table, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, "", "", 0.0, "no_complete_period", plan.Conn.TimeZone, through, 0.0, nil, nil, isoString(now())); err != nil {
				return err
			}
			continue
		}
		if o.Log != nil {
			o.Log("GA4 " + kind + ": fetching")
		}
		// Compatibility: every requested dimension and metric must be COMPATIBLE.
		// Cohort dimensions are not covered by checkCompatibility, so the response headers are checked instead
		if def.Range != "cohort" {
			payload := jsjson.Object{{Key: "dimensions", Value: prop(first, "dimensions")}, {Key: "metrics", Value: prop(first, "metrics")}}
			if f, ok := first.Get("dimensionFilter"); ok {
				payload = append(payload, jsjson.Member{Key: "dimensionFilter", Value: f})
			}
			c, err := client.CheckCompatibility(payload, kind)
			if err != nil {
				return err
			}
			// Each requested name appears exactly once and is COMPATIBLE (the response also lists other items)
			allCompatible := func(list any, meta string, want []any) bool {
				arr, ok := list.([]any)
				if !ok {
					return false
				}
				for _, n := range want {
					var hits []jsjson.Object
					for _, x := range arr {
						xo, isO := x.(jsjson.Object)
						if !isO {
							continue
						}
						mo, isMO := prop(xo, meta).(jsjson.Object)
						if isMO && prop(mo, "apiName") == n {
							hits = append(hits, xo)
						}
					}
					if len(hits) != 1 || prop(hits[0], "compatibility") != "COMPATIBLE" {
						return false
					}
				}
				return true
			}
			if !allCompatible(prop(c, "dimensionCompatibilities"), "dimensionMetadata", apiNames(def, true)) || !allCompatible(prop(c, "metricCompatibilities"), "metricMetadata", apiNames(def, false)) {
				return NewError("incompatible", 0, kind)
			}
		}

		var raw []RawRow
		offset := 0
		rowCount := -1.0
		pages := map[string]bool{}
		meta := jsjson.Object{}
		var q quota
		for {
			req := BuildRequest(def, opt, offset)
			j, err := client.RunReport(req, kind)
			if err != nil {
				return err
			}
			if q, err = checkQuota(j, kind); err != nil {
				return err
			}
			metricHeaders, _ := prop(j, "metricHeaders").([]any)
			if metricHeaders == nil {
				metricHeaders = []any{}
			}
			if jsjson.MustStringify(names(prop(j, "dimensionHeaders"))) != jsjson.MustStringify(apiNames(def, true)) || jsjson.MustStringify(names(metricHeaders)) != jsjson.MustStringify(apiNames(def, false)) {
				return NewError("invalid_response", 0, kind+" headers")
			}
			if m, ok := prop(j, "metadata").(jsjson.Object); ok {
				meta = m
			} else {
				meta = jsjson.Object{}
			}
			if prop(meta, "timeZone") != plan.Conn.TimeZone {
				return NewError("invalid_response", 0, kind+" time zone differs from the config")
			}
			// rowCount is fixed by the first page and must match on every page.
			// GA4 omits rowCount for empty results, so a missing value is read as 0 only when there are no rows
			rowsArr, _ := prop(j, "rows").([]any)
			rcv := prop(j, "rowCount")
			if rcv == undef && len(rowsArr) > 0 {
				return NewError("invalid_response", 0, kind+" rowCount")
			}
			if rcv == undef {
				rcv = 0.0
			}
			rc, ok := rcv.(float64)
			if !ok || rc != math.Trunc(rc) || math.Abs(rc) > 1<<53-1 || rc < 0 {
				return NewError("invalid_response", 0, kind+" rowCount")
			}
			if rowCount == -1 {
				rowCount = rc
			} else if rc != rowCount {
				return NewError("invalid_response", 0, kind+" rowCount differs between pages")
			}
			if rowsArr == nil {
				rowsArr = []any{}
			}
			// Fail if any earlier page comes back again
			fp := jsjson.MustStringify(rowsArr)
			if offset > 0 && (len(rowsArr) == 0 || pages[fp]) {
				return NewError("invalid_response", 0, kind+" paging")
			}
			pages[fp] = true
			types := make([]string, len(metricHeaders))
			for i, h := range metricHeaders {
				if ho, isO := h.(jsjson.Object); isO {
					types[i] = jsString(prop(ho, "type"))
				}
			}
			strs := func(v any, n int) ([]string, bool) {
				arr, ok := v.([]any)
				if !ok || len(arr) != n {
					return nil, false
				}
				out := make([]string, n)
				for i, x := range arr {
					xo, isO := x.(jsjson.Object)
					if !isO {
						return nil, false
					}
					s, isS := prop(xo, "value").(string)
					if !isS {
						return nil, false
					}
					out[i] = s
				}
				return out, true
			}
			for _, r := range rowsArr {
				ro, isO := r.(jsjson.Object)
				var dims, metrics []string
				okD, okM := false, false
				if isO {
					dims, okD = strs(prop(ro, "dimensionValues"), len(def.Dims))
					metrics, okM = strs(prop(ro, "metricValues"), len(def.Metrics))
				}
				if !isO || !okD || !okM {
					return NewError("invalid_response", 0, kind+" row format")
				}
				raw = append(raw, RawRow{Dims: dims, Metrics: metrics, Types: types})
			}
			if float64(len(raw)) > rowCount {
				return NewError("invalid_response", 0, kind+" more rows than rowCount")
			}
			if float64(len(raw)) == rowCount {
				break
			}
			if len(rowsArr) == 0 {
				return NewError("invalid_response", 0, kind+" paging")
			}
			offset += 250_000
		}

		rows, stats, err := TransformRows(def, raw, TransformOptions{Events: plan.Reports.Events, MinUsers: plan.Reports.MinUsers, Through: through})
		if err != nil {
			if se, ok := err.(*ReportShapeError); ok {
				return NewError("invalid_response", 0, kind+": "+se.Msg)
			}
			return err
		}
		ph := make([]string, len(cols))
		for i := range ph {
			ph[i] = "?"
		}
		ins := "INSERT INTO " + def.Table + " VALUES (" + strings.Join(ph, ", ") + ")"
		for _, r := range rows {
			for _, v := range r {
				if s, ok := v.(string); ok && secretShape(s) != "" {
					return NewError("invalid_response", 0, kind+": secret-looking value")
				}
			}
			if err := db.Exec(ins, r...); err != nil {
				return err
			}
		}
		var samples []jsjson.Object
		if arr, ok := prop(meta, "samplingMetadatas").([]any); ok {
			for _, x := range arr {
				if xo, isO := x.(jsjson.Object); isO {
					samples = append(samples, xo)
				}
			}
		}
		sampled := 0.0
		if len(samples) > 0 {
			sampled = 1
		}
		// Sampling summary: numbers only (samples read / sampling space size)
		parts := make([]string, len(samples))
		for i, x := range samples {
			parts[i] = countText(prop(x, "samplesReadCount")) + "/" + countText(prop(x, "samplingSpaceSize"))
		}
		truncated := ""
		if arr, ok := prop(meta, "dataTruncationReasons").([]any); ok {
			ts := make([]string, len(arr))
			for i, x := range arr {
				ts[i] = jsString(x)
			}
			truncated = strings.Join(ts, ",")
		}
		emptyReason, _ := prop(meta, "emptyReason").(string)
		if err := db.Exec(insLog, kind, def.Table, float64(len(rows)), rowCount, float64(stats.DroppedSmall), float64(stats.DroppedUnobserved), float64(stats.DroppedUnmapped), float64(stats.DroppedCollision), float64(stats.SuppressedCells),
			float64(truthy(prop(meta, "subjectToThresholding"))), float64(truthy(prop(meta, "dataLossFromOtherRow"))), sampled, strings.Join(parts, ";"), truncated, float64(truthy(prop(meta, "schemaRestrictionResponse"))),
			emptyReason, plan.Conn.TimeZone, through, float64(client.Calls-callsBefore), q.day, q.hour, isoString(now())); err != nil {
			return err
		}
	}
	return nil
}
