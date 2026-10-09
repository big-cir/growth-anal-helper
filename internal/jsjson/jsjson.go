// Package jsjson produces the same bytes as JavaScript JSON.stringify, so hashes and stored files stay stable.
package jsjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Member is one key of an Object.
type Member struct {
	Key   string
	Value any
}

// Object keeps key order like a JavaScript object.
type Object []Member

// Undefined is omitted from objects and written as null in arrays.
type undefined struct{}

var Undefined = undefined{}

// Get returns the value of a key.
func (o Object) Get(key string) (any, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// Stringify encodes v. Supported: nil, bool, ints, float64, string, []any, []string, Object, Undefined.
func Stringify(v any) (string, error) {
	var b strings.Builder
	if err := write(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// MustStringify panics on unsupported values.
func MustStringify(v any) string {
	s, err := Stringify(v)
	if err != nil {
		panic(err)
	}
	return s
}

func write(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case undefined:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(Number(float64(x)))
	case int64:
		b.WriteString(Number(float64(x)))
	case float64:
		b.WriteString(Number(x))
	case string:
		Quote(b, x)
	case UTF16:
		quote16(b, x)
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			Quote(b, s)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := write(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case Object:
		b.WriteByte('{')
		first := true
		for _, m := range jsOrder(x) {
			if _, skip := m.Value.(undefined); skip {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			Quote(b, m.Key)
			b.WriteByte(':')
			if err := write(b, m.Value); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("jsjson: unsupported type %T", v)
	}
	return nil
}

// jsOrder puts array-index keys first in ascending order, then the rest in insertion order.
func jsOrder(o Object) Object {
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
	var nums, rest Object
	for _, m := range o {
		if _, ok := idx(m.Key); ok {
			nums = append(nums, m)
		} else {
			rest = append(rest, m)
		}
	}
	if len(nums) == 0 {
		return o
	}
	sort.SliceStable(nums, func(i, j int) bool {
		a, _ := idx(nums[i].Key)
		c, _ := idx(nums[j].Key)
		return a < c
	})
	return append(nums, rest...)
}

// Number formats like JavaScript Number.prototype.toString; NaN and Infinity become null as in JSON.
func Number(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	if f < 0 {
		return "-" + Number(-f)
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k := len(digits)
	n := exp + 1
	switch {
	case k <= n && n <= 21:
		return digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return "0." + strings.Repeat("0", -n) + digits
	}
	sign := "+"
	if n-1 < 0 {
		sign = "-"
	}
	abs := n - 1
	if abs < 0 {
		abs = -abs
	}
	if k == 1 {
		return digits + "e" + sign + strconv.Itoa(abs)
	}
	return digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(abs)
}

// Quote writes a string literal the way JSON.stringify does (no HTML escaping).
func Quote(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// UTF16 is a JavaScript string as code units; it may hold lone surrogates.
type UTF16 []uint16

func quote16(b *strings.Builder, s UTF16) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		u := s[i]
		if u >= 0xd800 && u <= 0xdbff && i+1 < len(s) && s[i+1] >= 0xdc00 && s[i+1] <= 0xdfff {
			b.WriteRune(rune(0x10000 + (int(u)-0xd800)<<10 + int(s[i+1]) - 0xdc00))
			i++
			continue
		}
		if u >= 0xd800 && u <= 0xdfff {
			b.WriteString(`\u`)
			b.WriteByte(hex[u>>12])
			b.WriteByte(hex[u>>8&0xf])
			b.WriteByte(hex[u>>4&0xf])
			b.WriteByte(hex[u&0xf])
			continue
		}
		var one strings.Builder
		Quote(&one, string(rune(u)))
		q := one.String()
		b.WriteString(q[1 : len(q)-1])
	}
	b.WriteByte('"')
}

// QuoteString returns the JSON.stringify form of a string.
func QuoteString(s string) string {
	var b strings.Builder
	Quote(&b, s)
	return b.String()
}

// Parse decodes JSON text keeping object key order as JSON.parse does (array-index keys first).
func Parse(text string) (any, error) {
	d := json.NewDecoder(strings.NewReader(text))
	d.UseNumber()
	v, err := parseValue(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("jsjson: unexpected data after the value")
	}
	return v, nil
}

func parseValue(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch x := t.(type) {
	case json.Delim:
		switch x {
		case '[':
			arr := []any{}
			for d.More() {
				e, err := parseValue(d)
				if err != nil {
					return nil, err
				}
				arr = append(arr, e)
			}
			_, err := d.Token()
			return arr, err
		case '{':
			obj := Object{}
			for d.More() {
				kt, err := d.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				val, err := parseValue(d)
				if err != nil {
					return nil, err
				}
				replaced := false
				for i := range obj {
					if obj[i].Key == k {
						obj[i].Value = val
						replaced = true
						break
					}
				}
				if !replaced {
					obj = append(obj, Member{k, val})
				}
			}
			_, err := d.Token()
			return jsOrder(obj), err
		}
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil && !isRange(err) {
			return nil, err
		}
		return f, nil
	case string, bool, nil:
		return x, nil
	}
	return nil, fmt.Errorf("jsjson: unexpected token %v", t)
}

func isRange(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

// Compact is a helper for tests: re-encodes JSON text through Parse.
func Compact(text []byte) (string, error) {
	v, err := Parse(string(bytes.TrimSpace(text)))
	if err != nil {
		return "", err
	}
	return Stringify(v)
}

// Ordered returns the members in JavaScript object order (array-index keys first).
func Ordered(o Object) Object { return jsOrder(o) }

// Indent is JSON.stringify(v, null, indent).
func Indent(v any, indent int) string {
	var b strings.Builder
	writeIndent(&b, v, strings.Repeat(" ", indent), "")
	return b.String()
}

func writeIndent(b *strings.Builder, v any, step, cur string) {
	switch x := v.(type) {
	case Object:
		var members Object
		for _, m := range jsOrder(x) {
			if _, skip := m.Value.(undefined); !skip {
				members = append(members, m)
			}
		}
		if len(members) == 0 {
			b.WriteString("{}")
			return
		}
		next := cur + step
		b.WriteString("{\n")
		for i, m := range members {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString(next + QuoteString(m.Key) + ": ")
			writeIndent(b, m.Value, step, next)
		}
		b.WriteString("\n" + cur + "}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		next := cur + step
		b.WriteString("[\n")
		for i, e := range x {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString(next)
			writeIndent(b, e, step, next)
		}
		b.WriteString("\n" + cur + "]")
	case []string:
		a := make([]any, len(x))
		for i, s := range x {
			a[i] = s
		}
		writeIndent(b, a, step, cur)
	default:
		b.WriteString(MustStringify(v))
	}
}
