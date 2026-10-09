// Package demo generates the demo source DB: a fictional community board app. Same seed and anchor → same DB.
package demo

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/sqlitec"
)

// Options: Seed, Weeks and Members default to 20240501, 20 and 1800.
type Options struct {
	Anchor  string
	Seed    *float64
	Weeks   *int
	Members *int
}

// Summary counts the generated rows.
type Summary struct {
	Members, Boards, BoardMembers, Posts, Replies, Reactions int
}

// JSON is the summary as JSON.stringify writes it.
func (s Summary) JSON() string {
	return jsjson.MustStringify(jsjson.Object{{Key: "members", Value: s.Members}, {Key: "boards", Value: s.Boards}, {Key: "boardMembers", Value: s.BoardMembers},
		{Key: "posts", Value: s.Posts}, {Key: "replies", Value: s.Replies}, {Key: "reactions", Value: s.Reactions}})
}

// rng is mulberry32.
func rng(seed uint32) func() float64 {
	a := seed
	return func() float64 {
		a += 0x6d2b79f5
		t := a
		t = (t ^ (t >> 15)) * (t | 1)
		t ^= t + (t^(t>>7))*(t|61)
		return float64(t^(t>>14)) / 4294967296
	}
}

const msDay = 86_400_000

var anchorRE = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[ T](\d{2}):(\d{2}):(\d{2}))?$`)

func parseLocal(ts string) (float64, error) {
	m := anchorRE.FindStringSubmatch(ts)
	if m == nil {
		return 0, fmt.Errorf("invalid anchor: %s", ts)
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	y := n(m[1])
	if y >= 0 && y <= 99 {
		y += 1900
	}
	return float64(time.Date(y, time.Month(n(m[2])), n(m[3]), n(m[4]), n(m[5]), n(m[6]), 0, time.UTC).UnixMilli()), nil
}

// format: micro adds a 6-digit fraction (one random draw), otherwise seconds.
func format(ms float64, micro bool, r func() float64) string {
	base := time.UnixMilli(int64(ms)).UTC().Format("2006-01-02 15:04:05")
	if !micro {
		return base
	}
	return fmt.Sprintf("%s.%06d", base, int64(math.Floor(r()*1e6)))
}

const schema = `
CREATE TABLE member (
  id INTEGER PRIMARY KEY, created_at TEXT NOT NULL, deleted_at TEXT, country TEXT NOT NULL, nickname TEXT NOT NULL
);
CREATE TABLE board (
  id INTEGER PRIMARY KEY, created_at TEXT NOT NULL, deleted_at TEXT, owner_id INTEGER NOT NULL, title TEXT NOT NULL
);
CREATE TABLE board_member (
  board_id INTEGER NOT NULL, member_id INTEGER NOT NULL, joined_at TEXT NOT NULL, left_at TEXT,
  PRIMARY KEY (board_id, member_id)
);
CREATE TABLE post (
  id INTEGER PRIMARY KEY, board_id INTEGER NOT NULL, member_id INTEGER NOT NULL, created_at TEXT NOT NULL, deleted_at TEXT, body TEXT NOT NULL
);
CREATE TABLE reply (
  id INTEGER PRIMARY KEY, post_id INTEGER NOT NULL, member_id INTEGER NOT NULL, parent_reply_id INTEGER,
  created_at TEXT NOT NULL, deleted_at TEXT, body TEXT NOT NULL
);
CREATE TABLE reaction (
  id INTEGER PRIMARY KEY, post_id INTEGER NOT NULL, member_id INTEGER NOT NULL, created_at TEXT NOT NULL
);`

var countries = []string{"KR", "KR", "KR", "KR", "JP", "US", "TW"}

type member struct {
	id, signup, propensity float64
	deleted                *float64
	boards                 []float64
}

// orderedMembers is a JavaScript Map of member id → joined time (insertion order).
type orderedMembers struct {
	keys []float64
	at   map[float64]float64
}

func (o *orderedMembers) set(k, v float64) {
	if _, ok := o.at[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.at[k] = v
}

type board struct {
	id, created float64
	deleted     *float64
	members     *orderedMembers
}

type post struct {
	id, board, member, at float64
	deleted               *float64
}

type reply struct {
	id, post, member, at float64
	parent, deleted      *float64
}

type reaction struct{ id, post, member, at float64 }

func ptr(v float64) *float64 { return &v }

// Seed writes the demo DB to path (replacing it) and returns the row counts.
func Seed(path string, o Options) (Summary, error) {
	seed := 20240501.0
	if o.Seed != nil {
		seed = *o.Seed
	}
	r := rng(uint32(int64(seed)))
	anchor, err := parseLocal(o.Anchor)
	if err != nil {
		return Summary{}, err
	}
	weeks := 20
	if o.Weeks != nil {
		weeks = *o.Weeks
	}
	total := 1800
	if o.Members != nil {
		total = *o.Members
	}
	start := anchor - float64(weeks)*7*msDay
	pickIdx := func(n int) int { return int(math.Floor(r() * float64(n))) }

	// Members
	members := make([]*member, 0, total)
	for i := 0; i < total; i++ {
		u := math.Sqrt(r())
		signup := start + math.Floor(u*(anchor-start-msDay))
		members = append(members, &member{signup: signup, propensity: 0.25 + r()*0.6})
	}
	sort.SliceStable(members, func(i, j int) bool { return members[i].signup < members[j].signup })
	for i, m := range members {
		m.id = float64(1001 + i)
	}
	// Some members leave (no activity afterwards)
	for _, m := range members {
		if r() < 0.04 {
			m.deleted = ptr(m.signup + math.Floor((3+r()*40)*msDay))
		}
	}
	activeAt := func(m *member, at float64) bool { return at < anchor && (m.deleted == nil || at < *m.deleted) }

	// Create and join boards
	type event struct {
		at     float64
		create bool
		m      *member
	}
	var boards []*board
	var events []event
	for _, m := range members {
		roll := r()
		switch {
		case roll < 0.28:
			events = append(events, event{m.signup + math.Floor(r()*2*msDay), true, m})
		case roll < 0.78:
			events = append(events, event{m.signup + math.Floor(r()*7*msDay), false, m})
		case roll < 0.86:
			events = append(events, event{m.signup + math.Floor((7+r()*20)*msDay), false, m})
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at < events[j].at })
	for _, e := range events {
		if !activeAt(e.m, e.at) {
			continue
		}
		if e.create || len(boards) == 0 {
			var deleted *float64
			if r() < 0.03 {
				deleted = ptr(e.at + math.Floor((10+r()*30)*msDay))
			}
			b := &board{id: float64(501 + len(boards)), created: e.at, deleted: deleted, members: &orderedMembers{at: map[float64]float64{}}}
			b.members.set(e.m.id, e.at)
			boards = append(boards, b)
			e.m.boards = append(e.m.boards, b.id)
			continue
		}
		open := func(b *board) bool { return b.deleted == nil || e.at+1000 < *b.deleted }
		from := 0
		if len(boards) > 60 {
			from = len(boards) - 60
		}
		var recent []*board
		for _, b := range boards[from:] {
			if _, has := b.members.at[e.m.id]; len(b.members.keys) < 6 && !has && open(b) {
				recent = append(recent, b)
			}
		}
		var b *board
		if len(recent) > 0 {
			b = recent[pickIdx(len(recent))]
		} else {
			b = boards[pickIdx(len(boards))]
		}
		if _, has := b.members.at[e.m.id]; has || !open(b) {
			continue
		}
		b.members.set(e.m.id, math.Max(e.at, b.created+1000))
		e.m.boards = append(e.m.boards, b.id)
	}
	boardByID := map[float64]*board{}
	for _, b := range boards {
		boardByID[b.id] = b
	}
	memberByID := map[float64]*member{}
	for _, m := range members {
		memberByID[m.id] = m
	}

	// Some members leave boards
	type bm struct{ b, m float64 }
	leftAt := map[bm]float64{}
	for _, b := range boards {
		for _, mid := range b.members.keys {
			if mid != b.members.keys[0] && r() < 0.05 {
				leftAt[bm{b.id, mid}] = b.members.at[mid] + math.Floor((2+r()*30)*msDay)
			}
		}
	}
	inBoard := func(bid float64, m *member, at float64) bool {
		left, has := leftAt[bm{bid, m.id}]
		b := boardByID[bid]
		return activeAt(m, at) && (!has || at < left) && (b.deleted == nil || at < *b.deleted)
	}

	// Activity
	var posts []*post
	var replies []*reply
	var reactions []*reaction
	for _, m := range members {
		for w := 0; ; w++ {
			weekStart := m.signup + float64(w)*7*msDay
			if weekStart >= anchor {
				break
			}
			for _, bid := range m.boards {
				b := boardByID[bid]
				joined := b.members.at[m.id]
				if joined >= weekStart+7*msDay {
					continue
				}
				factor := 0.7
				if len(b.members.keys) >= 2 {
					factor = 1.7
				}
				p := m.propensity * math.Pow(0.82, float64(w)) * factor
				if r() >= math.Min(p, 0.95) {
					continue
				}
				n := 1 + int(math.Floor(r()*3))
				for k := 0; k < n; k++ {
					at := math.Max(joined+60_000, weekStart) + math.Floor(r()*7*msDay)
					if !inBoard(bid, m, at) {
						continue
					}
					var deleted *float64
					if r() < 0.03 {
						deleted = ptr(at + 1000 + math.Floor(r()*10*msDay))
					}
					posts = append(posts, &post{board: bid, member: m.id, at: at, deleted: deleted})
				}
			}
		}
	}
	sort.SliceStable(posts, func(i, j int) bool { return posts[i].at < posts[j].at })
	for i, p := range posts {
		p.id = float64(70001 + i)
	}

	for _, p := range posts {
		b := boardByID[p.board]
		for _, mid := range b.members.keys {
			joined := b.members.at[mid]
			if mid == p.member {
				continue
			}
			other := memberByID[mid]
			if r() < other.propensity*0.5 {
				at := math.Max(p.at, joined) + math.Floor(r()*2*msDay) + 30_000
				if inBoard(p.board, other, at) {
					reactions = append(reactions, &reaction{post: p.id, member: mid, at: at})
				}
			}
			if r() < other.propensity*0.35 {
				at := math.Max(p.at, joined) + math.Floor(r()*3*msDay) + 60_000
				if !inBoard(p.board, other, at) {
					continue
				}
				var deleted *float64
				if r() < 0.04 {
					deleted = ptr(at + msDay)
				}
				replies = append(replies, &reply{post: p.id, member: mid, at: at, deleted: deleted})
				if r() < 0.4 {
					at2 := at + math.Floor(r()*msDay) + 30_000
					if inBoard(p.board, memberByID[p.member], at2) {
						replies = append(replies, &reply{id: -1, post: p.id, member: p.member, at: at2})
					}
				}
			}
		}
	}
	sort.SliceStable(replies, func(i, j int) bool { return replies[i].at < replies[j].at })
	lastReply := map[float64]float64{}
	for i, x := range replies {
		nested := x.id == -1
		x.id = float64(900001 + i)
		x.parent = nil
		if nested {
			if v, ok := lastReply[x.post]; ok {
				x.parent = ptr(v)
			}
		} else {
			lastReply[x.post] = x.id
		}
	}
	sort.SliceStable(reactions, func(i, j int) bool { return reactions[i].at < reactions[j].at })
	for i, x := range reactions {
		x.id = float64(300001 + i)
	}

	// Write rows
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return Summary{}, err
		}
	}
	abs, _ := filepath.Abs(path)
	if err := os.MkdirAll(filepath.Dir(abs), 0o777); err != nil {
		return Summary{}, err
	}
	db, err := sqlitec.Open(path, false)
	if err != nil {
		return Summary{}, err
	}
	defer db.Close()
	ended := func(t *float64, micro bool) any {
		if t == nil || *t >= anchor {
			return nil
		}
		return format(*t, micro, r)
	}
	nullable := func(v *float64) any {
		if v == nil {
			return nil
		}
		return *v
	}
	err = func() error {
		if err := db.Exec(schema); err != nil {
			return err
		}
		if err := db.Exec("BEGIN"); err != nil {
			return err
		}
		run := func(sql string, rows func(ins *sqlitec.Stmt) error) error {
			ins, err := db.Prepare(sql)
			if err != nil {
				return err
			}
			defer ins.Finalize()
			return rows(ins)
		}
		steps := []struct {
			sql  string
			rows func(ins *sqlitec.Stmt) error
		}{
			{"INSERT INTO member VALUES (?, ?, ?, ?, ?)", func(ins *sqlitec.Stmt) error {
				for _, m := range members {
					created := format(m.signup, false, r)
					deleted := ended(m.deleted, false)
					country := countries[pickIdx(len(countries))]
					if _, err := ins.Run(m.id, created, deleted, country, "member"+jsjson.Number(m.id)); err != nil {
						return err
					}
				}
				return nil
			}},
			{"INSERT INTO board VALUES (?, ?, ?, ?, ?)", func(ins *sqlitec.Stmt) error {
				for _, b := range boards {
					created := format(b.created, true, r)
					deleted := ended(b.deleted, true)
					if _, err := ins.Run(b.id, created, deleted, b.members.keys[0], "Board "+jsjson.Number(b.id)); err != nil {
						return err
					}
				}
				return nil
			}},
			{"INSERT INTO board_member VALUES (?, ?, ?, ?)", func(ins *sqlitec.Stmt) error {
				for _, b := range boards {
					for _, mid := range b.members.keys {
						joined := format(b.members.at[mid], true, r)
						var left any
						if l, ok := leftAt[bm{b.id, mid}]; ok {
							left = ended(&l, true)
						}
						if _, err := ins.Run(b.id, mid, joined, left); err != nil {
							return err
						}
					}
				}
				return nil
			}},
			{"INSERT INTO post VALUES (?, ?, ?, ?, ?, ?)", func(ins *sqlitec.Stmt) error {
				for _, p := range posts {
					created := format(p.at, true, r)
					deleted := ended(p.deleted, true)
					if _, err := ins.Run(p.id, p.board, p.member, created, deleted, "Post "+jsjson.Number(p.id)); err != nil {
						return err
					}
				}
				return nil
			}},
			{"INSERT INTO reply VALUES (?, ?, ?, ?, ?, ?, ?)", func(ins *sqlitec.Stmt) error {
				for _, x := range replies {
					created := format(x.at, true, r)
					deleted := ended(x.deleted, true)
					if _, err := ins.Run(x.id, x.post, x.member, nullable(x.parent), created, deleted, "Comment "+jsjson.Number(x.id)); err != nil {
						return err
					}
				}
				return nil
			}},
			{"INSERT INTO reaction VALUES (?, ?, ?, ?)", func(ins *sqlitec.Stmt) error {
				for _, x := range reactions {
					if _, err := ins.Run(x.id, x.post, x.member, format(x.at, true, r)); err != nil {
						return err
					}
				}
				return nil
			}},
		}
		for _, s := range steps {
			if err := run(s.sql, s.rows); err != nil {
				return err
			}
		}
		return db.Exec("COMMIT")
	}()
	if err != nil {
		return Summary{}, err
	}
	bms := 0
	for _, b := range boards {
		bms += len(b.members.keys)
	}
	return Summary{Members: len(members), Boards: len(boards), BoardMembers: bms, Posts: len(posts), Replies: len(replies), Reactions: len(reactions)}, nil
}
