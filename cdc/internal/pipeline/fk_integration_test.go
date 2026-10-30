//go:build integration

package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"growth-lab-cdc/internal/cdc"
	"growth-lab-cdc/internal/check"
	"growth-lab-cdc/internal/testenv"
)

// sourceFK makes app.post.member_id reference app.member on the source with the given actions.
func (h *harness) sourceFK(actions string) {
	h.T.Helper()
	h.Exec(h.SrcDB, "ALTER TABLE app.post ADD CONSTRAINT post_member FOREIGN KEY (member_id) REFERENCES app.member (id) "+actions)
	h.Exec(h.SrcDB, "FLUSH BINARY LOGS")
}

// sourceDDL changes the source's tables, leaving the change out of the newest binlog file that
// boundary reads.
func (h *harness) sourceDDL(qs ...string) {
	h.T.Helper()
	for _, q := range qs {
		h.Exec(h.SrcDB, q)
	}
	h.Exec(h.SrcDB, "FLUSH BINARY LOGS")
}

func (h *harness) foreignKeys(db *sql.DB) int {
	h.T.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = 'app' OR UNIQUE_CONSTRAINT_SCHEMA = 'app'`).Scan(&n); err != nil {
		h.T.Fatal(err)
	}
	return n
}

func TestForeignKeysOnTheSource(t *testing.T) {
	h := newHarness(t)
	sub := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			parent := h.T
			h.T = t
			defer func() { h.T = parent }()
			h.freshForInit(false)
			f(t)
		})
	}
	stops := func(t *testing.T, err error, table string) {
		t.Helper()
		var s *cdc.StopError
		if !errors.As(err, &s) || !strings.Contains(err.Error(), table+": the source has foreign keys with cascading actions") {
			t.Fatalf("init should stop on %s: %v", table, err)
		}
	}

	for _, actions := range []string{"ON DELETE NO ACTION ON UPDATE NO ACTION", "ON DELETE RESTRICT ON UPDATE RESTRICT"} {
		sub("allows "+actions, func(t *testing.T) {
			h.sourceFK(actions)
			if err := Init(context.Background(), h.initOpts()); err != nil {
				t.Fatalf("init: %v", err)
			}
			if n := h.foreignKeys(h.DstDB); n != 0 {
				t.Fatalf("the copy has %d foreign keys", n)
			}
			conn, err := h.SrcDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// children before their parent, a multi-table delete, a replace, and with checks off
			// an orphan and a parent deleted under its children
			for _, q := range []string{
				"DELETE FROM app.post WHERE member_id = 110",
				"DELETE FROM app.member WHERE id = 110",
				"DELETE p, l FROM app.post p JOIN app.like_mark l ON l.member_id = p.member_id WHERE p.member_id = 120",
				"REPLACE INTO app.post (id, member_id, body) VALUES (130, 131, 'replaced')",
				"INSERT INTO app.post (id, member_id, body) VALUES (160, 161, 'dup') ON DUPLICATE KEY UPDATE member_id = VALUES(member_id), body = 'dup'",
				"SET SESSION foreign_key_checks = 0",
				"INSERT INTO app.post (member_id, body) VALUES (999999, 'orphan')",
				"DELETE FROM app.member WHERE id = 140",
				"SET SESSION foreign_key_checks = 1",
			} {
				if _, err := conn.ExecContext(context.Background(), q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			// a multi-table delete that a child row refuses is rolled back as a whole
			if _, err := conn.ExecContext(context.Background(), "DELETE m, l FROM app.member m JOIN app.like_mark l ON l.member_id = m.id WHERE m.id = 150"); err == nil {
				t.Fatal("deleting a member with posts should fail")
			}
			h.done(false)
			h.checkSame()
		})
	}

	sub("stops on a cascading delete from a followed table", func(t *testing.T) {
		h.sourceFK("ON DELETE CASCADE")
		stops(t, Init(context.Background(), h.initOpts()), "app.member")
	})

	sub("stops on a cascading update", func(t *testing.T) {
		h.sourceFK("ON UPDATE SET NULL")
		stops(t, Init(context.Background(), h.initOpts()), "app.member")
	})

	sub("stops on a cascading delete from a table it does not follow", func(t *testing.T) {
		h.Exec(h.SrcDB, `CREATE TABLE app.attachment (id bigint NOT NULL PRIMARY KEY, post_id bigint NOT NULL,
			FOREIGN KEY (post_id) REFERENCES app.post (id) ON DELETE CASCADE) ENGINE=InnoDB`)
		stops(t, Init(context.Background(), h.initOpts()), "app.post")
	})

	sub("looks at the schema, not only the table name", func(t *testing.T) {
		h.sourceDDL("CREATE TABLE app_other.member (id bigint NOT NULL PRIMARY KEY) ENGINE=InnoDB",
			`CREATE TABLE app_other.post (id bigint NOT NULL PRIMARY KEY, member_id bigint,
			FOREIGN KEY (member_id) REFERENCES app_other.member (id) ON DELETE CASCADE) ENGINE=InnoDB`)
		if err := Init(context.Background(), h.initOpts()); err != nil {
			t.Fatalf("a cascade between tables of another database stopped init: %v", err)
		}
		h.done(false)
	})

	// LOCK TABLES ... READ does not lock the tables related by foreign keys: an open write there
	// does not hold the lock back, and writes there go on while it is held
	sub("tables related by foreign keys stay writable around the lock", func(t *testing.T) {
		h.sourceDDL(`CREATE TABLE app.attachment (id bigint NOT NULL PRIMARY KEY, post_id bigint NOT NULL,
			FOREIGN KEY (post_id) REFERENCES app.post (id)) ENGINE=InnoDB`)
		tx, err := h.SrcDB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec("INSERT INTO app.attachment VALUES (1, 1)"); err != nil {
			t.Fatal(err)
		}
		wrote := false
		testHooks.initStep = func(s string) error {
			if s != "source-locked" || wrote {
				return nil
			}
			wrote = true
			conn, err := h.SrcDB.Conn(context.Background())
			if err != nil {
				return err
			}
			defer conn.Close()
			for _, q := range []string{"SET SESSION lock_wait_timeout = 2", "INSERT INTO app.attachment VALUES (2, 1)"} {
				if _, err := conn.ExecContext(context.Background(), q); err != nil {
					return fmt.Errorf("the related table should be writable while init holds the lock: %w", err)
				}
			}
			return nil
		}
		defer func() { testHooks.initStep = nil }()
		var logs []string
		o := h.initOpts()
		o.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)); h.T.Logf(f, a...) }
		if err := Init(context.Background(), o); err != nil {
			t.Fatalf("init: %v", err)
		}
		if !wrote {
			t.Fatal("the hook never fired")
		}
		m := regexp.MustCompile(`locked the source tables after waiting (\S+),`).FindStringSubmatch(strings.Join(logs, "\n"))
		if m == nil {
			t.Fatal("no lock line in the log")
		}
		if waited, err := time.ParseDuration(m[1]); err != nil || waited > time.Second {
			t.Fatalf("the open write on the related table held the lock back: waited %s", m[1])
		}
		tx.Rollback()
		testHooks.initStep = nil
		h.Exec(h.SrcDB, "DELETE FROM app.attachment")
		h.done(false)
	})

	// what an account cannot see is not checked, as the README says; the lock itself needs no
	// privilege on the related table
	sub("an account without access to a related table", func(t *testing.T) {
		h.sourceDDL(`CREATE TABLE app.attachment (id bigint NOT NULL PRIMARY KEY, post_id bigint NOT NULL,
			FOREIGN KEY (post_id) REFERENCES app.post (id) ON DELETE CASCADE) ENGINE=InnoDB`)
		qs := []string{"DROP USER IF EXISTS 'cdc_narrow'@'%'", "CREATE USER 'cdc_narrow'@'%' IDENTIFIED BY 'narrow-pw'",
			"GRANT REPLICATION CLIENT, REPLICATION SLAVE ON *.* TO 'cdc_narrow'@'%'",
			"GRANT LOCK TABLES ON app.* TO 'cdc_narrow'@'%'"}
		for _, tb := range testenv.Followed {
			qs = append(qs, "GRANT SELECT ON app."+tb+" TO 'cdc_narrow'@'%'")
		}
		for _, q := range qs {
			h.Exec(h.SrcDB, q)
		}
		defer h.Exec(h.SrcDB, "DROP USER IF EXISTS 'cdc_narrow'@'%'")
		o := h.initOpts()
		o.Source.User, o.Source.Password = "cdc_narrow", "narrow-pw"
		if err := Init(context.Background(), o); err != nil {
			t.Fatalf("init with the narrow account: %v", err)
		}
		if err := Init(context.Background(), h.initOpts()); err == nil || !strings.Contains(err.Error(), "cascading") {
			// the copy is ready, so a second init only checks; an account that sees the cascade stops
			t.Fatalf("an account that sees the cascade should stop: %v", err)
		}
	})

	sub("run stops on a cascading foreign key on the source", func(t *testing.T) {
		if err := Init(context.Background(), h.initOpts()); err != nil {
			t.Fatalf("init: %v", err)
		}
		h.sourceFK("ON DELETE CASCADE")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := <-h.start(ctx)
		if !strings.Contains(fmt.Sprint(err), "app.member: the source has foreign keys with cascading actions") {
			t.Fatalf("run should stop on the cascade: %v", err)
		}
	})

	sub("run still stops on a foreign key in the copy", func(t *testing.T) {
		if err := Init(context.Background(), h.initOpts()); err != nil {
			t.Fatalf("init: %v", err)
		}
		h.Exec(h.DstDB, "ALTER TABLE app.post ADD CONSTRAINT post_member FOREIGN KEY (member_id) REFERENCES app.member (id)")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := <-h.start(ctx)
		if !strings.Contains(fmt.Sprint(err), "the copy has foreign keys") {
			t.Fatalf("run should stop on the copy's foreign key: %v", err)
		}
	})
}

// checkSame compares the copy with the source, writes stopped, and wants every row the same.
func (h *harness) checkSame() {
	h.T.Helper()
	rep, err := check.Check(context.Background(), check.CheckOptions{Source: h.Src, Target: h.Dst, Database: "app", Tables: testenv.Followed,
		WritesStopped: true, ChunkRows: 100, Wait: 3 * time.Second, Rounds: 3, RoundGap: 300 * time.Millisecond, Logf: h.T.Logf})
	if err != nil {
		h.T.Fatalf("check: %v", err)
	}
	for name, t := range rep.Tables {
		if t.Missing+t.Extra+t.Differ+t.Undecided > 0 || t.Unchecked {
			h.T.Fatalf("%s: missing %d extra %d differ %d undecided %d unchecked %v", name, t.Missing, t.Extra, t.Differ, t.Undecided, t.Unchecked)
		}
	}
}
