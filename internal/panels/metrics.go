package panels

import (
	"fmt"
	"growth-lab/internal/jsstr"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"growth-lab/internal/jsjson"
)

// Metric is one dictionary metric: the tables and definition to use.
type Metric struct {
	ID         string
	Name       string
	Asks       []string
	Tables     []string
	Definition [][2]string
	SeedPanel  *string
}

// MetricDict is metrics.json.
type MetricDict struct {
	DimensionTables []string
	Metrics         []Metric
}

// MetricsError is a metrics.json error.
type MetricsError struct{ Msg string }

func (e *MetricsError) Error() string { return e.Msg }

// MetricsLimits are the dictionary limits.
var MetricsLimits = struct{ Metrics, FileChars, Name, Asks, AskText, Tables, Definition, DefinitionText int }{30, 12000, 40, 6, 100, 8, 10, 300}

var (
	metricIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	tableRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func mFail(path, msg string) { throw(&MetricsError{"metrics.json" + path + ": " + msg}) }

func mStr(v any, path string, max int) string {
	s, ok := v.(string)
	if !ok || jsstr.Trim(s) == "" {
		mFail(path, "must be a non-empty string")
	}
	if jsstr.CPLen(s) > max {
		mFail(path, fmt.Sprintf("at most %d characters", max))
	}
	return s
}

func mList(v any, path string, min, max int) []any {
	a, ok := v.([]any)
	if !ok || len(a) < min || len(a) > max {
		mFail(path, fmt.Sprintf("must be an array of %d–%d items", min, max))
	}
	return a
}

func tableName(v any, path string, prefixes []string) string {
	s, ok := v.(string)
	if !ok || !tableRE.MatchString(s) {
		mFail(path, "must be a table name")
	}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return s
		}
	}
	mFail(path, fmt.Sprintf("must be a panel table (%s): %s", strings.Join(prefixes, ", "), s))
	return ""
}

// ParseMetrics validates metrics.json text.
func ParseMetrics(text string, panelPrefixes []string) (dict MetricDict, err error) {
	defer catch(&err)
	if jsstr.U16Len(text) > MetricsLimits.FileChars {
		mFail("", fmt.Sprintf("at most %d characters", MetricsLimits.FileChars))
	}
	raw, perr := jsjson.Parse(text)
	if perr != nil {
		mFail("", "JSON error: "+perr.Error())
	}
	o, ok := raw.(jsjson.Object)
	if !ok {
		mFail("", "must be an object")
	}
	for _, m := range o {
		if m.Key != "dimension_tables" && m.Key != "metrics" {
			mFail("."+m.Key, "unknown key")
		}
	}
	dict.DimensionTables = []string{}
	if v, ok := o.Get("dimension_tables"); ok {
		for i, t := range mList(v, ".dimension_tables", 0, 8) {
			dict.DimensionTables = append(dict.DimensionTables, tableName(t, fmt.Sprintf(".dimension_tables[%d]", i), panelPrefixes))
		}
	}
	ids := map[string]bool{}
	mv, _ := o.Get("metrics")
	dict.Metrics = []Metric{}
	for i, mr := range mList(mv, ".metrics", 0, MetricsLimits.Metrics) {
		p := fmt.Sprintf(".metrics[%d]", i)
		m, ok := mr.(jsjson.Object)
		if !ok {
			mFail(p, "must be an object")
		}
		for _, e := range m {
			if !contains([]string{"id", "name", "asks", "tables", "definition", "seed_panel"}, e.Key) {
				mFail(p+"."+e.Key, "unknown key")
			}
		}
		idv, _ := m.Get("id")
		id, ok := idv.(string)
		if !ok || !metricIDRE.MatchString(id) {
			mFail(p+".id", "^[a-z][a-z0-9_]{0,47}$")
		}
		if ids[id] {
			mFail(p+".id", "duplicate: "+id)
		}
		ids[id] = true
		tv, _ := m.Get("tables")
		var tables []string
		for j, t := range mList(tv, p+".tables", 1, MetricsLimits.Tables) {
			tables = append(tables, tableName(t, fmt.Sprintf("%s.tables[%d]", p, j), panelPrefixes))
		}
		for _, t := range tables {
			if contains(dict.DimensionTables, t) {
				mFail(p+".tables", "also in dimension_tables: "+t)
			}
		}
		nv, _ := m.Get("name")
		metric := Metric{ID: id, Name: mStr(nv, p+".name", MetricsLimits.Name)}
		av, _ := m.Get("asks")
		for j, a := range mList(av, p+".asks", 1, MetricsLimits.Asks) {
			metric.Asks = append(metric.Asks, mStr(a, fmt.Sprintf("%s.asks[%d]", p, j), MetricsLimits.AskText))
		}
		for _, t := range tables {
			if !contains(metric.Tables, t) {
				metric.Tables = append(metric.Tables, t)
			}
		}
		dv, _ := m.Get("definition")
		for j, d := range mList(dv, p+".definition", 1, MetricsLimits.Definition) {
			pair, ok := d.([]any)
			if !ok || len(pair) != 2 {
				mFail(fmt.Sprintf("%s.definition[%d]", p, j), "must be an [item, text] pair")
			}
			metric.Definition = append(metric.Definition, [2]string{mStr(pair[0], fmt.Sprintf("%s.definition[%d][0]", p, j), 40), mStr(pair[1], fmt.Sprintf("%s.definition[%d][1]", p, j), MetricsLimits.DefinitionText)})
		}
		if sv, ok := m.Get("seed_panel"); ok && sv != nil {
			s := mStr(sv, p+".seed_panel", 64)
			metric.SeedPanel = &s
		}
		dict.Metrics = append(dict.Metrics, metric)
	}
	return dict, nil
}

// Seed is a seed panel's id and metric.
type Seed struct {
	ID     string
	Metric *string
}

// LoadMetrics reads metrics.json (an empty dictionary if missing) and checks that seed panels and dictionary seed_panel entries agree.
func LoadMetrics(wsDir string, panelPrefixes []string, seeds []Seed) (dict MetricDict, text string, err error) {
	b, rerr := os.ReadFile(filepath.Join(wsDir, "metrics.json"))
	if os.IsNotExist(rerr) {
		return MetricDict{DimensionTables: []string{}, Metrics: []Metric{}}, "", nil
	}
	if rerr != nil {
		return dict, "", rerr
	}
	text = string(b)
	if dict, err = ParseMetrics(text, panelPrefixes); err != nil {
		return dict, "", err
	}
	err = seedCheck(dict, seeds)
	return dict, text, err
}

func seedCheck(dict MetricDict, seeds []Seed) (err error) {
	defer catch(&err)
	for _, m := range dict.Metrics {
		if m.SeedPanel == nil {
			continue
		}
		var s *Seed
		for i := range seeds {
			if seeds[i].ID == *m.SeedPanel {
				s = &seeds[i]
				break
			}
		}
		if s == nil {
			mFail("("+m.ID+").seed_panel", "no such seed panel: "+*m.SeedPanel)
		}
		if s.Metric == nil || *s.Metric != m.ID {
			mFail("("+m.ID+").seed_panel", "seed panel "+s.ID+" has a metric other than "+m.ID)
		}
	}
	for _, s := range seeds {
		if s.Metric == nil {
			continue
		}
		found := false
		for _, m := range dict.Metrics {
			if m.ID == *s.Metric {
				found = true
			}
		}
		if !found {
			mFail("", "seed panel "+s.ID+" has a metric not in the dictionary: "+*s.Metric)
		}
	}
	return nil
}

// MetricTablesProblem checks the table rule for dictionary-metric panels; "" when fine.
func MetricTablesProblem(dict MetricDict, metric string, tables []string) string {
	var m *Metric
	for i := range dict.Metrics {
		if dict.Metrics[i].ID == metric {
			m = &dict.Metrics[i]
			break
		}
	}
	if m == nil {
		return "metric not in the dictionary: " + metric + " (use a dictionary id, or null for a definition outside the dictionary)"
	}
	var allowed []string
	for _, t := range append(append([]string{}, m.Tables...), dict.DimensionTables...) {
		if !contains(allowed, t) {
			allowed = append(allowed, t)
		}
	}
	var outside []string
	for _, t := range tables {
		if !contains(allowed, t) {
			outside = append(outside, t)
		}
	}
	if len(outside) > 0 {
		return fmt.Sprintf("metric %s (%s) may only use these tables: %s. Outside tables: %s", m.ID, m.Name, strings.Join(allowed, ", "), strings.Join(outside, ", "))
	}
	for _, t := range m.Tables {
		if contains(tables, t) {
			return ""
		}
	}
	return fmt.Sprintf("metric %s (%s) must read at least one of its tables (%s)", m.ID, m.Name, strings.Join(m.Tables, ", "))
}

// MetricsContext is the dictionary as given to the agent.
func MetricsContext(dict MetricDict) string {
	if len(dict.Metrics) == 0 {
		return "(no metrics in the dictionary; every panel uses metric: null)"
	}
	dims := ""
	if len(dict.DimensionTables) > 0 {
		dims = "Dimension tables (usable with every metric): " + strings.Join(dict.DimensionTables, ", ") + "\n\n"
	}
	blocks := make([]string, len(dict.Metrics))
	for i, m := range dict.Metrics {
		lines := []string{"### " + m.ID + ": " + m.Name, "- Questions like: " + strings.Join(m.Asks, " / "), "- Tables: " + strings.Join(m.Tables, ", ")}
		for _, d := range m.Definition {
			lines = append(lines, "- "+d[0]+": "+d[1])
		}
		if m.SeedPanel != nil && *m.SeedPanel != "" {
			lines = append(lines, "- Example panel: "+*m.SeedPanel)
		}
		blocks[i] = strings.Join(lines, "\n")
	}
	return dims + strings.Join(blocks, "\n\n")
}

// JS returns the dictionary in its stored JSON form.
func (d MetricDict) JS() jsjson.Object {
	ms := make([]any, len(d.Metrics))
	for i, m := range d.Metrics {
		ms[i] = jsjson.Object{{Key: "id", Value: m.ID}, {Key: "name", Value: m.Name}, {Key: "asks", Value: m.Asks}, {Key: "tables", Value: m.Tables}, {Key: "definition", Value: definitionJS(m.Definition)}, {Key: "seed_panel", Value: strPtr(m.SeedPanel)}}
	}
	return jsjson.Object{{Key: "dimension_tables", Value: d.DimensionTables}, {Key: "metrics", Value: ms}}
}
