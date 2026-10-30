package source

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "client.cnf")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSource(t *testing.T) {
	s, err := LoadSource(writeFile(t, "[mysqld]\nhost=wrong\n[client]\nhost=\"db.example\"\nuser='reader'\nport=2016\npassword=p@ss=1\nssl-mode=REQUIRED\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "db.example" || s.User != "reader" || s.Port != 2016 || s.Password != "p@ss=1" {
		t.Fatalf("got %+v", s)
	}
	for name, body := range map[string]string{
		"include":        "!include other.cnf\n[client]\nhost=h\nuser=u\n",
		"inline comment": "[client]\nhost=h # note\nuser=u\n",
		"escape":         "[client]\nhost=h\nuser=u\npassword=\"a\\\"b\"\n",
		"unbalanced":     "[client]\nhost=h\nuser=u\npassword=\"ab\n",
		"bad port":       "[client]\nhost=h\nuser=u\nport=x\n",
		"no user":        "[client]\nhost=h\n",
	} {
		if _, err := LoadSource(writeFile(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestVersionNumber(t *testing.T) {
	for v, want := range map[string]int{"8.4.8": 80408, "8.0.36-log": 80036, "5.7.44-rds": 50744} {
		if got, err := versionNumber(v); err != nil || got != want {
			t.Errorf("%s: %d %v", v, got, err)
		}
	}
	if _, err := versionNumber("x"); err == nil {
		t.Error("bad version should fail")
	}
}

func TestPickServerID(t *testing.T) {
	st := Status{ServerID: 5, ReplicaIDs: []uint32{7}}
	if _, err := PickServerID(5, st); err == nil {
		t.Error("source id should be rejected")
	}
	if _, err := PickServerID(7, st); err == nil {
		t.Error("replica id should be rejected")
	}
	if id, err := PickServerID(9, st); err != nil || id != 9 {
		t.Errorf("got %d %v", id, err)
	}
	for range 100 {
		id, err := PickServerID(0, st)
		if err != nil || id < 1<<31 {
			t.Fatalf("got %d %v", id, err)
		}
	}
}
