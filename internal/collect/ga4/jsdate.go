package ga4

import (
	"errors"
	"fmt"
	"growth-lab/internal/jsstr"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"growth-lab/internal/jsjson"
)

// errInvalidTime is what JavaScript throws for toISOString on an invalid date.
var errInvalidTime = errors.New("Invalid time value")

const dayMs = 86_400_000

var (
	isoDateRE = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	// ECMAScript date-only forms: YYYY, YYYY-MM, YYYY-MM-DD, with ±YYYYYY extended years
	esDateRE = regexp.MustCompile(`^(\d{4}|[+-]\d{6})(?:-(\d{2})(?:-(\d{2}))?)?$`)
)

// civilMs is Date.UTC without the 0-99 year quirk.
func civilMs(y, m, d int) int64 {
	return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).UnixMilli()
}

// parseDate is Date.parse(`${date}T00:00:00Z`): month 1-12, day 1-31 (overflowing into the next month).
func parseDate(date string) (int64, bool) {
	m := esDateRE.FindStringSubmatch(date)
	if m == nil || m[1] == "-000000" {
		return 0, false
	}
	y, _ := strconv.Atoi(m[1])
	mo, d := 1, 1
	if m[2] != "" {
		mo, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		d, _ = strconv.Atoi(m[3])
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return 0, false
	}
	return civilMs(y, mo, 1) + int64(d-1)*dayMs, true
}

// isoString is Date.prototype.toISOString.
func isoString(ms int64) string {
	t := time.UnixMilli(ms).UTC()
	y := t.Year()
	ys := fmt.Sprintf("%04d", y)
	if y < 0 {
		ys = fmt.Sprintf("-%06d", -y)
	} else if y > 9999 {
		ys = fmt.Sprintf("+%06d", y)
	}
	return ys + t.Format("-01-02T15:04:05.000Z")
}

func ymd(ms int64) string { return isoString(ms)[:10] }

// AddDays adds n days to YYYY-MM-DD.
func AddDays(date string, n int) (string, error) {
	ms, ok := parseDate(date)
	if !ok {
		return "", errInvalidTime
	}
	return ymd(ms + int64(n)*dayMs), nil
}

func mustAddDays(date string, n int) string {
	s, err := AddDays(date, n)
	if err != nil {
		panic(err)
	}
	return s
}

func dow(date string) int {
	ms, _ := parseDate(date)
	return int(time.UnixMilli(ms).UTC().Weekday())
}

// realDate: YYYY-MM-DD that round-trips (an invalid month or day 0 is an error, as in JavaScript).
func realDate(d string) (bool, error) {
	if !isoDateRE.MatchString(d) {
		return false, nil
	}
	ms, ok := parseDate(d)
	if !ok {
		return false, errInvalidTime
	}
	return ymd(ms) == d, nil
}

// jsUTC is Date.UTC(year, month0, day): years 0-99 mean 1900-1999.
func jsUTC(year, month0, day int) int64 {
	if year >= 0 && year <= 99 {
		year += 1900
	}
	return civilMs(year, month0+1, day)
}

var (
	decimalRE = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	radixRE   = regexp.MustCompile(`^0([xXoObB])([0-9A-Za-z]+)$`)
)

// jsNumber is Number(string).
func jsNumber(s string) float64 {
	t := strings.TrimFunc(s, jsstr.IsSpace)
	if t == "" {
		return 0
	}
	switch t {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if m := radixRE.FindStringSubmatch(t); m != nil {
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[strings.ToLower(m[1])[0]]
		n, ok := new(big.Int).SetString(m[2], base)
		if !ok {
			return math.NaN()
		}
		f, _ := new(big.Float).SetInt(n).Float64()
		return f
	}
	if !decimalRE.MatchString(t) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f
		}
		return math.NaN()
	}
	return f
}

// jsString is String(v) for parsed JSON values.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case undefinedT:
		return "undefined"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if math.IsNaN(x) {
			return "NaN"
		}
		if math.IsInf(x, 1) {
			return "Infinity"
		}
		if math.IsInf(x, -1) {
			return "-Infinity"
		}
		return jsjson.Number(x)
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				continue
			}
			if _, u := e.(undefinedT); u {
				continue
			}
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	case jsjson.Object:
		return "[object Object]"
	}
	return ""
}

// undefinedT marks a missing property.
type undefinedT struct{}

var undef = undefinedT{}

func prop(o jsjson.Object, k string) any {
	if v, ok := o.Get(k); ok {
		return v
	}
	return undef
}

func isObj(v any) bool {
	_, ok := v.(jsjson.Object)
	return ok
}
