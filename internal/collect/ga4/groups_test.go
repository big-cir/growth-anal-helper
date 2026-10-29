package ga4_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/collect/ga4"
	"growth-lab/internal/jsjson"
)

func TestEventGroupsConfig(t *testing.T) {
	base := `"start": "2024-03-01", "reports": ["daily_events"], "events": {"POST_WRITE": "post_write"}`
	r, err := ga4.ParseReports(mustParse(t, `{`+base+`, "event_groups": {"reply_write": ["REPLY_WRITE", "reply_write_click"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := js(r.Events); got != `{"POST_WRITE":"post_write","REPLY_WRITE":"reply_write","reply_write_click":"reply_write"}` {
		t.Errorf("events %s", got)
	}
	if got := strings.Join(r.GroupNames("reply_write"), ","); got != "REPLY_WRITE,reply_write_click" {
		t.Errorf("group names %s", got)
	}
	plain, _ := ga4.ParseReports(mustParse(t, `{`+base+`}`))
	if _, has := plain.JS().Get("eventGroups"); has {
		t.Error("eventGroups shown without groups")
	}
	if ga4.SpecHash(conn, r) == ga4.SpecHash(conn, plain) {
		t.Error("groups do not change the spec hash")
	}
	for _, c := range []struct{ groups, want string }{
		{`[]`, "event_groups must be"},
		{`{"reply_write": ["REPLY_WRITE"]}`, "event_groups must be"},
		{`{"reply_write": "REPLY_WRITE"}`, "event_groups must be"},
		{`{"Reply": ["A", "B"]}`, "lowercase identifiers"},
		{`{"post_write": ["A", "B"]}`, "event key post_write is used twice"},
		{`{"reply_write": ["POST_WRITE", "B"]}`, "event name POST_WRITE is used twice"},
		{`{"reply_write": ["A", "A"]}`, "event name A is used twice"},
		{`{"a": ["X", "Y"], "b": ["Y", "Z"]}`, "event name Y is used twice"},
		{`{"reply_write": ["A", "bad name"]}`, "invalid event name"},
	} {
		_, err := ga4.ParseReports(mustParse(t, `{`+base+`, "event_groups": `+c.groups+`}`))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.groups, err, c.want)
		}
	}
	if _, err := ga4.ParseReports(mustParse(t, `{"start": "2024-03-01", "reports": ["daily_events"], "event_groups": {"reply_write": ["A", "B"]}}`)); err != nil {
		t.Errorf("groups alone: %v", err)
	}
}

func mustParse(t *testing.T, s string) any {
	t.Helper()
	v, err := jsjson.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func ga4Body(rows []string, dims ...string) string {
	h := make([]string, len(dims))
	for i, d := range dims {
		h[i] = `{"name":"` + d + `"}`
	}
	return `{"dimensionHeaders":[` + strings.Join(h, ",") + `],"metricHeaders":[{"name":"eventCount","type":"TYPE_INTEGER"},{"name":"totalUsers","type":"TYPE_INTEGER"}],"rows":[` + strings.Join(rows, ",") +
		fmt.Sprintf(`],"rowCount":%d,"metadata":{"timeZone":"America/Los_Angeles"},"propertyQuota":{"tokensPerDay":{"consumed":1,"remaining":24999},"tokensPerHour":{"consumed":1,"remaining":4999}}}`, len(rows))
}

func gaRow(vals ...string) string {
	d := make([]string, len(vals)-2)
	for i, v := range vals[:len(vals)-2] {
		d[i] = `{"value":"` + v + `"}`
	}
	return `{"dimensionValues":[` + strings.Join(d, ",") + `],"metricValues":[{"value":"` + vals[len(vals)-2] + `"},{"value":"` + vals[len(vals)-1] + `"}]}`
}

const compatOK = `{"dimensionCompatibilities":[{"dimensionMetadata":{"apiName":"date"},"compatibility":"COMPATIBLE"},{"dimensionMetadata":{"apiName":"eventName"},"compatibility":"COMPATIBLE"}],"metricCompatibilities":[{"metricMetadata":{"apiName":"eventCount"},"compatibility":"COMPATIBLE"},{"metricMetadata":{"apiName":"totalUsers"},"compatibility":"COMPATIBLE"}]}`

func runGroups(t *testing.T, reports string, respond func(path, body string) string) (*sql.DB, []string, error) {
	t.Helper()
	k := newKeys(t)
	dir := t.TempDir()
	key := writeKey(t, dir, saJSON(k, nil), 0o600)
	os.WriteFile(filepath.Join(dir, "ga4-reports.json"), []byte(reports), 0o644)
	plan, err := ga4.LoadPlan(dir, ga4.Connection{PropertyID: "123456", TimeZone: "America/Los_Angeles", KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	db, _ := sql.Open("sqlite3", ":memory:")
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(engineTablesSQL(t)); err != nil {
		t.Fatal(err)
	}
	var calls []string
	transport := func(req ga4.HTTPRequest) (ga4.HTTPResponse, error) {
		if req.URL == ga4.TokenURL {
			calls = append(calls, "token")
			return ga4.HTTPResponse{Status: 200, Body: `{"access_token":"ya29.fake","token_type":"Bearer","expires_in":3600}`}, nil
		}
		path := req.URL[strings.LastIndex(req.URL, ":")+1:]
		calls = append(calls, path+" "+req.Body)
		return ga4.HTTPResponse{Status: 200, Body: respond(path, req.Body)}, nil
	}
	err = ga4.Import(sqlDB{db}, plan, ga4.ImportOptions{Transport: transport, Now: func() int64 { return 1718222400000 }, Sleep: func(int) {}, Jitter: func(int) int { return 0 }})
	return db, calls, err
}

func TestEventGroupsImport(t *testing.T) {
	reports := `{"start": "2024-03-01", "reports": ["daily_events"], "events": {"POST_WRITE": "post_write"}, "event_groups": {"reply_write": ["REPLY_WRITE", "reply_write_click"]}, "min_users": 10}`
	db, calls, err := runGroups(t, reports, func(path, body string) string {
		switch {
		case path == "checkCompatibility":
			return compatOK
		case strings.Contains(body, `{"name":"eventName"}`):
			return ga4Body([]string{gaRow("20240610", "POST_WRITE", "80", "40"), gaRow("20240610", "UNLISTED", "70", "30")}, "date", "eventName")
		default:
			// The group is asked without eventName: GA4 counts each person once across both names
			return ga4Body([]string{gaRow("20240609", "9", "8"), gaRow("20240610", "30", "15")}, "date")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if len(calls) != 5 || calls[0] != "token" || !strings.HasPrefix(calls[1], "checkCompatibility") || !strings.HasPrefix(calls[2], "checkCompatibility") {
		t.Fatalf("calls %v", calls)
	}
	group := calls[4]
	if strings.Contains(group, `{"name":"eventName"}`) || !strings.Contains(group, `"values":["REPLY_WRITE","reply_write_click"]`) {
		t.Errorf("group request %s", group)
	}
	if strings.Contains(calls[3], "reply_write_click") || !strings.Contains(calls[3], `"values":["POST_WRITE"]`) {
		t.Errorf("single request %s", calls[3])
	}
	rows, err := db.Query(`SELECT date, event_key, event_count, total_users FROM r_ga4_daily_events ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var d, k string
		var c, u float64
		rows.Scan(&d, &k, &c, &u)
		got = append(got, fmt.Sprintf("%s %s %v %v", d, k, c, u))
	}
	rows.Close()
	// 2024-06-09 has 8 people for the whole group, under min_users, so the row is dropped
	if js(stringsAny(got)) != `["2024-06-10 post_write 80 40","2024-06-10 reply_write 30 15"]` {
		t.Errorf("rows %v", got)
	}
	var n, rc, small, unmapped, nc float64
	db.QueryRow(`SELECT rows, row_count, dropped_small, dropped_unmapped, calls FROM r_ga4_collect_log WHERE report = 'daily_events'`).Scan(&n, &rc, &small, &unmapped, &nc)
	if n != 2 || rc != 4 || small != 1 || unmapped != 1 || nc != 5 {
		t.Errorf("log rows=%v row_count=%v small=%v unmapped=%v calls=%v", n, rc, small, unmapped, nc)
	}
}

func TestEventGroupsCallLimit(t *testing.T) {
	var groups []string
	for i := 0; i < ga4.ClientLimits.MaxCalls; i++ {
		groups = append(groups, fmt.Sprintf(`"k%d": ["A%d", "B%d"]`, i, i, i))
	}
	reports := `{"start": "2024-03-01", "reports": ["daily_events"], "event_groups": {` + strings.Join(groups, ", ") + `}}`
	_, calls, err := runGroups(t, reports, func(string, string) string { return "" })
	var ge *ga4.Error
	if err == nil || !errors.As(err, &ge) || ge.Kind != "limit" || len(calls) != 0 {
		t.Errorf("got %v after %d calls, want a limit error before any call", err, len(calls))
	}
}

func TestEventGroupsPagingAndCustom(t *testing.T) {
	reports := `{"start": "2024-03-01", "reports": [], "event_groups": {"reply_write": ["REPLY_WRITE", "reply_write_click"]}, "min_users": 10,
 "custom": [{"id": "ev_platform", "range": "daily", "dimensions": ["platform", "eventName"], "metrics": ["totalUsers", "eventCount"]}]}`
	page := 0
	db, calls, err := runGroups(t, reports, func(path, body string) string {
		if path == "checkCompatibility" {
			return `{"dimensionCompatibilities":[{"dimensionMetadata":{"apiName":"date"},"compatibility":"COMPATIBLE"},{"dimensionMetadata":{"apiName":"platform"},"compatibility":"COMPATIBLE"}],"metricCompatibilities":[{"metricMetadata":{"apiName":"totalUsers"},"compatibility":"COMPATIBLE"},{"metricMetadata":{"apiName":"eventCount"},"compatibility":"COMPATIBLE"}]}`
		}
		page++
		row := func(d, p, u, c string) string {
			return `{"dimensionValues":[{"value":"` + d + `"},{"value":"` + p + `"}],"metricValues":[{"value":"` + u + `"},{"value":"` + c + `"}]}`
		}
		rows := row("20240610", "iOS", "12", "30")
		if strings.Contains(body, `"offset":250000`) {
			rows = row("20240610", "Android", "20", "44")
		}
		return `{"dimensionHeaders":[{"name":"date"},{"name":"platform"}],"metricHeaders":[{"name":"totalUsers","type":"TYPE_INTEGER"},{"name":"eventCount","type":"TYPE_INTEGER"}],"rows":[` + rows +
			`],"rowCount":2,"metadata":{"timeZone":"America/Los_Angeles"},"propertyQuota":{"tokensPerDay":{"consumed":1,"remaining":24999},"tokensPerHour":{"consumed":1,"remaining":4999}}}`
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if page != 2 || len(calls) != 4 || strings.Contains(calls[2], `{"name":"eventName"}`) {
		t.Fatalf("pages %d calls %v", page, calls)
	}
	rows, _ := db.Query(`SELECT date, platform, event_key, total_users, event_count FROM r_ga4_x_ev_platform ORDER BY 2`)
	var got []string
	for rows.Next() {
		var d, p, k string
		var u, c float64
		rows.Scan(&d, &p, &k, &u, &c)
		got = append(got, fmt.Sprintf("%s %s %s %v %v", d, p, k, u, c))
	}
	rows.Close()
	if js(stringsAny(got)) != `["2024-06-10 Android reply_write 20 44","2024-06-10 iOS reply_write 12 30"]` {
		t.Errorf("rows %v", got)
	}
}

func TestRowsNotArray(t *testing.T) {
	reports := `{"start": "2024-03-01", "reports": ["daily_events"], "event_groups": {"reply_write": ["A", "B"]}}`
	_, _, err := runGroups(t, reports, func(path, body string) string {
		if path == "checkCompatibility" {
			return compatOK
		}
		return `{"dimensionHeaders":[{"name":"date"}],"metricHeaders":[{"name":"eventCount","type":"TYPE_INTEGER"},{"name":"totalUsers","type":"TYPE_INTEGER"}],"rows":"x","rowCount":0,"metadata":{"timeZone":"America/Los_Angeles"},"propertyQuota":{"tokensPerDay":{"consumed":1,"remaining":24999},"tokensPerHour":{"consumed":1,"remaining":4999}}}`
	})
	if err == nil || !strings.Contains(err.Error(), "row format") {
		t.Errorf("got %v", err)
	}
}
