package panels

import (
	"growth-lab/internal/jsstr"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"growth-lab/internal/jsjson"
)

// Value is a result cell: nil, float64 or string (jsjson.Undefined for a missing column).
type Value = any

// numStr is String(number).
func numStr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return jsjson.Number(f)
}

// show is the display form of a cell: NULL, or String(v).
func show(v Value) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case float64:
		return numStr(x)
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return "undefined"
}

func isNum(v Value) bool {
	_, ok := v.(float64)
	return ok
}

// isInteger is Number.isInteger.
func isInteger(v Value) bool {
	f, ok := v.(float64)
	return ok && !math.IsInf(f, 0) && f == math.Trunc(f)
}

// isCount: an integer ≥ 0.
func isCount(v Value) bool {
	f, ok := v.(float64)
	return isInteger(v) && ok && f >= 0
}

var (
	decimalRE = regexp.MustCompile(`^[+-]?(?:Infinity|(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)$`)
	hexRE     = regexp.MustCompile(`^0[xX][0-9a-fA-F]+$`)
	octRE     = regexp.MustCompile(`^0[oO][0-7]+$`)
	binRE     = regexp.MustCompile(`^0[bB][01]+$`)
)

// toNumber is the ToNumber conversion (used by arithmetic on cells).
func toNumber(v Value) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		s := jsstr.Trim(x)
		switch {
		case s == "":
			return 0
		case decimalRE.MatchString(s):
			if strings.HasSuffix(s, "Infinity") {
				if strings.HasPrefix(s, "-") {
					return math.Inf(-1)
				}
				return math.Inf(1)
			}
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
					return f
				}
				return math.NaN()
			}
			return f
		case hexRE.MatchString(s), octRE.MatchString(s), binRE.MatchString(s):
			base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]]
			n, ok := new(big.Int).SetString(s[2:], base)
			if !ok {
				return math.NaN()
			}
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
		return math.NaN()
	}
	return math.NaN()
}

// jsRound is Math.round (halves round toward +∞).
func jsRound(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	f := math.Floor(x)
	if x-f >= 0.5 {
		f++
	}
	if f == 0 && (x < 0 || math.Signbit(x)) {
		return math.Copysign(0, -1)
	}
	return f
}

// toFixed1 is Number.prototype.toFixed(1).
func toFixed1(x float64) string {
	if math.IsNaN(x) {
		return "NaN"
	}
	if math.Abs(x) >= 1e21 || math.IsInf(x, 0) {
		return numStr(x)
	}
	sign := ""
	if x < 0 {
		sign = "-"
		x = -x
	}
	r := new(big.Rat).SetFloat64(x)
	r.Mul(r, big.NewRat(10, 1))
	r.Add(r, big.NewRat(1, 2))
	n := new(big.Int).Quo(r.Num(), r.Denom())
	s := n.String()
	if len(s) < 2 {
		s = "0" + s
	}
	return sign + s[:len(s)-1] + "." + s[len(s)-1:]
}

// localeInt is Number.prototype.toLocaleString('en-US') for an integer.
func localeInt(x float64) string {
	sign := ""
	if x < 0 || (x == 0 && math.Signbit(x)) {
		sign = "-"
		x = -x
	}
	if math.IsInf(x, 0) {
		return sign + "∞"
	}
	// Intl formats the shortest round-trip digits, padded with zeros
	digits := "0"
	if x != 0 {
		mant, exp, _ := strings.Cut(strconv.FormatFloat(x, 'e', -1, 64), "e")
		d := strings.Replace(mant, ".", "", 1)
		n, _ := strconv.Atoi(exp)
		digits = d + strings.Repeat("0", n+1-len(d))
	}
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return sign + b.String()
}

// jsonOf is JSON.stringify for cells and simple values.
func jsonOf(v any) string { return jsjson.MustStringify(v) }
