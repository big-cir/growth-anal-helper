package ga4

// GA4 report kinds known to the engine: building requests and turning responses into table rows.
// No free-text dimensions. Category values come from known lists; events are stored only as mapped keys.

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"growth-lab/internal/jsjson"
)

// ReportDefsVersion is bumped when report definitions, allow-lists, column names or small-value rules change.
const ReportDefsVersion = 2

// DimKind is how a dimension value is read.
type DimKind string

// Dim is one dimension. Fine: fine-grained (one per report, not with activity metrics).
type Dim struct {
	API     string
	Col     string
	Kind    DimKind
	Values  []string
	Pattern *regexp.Regexp
	Fine    bool
}

// Metric is one metric. User: user count, Group: user count that sets group size, Max: upper bound.
type Metric struct {
	API    string
	Col    string
	Int    bool
	User   bool
	Group  bool
	Max    *float64
	Custom bool
}

// ReportDef is a report. Key: report key in the collect log.
type ReportDef struct {
	Key     string
	Table   string
	Range   string // daily, weekly, monthly, cohort
	Dims    []Dim
	Metrics []Metric
	Custom  bool
}

var channels = []string{"Direct", "Organic Search", "Paid Search", "Organic Social", "Paid Social", "Email", "Affiliates", "Referral", "Paid Shopping", "Organic Shopping", "Display", "Paid Video", "Organic Video", "Audio", "SMS", "Mobile Push Notifications", "Cross-network", "Paid Other", "Unassigned"}

// CategoryValues are the category values defined by Google.
var CategoryValues = map[string][]string{
	"firstUserDefaultChannelGroup": channels,
	"sessionDefaultChannelGroup":   channels,
	"platform":                     {"Android", "iOS", "web"},
	"deviceCategory":               {"desktop", "mobile", "tablet", "smart tv"},
	"newVsReturning":               {"new", "returning"},
	"operatingSystem":              {"Android", "iOS", "Windows", "Macintosh", "Linux", "Chrome OS"},
}

func cat(api, col string) Dim {
	return Dim{API: api, Col: col, Kind: "category", Values: CategoryValues[api]}
}

// CustomDimensions are the dimensions available to custom reports, in definition order.
var CustomDimensions = []Dim{
	cat("firstUserDefaultChannelGroup", "first_user_channel_group"),
	cat("sessionDefaultChannelGroup", "session_channel_group"),
	cat("platform", "platform"),
	cat("deviceCategory", "device_category"),
	cat("newVsReturning", "new_vs_returning"),
	cat("operatingSystem", "operating_system"),
	{API: "dayOfWeek", Col: "day_of_week", Kind: "pattern", Pattern: regexp.MustCompile(`^[0-6]$`)},
	{API: "countryId", Col: "country_id", Kind: "pattern", Pattern: regexp.MustCompile(`^[A-Z]{2}$`), Fine: true},
	{API: "languageCode", Col: "language_code", Kind: "pattern", Pattern: regexp.MustCompile(`^[a-z]{2,3}$`), Fine: true},
	{API: "hour", Col: "hour", Kind: "pattern", Pattern: regexp.MustCompile(`^([01]\d|2[0-3])$`), Fine: true},
	{API: "eventName", Col: "event_key", Kind: "event", Fine: true},
}

func customDim(api string) (Dim, bool) {
	for _, d := range CustomDimensions {
		if d.API == api {
			return d, true
		}
	}
	return Dim{}, false
}

type mo struct {
	notInt, user, group bool
	max                 *float64
}

func mk(api, col string, o mo, custom bool) Metric {
	return Metric{API: api, Col: col, Int: !o.notInt, User: o.user, Group: o.group, Max: o.max, Custom: custom}
}

var one = 1.0

// Metrics known to the engine, in definition order. Custom: available to custom reports.
var Metrics = []Metric{
	mk("activeUsers", "active_users", mo{user: true, group: true}, true),
	mk("totalUsers", "total_users", mo{user: true, group: true}, true),
	mk("newUsers", "new_users", mo{user: true}, true),
	mk("sessions", "sessions", mo{}, true),
	mk("engagedSessions", "engaged_sessions", mo{}, true),
	mk("eventCount", "event_count", mo{}, true),
	mk("screenPageViews", "screen_page_views", mo{}, true),
	mk("userEngagementDuration", "engagement_seconds", mo{notInt: true}, true),
	mk("averageSessionDuration", "average_session_seconds", mo{notInt: true}, true),
	mk("engagementRate", "engagement_rate", mo{notInt: true, max: &one}, true),
	mk("cohortActiveUsers", "cohort_active_users", mo{user: true}, false),
	mk("cohortTotalUsers", "cohort_total_users", mo{user: true, group: true}, false),
}

func metric(api string) (Metric, bool) {
	for _, m := range Metrics {
		if m.API == api {
			return m, true
		}
	}
	return Metric{}, false
}

func mustMetric(api string) Metric {
	m, _ := metric(api)
	return m
}

// UserMetrics are the custom-report user-count metrics.
var UserMetrics = func() []string {
	var out []string
	for _, m := range Metrics {
		if m.User && m.Custom {
			out = append(out, m.API)
		}
	}
	return out
}()

var (
	dateDim  = Dim{API: "date", Col: "date", Kind: "date"}
	weekDim  = Dim{API: "isoYearIsoWeek", Col: "iso_year_week", Kind: "week"}
	monthDim = Dim{API: "yearMonth", Col: "year_month", Kind: "month"}
	timeDim  = map[string]Dim{"daily": dateDim, "weekly": weekDim, "monthly": monthDim}
)

func builtin(key, rng string, dims []Dim, metrics ...string) ReportDef {
	ms := make([]Metric, len(metrics))
	for i, m := range metrics {
		ms[i] = mustMetric(m)
	}
	return ReportDef{Key: key, Table: "r_ga4_" + key, Range: rng, Dims: dims, Metrics: ms}
}

func mustDim(api string) Dim {
	d, _ := customDim(api)
	return d
}

// ReportKindNames are the built-in report names in definition order.
var ReportKindNames = []string{"daily_overview", "weekly_users", "monthly_users", "daily_events", "daily_channel", "daily_platform", "daily_new_returning", "weekly_cohort"}

// ReportKinds are the built-in reports.
var ReportKinds = func() map[string]ReportDef {
	ch := mustDim("firstUserDefaultChannelGroup")
	ch.Col = "channel_group"
	return map[string]ReportDef{
		"daily_overview":      builtin("daily_overview", "daily", []Dim{dateDim}, "activeUsers", "newUsers", "totalUsers", "sessions", "engagedSessions", "userEngagementDuration"),
		"weekly_users":        builtin("weekly_users", "weekly", []Dim{weekDim}, "activeUsers", "newUsers"),
		"monthly_users":       builtin("monthly_users", "monthly", []Dim{monthDim}, "activeUsers", "newUsers"),
		"daily_events":        builtin("daily_events", "daily", []Dim{dateDim, mustDim("eventName")}, "eventCount", "totalUsers"),
		"daily_channel":       builtin("daily_channel", "daily", []Dim{dateDim, ch}, "newUsers", "activeUsers", "engagedSessions"),
		"daily_platform":      builtin("daily_platform", "daily", []Dim{dateDim, mustDim("platform"), mustDim("deviceCategory")}, "activeUsers", "newUsers"),
		"daily_new_returning": builtin("daily_new_returning", "daily", []Dim{dateDim, mustDim("newVsReturning")}, "activeUsers", "engagedSessions"),
		"weekly_cohort":       builtin("weekly_cohort", "cohort", []Dim{{API: "cohort", Col: "cohort", Kind: "cohort"}, {API: "cohortNthWeek", Col: "cohort_nth_week", Kind: "nth"}}, "cohortActiveUsers", "cohortTotalUsers"),
	}
}()

// CustomReport is a custom report from ga4-reports.json.
type CustomReport struct {
	ID         string
	Range      string
	Dimensions []string
	Metrics    []string
}

// CustomDef is the definition of a validated custom report.
func CustomDef(c CustomReport) ReportDef {
	dims := []Dim{timeDim[c.Range]}
	for _, d := range c.Dimensions {
		dims = append(dims, mustDim(d))
	}
	ms := make([]Metric, len(c.Metrics))
	for i, m := range c.Metrics {
		ms[i] = mustMetric(m)
	}
	return ReportDef{Key: "custom:" + c.ID, Table: "r_ga4_x_" + c.ID, Range: c.Range, Dims: dims, Metrics: ms, Custom: true}
}

// IsBreakdown: the table is split by category or event (time dimensions alone do not count).
func IsBreakdown(def ReportDef) bool {
	if def.Range == "cohort" {
		return false
	}
	for _, d := range def.Dims {
		if d.Kind == "category" || d.Kind == "pattern" || d.Kind == "event" {
			return true
		}
	}
	return false
}

// Internal codes that never clash with real values.
const (
	CodeUnknown  = "_unknown"
	CodeGA4Other = "_ga4_other"
)

var realTypes = map[string]bool{"TYPE_FLOAT": true, "TYPE_SECONDS": true, "TYPE_MILLISECONDS": true, "TYPE_MINUTES": true, "TYPE_HOURS": true, "TYPE_STANDARD": true, "TYPE_CURRENCY": true, "TYPE_FEET": true, "TYPE_MILES": true, "TYPE_METERS": true, "TYPE_KILOMETERS": true}

// ── Dates ───────────────────────────────────────────────

// DataThrough is "today minus 2 days" in the property time zone.
func DataThrough(nowMs int64, timeZone string) string {
	loc := loadLocation(timeZone)
	t := time.UnixMilli(nowMs).In(loc)
	return mustAddDays(fmt.Sprintf("%04d-%02d-%02d", t.Year(), int(t.Month()), t.Day()), -2)
}

var offsetPartsRE = regexp.MustCompile(`^([+-])(\d{2}):?(\d{2})?$`)

// loadLocation resolves what ValidTimeZone accepts.
func loadLocation(tz string) *time.Location {
	if m := offsetPartsRE.FindStringSubmatch(tz); m != nil && offsetRE.MatchString(tz) {
		h, _ := strconv.Atoi(m[2])
		mi, _ := strconv.Atoi(m[3])
		off := h*3600 + mi*60
		if m[1] == "-" {
			off = -off
		}
		return time.FixedZone(tz, off)
	}
	if l, err := time.LoadLocation(tz); err == nil {
		return l
	}
	if name, ok := zoneNames()[strings.ToLower(tz)]; ok {
		if l, err := time.LoadLocation(name); err == nil {
			return l
		}
	}
	return time.UTC
}

// Period is a date range.
type Period struct{ Start, End string }

// CompleteRange trims to complete ISO weeks (Mon-Sun) or months; nil if none.
func CompleteRange(rng, start, through string) *Period {
	var s, e string
	if rng == "weekly" {
		s = mustAddDays(start, (8-dow(start))%7)
		e = mustAddDays(through, -dow(through))
	} else {
		if hasSuffix(start, "-01") {
			s = start
		} else {
			s = mustAddDays(start[:7]+"-01", 32)[:7] + "-01"
		}
		if hasSuffix(mustAddDays(through, 1), "-01") {
			e = through
		} else {
			e = mustAddDays(through[:7]+"-01", -1)
		}
	}
	if s <= e {
		return &Period{s, e}
	}
	return nil
}

// CohortWeeks are the last n complete acquisition weeks (Sun-Sat).
func CohortWeeks(through string, n int) []string {
	lastSat := mustAddDays(through, -((dow(through) + 1) % 7))
	out := make([]string, n)
	for i := range out {
		out[i] = mustAddDays(lastSat, -6-7*(n-1-i))
	}
	return out
}

// isoWeekMonday is the Monday of an ISO week.
func isoWeekMonday(year, week int) string {
	jan4 := jsUTC(year, 0, 4)
	wd := int(time.UnixMilli(jan4).UTC().Weekday())
	mondayW1 := jan4 - int64((wd+6)%7)*dayMs
	return ymd(mondayW1 + int64(week-1)*7*dayMs)
}

// ── Requests ────────────────────────────────────────────

// RequestOptions are the date range and the event map.
type RequestOptions struct {
	Start   string
	Through string
	Events  jsjson.Object // GA4 event name → key
}

// BuildRequest builds a runReport body, or nil when there is no complete period.
func BuildRequest(def ReportDef, o RequestOptions, offset int) jsjson.Object {
	dims := make([]any, len(def.Dims))
	order := make([]any, len(def.Dims))
	for i, d := range def.Dims {
		dims[i] = jsjson.Object{{Key: "name", Value: d.API}}
		order[i] = jsjson.Object{{Key: "dimension", Value: jsjson.Object{{Key: "dimensionName", Value: d.API}}}}
	}
	metrics := make([]any, len(def.Metrics))
	for i, m := range def.Metrics {
		metrics[i] = jsjson.Object{{Key: "name", Value: m.API}}
	}
	base := jsjson.Object{{Key: "dimensions", Value: dims}, {Key: "metrics", Value: metrics}, {Key: "orderBys", Value: order}, {Key: "limit", Value: 250000}, {Key: "offset", Value: offset}, {Key: "returnPropertyQuota", Value: true}}
	if def.Range == "cohort" {
		var cohorts []any
		for _, w := range CohortWeeks(o.Through, 12) {
			if w >= o.Start {
				cohorts = append(cohorts, jsjson.Object{{Key: "name", Value: w}, {Key: "dimension", Value: "firstSessionDate"}, {Key: "dateRange", Value: jsjson.Object{{Key: "startDate", Value: w}, {Key: "endDate", Value: mustAddDays(w, 6)}}}})
			}
		}
		if len(cohorts) == 0 {
			return nil
		}
		return append(base, jsjson.Member{Key: "cohortSpec", Value: jsjson.Object{
			{Key: "cohorts", Value: cohorts},
			{Key: "cohortsRange", Value: jsjson.Object{{Key: "granularity", Value: "WEEKLY"}, {Key: "startOffset", Value: 0}, {Key: "endOffset", Value: 11}}},
		}})
	}
	var r *Period
	if def.Range == "daily" {
		if o.Start <= o.Through {
			r = &Period{o.Start, o.Through}
		}
	} else {
		r = CompleteRange(def.Range, o.Start, o.Through)
	}
	if r == nil {
		return nil
	}
	req := append(base, jsjson.Member{Key: "dateRanges", Value: []any{jsjson.Object{{Key: "startDate", Value: r.Start}, {Key: "endDate", Value: r.End}}}})
	for _, d := range def.Dims {
		if d.Kind == "event" {
			names := make([]any, len(o.Events))
			for i, e := range o.Events {
				names[i] = e.Key
			}
			req = append(req, jsjson.Member{Key: "dimensionFilter", Value: jsjson.Object{{Key: "filter", Value: jsjson.Object{{Key: "fieldName", Value: "eventName"}, {Key: "inListFilter", Value: jsjson.Object{{Key: "values", Value: names}, {Key: "caseSensitive", Value: true}}}}}}})
			break
		}
	}
	return req
}

// ── Response → rows ─────────────────────────────────────

// ReportShapeError is an unexpected response shape.
type ReportShapeError struct{ Msg string }

func (e *ReportShapeError) Error() string { return e.Msg }

// Row values are string, float64 or nil.
type Row []any

// TransformStats count dropped rows and blanked cells.
type TransformStats struct{ DroppedSmall, DroppedUnobserved, DroppedUnmapped, DroppedCollision, SuppressedCells int }

// RawRow is one response row.
type RawRow struct {
	Dims, Metrics, Types []string
}

var digits8RE = regexp.MustCompile(`^\d{8}$`)
var digits6RE = regexp.MustCompile(`^\d{6}$`)
var nthRE = regexp.MustCompile(`^\d{1,4}$`)

// dimValue: nil for events not in the map (the row is not stored).
func dimValue(d Dim, v string, events jsjson.Object) ([]any, error) {
	shape := func() error { return &ReportShapeError{d.API + " value format"} }
	switch d.Kind {
	case "date":
		date := sl(v, 0, 4) + "-" + sl(v, 4, 6) + "-" + sl(v, 6, 8)
		if !digits8RE.MatchString(v) {
			return nil, shape()
		}
		ok, err := realDate(date)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, shape()
		}
		return []any{date}, nil
	case "week":
		week := jsNumber(sl(v, 4, len(v)))
		if !digits6RE.MatchString(v) || week < 1 || week > 53 {
			return nil, shape()
		}
		y, _ := strconv.Atoi(v[:4])
		mon := isoWeekMonday(y, int(week))
		end, err := AddDays(mon, 6)
		if err != nil {
			return nil, err
		}
		return []any{v, mon, end}, nil
	case "month":
		first := sl(v, 0, 4) + "-" + sl(v, 4, len(v)) + "-01"
		if !digits6RE.MatchString(v) {
			return nil, shape()
		}
		ok, err := realDate(first)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, shape()
		}
		return []any{v, first, mustAddDays(mustAddDays(first, 32)[:7]+"-01", -1)}, nil
	case "nth":
		if !nthRE.MatchString(v) {
			return nil, shape()
		}
		return []any{jsNumber(v)}, nil
	case "cohort":
		ok, err := realDate(v)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, shape()
		}
		return []any{v}, nil
	case "event":
		if k, ok := events.Get(v); ok {
			return []any{k}, nil
		}
		return nil, nil
	case "category":
		if v == "(other)" {
			return []any{CodeGA4Other}, nil
		}
		for _, x := range d.Values {
			if x == v {
				return []any{v}, nil
			}
		}
		return []any{CodeUnknown}, nil
	case "pattern":
		if v == "(other)" {
			return []any{CodeGA4Other}, nil
		}
		if d.Pattern.MatchString(v) {
			return []any{v}, nil
		}
		return []any{CodeUnknown}, nil
	}
	return nil, fmt.Errorf("unknown dimension kind %s", d.Kind)
}

// sl is String.prototype.slice on UTF-16 indexes, for the ASCII-checked values used here.
func sl(s string, a, b int) string {
	u := []rune(s)
	if a > len(u) {
		a = len(u)
	}
	if b > len(u) {
		b = len(u)
	}
	if a >= b {
		return ""
	}
	return string(u[a:b])
}

// Column is a table column.
type Column struct {
	Name     string
	Type     string // TEXT, INTEGER, REAL
	Key      bool
	Nullable bool
}

// TableColumns: weeks and months add period start and end. Integer metrics in breakdown tables and cohort active users allow NULL.
func TableColumns(def ReportDef) []Column {
	var cols []Column
	for _, d := range def.Dims {
		t := "TEXT"
		if d.Kind == "nth" {
			t = "INTEGER"
		}
		cols = append(cols, Column{Name: d.Col, Type: t, Key: true})
		if d.Kind == "week" || d.Kind == "month" {
			cols = append(cols, Column{Name: "period_start", Type: "TEXT"}, Column{Name: "period_end", Type: "TEXT"})
		}
	}
	breakdown := IsBreakdown(def)
	for _, m := range def.Metrics {
		t := "REAL"
		if m.Int {
			t = "INTEGER"
		}
		cols = append(cols, Column{Name: m.Col, Type: t, Nullable: (breakdown && m.Int) || (def.Range == "cohort" && m.API == "cohortActiveUsers")})
	}
	return cols
}

// TransformOptions are the event map, the small-value threshold and the last data day.
type TransformOptions struct {
	Events   jsjson.Object
	MinUsers float64
	Through  string
}

// TransformRows turns response rows into table rows.
func TransformRows(def ReportDef, raw []RawRow, o TransformOptions) ([]Row, TransformStats, error) {
	var stats TransformStats
	cols := TableColumns(def)
	var keyCols []int
	for i, c := range cols {
		if c.Key {
			keyCols = append(keyCols, i)
		}
	}
	mOff := len(cols) - len(def.Metrics)
	// 1) Raw duplicates are a response error. Rows that collide after normalization fail built-in reports; custom reports drop all rows with that key
	rawSeen := map[string]bool{}
	var order []string
	groups := map[string][]Row{}
	for _, r := range raw {
		dimsAny := make([]any, len(r.Dims))
		for i, d := range r.Dims {
			dimsAny[i] = d
		}
		rawKey := jsjson.MustStringify(dimsAny)
		if rawSeen[rawKey] {
			return nil, stats, &ReportShapeError{"more than one row with the same raw key (" + def.Table + ")"}
		}
		rawSeen[rawKey] = true
		var dims []any
		unmapped := false
		for i, d := range def.Dims {
			v := ""
			if i < len(r.Dims) {
				v = r.Dims[i]
			}
			p, err := dimValue(d, v, o.Events)
			if err != nil {
				return nil, stats, err
			}
			if p == nil {
				unmapped = true
			}
			dims = append(dims, p...)
		}
		if unmapped {
			stats.DroppedUnmapped++
			continue
		}
		row := Row(dims)
		for i, m := range def.Metrics {
			typ := r.Types[i]
			n := jsNumber(r.Metrics[i])
			if r.Metrics[i] == "" || math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, stats, &ReportShapeError{m.API + ": not a number"}
			}
			if typ == "TYPE_INTEGER" {
				if n != math.Trunc(n) || math.Abs(n) > 1<<53-1 {
					return nil, stats, &ReportShapeError{m.API + ": not a safe integer"}
				}
			} else if !realTypes[typ] || m.Int {
				return nil, stats, &ReportShapeError{m.API + ": metric type " + typ}
			}
			if n < 0 || (m.Max != nil && n > *m.Max) {
				return nil, stats, &ReportShapeError{m.API + ": value out of range"}
			}
			row = append(row, n)
		}
		keyParts := make([]any, len(keyCols))
		for i, k := range keyCols {
			keyParts[i] = row[k]
		}
		key := jsjson.MustStringify(keyParts)
		if g, ok := groups[key]; ok {
			if !def.Custom {
				return nil, stats, &ReportShapeError{"more than one row with the same key (" + def.Table + ")"}
			}
			groups[key] = append(g, row)
		} else {
			groups[key] = []Row{row}
			order = append(order, key)
		}
	}
	var all []Row
	for _, k := range order {
		g := groups[k]
		if len(g) > 1 {
			stats.DroppedCollision += len(g)
		} else {
			all = append(all, g[0])
		}
	}
	at := func(api string) int {
		for i, m := range def.Metrics {
			if m.API == api {
				return mOff + i
			}
		}
		return mOff - 1
	}
	// 2) Cohorts: drop cells still being observed and small cohorts; blank small active counts
	if def.Range == "cohort" {
		totalIdx, activeIdx := at("cohortTotalUsers"), at("cohortActiveUsers")
		small := map[string]bool{}
		for _, r := range all {
			if r[totalIdx].(float64) < o.MinUsers {
				small[r[0].(string)] = true
			}
		}
		rows := []Row{}
		for _, r := range all {
			end, err := AddDays(r[0].(string), int(7*(r[1].(float64)+1)-1))
			if err != nil {
				return nil, stats, err
			}
			if end > o.Through {
				stats.DroppedUnobserved++
				continue
			}
			if small[r[0].(string)] {
				stats.DroppedSmall++
				continue
			}
			if r[activeIdx].(float64) < o.MinUsers {
				stats.SuppressedCells++
				c := append(Row(nil), r...)
				c[activeIdx] = nil
				r = c
			}
			rows = append(rows, r)
		}
		return rows, stats, nil
	}
	// 3) Breakdown tables: drop rows whose group (max of active and total users) is small; blank small integer values in the rest
	if !IsBreakdown(def) {
		if all == nil {
			all = []Row{}
		}
		return all, stats, nil
	}
	var groupIdx, intIdx []int
	for i, m := range def.Metrics {
		if m.Group {
			groupIdx = append(groupIdx, mOff+i)
		}
		if m.Int {
			intIdx = append(intIdx, mOff+i)
		}
	}
	rows := []Row{}
	for _, r := range all {
		max := math.Inf(-1)
		for _, i := range groupIdx {
			max = math.Max(max, r[i].(float64))
		}
		if max < o.MinUsers {
			stats.DroppedSmall++
			continue
		}
		c := append(Row(nil), r...)
		for _, i := range intIdx {
			if c[i].(float64) < o.MinUsers {
				stats.SuppressedCells++
				c[i] = nil
			}
		}
		rows = append(rows, c)
	}
	return rows, stats, nil
}

func hasSuffix(s, suf string) bool { return len(s) >= len(suf) && s[len(s)-len(suf):] == suf }
