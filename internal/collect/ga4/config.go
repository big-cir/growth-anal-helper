package ga4

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"growth-lab/internal/jsjson"
)

// Reports is ga4-reports.json after validation.
type Reports struct {
	Start    string
	Reports  []string
	Events   jsjson.Object // GA4 event name → key, in JavaScript key order
	MinUsers float64
	Custom   []CustomReport
}

// JS returns the reports in their stored JSON form.
func (r Reports) JS() jsjson.Object {
	custom := make([]any, len(r.Custom))
	for i, c := range r.Custom {
		custom[i] = jsjson.Object{{Key: "id", Value: c.ID}, {Key: "range", Value: c.Range}, {Key: "dimensions", Value: c.Dimensions}, {Key: "metrics", Value: c.Metrics}}
	}
	return jsjson.Object{{Key: "start", Value: r.Start}, {Key: "reports", Value: r.Reports}, {Key: "events", Value: r.Events}, {Key: "minUsers", Value: r.MinUsers}, {Key: "custom", Value: custom}}
}

// Credentials are the two values used for signing.
type Credentials struct {
	ClientEmail string
	Key         *rsa.PrivateKey
}

// ReadOwnerOnly reads a regular file owned by the current user with no group/other permissions, up to maxBytes.
// Symlinks are not opened. label names the file's role in errors (paths are never put in errors).
func ReadOwnerOnly(path string, maxBytes int64, label string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", &ConfigError{label + " not found"}
		}
		return "", &ConfigError{"cannot open " + label + " (symlinks are not allowed)"}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", &ConfigError{"cannot open " + label + " (symlinks are not allowed)"}
	}
	if !st.Mode().IsRegular() {
		return "", &ConfigError{label + " is not a regular file"}
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return "", &ConfigError{label + " is not owned by the current user"}
	}
	if st.Mode().Perm() != 0o600 {
		return "", &ConfigError{label + " permissions must be 0600"}
	}
	if st.Size() > maxBytes {
		return "", &ConfigError{fmt.Sprintf("%s is too large (max %d bytes)", label, maxBytes)}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(b), "�"), nil
}

const notSpaceAt = `[^\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}@]`

var emailRE = regexp.MustCompile(`^` + notSpaceAt + `+@` + notSpaceAt + `+$`)

// LoadCredentials reads only the client email and the private key (token_uri and the rest are ignored).
func LoadCredentials(keyFile string) (*Credentials, error) {
	text, err := ReadOwnerOnly(keyFile, 64*1024, "service account key file")
	if err != nil {
		return nil, err
	}
	raw, err := jsjson.Parse(text)
	if err != nil {
		return nil, &ConfigError{"invalid service account key file"}
	}
	o, ok := raw.(jsjson.Object)
	email, _ := prop(o, "client_email").(string)
	pk, pkOK := prop(o, "private_key").(string)
	if !ok || prop(o, "type") != "service_account" || !emailRE.MatchString(email) || !pkOK {
		return nil, &ConfigError{"service account key file is missing required values"}
	}
	key, err := parsePrivateKey(pk)
	if err != nil {
		return nil, err
	}
	return &Credentials{ClientEmail: email, Key: key}, nil
}

func parsePrivateKey(text string) (*rsa.PrivateKey, error) {
	unreadable := &ConfigError{"cannot read the service account private key"}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, unreadable
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, unreadable
	}
	if err != nil {
		return nil, unreadable
	}
	rk, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, &ConfigError{"service account private key is not RSA"}
	}
	return rk, nil
}

var (
	dateRE      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	eventNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,39}$`)
	eventKeyRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	customIDRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)
	ranges      = []string{"daily", "weekly", "monthly"}
)

const customMax = 10

func cfgErr(msg string) error { return &ConfigError{msg} }

// jsKeys orders object members like Object.keys: array-index keys ascending first, then insertion order.
func jsKeys(o jsjson.Object) jsjson.Object {
	idx := func(k string) (uint64, bool) {
		if k == "" || (len(k) > 1 && k[0] == '0') {
			return 0, false
		}
		n, err := strconv.ParseUint(k, 10, 64)
		if err != nil || n >= math.MaxUint32 {
			return 0, false
		}
		return n, true
	}
	var nums, rest jsjson.Object
	for _, m := range o {
		if _, ok := idx(m.Key); ok {
			nums = append(nums, m)
		} else {
			rest = append(rest, m)
		}
	}
	sort.SliceStable(nums, func(i, j int) bool {
		a, _ := idx(nums[i].Key)
		b, _ := idx(nums[j].Key)
		return a < b
	})
	return append(nums, rest...)
}

func has(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// LoadReports reads and validates ga4-reports.json in the workspace.
func LoadReports(wsDir string) (*Reports, error) {
	b, err := os.ReadFile(filepath.Join(wsDir, "ga4-reports.json"))
	if err != nil {
		return nil, cfgErr("cannot read ga4-reports.json")
	}
	raw, err := jsjson.Parse(string(b))
	if err != nil {
		return nil, cfgErr("cannot read ga4-reports.json")
	}
	return ParseReports(raw)
}

// ParseReports validates ga4-reports.json content.
func ParseReports(raw any) (*Reports, error) {
	o, ok := raw.(jsjson.Object)
	if !ok {
		return nil, cfgErr("ga4-reports.json: must be an object")
	}
	o = jsKeys(o)
	for _, m := range o {
		if !has([]string{"start", "reports", "events", "min_users", "custom"}, m.Key) {
			return nil, cfgErr("ga4-reports.json: unknown key " + m.Key)
		}
	}
	start, isStr := prop(o, "start").(string)
	if !isStr || !dateRE.MatchString(start) {
		return nil, cfgErr("ga4-reports.json: start must be a real date YYYY-MM-DD")
	}
	if ok, err := realDate(start); err != nil {
		return nil, err
	} else if !ok {
		return nil, cfgErr("ga4-reports.json: start must be a real date YYYY-MM-DD")
	}
	arr, isArr := prop(o, "reports").([]any)
	if !isArr {
		return nil, cfgErr("ga4-reports.json: reports must be an array")
	}
	reports := make([]string, len(arr))
	for i, r := range arr {
		s, isStr := r.(string)
		if _, known := ReportKinds[s]; !isStr || !known {
			return nil, cfgErr("ga4-reports.json: unknown report " + jsString(r) + " (" + strings.Join(ReportKindNames, ", ") + ")")
		}
		reports[i] = s
	}
	if len(uniq(reports)) != len(reports) {
		return nil, cfgErr("ga4-reports.json: duplicate reports")
	}
	events := jsjson.Object{}
	if ev := prop(o, "events"); ev != undef {
		eo, isO := ev.(jsjson.Object)
		if !isO {
			return nil, cfgErr(`ga4-reports.json: events must be { "GA4 event name": "key" }`)
		}
		seen := map[string]bool{}
		for _, m := range jsKeys(eo) {
			if !eventNameRE.MatchString(m.Key) {
				return nil, cfgErr("ga4-reports.json: invalid event name " + m.Key)
			}
			key, isStr := m.Value.(string)
			if !isStr || !eventKeyRE.MatchString(key) {
				return nil, cfgErr("ga4-reports.json: event keys must be lowercase identifiers (" + m.Key + ")")
			}
			if seen[key] {
				return nil, cfgErr("ga4-reports.json: event key " + key + " is used twice (one-to-one only)")
			}
			seen[key] = true
			events = append(events, jsjson.Member{Key: m.Key, Value: key})
		}
	}
	if has(reports, "daily_events") && len(events) == 0 {
		return nil, cfgErr("ga4-reports.json: daily_events needs an events map")
	}
	minUsers := 10.0
	if v := prop(o, "min_users"); v != undef {
		f, isNum := v.(float64)
		if !isNum || f != math.Trunc(f) || f < 1 || f > 1000 {
			return nil, cfgErr("ga4-reports.json: min_users must be an integer 1-1000")
		}
		minUsers = f
	}
	custom, err := loadCustom(prop(o, "custom"), events)
	if err != nil {
		return nil, err
	}
	if len(reports) == 0 && len(custom) == 0 {
		return nil, cfgErr("ga4-reports.json: reports or custom must have at least one report")
	}
	return &Reports{Start: start, Reports: reports, Events: events, MinUsers: minUsers, Custom: custom}, nil
}

// loadCustom: allow-listed dimensions (0-2) and metrics (1-8) only; at most one fine dimension, which allows user-count metrics only.
func loadCustom(v any, events jsjson.Object) ([]CustomReport, error) {
	if v == undef {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok || len(arr) > customMax {
		return nil, cfgErr(fmt.Sprintf("ga4-reports.json: custom must be an array of at most %d", customMax))
	}
	ids := map[string]bool{}
	var out []CustomReport
	for i, c := range arr {
		at := fmt.Sprintf("ga4-reports.json: custom[%d]", i)
		co, ok := c.(jsjson.Object)
		if !ok {
			return nil, cfgErr(at + " must be an object")
		}
		for _, m := range jsKeys(co) {
			if !has([]string{"id", "range", "dimensions", "metrics"}, m.Key) {
				return nil, cfgErr(at + ": unknown key " + m.Key)
			}
		}
		id, isStr := prop(co, "id").(string)
		if !isStr || !customIDRE.MatchString(id) {
			return nil, cfgErr(at + ".id must be a lowercase identifier (31 characters or fewer)")
		}
		if _, builtin := ReportKinds[id]; ids[id] || builtin {
			return nil, cfgErr(at + ".id " + id + " clashes with another report")
		}
		ids[id] = true
		rng, isStr := prop(co, "range").(string)
		if !isStr || !has(ranges, rng) {
			return nil, cfgErr(at + ".range must be " + strings.Join(ranges, " | "))
		}
		list := func(x any, name string, min, max int, allowed func(string) bool) ([]string, error) {
			xs, ok := x.([]any)
			bad := cfgErr(fmt.Sprintf("%s.%s must be an array of %d-%d strings", at, name, min, max))
			if !ok || len(xs) < min || len(xs) > max {
				return nil, bad
			}
			out := make([]string, len(xs))
			for i, e := range xs {
				s, isStr := e.(string)
				if !isStr {
					return nil, bad
				}
				out[i] = s
			}
			for _, n := range out {
				if !allowed(n) {
					return nil, cfgErr(at + "." + name + ": not allowed: " + n)
				}
			}
			if len(uniq(out)) != len(out) {
				return nil, cfgErr(at + "." + name + ": duplicate")
			}
			return out, nil
		}
		dims, err := list(prop(co, "dimensions"), "dimensions", 0, 2, func(n string) bool { _, ok := customDim(n); return ok })
		if err != nil {
			return nil, err
		}
		metrics, err := list(prop(co, "metrics"), "metrics", 1, 8, func(n string) bool { m, ok := metric(n); return ok && m.Custom })
		if err != nil {
			return nil, err
		}
		var fine []string
		for _, d := range dims {
			if mustDim(d).Fine {
				fine = append(fine, d)
			}
		}
		if len(fine) > 1 {
			return nil, cfgErr(at + ": only one fine dimension per report (" + strings.Join(fine, ", ") + ")")
		}
		anyGroup := false
		for _, m := range metrics {
			if mustMetric(m).Group {
				anyGroup = true
			}
		}
		if len(dims) > 0 && !anyGroup {
			return nil, cfgErr(at + ": breakdown dimensions need activeUsers or totalUsers")
		}
		if has(dims, "eventName") {
			if len(events) == 0 {
				return nil, cfgErr(at + ": eventName needs an events map")
			}
			if !has(metrics, "totalUsers") {
				return nil, cfgErr(at + ": eventName needs totalUsers")
			}
			var bad []string
			for _, m := range metrics {
				if !has(UserMetrics, m) && m != "eventCount" {
					bad = append(bad, m)
				}
			}
			if len(bad) > 0 {
				return nil, cfgErr(at + ": metrics not allowed with eventName: " + strings.Join(bad, ", "))
			}
		} else if len(fine) > 0 {
			var bad []string
			for _, m := range metrics {
				if !has(UserMetrics, m) {
					bad = append(bad, m)
				}
			}
			if len(bad) > 0 {
				return nil, cfgErr(at + ": the fine dimension " + fine[0] + " allows user-count metrics only (" + strings.Join(bad, ", ") + ")")
			}
		}
		r := CustomReport{ID: id, Range: rng, Dimensions: dims, Metrics: metrics}
		var names []string
		for _, c := range TableColumns(CustomDef(r)) {
			names = append(names, c.Name)
		}
		if len(uniq(names)) != len(names) {
			return nil, cfgErr(at + ": duplicate column names")
		}
		out = append(out, r)
	}
	return out, nil
}

func uniq(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !has(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// SpecHash is the GA4 config hash used in the snapshot ID, reuse check and schema version.
func SpecHash(conn Connection, r *Reports) string {
	keys := make([]string, len(r.Events))
	for i, m := range r.Events {
		keys[i] = m.Key
	}
	sort.Strings(keys)
	events := make([]any, len(keys))
	for i, k := range keys {
		v, _ := r.Events.Get(k)
		events[i] = []any{k, v}
	}
	// Custom reports sorted by id (array order has no meaning); dimension and metric order is the column order, so kept
	cs := append([]CustomReport(nil), r.Custom...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
	custom := make([]any, len(cs))
	for i, c := range cs {
		custom[i] = []any{c.ID, c.Range, c.Dimensions, c.Metrics}
	}
	reports := append([]string(nil), r.Reports...)
	sort.Strings(reports)
	norm := jsjson.Object{
		{Key: "property_id", Value: conn.PropertyID}, {Key: "time_zone", Value: conn.TimeZone}, {Key: "start", Value: r.Start}, {Key: "reports", Value: reports},
		{Key: "custom", Value: custom}, {Key: "events", Value: events}, {Key: "min_users", Value: r.MinUsers}, {Key: "defs", Value: ReportDefsVersion}, {Key: "api", Value: "v1beta"},
	}
	h := sha256.Sum256([]byte(jsjson.MustStringify(norm)))
	return hex.EncodeToString(h[:])
}
