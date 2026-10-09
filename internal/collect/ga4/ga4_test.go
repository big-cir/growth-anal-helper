package ga4_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"growth-lab/internal/collect"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"growth-lab/internal/collect/ga4"
	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

// norm re-encodes JSON text through jsjson so key order and number form follow JavaScript.
func norm(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	s, err := jsjson.Compact(raw)
	if err != nil {
		t.Fatalf("bad vector JSON: %v", err)
	}
	return s
}

func js(v any) string { return jsjson.MustStringify(v) }

type outV struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
	Type  string          `json:"type"`
}

func errType(err error) string {
	var se *ga4.ReportShapeError
	var ce *ga4.ConfigError
	var ge *ga4.Error
	switch {
	case errors.As(err, &se):
		return "ReportShapeError"
	case errors.As(err, &ce):
		return "Ga4ConfigError"
	case errors.As(err, &ge):
		return "Ga4Error"
	case err.Error() == "Invalid time value":
		return "RangeError"
	}
	return "Error"
}

func checkOut(t *testing.T, label string, want outV, got any, err error) {
	t.Helper()
	if !want.OK {
		if err == nil || err.Error() != want.Error || errType(err) != want.Type {
			t.Errorf("%s: got %v (%v), want %s %q", label, err, got, want.Type, want.Error)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: unexpected error %v", label, err)
		return
	}
	if g, w := js(got), norm(t, want.Value); g != w {
		t.Errorf("%s:\n got  %s\n want %s", label, g, w)
	}
}

var conn = ga4.Connection{PropertyID: "123456", TimeZone: "America/Los_Angeles", KeyFile: "/k"}

func TestReportsConfig(t *testing.T) {
	var v struct {
		Reports []struct {
			Input json.RawMessage
			Out   outV
		}
	}
	if err := contract.Load("ga4-config.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Reports {
		dir := t.TempDir()
		var s string
		text := ""
		if json.Unmarshal(c.Input, &s) == nil && strings.HasPrefix(s, "raw:") {
			text = s[4:]
		} else {
			text = norm(t, c.Input)
		}
		os.WriteFile(filepath.Join(dir, "ga4-reports.json"), []byte(text), 0o644)
		r, err := ga4.LoadReports(dir)
		var got any
		if err == nil {
			got = jsjson.Object{{Key: "reports", Value: r.JS()}, {Key: "specHash", Value: ga4.SpecHash(conn, r)}}
		}
		checkOut(t, string(c.Input), c.Out, got, err)
	}
	t.Logf("%d ga4-reports.json inputs", len(v.Reports))
}

type keys struct {
	pkcs8, pkcs1, pub, ec, ec8 string
	key                        *rsa.PrivateKey
}

func newKeys(t *testing.T) keys {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p8, _ := x509.MarshalPKCS8PrivateKey(k)
	pub, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	e, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sec1, _ := x509.MarshalECPrivateKey(e)
	e8, _ := x509.MarshalPKCS8PrivateKey(e)
	enc := func(typ string, b []byte) string { return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b})) }
	return keys{pkcs8: enc("PRIVATE KEY", p8), pkcs1: enc("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k)), pub: enc("PUBLIC KEY", pub), ec: enc("EC PRIVATE KEY", sec1), ec8: enc("PRIVATE KEY", e8), key: k}
}

func saJSON(k keys, over jsjson.Object) string {
	o := jsjson.Object{{Key: "type", Value: "service_account"}, {Key: "client_email", Value: "reader@example.iam.gserviceaccount.com"}, {Key: "private_key", Value: k.pkcs8}, {Key: "token_uri", Value: "https://attacker.example/token"}}
	for _, m := range over {
		replaced := false
		for i := range o {
			if o[i].Key == m.Key {
				o[i].Value = m.Value
				replaced = true
			}
		}
		if !replaced {
			o = append(o, m)
		}
	}
	return js(o)
}

func writeKey(t *testing.T, dir, body string, mode os.FileMode) string {
	p := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

func TestCredentials(t *testing.T) {
	var v struct {
		Keys []struct {
			Name string
			Out  outV
		}
	}
	if err := contract.Load("ga4-config.json", &v); err != nil {
		t.Fatal(err)
	}
	k := newKeys(t)
	make := map[string]func(dir string) string{
		"ok": func(d string) string { return writeKey(t, d, saJSON(k, nil), 0o600) },
		"ok pkcs1": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "private_key", Value: k.pkcs1}}), 0o600)
		},
		"mode 0644": func(d string) string { return writeKey(t, d, saJSON(k, nil), 0o644) },
		"mode 0400": func(d string) string { return writeKey(t, d, saJSON(k, nil), 0o400) },
		"symlink": func(d string) string {
			p := writeKey(t, d, saJSON(k, nil), 0o600)
			os.Symlink(p, filepath.Join(d, "link.json"))
			return filepath.Join(d, "link.json")
		},
		"missing": func(d string) string { return filepath.Join(d, "nope.json") },
		"directory": func(d string) string {
			p := filepath.Join(d, "dir.json")
			os.Mkdir(p, 0o600)
			os.Chmod(p, 0o600)
			return p
		},
		"too large": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "pad", Value: strings.Repeat("x", 70*1024)}}), 0o600)
		},
		"not json":   func(d string) string { return writeKey(t, d, "not json", 0o600) },
		"json array": func(d string) string { return writeKey(t, d, "[1]", 0o600) },
		"wrong type": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "type", Value: "user"}}), 0o600)
		},
		"email with space": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "client_email", Value: "a b@x"}}), 0o600)
		},
		"email two @": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "client_email", Value: "a@b@c"}}), 0o600)
		},
		"key not string": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "private_key", Value: 5.0}}), 0o600)
		},
		"key garbage": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "private_key", Value: "garbage"}}), 0o600)
		},
		"public key": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "private_key", Value: k.pub}}), 0o600)
		},
		"ec sec1": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "private_key", Value: k.ec}}), 0o600)
		},
		"ec pkcs8": func(d string) string {
			return writeKey(t, d, saJSON(k, jsjson.Object{{Key: "private_key", Value: k.ec8}}), 0o600)
		},
	}
	for _, c := range v.Keys {
		f, ok := make[c.Name]
		if !ok {
			t.Errorf("no Go scenario for %q", c.Name)
			continue
		}
		dir := t.TempDir()
		cred, err := ga4.LoadCredentials(f(dir))
		var got any
		if err == nil {
			got = cred.ClientEmail
		} else if strings.Contains(err.Error(), dir) {
			t.Errorf("%s: path in error", c.Name)
		}
		checkOut(t, c.Name, c.Out, got, err)
	}
}

var customs = map[string]ga4.CustomReport{
	"custom:daily_country": {ID: "daily_country", Range: "daily", Dimensions: []string{"countryId"}, Metrics: []string{"activeUsers", "newUsers"}},
	"custom:weekly_os":     {ID: "weekly_os", Range: "weekly", Dimensions: []string{"operatingSystem", "deviceCategory"}, Metrics: []string{"activeUsers", "sessions", "engagementRate"}},
	"custom:monthly_x":     {ID: "monthly_x", Range: "monthly", Dimensions: []string{}, Metrics: []string{"activeUsers", "averageSessionDuration"}},
	"custom:ev":            {ID: "ev", Range: "daily", Dimensions: []string{"eventName"}, Metrics: []string{"totalUsers", "eventCount"}},
	"custom:dow":           {ID: "dow", Range: "weekly", Dimensions: []string{"dayOfWeek"}, Metrics: []string{"totalUsers"}},
}

func def(key string) ga4.ReportDef {
	if c, ok := customs[key]; ok {
		return ga4.CustomDef(c)
	}
	return ga4.ReportKinds[key]
}

func columnsJS(cols []ga4.Column) []any {
	out := make([]any, len(cols))
	for i, c := range cols {
		out[i] = jsjson.Object{{Key: "name", Value: c.Name}, {Key: "type", Value: c.Type}, {Key: "key", Value: c.Key}, {Key: "nullable", Value: c.Nullable}}
	}
	return out
}

var events = jsjson.Object{{Key: "POST_WRITE", Value: "post_write"}, {Key: "REPLY_WRITE", Value: "reply_write"}}

func TestDefinitions(t *testing.T) {
	var v struct {
		Defs []struct {
			Key, Table string
			Breakdown  bool
			Columns    json.RawMessage
			Requests   []struct {
				Start, Through string
				Offset         int
				Out            json.RawMessage
			}
		}
		DataThrough []struct {
			TZ, Now string
			Out     outV
		}
		CompleteRange []struct {
			Range, Start, Through string
			Out                   json.RawMessage
		}
		CohortWeeks []struct {
			Through string
			Out     json.RawMessage
		}
		AddDays []struct {
			Date string
			N    int
			Out  outV
		}
		SecretShape []struct {
			Hex string
			Out *string
		}
	}
	if err := contract.Load("ga4-defs.json", &v); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range v.Defs {
		g := def(d.Key)
		if g.Table != d.Table || ga4.IsBreakdown(g) != d.Breakdown || js(columnsJS(ga4.TableColumns(g))) != norm(t, d.Columns) {
			t.Errorf("%s: table/breakdown/columns differ", d.Key)
		}
		for _, r := range d.Requests {
			req := ga4.BuildRequest(g, ga4.RequestOptions{Start: r.Start, Through: r.Through, Events: events}, r.Offset)
			got := "null"
			if req != nil {
				got = js(req)
			}
			if want := norm(t, r.Out); got != want {
				t.Errorf("%s request %s..%s @%d:\n got  %s\n want %s", d.Key, r.Start, r.Through, r.Offset, got, want)
			}
			n++
		}
	}
	for _, c := range v.DataThrough {
		now, _ := time.Parse(time.RFC3339, c.Now)
		checkOut(t, "dataThrough "+c.TZ+" "+c.Now, c.Out, ga4.DataThrough(now.UnixMilli(), c.TZ), nil)
	}
	for _, c := range v.CompleteRange {
		got := "null"
		if p := ga4.CompleteRange(c.Range, c.Start, c.Through); p != nil {
			got = js(jsjson.Object{{Key: "start", Value: p.Start}, {Key: "end", Value: p.End}})
		}
		if want := norm(t, c.Out); got != want {
			t.Errorf("completeRange %s %s %s: got %s want %s", c.Range, c.Start, c.Through, got, want)
		}
	}
	for _, c := range v.CohortWeeks {
		if got, want := js(ga4.CohortWeeks(c.Through, 12)), norm(t, c.Out); got != want {
			t.Errorf("cohortWeeks %s: got %s want %s", c.Through, got, want)
		}
	}
	for _, c := range v.AddDays {
		got, err := ga4.AddDays(c.Date, c.N)
		checkOut(t, fmt.Sprintf("addDays %s %d", c.Date, c.N), c.Out, got, err)
	}
	for _, c := range v.SecretShape {
		in, _ := hex.DecodeString(c.Hex)
		got := ga4.SecretShapeForTest(string(in))
		want := ""
		if c.Out != nil {
			want = *c.Out
		}
		if got != want {
			t.Errorf("secretShape %q: got %q want %q", in, got, want)
		}
	}
	t.Logf("%d defs, %d requests, %d dataThrough, %d addDays", len(v.Defs), n, len(v.DataThrough), len(v.AddDays))
}

func TestTransform(t *testing.T) {
	var v struct {
		Cases []struct {
			Name, Def string
			Raw       []struct{ Dims, Metrics, Types []string }
			MinUsers  *float64
			Through   string
			Events    json.RawMessage
			Out       outV
		}
	}
	if err := contract.Load("ga4-transform.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		raw := make([]ga4.RawRow, len(c.Raw))
		for i, r := range c.Raw {
			raw[i] = ga4.RawRow{Dims: r.Dims, Metrics: r.Metrics, Types: r.Types}
		}
		ev := jsjson.Object{}
		if len(c.Events) > 0 {
			p, _ := jsjson.Parse(string(c.Events))
			ev = p.(jsjson.Object)
		}
		min := 10.0
		if c.MinUsers != nil {
			min = *c.MinUsers
		}
		through := c.Through
		if through == "" {
			through = "2024-06-10"
		}
		rows, stats, err := ga4.TransformRows(def(c.Def), raw, ga4.TransformOptions{Events: ev, MinUsers: min, Through: through})
		var got any
		if err == nil {
			rs := make([]any, len(rows))
			for i, r := range rows {
				rs[i] = []any(r)
			}
			got = jsjson.Object{{Key: "rows", Value: rs}, {Key: "stats", Value: jsjson.Object{
				{Key: "droppedSmall", Value: stats.DroppedSmall}, {Key: "droppedUnobserved", Value: stats.DroppedUnobserved}, {Key: "droppedUnmapped", Value: stats.DroppedUnmapped},
				{Key: "droppedCollision", Value: stats.DroppedCollision}, {Key: "suppressedCells", Value: stats.SuppressedCells}}}}
		}
		checkOut(t, c.Name, c.Out, got, err)
	}
	t.Logf("%d transform cases", len(v.Cases))
}

type sqlDB struct{ db *sql.DB }

func (s sqlDB) Exec(q string, args ...any) error {
	_, err := s.db.Exec(q, args...)
	return err
}

func engineTablesSQL(t *testing.T) string { return collect.EngineTablesSQL }

func dumpTables(t *testing.T, db *sql.DB) any {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'r\_ga4\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	out := []any{}
	for _, tb := range names {
		ir, _ := db.Query("PRAGMA table_info(" + tb + ")")
		var info []any
		var cols []string
		for ir.Next() {
			var cid, notnull, pk int64
			var name, typ string
			var dflt sql.NullString
			ir.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
			var d any
			if dflt.Valid {
				d = dflt.String
			}
			info = append(info, jsjson.Object{{Key: "cid", Value: cid}, {Key: "name", Value: name}, {Key: "type", Value: typ}, {Key: "notnull", Value: notnull}, {Key: "dflt_value", Value: d}, {Key: "pk", Value: pk}})
			cols = append(cols, name)
		}
		ir.Close()
		sel := make([]string, len(cols))
		ord := make([]string, len(cols))
		for i, c := range cols {
			sel[i] = "typeof(" + c + ") || ':' || quote(" + c + ")"
			ord[i] = fmt.Sprint(i + 1)
		}
		rr, err := db.Query("SELECT " + strings.Join(sel, ", ") + " FROM " + tb + " ORDER BY " + strings.Join(ord, ", "))
		if err != nil {
			t.Fatal(err)
		}
		data := []any{}
		for rr.Next() {
			vals := make([]string, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rr.Scan(ptrs...)
			row := make([]any, len(vals))
			for i, s := range vals {
				row[i] = s
			}
			data = append(data, row)
		}
		rr.Close()
		out = append(out, jsjson.Object{{Key: "table", Value: tb}, {Key: "info", Value: info}, {Key: "rows", Value: data}})
	}
	return out
}

type exchange struct {
	Req struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"req"`
	Res struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"res"`
}

var assertionRE = regexp.MustCompile(`assertion=([^.&]+)\.([^.&]+)\.[^&]+`)

// maskBody drops the key-dependent JWT signature and decodes header and claims.
func maskBody(body string) string {
	return assertionRE.ReplaceAllStringFunc(body, func(m string) string {
		p := assertionRE.FindStringSubmatch(m)
		h, _ := base64.RawURLEncoding.DecodeString(p[1])
		c, _ := base64.RawURLEncoding.DecodeString(p[2])
		return "assertion=" + string(h) + "|" + string(c)
	})
}

func verifyJWT(t *testing.T, body string, pub *rsa.PublicKey) {
	vals, err := url.ParseQuery(body)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(vals.Get("assertion"), ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("JWT signature: %v", err)
	}
}

func TestImport(t *testing.T) {
	var v struct {
		Scenarios []struct {
			Name     string
			Reports  json.RawMessage
			Now      int64
			TimeZone string
			Result   struct {
				OK    bool    `json:"ok"`
				Error string  `json:"error"`
				Kind  *string `json:"kind"`
			}
			Exchanges []exchange
			Waits     []int
			Tables    json.RawMessage
		}
		ClientLimit struct {
			Kinds       []string
			Calls       int
			BadProperty outV
		}
	}
	if err := contract.Load("ga4-import.json", &v); err != nil {
		t.Fatal(err)
	}
	k := newKeys(t)
	engine := engineTablesSQL(t)
	for _, s := range v.Scenarios {
		dir := t.TempDir()
		key := writeKey(t, dir, saJSON(k, nil), 0o600)
		os.WriteFile(filepath.Join(dir, "ga4-reports.json"), []byte(norm(t, s.Reports)), 0o644)
		plan, err := ga4.LoadPlan(dir, ga4.Connection{PropertyID: "123456", TimeZone: s.TimeZone, KeyFile: key})
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		db, _ := sql.Open("sqlite3", ":memory:")
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(engine); err != nil {
			t.Fatal(err)
		}
		i := 0
		var waits []int
		transport := func(req ga4.HTTPRequest) (ga4.HTTPResponse, error) {
			if i >= len(s.Exchanges) {
				t.Errorf("%s: extra call %s", s.Name, req.URL)
				return ga4.HTTPResponse{Status: 500}, nil
			}
			e := s.Exchanges[i]
			i++
			body := req.Body
			if req.URL == ga4.TokenURL {
				verifyJWT(t, body, &k.key.PublicKey)
				body = maskBody(body)
			}
			if req.URL != e.Req.URL || req.Method != e.Req.Method || body != e.Req.Body || js(headers(req.Headers)) != js(headers(e.Req.Headers)) {
				t.Errorf("%s call %d:\n got  %s %s %v\n want %s %s %v", s.Name, i, req.URL, body, req.Headers, e.Req.URL, e.Req.Body, e.Req.Headers)
			}
			return ga4.HTTPResponse{Status: e.Res.Status, Headers: e.Res.Headers, Body: e.Res.Body}, nil
		}
		err = ga4.Import(sqlDB{db}, plan, ga4.ImportOptions{Transport: transport, Now: func() int64 { return s.Now }, Sleep: func(ms int) { waits = append(waits, ms) }, Jitter: func(int) int { return 0 }})
		if s.Result.OK != (err == nil) {
			t.Errorf("%s: got error %v, want ok=%v %q", s.Name, err, s.Result.OK, s.Result.Error)
		} else if err != nil {
			var ge *ga4.Error
			kind := (*string)(nil)
			if errors.As(err, &ge) {
				kind = &ge.Kind
			}
			if err.Error() != s.Result.Error || (kind == nil) != (s.Result.Kind == nil) || (kind != nil && *kind != *s.Result.Kind) {
				t.Errorf("%s: got %v, want %q", s.Name, err, s.Result.Error)
			}
		}
		if i != len(s.Exchanges) {
			t.Errorf("%s: %d calls, want %d", s.Name, i, len(s.Exchanges))
		}
		if js(intsAny(waits)) != js(intsAny(s.Waits)) {
			t.Errorf("%s: waits %v, want %v", s.Name, waits, s.Waits)
		}
		if got, want := js(dumpTables(t, db)), norm(t, s.Tables); got != want {
			t.Errorf("%s tables:\n got  %s\n want %s", s.Name, got, want)
		}
		db.Close()
	}

	dir := t.TempDir()
	cred, err := ga4.LoadCredentials(writeKey(t, dir, saJSON(k, nil), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	fail503 := func(ga4.HTTPRequest) (ga4.HTTPResponse, error) {
		return ga4.HTTPResponse{Status: 503, Headers: map[string]string{"retry-after": "1"}, Body: `{"error":"secret detail token=abc"}`}, nil
	}
	c, _ := ga4.NewClient(cred, "123456", ga4.ClientOptions{Transport: fail503, Sleep: func(int) {}})
	var kinds []string
	for range 60 {
		_, err := c.RunReport(jsjson.Object{}, "x")
		var ge *ga4.Error
		if errors.As(err, &ge) {
			kinds = append(kinds, ge.Kind+":"+ge.Error())
		} else {
			kinds = append(kinds, "other")
		}
	}
	if js(stringsAny(kinds)) != js(stringsAny(v.ClientLimit.Kinds)) || c.Calls != v.ClientLimit.Calls {
		t.Errorf("client limit: calls %d (want %d), kinds differ: %v", c.Calls, v.ClientLimit.Calls, kinds[len(kinds)-2:])
	}
	_, err = ga4.NewClient(cred, "12a", ga4.ClientOptions{})
	checkOut(t, "bad property", v.ClientLimit.BadProperty, nil, err)
	t.Logf("%d import scenarios", len(v.Scenarios))
}

func headers(h map[string]string) jsjson.Object {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sortStrings(keys)
	o := jsjson.Object{}
	for _, k := range keys {
		o = append(o, jsjson.Member{Key: k, Value: h[k]})
	}
	return o
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func intsAny(xs []int) []any {
	out := []any{}
	for _, x := range xs {
		out = append(out, x)
	}
	return out
}

func stringsAny(xs []string) []any {
	out := []any{}
	for _, x := range xs {
		out = append(out, x)
	}
	return out
}
