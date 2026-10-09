// Package jsstr holds JavaScript string semantics the engine keeps for compatibility: whitespace, trimming, UTF-16 lengths and order.
package jsstr

import (
	"strings"
	"unicode/utf16"

	"growth-lab/internal/jsjson"
)

// SpaceChars is the character set of JavaScript's \s, for use inside a regexp class.
const SpaceChars = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// Space is JavaScript's \s as a regexp class.
const Space = `[` + SpaceChars + `]`

// IsSpace matches JavaScript's \s.
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Trim is String.prototype.trim.
func Trim(s string) string { return strings.TrimFunc(s, IsSpace) }

// TrimEnd is String.prototype.trimEnd.
func TrimEnd(s string) string { return strings.TrimRightFunc(s, IsSpace) }

// CPLen is [...s].length (code points).
func CPLen(s string) int { return len([]rune(s)) }

// U16Len is s.length (UTF-16 code units).
func U16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// U16Slice is s.slice(0, n) on code units (a split surrogate pair keeps its lone half).
func U16Slice(s string, n int) jsjson.UTF16 {
	u := utf16.Encode([]rune(s))
	n = max(0, min(n, len(u)))
	return jsjson.UTF16(u[:n])
}

// U16String turns code units back into a Go string (lone surrogates become U+FFFD).
func U16String(u jsjson.UTF16) string { return string(utf16.Decode(u)) }

// Less compares by UTF-16 code units, like `<` and Array.prototype.sort.
func Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}
