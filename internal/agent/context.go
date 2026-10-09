package agent

import (
	"fmt"
	"growth-lab/internal/jsstr"
	"regexp"
	"strings"

	"growth-lab/internal/assets"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/sqlitec"
)

// ContextLimit is the system prompt limit in characters.
const ContextLimit = 60000

var engineGuideText = assets.EngineGuide

// LanguageNames are the output language names.
var LanguageNames = map[string]string{"en": "English", "ko": "Korean"}

// EngineGuide is the engine guide with the output language filled in.
func EngineGuide(lang string) string {
	return strings.ReplaceAll(engineGuideText, "{{LANGUAGE}}", LanguageNames[lang])
}

// ContextError: the context cannot be built.
type ContextError struct{ Msg string }

func (e *ContextError) Error() string { return e.Msg }

var (
	tableDescRE = regexp.MustCompile("^\\|" + jsstr.Space + "*`([A-Za-z_][A-Za-z0-9_]*\\.[A-Za-z_][A-Za-z0-9_]*)`" + jsstr.Space + "*\\|" + jsstr.Space + "*(" + jsDot + "+?)" + jsstr.Space + "*\\|?" + jsstr.Space + "*$")
	listDescRE  = regexp.MustCompile("^" + jsstr.Space + "*[-*]" + jsstr.Space + "*`([A-Za-z_][A-Za-z0-9_]*\\.[A-Za-z_][A-Za-z0-9_]*)`" + jsstr.Space + "*[:：—-]" + jsstr.Space + "*(" + jsDot + "+)$")
	pipeEndRE   = regexp.MustCompile(jsstr.Space + "*\\|" + jsstr.Space + "*$")
)

// jsDot is JavaScript's `.` (no line terminators).
const jsDot = `[^\n\r\x{2028}\x{2029}]`

// ColumnDescriptions are the `table.column` descriptions in guide.md (table rows or list items), in order.
func ColumnDescriptions(guide string) (keys []string, desc map[string]string) {
	desc = map[string]string{}
	for _, line := range strings.Split(guide, "\n") {
		m := tableDescRE.FindStringSubmatch(line)
		if m == nil {
			m = listDescRE.FindStringSubmatch(line)
		}
		if m == nil {
			continue
		}
		if _, ok := desc[m[1]]; !ok {
			keys = append(keys, m[1])
		}
		desc[m[1]] = pipeEndRE.ReplaceAllString(m[2], "")
	}
	return keys, desc
}

// SnapshotSchema is the schema split into panel tables and probe-only tables; tables lists every readable table.
func SnapshotSchema(agentPath string, readablePrefixes, panelPrefixes []string, guide string) (string, []string, error) {
	_, desc := ColumnDescriptions(guide)
	db, err := sqlitec.Open(agentPath, true)
	if err != nil {
		return "", nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		return "", nil, err
	}
	hasPrefix := func(n string, ps []string) bool {
		for _, p := range ps {
			if strings.HasPrefix(n, p) {
				return true
			}
		}
		return false
	}
	tables := []string{}
	for _, r := range rows {
		if hasPrefix(r[0].Text, readablePrefixes) {
			tables = append(tables, r[0].Text)
		}
	}
	describe := func(t string) (string, error) {
		n, err := db.Query(`SELECT count(*) n FROM "` + t + `"`)
		if err != nil {
			return "", err
		}
		out := []string{fmt.Sprintf("### %s (%d rows)", t, n[0][0].Int)}
		cols, err := db.Query(`PRAGMA table_info("` + t + `")`)
		if err != nil {
			return "", err
		}
		for _, c := range cols {
			line := "- " + c[1].Text
			if c[2].Text != "" {
				line += " " + c[2].Text
			}
			if d, ok := desc[t+"."+c[1].Text]; ok && d != "" {
				line += " — " + d
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n"), nil
	}
	var forPanel, probeOnly []string
	for _, t := range tables {
		if hasPrefix(t, panelPrefixes) {
			forPanel = append(forPanel, t)
		} else {
			probeOnly = append(probeOnly, t)
		}
	}
	section := func(ts []string) (string, error) {
		var parts []string
		for _, t := range ts {
			d, err := describe(t)
			if err != nil {
				return "", err
			}
			parts = append(parts, d)
		}
		if len(parts) == 0 {
			return "(none)", nil
		}
		return strings.Join(parts, "\n"), nil
	}
	a, err := section(forPanel)
	if err != nil {
		return "", nil, err
	}
	b, err := section(probeOnly)
	if err != nil {
		return "", nil, err
	}
	text := "## Tables for panels (panel SQL reads only these)\n\n" + a + "\n\n## Probe-only tables (for probe checks only; panel SQL reading them is rejected)\n\n" + b
	return text, tables, nil
}

// SeedPanel is an example panel.
type SeedPanel struct {
	ID   string
	Spec panels.Spec
}

// ContextState is the current state block.
type ContextState struct {
	AsOf, Today   string
	CalendarStart string // "" when not set
	Params        jsjson.Object
}

// ContextInput are the parts of the system prompt.
type ContextInput struct {
	Schema, Metrics, Guide string
	SeedPanels             []SeedPanel
	State                  ContextState
	Lang                   string
}

func seedText(ps []SeedPanel, withSQL []bool) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		s := p.Spec
		metric := "null (off-dictionary)"
		if s.Metric != nil {
			metric = *s.Metric
		}
		lines := []string{"### " + p.ID + ": " + s.Title, "metric: " + metric, "question: " + s.Question, "pattern: " + jsjson.MustStringify(panels.DisplayJSON(s.Display))}
		for _, d := range s.Definition {
			lines = append(lines, "- "+d[0]+": "+d[1])
		}
		if withSQL[i] {
			lines = append(lines, "```sql", s.SQL, "```")
		}
		parts[i] = strings.Join(lines, "\n")
	}
	return strings.Join(parts, "\n\n")
}

// BuildContext assembles the system prompt. Over the limit it drops seed panel SQL, then seed panels, from the end.
// seedSQL false leaves out all seed SQL (blind evaluation).
func BuildContext(o ContextInput, seedSQL bool) (string, error) {
	state := []string{"# Current state", "- Snapshot cutoff (:as_of): " + o.State.AsOf, "- Today: " + o.State.Today}
	if o.State.CalendarStart != "" {
		state = append(state, "- Data start (calendar_start): "+o.State.CalendarStart)
	}
	avail := "- Available parameters: :as_of"
	for _, m := range o.State.Params {
		if _, isArr := m.Value.([]string); isArr {
			continue
		}
		if _, isArr := m.Value.([]any); isArr {
			continue
		}
		avail += ", :" + m.Key
	}
	state = append(state, avail)
	stateText := strings.Join(state, "\n")
	assemble := func(ps []SeedPanel, withSQL []bool) string {
		parts := []string{jsstr.Trim(EngineGuide(o.Lang)), "# Metric dictionary\n\n" + o.Metrics, "# Snapshot schema (ID values are pseudonyms)\n\n" + o.Schema, "# Workspace guide\n\n" + jsstr.Trim(o.Guide)}
		if len(ps) > 0 {
			parts = append(parts, "# Example panels (SQL that applies the interpretation rules correctly)\n\n"+seedText(ps, withSQL))
		}
		parts = append(parts, stateText)
		var kept []string
		for _, p := range parts {
			if p != "" {
				kept = append(kept, p)
			}
		}
		return strings.Join(kept, "\n\n---\n\n")
	}
	ps := append([]SeedPanel(nil), o.SeedPanels...)
	withSQL := make([]bool, len(ps))
	for i := range withSQL {
		withSQL[i] = seedSQL
	}
	t := assemble(ps, withSQL)
	for i := len(ps) - 1; i >= 0 && jsstr.U16Len(t) > ContextLimit; i-- {
		withSQL[i] = false
		t = assemble(ps, withSQL)
	}
	for len(ps) > 0 && jsstr.U16Len(t) > ContextLimit {
		ps = ps[:len(ps)-1]
		t = assemble(ps, withSQL[:len(ps)])
	}
	if jsstr.U16Len(t) > ContextLimit {
		return "", &ContextError{fmt.Sprintf("context is over %d characters (%d): shorten guide.md", ContextLimit, jsstr.U16Len(t))}
	}
	return t, nil
}
