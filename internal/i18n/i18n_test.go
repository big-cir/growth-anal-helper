package i18n

import "testing"

func TestTr(t *testing.T) {
	if Language() != "en" || Tr("a", "b") != "a" {
		t.Fatal("default must be en")
	}
	SetLanguage("ko")
	if Tr("a", "b") != "b" {
		t.Fatal("ko")
	}
	SetLanguage("en")
}
