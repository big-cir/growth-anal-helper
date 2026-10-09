package query

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"growth-lab/internal/contract"
)

type idKey struct{}

func TestSlotScenarios(t *testing.T) {
	var scenarios []struct {
		Total int `json:"total"`
		Bg    int `json:"bg"`
		Steps []struct {
			Op     []string `json:"op"`
			Events []string `json:"events"`
			Stats  struct {
				InUse           int `json:"inUse"`
				BackgroundInUse int `json:"backgroundInUse"`
				Waiting         int `json:"waiting"`
			} `json:"stats"`
		} `json:"steps"`
	}
	if err := contract.Load("exec-slots.json", &scenarios); err != nil {
		t.Fatal(err)
	}
	for si, sc := range scenarios {
		s := NewExecutionSlots(sc.Total, sc.Bg)
		var mu sync.Mutex
		var events []string
		leases := map[string]*SlotLease{}
		cancels := map[string]context.CancelFunc{}
		var wg sync.WaitGroup
		s.onGrant = func(ctx context.Context) {
			mu.Lock()
			events = append(events, "granted "+ctx.Value(idKey{}).(string))
			mu.Unlock()
		}
		for _, step := range sc.Steps {
			op := step.Op
			switch op[0] {
			case "acquire", "acquire_aborted":
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), idKey{}, op[1]))
				if op[0] == "acquire_aborted" {
					cancel()
				}
				cancels[op[1]] = cancel
				wg.Add(1)
				id := op[1]
				go func() {
					defer wg.Done()
					l, err := s.Acquire(ctx, SlotKind(op[2]))
					mu.Lock()
					if err != nil {
						events = append(events, fmt.Sprintf("rejected %s: %s", id, err.Error()))
					} else {
						leases[id] = l
					}
					mu.Unlock()
				}()
			case "abort":
				cancels[op[1]]()
			case "release":
				mu.Lock()
				l := leases[op[1]]
				mu.Unlock()
				if l != nil {
					l.Release()
				}
			}
			time.Sleep(30 * time.Millisecond)
			mu.Lock()
			got := events
			events = nil
			mu.Unlock()
			st := s.Stats()
			want, _ := json.Marshal(step.Events)
			have, _ := json.Marshal(append([]string{}, got...))
			if string(want) != string(have) || st.InUse != step.Stats.InUse || st.BackgroundInUse != step.Stats.BackgroundInUse || st.Waiting != step.Stats.Waiting {
				t.Errorf("scenario %d op %v: events %s want %s; stats %+v want %+v", si, op, have, want, st, step.Stats)
			}
		}
		for _, c := range cancels {
			c()
		}
		for _, l := range leases {
			l.Release()
		}
	}
	t.Logf("%d scenarios", len(scenarios))
}

func TestLeaseRules(t *testing.T) {
	s := NewExecutionSlots(1, 1)
	forged := &SlotLease{Kind: Interactive}
	if r := RunQuery(Request{Lease: forged, SQL: "SELECT 1"}); r.Kind != "input" || r.Message != "cannot run a query without an execution slot (or two at once on the same slot)" {
		t.Fatal(r)
	}
	l, _ := s.Acquire(context.Background(), Interactive)
	if !l.begin() || l.begin() {
		t.Fatal("a lease runs one query at a time")
	}
	l.Release()
	if !l.Active() || s.Stats().InUse != 1 {
		t.Fatal("release during a query waits for the query")
	}
	l.end()
	if l.Active() || s.Stats().InUse != 0 || l.begin() {
		t.Fatal("released after the query")
	}
}
