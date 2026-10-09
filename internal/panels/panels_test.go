package panels

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

type out struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
}

func canon(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return "undefined"
	}
	s, err := jsjson.Compact(raw)
	if err != nil {
		t.Fatalf("bad vector JSON %s: %v", raw, err)
	}
	return s
}

func parse(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	v, err := jsjson.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func load(t *testing.T, name string, v any) {
	t.Helper()
	if err := contract.Load(name, v); err != nil {
		t.Fatal(err)
	}
}

// expect compares a Go result (value or error) with an expected out.
func expect(t *testing.T, label string, want out, got any, err error) {
	t.Helper()
	if !want.OK {
		if err == nil || err.Error() != want.Error {
			t.Errorf("%s: got %v / %v, want error %q", label, jsonOrNil(got), err, want.Error)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: unexpected error %v, want %s", label, err, want.Value)
		return
	}
	if g, w := jsjson.MustStringify(got), canon(t, want.Value); g != w {
		t.Errorf("%s:\n got  %s\n want %s", label, g, w)
	}
}

func jsonOrNil(v any) string {
	s, err := jsjson.Stringify(v)
	if err != nil {
		return "?"
	}
	return s
}

func columnsOf(names []string) []Column {
	out := make([]Column, len(names))
	for i, n := range names {
		out[i] = Column{Name: n}
	}
	return out
}

func rowsOf(t *testing.T, raw json.RawMessage) []Row {
	v := parse(t, raw).([]any)
	out := make([]Row, len(v))
	for i, r := range v {
		out[i] = r.([]any)
	}
	return out
}

func mustSpec(t *testing.T, raw json.RawMessage) Spec {
	s, err := ParseSpec(parse(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func violationsJS(vs []Violation) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = v.JS()
	}
	return out
}

func TestSpec(t *testing.T) {
	var v struct {
		Cases []struct {
			Input json.RawMessage `json:"input"`
			Out   out             `json:"out"`
			Extra *struct {
				Display    json.RawMessage `json:"display"`
				Semantic   string          `json:"semantic"`
				Normalized string          `json:"normalized"`
			} `json:"extra"`
		} `json:"cases"`
	}
	load(t, "panels-spec.json", &v)
	for i, c := range v.Cases {
		label := "spec " + strconv.Itoa(i) + " " + string(c.Input)
		s, err := ParseSpec(parse(t, c.Input))
		var got any
		if err == nil {
			got = s.JS()
		}
		expect(t, label, c.Out, got, err)
		if err == nil && c.Extra != nil {
			if g, w := jsjson.MustStringify(DisplayJSON(s.Display)), canon(t, c.Extra.Display); g != w {
				t.Errorf("%s display: got %s want %s", label, g, w)
			}
			if g := SemanticHash(s, "params-hash"); g != c.Extra.Semantic {
				t.Errorf("%s semanticHash: got %s want %s", label, g, c.Extra.Semantic)
			}
			if g := NormalizeSQL(s.SQL); g != c.Extra.Normalized {
				t.Errorf("%s normalizeSql: got %q want %q", label, g, c.Extra.Normalized)
			}
		}
	}
	t.Logf("%d spec cases", len(v.Cases))
}

func TestResults(t *testing.T) {
	var v struct {
		Cases []struct {
			Spec          json.RawMessage `json:"spec"`
			Columns       []string        `json:"columns"`
			Rows          json.RawMessage `json:"rows"`
			Contract      out             `json:"contract"`
			Invariants    out             `json:"invariants"`
			Formatted     out             `json:"formatted"`
			CaveatsEN     out             `json:"caveats_en"`
			CaveatsKO     out             `json:"caveats_ko"`
			Headline      out             `json:"headline"`
			SummaryResult json.RawMessage `json:"summaryResult"`
			Labels        []string        `json:"labels"`
		} `json:"cases"`
		Compare []struct {
			A, B json.RawMessage
			Out  float64 `json:"out"`
		} `json:"compare"`
		Big struct {
			Columns []string        `json:"columns"`
			Rows    json.RawMessage `json:"rows"`
			Out     json.RawMessage `json:"out"`
		} `json:"big"`
	}
	load(t, "panels-results.json", &v)
	for i, c := range v.Cases {
		label := "result " + strconv.Itoa(i) + " " + string(c.Spec)
		s := mustSpec(t, c.Spec)
		cols := columnsOf(c.Columns)
		rows := rowsOf(t, c.Rows)
		expect(t, label+" contract", c.Contract, nil, CheckContract(s, cols))
		vs, err := CheckInvariants(s, cols, rows)
		expect(t, label+" invariants", c.Invariants, violationsJS(vs), err)
		var formatted any
		if err == nil {
			formatted = FormatViolations(vs, 2)
		}
		expect(t, label+" formatted", c.Formatted, formatted, err)
		for _, lc := range []struct {
			lang string
			want out
		}{{"en", c.CaveatsEN}, {"ko", c.CaveatsKO}} {
			cv, err := EffectiveCaveats(s, cols, rows, lc.lang)
			expect(t, label+" caveats "+lc.lang, lc.want, cv, err)
		}
		h, err := ComputeHeadline(s, cols, rows)
		var hv any
		if err == nil && h != nil {
			hv = jsjson.Object{{Key: "json", Value: jsjson.MustStringify(h.JS())}, {Key: "value", Value: jsString(h.Value)}}
		}
		expect(t, label+" headline", c.Headline, hv, err)
		if g, w := jsjson.MustStringify(SummaryResult(cols, rows)), canon(t, c.SummaryResult); g != w {
			t.Errorf("%s summaryResult: got %s want %s", label, g, w)
		}
		if g, w := strings.Join(AllowedLabels(s, cols, rows), "|"), strings.Join(c.Labels, "|"); g != w {
			t.Errorf("%s labels: got %s want %s", label, g, w)
		}
	}
	for _, c := range v.Compare {
		if g := CompareValues(parse(t, c.A), parse(t, c.B)); g != c.Out {
			t.Errorf("compare %s %s: got %v want %v", c.A, c.B, g, c.Out)
		}
	}
	if g, w := jsjson.MustStringify(SummaryResult(columnsOf(v.Big.Columns), rowsOf(t, v.Big.Rows))), canon(t, v.Big.Out); g != w {
		t.Errorf("big summaryResult:\n got  %s\n want %s", g, w)
	}
	t.Logf("%d result cases", len(v.Cases))
}

// jsString is String(v).
func jsString(v Value) string {
	if v == nil {
		return "null"
	}
	return show(v)
}

func TestSummary(t *testing.T) {
	var v struct {
		Parse []struct {
			Input json.RawMessage `json:"input"`
			Out   out             `json:"out"`
		} `json:"parse"`
		Spec    json.RawMessage `json:"spec"`
		Columns []string        `json:"columns"`
		Rows    json.RawMessage `json:"rows"`
		Checks  []struct {
			Lang  string          `json:"lang"`
			Draft json.RawMessage `json:"draft"`
			Out   json.RawMessage `json:"out"`
		} `json:"checks"`
		Formats []struct {
			Lang string `json:"lang"`
			Op   string `json:"op"`
			X    string `json:"x"`
			Out  string `json:"out"`
		} `json:"formats"`
		Labels struct {
			Input   json.RawMessage `json:"input"`
			Columns []string        `json:"columns"`
			Rows    json.RawMessage `json:"rows"`
			Out     []string        `json:"out"`
		} `json:"labels"`
		Stray []struct {
			Text   string   `json:"text"`
			Labels []string `json:"labels"`
			Out    []string `json:"out"`
		} `json:"stray"`
	}
	load(t, "panels-summary.json", &v)
	for i, c := range v.Parse {
		d, err := ParseSummary(parse(t, c.Input))
		var got any
		if err == nil {
			got = draftJS(d)
		}
		expect(t, "parse "+strconv.Itoa(i)+" "+string(c.Input), c.Out, got, err)
	}
	s := mustSpec(t, v.Spec)
	cols := columnsOf(v.Columns)
	rows := rowsOf(t, v.Rows)
	for i, c := range v.Checks {
		d, err := ParseSummary(parse(t, c.Draft))
		if err != nil {
			t.Fatal(err)
		}
		if g, w := jsjson.MustStringify(CheckSummary(d, s, cols, rows, c.Lang).JS()), canon(t, c.Out); g != w {
			t.Errorf("check %d %s %s:\n got  %s\n want %s", i, c.Lang, c.Draft, g, w)
		}
	}
	for _, c := range v.Formats {
		x, _ := strconv.ParseFloat(c.X, 64)
		if g := FormatClaim(c.Op, x, c.Lang); g != c.Out {
			t.Errorf("formatClaim %s %s %s: got %q want %q", c.Lang, c.Op, c.X, g, c.Out)
		}
	}
	ls := mustSpec(t, v.Labels.Input)
	if g, w := strings.Join(AllowedLabels(ls, columnsOf(v.Labels.Columns), rowsOf(t, v.Labels.Rows)), "|"), strings.Join(v.Labels.Out, "|"); g != w {
		t.Errorf("labels: got %s want %s", g, w)
	}
	for _, c := range v.Stray {
		if g, w := strings.Join(StrayNumbers(c.Text, c.Labels), "|"), strings.Join(c.Out, "|"); g != w {
			t.Errorf("stray %q: got %s want %s", c.Text, g, w)
		}
	}
	t.Logf("%d parse, %d checks, %d formats, %d stray", len(v.Parse), len(v.Checks), len(v.Formats), len(v.Stray))
}

func draftJS(d SummaryDraft) jsjson.Object {
	claims := make([]any, len(d.Claims))
	for i, c := range d.Claims {
		refs := make([]any, len(c.Refs))
		for j, r := range c.Refs {
			row := make([]any, len(r.Row))
			for k, kv := range r.Row {
				row[k] = jsjson.Object{{Key: "column", Value: kv[0]}, {Key: "value", Value: kv[1]}}
			}
			refs[j] = jsjson.Object{{Key: "row", Value: row}, {Key: "column", Value: r.Column}}
		}
		claims[i] = jsjson.Object{{Key: "id", Value: c.ID}, {Key: "op", Value: c.Op}, {Key: "refs", Value: refs}, {Key: "display", Value: c.Display}}
	}
	return jsjson.Object{{Key: "prose", Value: d.Prose}, {Key: "claims", Value: claims}}
}

func TestMetrics(t *testing.T) {
	var v struct {
		Parse []struct {
			Text      string   `json:"text"`
			Prefixes  []string `json:"prefixes"`
			Out       out      `json:"out"`
			JSONError bool     `json:"jsonError"`
		} `json:"parse"`
		Seeds []struct {
			Metrics string `json:"metrics"`
			Seeds   []struct {
				ID     string  `json:"id"`
				Metric *string `json:"metric"`
			} `json:"seeds"`
			Out out `json:"out"`
		} `json:"seeds"`
		Problems []struct {
			Metric string   `json:"metric"`
			Tables []string `json:"tables"`
			Out    *string  `json:"out"`
		} `json:"problems"`
		Context []struct {
			Text string `json:"text"`
			Out  string `json:"out"`
		} `json:"context"`
	}
	load(t, "panels-metrics.json", &v)
	for i, c := range v.Parse {
		d, err := ParseMetrics(c.Text, c.Prefixes)
		if c.JSONError {
			if err == nil || !strings.HasPrefix(err.Error(), c.Out.Error) {
				t.Errorf("metrics parse %d: got %v, want a JSON error", i, err)
			}
			continue
		}
		var got any
		if err == nil {
			got = d.JS()
		}
		expect(t, "metrics parse "+strconv.Itoa(i)+" "+c.Text, c.Out, got, err)
	}
	for i, c := range v.Seeds {
		dir := t.TempDir()
		if c.Metrics != "" {
			if err := os.WriteFile(filepath.Join(dir, "metrics.json"), []byte(c.Metrics), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		seeds := make([]Seed, len(c.Seeds))
		for j, s := range c.Seeds {
			seeds[j] = Seed{ID: s.ID, Metric: s.Metric}
		}
		d, text, err := LoadMetrics(dir, []string{"d_"}, seeds)
		var got any
		if err == nil {
			got = jsjson.Object{{Key: "dict", Value: d.JS()}, {Key: "text", Value: text}}
		}
		expect(t, "loadMetrics "+strconv.Itoa(i), c.Out, got, err)
	}
	dict, err := ParseMetrics(v.Parse[0].Text, []string{"d_"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Problems {
		g := MetricTablesProblem(dict, c.Metric, c.Tables)
		w := ""
		if c.Out != nil {
			w = *c.Out
		}
		if g != w {
			t.Errorf("problem %s %v: got %q want %q", c.Metric, c.Tables, g, w)
		}
	}
	for _, c := range v.Context {
		d, err := ParseMetrics(c.Text, []string{"d_"})
		if err != nil {
			t.Fatal(err)
		}
		if g := MetricsContext(d); g != c.Out {
			t.Errorf("context: got %q want %q", g, c.Out)
		}
	}
	t.Logf("%d parse, %d seed checks", len(v.Parse), len(v.Seeds))
}

func TestHash(t *testing.T) {
	var v struct {
		Normalize []struct{ SQL, Out string } `json:"normalize"`
		Policy    []struct {
			Readable []string `json:"readable"`
			Panel    []string `json:"panel"`
			Out      string   `json:"out"`
		} `json:"policy"`
		Docs []struct {
			Guide   string `json:"guide"`
			Seeds   []struct{ Name, Text string }
			Metrics string `json:"metrics"`
			Out     string `json:"out"`
		} `json:"docs"`
		Prompt  []struct{ A, B, Out string } `json:"prompt"`
		Schema  []struct{ A, B, Out string } `json:"schema"`
		Context struct {
			Ctx struct {
				SnapshotID    string `json:"snapshot_id"`
				SchemaVersion string `json:"schema_version"`
				PolicyVersion string `json:"policy_version"`
				DocsVersion   string `json:"docs_version"`
				PromptVersion string `json:"prompt_version"`
			} `json:"ctx"`
			Key   string `json:"key"`
			Cache string `json:"cache"`
		} `json:"context"`
	}
	load(t, "panels-hash.json", &v)
	for _, c := range v.Normalize {
		if g := NormalizeSQL(c.SQL); g != c.Out {
			t.Errorf("normalizeSql %q: got %q want %q", c.SQL, g, c.Out)
		}
	}
	for _, c := range v.Policy {
		if g := PolicyVersion(c.Readable, c.Panel); g != c.Out {
			t.Errorf("policyVersion %v %v", c.Readable, c.Panel)
		}
	}
	for _, c := range v.Docs {
		seeds := make([]SeedDoc, len(c.Seeds))
		for i, s := range c.Seeds {
			seeds[i] = SeedDoc{s.Name, s.Text}
		}
		if g := DocsVersion(c.Guide, seeds, c.Metrics); g != c.Out {
			t.Errorf("docsVersion %q", c.Guide)
		}
	}
	for _, c := range v.Prompt {
		if g := PromptVersion(c.A, c.B); g != c.Out {
			t.Errorf("promptVersion %q", c.A)
		}
	}
	for _, c := range v.Schema {
		if g := SchemaVersion(c.A, c.B); g != c.Out {
			t.Errorf("schemaVersion %q", c.A)
		}
	}
	x := v.Context.Ctx
	ctx := ContextVersion{x.SnapshotID, x.SchemaVersion, x.PolicyVersion, x.DocsVersion, x.PromptVersion}
	if ctx.Key() != v.Context.Key || ResultCacheKey("sem", ctx) != v.Context.Cache {
		t.Errorf("context key / result cache key differ")
	}
}
