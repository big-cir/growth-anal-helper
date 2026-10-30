package cdc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	gosql "github.com/go-sql-driver/mysql"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestFilter(t *testing.T) {
	got, err := ParseList("--x", " app , shop,app ")
	if err != nil || strings.Join(got, ",") != "app,shop" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := ParseList("--x", "app,,shop"); err == nil {
		t.Error("empty item should fail")
	}
	if _, err := NewFilter(nil, nil); err == nil {
		t.Error("database list is required")
	}
	if _, err := NewFilter([]string{"app("}, nil); err == nil {
		t.Error("invalid pattern should fail")
	}
	f, err := NewFilter([]string{"app", ".*"}, []string{`app\.user`, `shop\..*`})
	must(t, err)
	for _, c := range []struct {
		db, table string
		want      bool
	}{
		{"app", "user", true},
		{"app", "users", false},
		{"app", "team_member", false},
		{"shop", "orders", true},
		{"App", "user", false},
		{"mysql", "user", false},
		{"information_schema", "TABLES", false},
	} {
		if f.Table(c.db, c.table) != c.want {
			t.Errorf("%s.%s: want %v", c.db, c.table, c.want)
		}
	}
	all, err := NewFilter([]string{"app"}, nil)
	must(t, err)
	if !all.Table("app", "anything") || all.Table("application", "user") {
		t.Error("without a table list every table of a matched database is followed, names match whole")
	}
}

func TestSafeErrorKeepsOnlyFixedInformation(t *testing.T) {
	raw := &replication.EventError{Header: &replication.EventHeader{EventType: replication.WRITE_ROWS_EVENTv2, LogPos: 900}, Err: "bad", Data: []byte("secret-row-value")}
	for _, err := range []error{raw, fmt.Errorf("wrapped: %w", raw)} {
		msg := SafeError(err).Error()
		if strings.Contains(msg, "secret") || !strings.Contains(msg, "WriteRowsEventV2") || !strings.Contains(msg, "900") {
			t.Errorf("got %q", msg)
		}
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{&gosql.MySQLError{Number: 1062, Message: "Duplicate entry 'secret' for key"}, "MySQL error 1062"},
		{&mysql.MyError{Code: 1045, Message: "Access denied for user 'secret'"}, "MySQL error 1045: access denied (user or password)"},
		{fmt.Errorf("query: %w", context.DeadlineExceeded), "time limit reached"},
		{errors.New("value 'secret' is short"), "error of type *errors.errorString"},
		{&Error{"own message"}, "own message"},
	} {
		if got := SafeError(c.err).Error(); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}
