// Package quality builds the data quality checks: built-in checks from the collection spec, plus workspace quality/*.sql.
package quality

import (
	"fmt"
	"growth-lab/internal/i18n"
	"growth-lab/internal/jsstr"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"growth-lab/internal/civiltime"
	"growth-lab/internal/collect"
	"growth-lab/internal/jsjson"
)

// Check is one quality check; Display is table or line.
type Check struct {
	ID, Title, Display, SQL string
	Builtin                 bool
}

// JS returns the check in its stored JSON form.
func (c Check) JS() jsjson.Object {
	return jsjson.Object{{Key: "id", Value: c.ID}, {Key: "title", Value: c.Title}, {Key: "display", Value: c.Display}, {Key: "sql", Value: c.SQL}, {Key: "builtin", Value: c.Builtin}}
}

// Error is a quality check definition error.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func tsParam(v any, name string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", &Error{"params." + name + ": must be a timestamp string"}
	}
	t, err := civiltime.Normalize(s)
	if err != nil {
		return "", &Error{"params." + name + ": " + err.Error()}
	}
	return t, nil
}

// gapRanges reads params.quality_gap_ranges: ["start~end", …].
func gapRanges(v any, present bool) ([][2]string, error) {
	if !present {
		return nil, nil
	}
	var arr []any
	switch x := v.(type) {
	case []any:
		arr = x
	case []string:
		for _, s := range x {
			arr = append(arr, s)
		}
	default:
		return nil, &Error{`params.quality_gap_ranges: must be an array of "start~end" strings`}
	}
	out := make([][2]string, len(arr))
	for i, s := range arr {
		var parts []string
		if str, ok := s.(string); ok {
			for _, p := range strings.Split(str, "~") {
				parts = append(parts, jsstr.Trim(p))
			}
		}
		if len(parts) != 2 {
			return nil, &Error{fmt.Sprintf(`params.quality_gap_ranges[%d]: must be "start~end"`, i)}
		}
		for j := range 2 {
			t, err := tsParam(parts[j], fmt.Sprintf("quality_gap_ranges[%d]", i))
			if err != nil {
				return nil, err
			}
			out[i][j] = t
		}
	}
	return out, nil
}

// Builtin returns the built-in checks. params may hold "__ga4": true when GA4 is configured.
func Builtin(specs []collect.TableSpec, params jsjson.Object, lang string) ([]Check, error) {
	counts := make([]string, len(specs))
	for i, t := range specs {
		k := "NULL"
		if len(t.Key) == 1 {
			for _, c := range t.Columns {
				if c.As == t.Key[0] && c.Role != collect.Private {
					k = `"` + t.Key[0] + `"`
				}
			}
		}
		counts[i] = fmt.Sprintf(`SELECT %s AS table_name, count(*) AS rows_now, min(%s) AS min_key, max(%s) AS max_key FROM "%s"`, q(t.Target), k, k, t.Target)
	}
	collectSQL := "WITH t AS (\n  " + strings.Join(counts, "\n  UNION ALL ") + "\n)\nSELECT t.table_name, l.rows AS collected_rows, t.rows_now, t.min_key, t.max_key, l.ms AS collect_ms, l.collected_at\nFROM t LEFT JOIN r_collect_log l ON l.table_name = t.table_name\nORDER BY t.table_name"
	cutoff := "SELECT table_name, dropped_after_cutoff, nulled_after_cutoff FROM r_collect_log ORDER BY table_name"

	minTs := ""
	if v, ok := params.Get("quality_min_ts"); ok {
		t, err := tsParam(v, "quality_min_ts")
		if err != nil {
			return nil, err
		}
		minTs = t
	}
	gv, gok := params.Get("quality_gap_ranges")
	gaps, err := gapRanges(gv, gok)
	if err != nil {
		return nil, err
	}
	var dateRows []string
	for _, t := range specs {
		for _, c := range t.Columns {
			if c.Kind != "ts" || c.Role == collect.Private {
				continue
			}
			col := `"` + c.As + `"`
			before := "NULL"
			if minTs != "" {
				before = "coalesce(sum(" + col + " < " + q(minTs) + "), 0)"
			}
			inGap := "NULL"
			if len(gaps) > 0 {
				conds := make([]string, len(gaps))
				for i, g := range gaps {
					conds[i] = "(" + col + " >= " + q(g[0]) + " AND " + col + " < " + q(g[1]) + ")"
				}
				inGap = "coalesce(sum(" + strings.Join(conds, " OR ") + "), 0)"
			}
			parts := []string{q(t.Target) + " AS table_name", q(c.As) + " AS column_name", "count(*) - count(" + col + ") AS nulls", "coalesce(sum(" + col + " > :as_of), 0) AS after_as_of", before + " AS before_min", inGap + " AS in_gap"}
			dateRows = append(dateRows, "SELECT "+strings.Join(parts, ", ")+` FROM "`+t.Target+`"`)
		}
	}
	dates := "SELECT NULL AS table_name WHERE 0"
	if len(dateRows) > 0 {
		dates = strings.Join(dateRows, "\nUNION ALL\n")
	}
	var out []Check
	if v, _ := params.Get("__ga4"); v == true {
		out = append(out, Check{"q_ga4_collect", i18n.In(lang, "GA4 import results", "GA4 가져오기 결과"), "table", "SELECT report, rows, row_count, dropped_small, suppressed_cells, dropped_collision, dropped_unobserved, dropped_unmapped, subject_to_thresholding, data_loss_from_other_row, sampled, truncated, schema_restricted, empty_reason, data_through, calls FROM r_ga4_collect_log ORDER BY report", true})
	}
	return append(out,
		Check{"q_collect", i18n.In(lang, "Collection results by table", "테이블별 수집 결과"), "table", collectSQL, true},
		Check{"q_cutoff_drops", i18n.In(lang, "Values dropped or blanked at the cutoff", "기준 시각 정리로 버리거나 비운 값"), "table", cutoff, true},
		Check{"q_dates", i18n.In(lang, "Date column checks", "날짜 칸 점검"), "table", dates, true},
	), nil
}

var idRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// Workspace reads <workspace>/quality/<id>.sql + <id>.json ({ title, display }).
func Workspace(wsDir string) ([]Check, error) {
	dir := filepath.Join(wsDir, "quality")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []Check
	for _, f := range names {
		id := f[:len(f)-4]
		if !idRE.MatchString(id) {
			return nil, &Error{"quality/" + f + ": file names must match ^[a-z][a-z0-9_]*$"}
		}
		metaText, err := os.ReadFile(filepath.Join(dir, id+".json"))
		if err != nil {
			return nil, &Error{"quality/" + id + ".json not found"}
		}
		raw, err := jsjson.Parse(string(metaText))
		if err != nil {
			return nil, err
		}
		meta, _ := raw.(jsjson.Object)
		title, _ := meta.Get("title")
		ts, ok := title.(string)
		if !ok || jsstr.Trim(ts) == "" {
			return nil, &Error{"quality/" + id + ".json: title is required"}
		}
		display, _ := meta.Get("display")
		if display != "table" && display != "line" {
			return nil, &Error{"quality/" + id + ".json: display must be table|line"}
		}
		sql, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		out = append(out, Check{id, ts, display.(string), string(sql), false})
	}
	return out, nil
}
