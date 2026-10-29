// Package coverage knows where each source has data and checks panels against the data gaps.
package coverage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/quality"
	"growth-lab/internal/sqlitec"
)

// Config is the workspace coverage.json: which tables hold events and GA4 days.
type Config struct {
	// Events are event tables and their time column; the gap copies drop their rows before a gap's end.
	Events []Column `json:"events"`
	// Kinds is the event kind column; kinds whose first record is after the last gap are named in the guide.
	Kinds *struct {
		Table, Column, Time string
	} `json:"kinds"`
	// GA4 is the daily GA4 table, its date column and the prefix of all GA4 tables.
	GA4 *struct {
		Table, Date, Prefix string
	} `json:"ga4"`
}

// Column is a table and its time column.
type Column struct {
	Table, Time string
}

var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadConfig reads coverage.json; a missing file is an empty config, a broken one is an error.
func LoadConfig(dir string) (*Config, []byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "coverage.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, nil, fmt.Errorf("coverage.json: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("coverage.json: extra data after the object")
	}
	names := map[string]string{}
	for i, e := range c.Events {
		names[fmt.Sprintf("events[%d].table", i)], names[fmt.Sprintf("events[%d].time", i)] = e.Table, e.Time
	}
	if c.Kinds != nil {
		names["kinds.table"], names["kinds.column"], names["kinds.time"] = c.Kinds.Table, c.Kinds.Column, c.Kinds.Time
	}
	if c.GA4 != nil {
		names["ga4.table"], names["ga4.date"] = c.GA4.Table, c.GA4.Date
		if c.GA4.Prefix != "" && !ident.MatchString(c.GA4.Prefix) {
			return nil, nil, fmt.Errorf("coverage.json ga4.prefix: must be an identifier")
		}
	}
	for path, v := range names {
		if !ident.MatchString(v) {
			return nil, nil, fmt.Errorf("coverage.json %s: must be an identifier", path)
		}
	}
	return &c, raw, nil
}

// Resolved is the coverage of one snapshot. Dates are YYYY-MM-DD and inclusive; gaps are [start, end) timestamps.
type Resolved struct {
	AppEnd           string
	GA4Start, GA4End string
	GA4Prefix        string
	Gaps             [][2]string
	Events           []Column
	// Kinds are the first record days of event kinds that only have rows after the last gap.
	Kinds      map[string]string
	KindColumn string
	// GapPaths are the copies of the real snapshot, one per gap, without event rows before that gap's end.
	GapPaths []string
	// GapError is why the gap copies could not be made ("" = made or no gaps).
	GapError string
}

const day = "2006-01-02"

func addDays(d string, n int) string {
	t, err := time.Parse(day, d)
	if err != nil {
		return d
	}
	return t.AddDate(0, 0, n).Format(day)
}

// Resolve reads the gaps from the workspace params and the GA4 range and kind first days from the snapshot.
// App data is complete through the day before as_of. Tables or columns in the config that the snapshot lacks are an error.
func Resolve(c *Config, realPath, asOf string, params jsjson.Object) (*Resolved, error) {
	if len(asOf) < 10 {
		return nil, fmt.Errorf("coverage: snapshot as_of is not a timestamp")
	}
	gaps, err := quality.GapRanges(params)
	if err != nil {
		return nil, err
	}
	r := &Resolved{AppEnd: addDays(asOf[:10], -1), Gaps: gaps, Events: c.Events, Kinds: map[string]string{}}
	db, err := sqlitec.Open(realPath, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	for _, e := range c.Events {
		if err := hasColumn(db, e.Table, e.Time); err != nil {
			return nil, err
		}
	}
	if g := c.GA4; g != nil {
		if err := hasColumn(db, g.Table, g.Date); err != nil {
			return nil, err
		}
		r.GA4Prefix = g.Prefix
		if rows, err := db.Query(fmt.Sprintf("SELECT min(%q), max(%q) FROM %q", g.Date, g.Date, g.Table)); err == nil && len(rows) == 1 && rows[0][1].Type == sqlitec.Text && len(rows[0][1].Text) >= 10 {
			r.GA4Start, r.GA4End = rows[0][0].Text[:10], rows[0][1].Text[:10]
		}
	}
	if k := c.Kinds; k != nil {
		for _, col := range []string{k.Column, k.Time} {
			if err := hasColumn(db, k.Table, col); err != nil {
				return nil, err
			}
		}
		r.KindColumn = k.Table + "." + k.Column
		if last := r.lastGapEnd(); last != "" {
			st, err := db.Prepare(fmt.Sprintf("SELECT %q, min(%q) FROM %q GROUP BY 1 HAVING min(%q) >= ? ORDER BY 1", k.Column, k.Time, k.Table, k.Time))
			if err != nil {
				return nil, err
			}
			if err := st.Bind(last); err == nil {
				for {
					ok, err := st.Step()
					if err != nil || !ok {
						break
					}
					if kv, v := st.Value(0), st.Value(1); kv.Type == sqlitec.Text && v.Type == sqlitec.Text && len(v.Text) >= 10 {
						r.Kinds[kv.Text] = v.Text[:10]
					}
				}
			}
			st.Finalize()
		}
	}
	return r, nil
}

func hasColumn(db *sqlitec.DB, table, column string) error {
	rows, err := db.Query(fmt.Sprintf("SELECT 1 FROM pragma_table_info('%s') WHERE name = '%s'", table, column))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("coverage.json: the snapshot has no column %s.%s", table, column)
	}
	return nil
}

func (r *Resolved) lastGapEnd() string {
	last := ""
	for _, g := range r.Gaps {
		if g[1] > last {
			last = g[1]
		}
	}
	return last
}

// Params are the extra query parameters: the last complete day of each source.
func (r *Resolved) Params() jsjson.Object {
	out := jsjson.Object{{Key: "data_end", Value: r.AppEnd}}
	if r.GA4End != "" {
		out = append(out, jsjson.Member{Key: "ga4_data_end", Value: r.GA4End})
	}
	return out
}

// Guide describes the coverage to the agent.
func (r *Resolved) Guide() string {
	var b strings.Builder
	b.WriteString("## Data coverage\n\n")
	if r.GA4End != "" {
		fmt.Fprintf(&b, "- Tables other than the GA4 tables (%s*): complete through %s (:data_end).\n", r.GA4Prefix, r.AppEnd)
		fmt.Fprintf(&b, "- GA4 tables (%s*): data from %s, complete through %s (:ga4_data_end).\n", r.GA4Prefix, r.GA4Start, r.GA4End)
	} else {
		fmt.Fprintf(&b, "- Tables: complete through %s (:data_end).\n", r.AppEnd)
	}
	for _, g := range r.Gaps {
		fmt.Fprintf(&b, "- Data gap from %s until %s: events in it are missing. Do not let rows before the gap feed values after it (for example lag(), first/last values or running totals across the gap); bound such panels to after the gap.", g[0], g[1])
		if len(r.Events) > 0 {
			b.WriteString(" The engine reruns panels without the event rows before each gap and asks you when the result changes.")
		}
		b.WriteString("\n")
	}
	for _, k := range sortedKeys(r.Kinds) {
		fmt.Fprintf(&b, "- %s = '%s': first record in this snapshot on %s. This may not be the launch day or the full start of collection; do not read earlier periods as no use (0), and when comparing use of this kind, count only periods and people observed from that day.\n", r.KindColumn, k, r.Kinds[k])
	}
	b.WriteString("- Relative periods (last N weeks, recent days) end at the source's last complete day, not at :today, when :today is later. State the dates you used.\n")
	return b.String()
}

// EnsureGapCopies writes <snapshot>.gap<N>.sqlite for each gap: the real snapshot without event rows before that gap's end.
// A copy is reused only when its stamp matches the source file, the gap and the event tables.
func (r *Resolved) EnsureGapCopies(realPath string) error {
	r.GapPaths = nil
	if len(r.Events) == 0 {
		return nil
	}
	st, err := os.Stat(realPath)
	if err != nil {
		return err
	}
	var paths []string
	for i, g := range r.Gaps {
		stamp := fmt.Sprintf("%d|%d|%s|%v", st.Size(), st.ModTime().UnixNano(), g[1], r.Events)
		path := fmt.Sprintf("%s.gap%d.sqlite", strings.TrimSuffix(realPath, ".sqlite"), i+1)
		if err := r.gapCopy(realPath, path, stamp, g[1]); err != nil {
			return err
		}
		paths = append(paths, path)
	}
	r.GapPaths = paths
	return nil
}

func (r *Resolved) gapCopy(realPath, path, stamp, end string) error {
	if b, err := os.ReadFile(path + ".stamp"); err == nil && string(b) == stamp {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	defer os.Remove(tmp)
	if err := copyFile(realPath, tmp); err != nil {
		return err
	}
	db, err := sqlitec.Open(tmp, false)
	if err != nil {
		return err
	}
	for _, e := range r.Events {
		if _, err := db.RunOnce(fmt.Sprintf("DELETE FROM %q WHERE %q < ?", e.Table, e.Time), end); err != nil {
			db.Close()
			return fmt.Errorf("gap copy %s: %w", e.Table, err)
		}
	}
	db.Close()
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.WriteFile(path+".stamp", []byte(stamp), 0o644)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
