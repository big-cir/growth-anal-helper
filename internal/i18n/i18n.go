// Package i18n holds the language of user-facing text (workspace.json `language`).
package i18n

import "sync/atomic"

var current atomic.Value

// SetLanguage sets "en" or "ko".
func SetLanguage(l string) { current.Store(l) }

// Language is the configured language ("en" by default).
func Language() string {
	if l, ok := current.Load().(string); ok {
		return l
	}
	return "en"
}

// Tr picks the text in the configured language.
func Tr(en, ko string) string {
	if Language() == "ko" {
		return ko
	}
	return en
}

// In picks the text for a given language.
func In(lang, en, ko string) string {
	if lang == "ko" {
		return ko
	}
	return en
}
