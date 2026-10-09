package collect

import (
	"regexp"
	"strings"
)

// Sensitive-looking name rules. Matching columns and tables can only be collected as private.

var (
	camelRE   = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	splitRE   = regexp.MustCompile(`[^a-z0-9]+`)
	nonWordRE = regexp.MustCompile(`[^a-z0-9]`)
)

// strong words, matched even without separators
var strong = []string{"password", "passwd", "secret", "apikey", "privatekey", "credential", "accesstoken", "refreshtoken", "deviceid", "ipaddr", "emailaddr", "phonenumber"}

// matched when a word of the name is this word
var wordSet = map[string]bool{"pass": true, "pwd": true, "token": true, "salt": true, "hash": true, "otp": true, "mfa": true, "totp": true, "bearer": true, "oauth": true, "cookie": true, "signature": true, "email": true, "phone": true, "mobile": true, "address": true, "ip": true}

// matched only together with other words (analytics columns like session_count are allowed)
var paired = []struct {
	word string
	with []string
}{{"session", []string{"id", "key", "token", "cookie", "secret"}}, {"push", []string{"token", "id", "key"}}}

func tokens(name string) []string {
	var out []string
	for _, t := range splitRE.Split(strings.ToLower(camelRE.ReplaceAllString(name, "${1}_${2}")), -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// SensitiveColumnName returns the matched rule for a column or table name, or "".
func SensitiveColumnName(name string) string {
	flat := nonWordRE.ReplaceAllString(strings.ToLower(name), "")
	for _, w := range strong {
		if strings.Contains(flat, w) {
			return w
		}
	}
	ts := tokens(name)
	for _, t := range ts {
		if wordSet[t] {
			return t
		}
	}
	for _, p := range paired {
		if has(ts, p.word) {
			for _, t := range ts {
				if has(p.with, t) {
					return p.word
				}
			}
		}
	}
	return ""
}
