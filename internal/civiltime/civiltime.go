// Package civiltime handles timestamps as `YYYY-MM-DD HH:MM:SS.ffffff` (26 chars) so string order equals time order. No time zone conversion.
package civiltime

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"growth-lab/internal/jsjson"
)

var (
	tsRE   = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,6}))?$`)
	dateRE = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
)

type parts struct {
	y, mo, d, h, mi, s int
	frac               string
}

func parse(value string) (parts, error) {
	m := tsRE.FindStringSubmatch(value)
	if m == nil && dateRE.MatchString(value) {
		m = tsRE.FindStringSubmatch(value + " 00:00:00")
	}
	if m == nil {
		return parts{}, fmt.Errorf("not a timestamp: %s", jsjson.QuoteString(value))
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	p := parts{y: n(m[1]), mo: n(m[2]), d: n(m[3]), h: n(m[4]), mi: n(m[5]), s: n(m[6]), frac: m[7] + strings.Repeat("0", 6-len(m[7]))}
	if p.y < 1000 {
		return parts{}, fmt.Errorf("unsupported year (must be 1000 or later): %s", jsjson.QuoteString(value))
	}
	check := time.Date(p.y, time.Month(p.mo), p.d, 0, 0, 0, 0, time.UTC)
	if check.Year() != p.y || int(check.Month()) != p.mo || check.Day() != p.d {
		return parts{}, fmt.Errorf("no such date: %s", jsjson.QuoteString(value))
	}
	if p.h > 23 || p.mi > 59 || p.s > 59 {
		return parts{}, fmt.Errorf("no such time: %s", jsjson.QuoteString(value))
	}
	return p, nil
}

func format(p parts) (string, error) {
	if p.y < 1000 || p.y > 9999 {
		return "", fmt.Errorf("date outside the supported range (years 1000-9999): %d", p.y)
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%s", p.y, p.mo, p.d, p.h, p.mi, p.s, p.frac), nil
}

// Normalize returns the 26-char form. A bare date gets 00:00:00.
func Normalize(value string) (string, error) {
	p, err := parse(value)
	if err != nil {
		return "", err
	}
	return format(p)
}

// IsNormalized reports whether value is already in the 26-char form.
func IsNormalized(value string) bool {
	if len(value) != 26 {
		return false
	}
	n, err := Normalize(value)
	return err == nil && n == value
}

func dayNumber(p parts) int64 {
	sec := time.Date(p.y, time.Month(p.mo), p.d, 0, 0, 0, 0, time.UTC).Unix()
	days := sec / 86400
	if sec%86400 != 0 && sec < 0 {
		days--
	}
	return days
}

func fromDayNumber(days int64, rest parts) parts {
	t := time.Unix(days*86400, 0).UTC()
	rest.y, rest.mo, rest.d = t.Year(), int(t.Month()), t.Day()
	return rest
}

// AddDays adds n days, keeping the time of day.
func AddDays(value string, n int) (string, error) {
	p, err := parse(value)
	if err != nil {
		return "", err
	}
	return format(fromDayNumber(dayNumber(p)+int64(n), parts{h: p.h, mi: p.mi, s: p.s, frac: p.frac}))
}

// WeekStart returns Monday 00:00 of that week.
func WeekStart(value string) (string, error) {
	p, err := parse(value)
	if err != nil {
		return "", err
	}
	days := dayNumber(p)
	dow := (int64(time.Unix(days*86400, 0).UTC().Weekday()) + 6) % 7
	return format(fromDayNumber(days-dow, parts{frac: "000000"}))
}
