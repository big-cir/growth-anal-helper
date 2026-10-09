package query

import (
	"context"
	"errors"
	"sync"
)

// SlotKind is interactive (user requests) or background.
type SlotKind string

const (
	Interactive SlotKind = "interactive"
	Background  SlotKind = "background"
)

// ErrSlotCancelled is returned when waiting for a slot is cancelled.
var ErrSlotCancelled = errors.New("cancelled")

// SlotLease is a held execution slot; it runs one query at a time. The zero value is not a lease.
type SlotLease struct {
	Kind             SlotKind
	mu               sync.Mutex
	active           bool
	busy             bool
	releaseRequested bool
	onRelease        func()
}

// Active reports whether the lease still holds its slot.
func (l *SlotLease) Active() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// Release gives the slot back; if a query is running, when it ends.
func (l *SlotLease) Release() {
	l.mu.Lock()
	if !l.active {
		l.mu.Unlock()
		return
	}
	if l.busy {
		l.releaseRequested = true
		l.mu.Unlock()
		return
	}
	l.active = false
	f := l.onRelease
	l.mu.Unlock()
	f()
}

func (l *SlotLease) begin() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active || l.busy || l.releaseRequested || l.onRelease == nil {
		return false
	}
	l.busy = true
	return true
}

func (l *SlotLease) end() {
	l.mu.Lock()
	l.busy = false
	again := l.releaseRequested
	l.releaseRequested = false
	l.mu.Unlock()
	if again {
		l.Release()
	}
}

type waiter struct {
	kind  SlotKind
	grant chan *SlotLease
	ctx   context.Context
}

// ExecutionSlots are shared by agent calls and queries. Interactive requests first; at most backgroundMax background jobs.
type ExecutionSlots struct {
	mu            sync.Mutex
	total, bgMax  int
	inUse, bgUsed int
	waiting       []*waiter
	onGrant       func(context.Context)
}

// NewExecutionSlots makes total slots, of which at most backgroundMax background.
func NewExecutionSlots(total, backgroundMax int) *ExecutionSlots {
	return &ExecutionSlots{total: total, bgMax: backgroundMax}
}

// SlotStats are the counts in use and waiting.
type SlotStats struct{ InUse, BackgroundInUse, Waiting int }

// Stats returns the current counts.
func (s *ExecutionSlots) Stats() SlotStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SlotStats{s.inUse, s.bgUsed, len(s.waiting)}
}

// Acquire waits for a slot; ctx cancels the wait.
func (s *ExecutionSlots) Acquire(ctx context.Context, kind SlotKind) (*SlotLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, ErrSlotCancelled
	}
	w := &waiter{kind: kind, grant: make(chan *SlotLease, 1), ctx: ctx}
	s.mu.Lock()
	s.waiting = append(s.waiting, w)
	s.pump()
	s.mu.Unlock()
	select {
	case l := <-w.grant:
		return l, nil
	case <-ctx.Done():
		s.mu.Lock()
		for i, x := range s.waiting {
			if x == w {
				s.waiting = append(s.waiting[:i], s.waiting[i+1:]...)
				s.mu.Unlock()
				return nil, ErrSlotCancelled
			}
		}
		s.mu.Unlock()
		// granted at the same moment: keep the order of events, then give it back
		l := <-w.grant
		l.Release()
		return nil, ErrSlotCancelled
	}
}

// Run holds a slot for fn and releases it afterwards.
func (s *ExecutionSlots) Run(ctx context.Context, kind SlotKind, fn func(*SlotLease) error) error {
	l, err := s.Acquire(ctx, kind)
	if err != nil {
		return err
	}
	defer l.Release()
	return fn(l)
}

// pump grants waiting requests; called with the lock held.
func (s *ExecutionSlots) pump() {
	for s.inUse < s.total {
		i := -1
		for j, w := range s.waiting {
			if w.kind == Interactive {
				i = j
				break
			}
		}
		if i < 0 && s.bgUsed < s.bgMax {
			for j, w := range s.waiting {
				if w.kind == Background {
					i = j
					break
				}
			}
		}
		if i < 0 {
			return
		}
		w := s.waiting[i]
		s.waiting = append(s.waiting[:i], s.waiting[i+1:]...)
		s.inUse++
		if w.kind == Background {
			s.bgUsed++
		}
		if s.onGrant != nil {
			s.onGrant(w.ctx)
		}
		kind := w.kind
		w.grant <- &SlotLease{Kind: kind, active: true, onRelease: func() {
			s.mu.Lock()
			s.inUse--
			if kind == Background {
				s.bgUsed--
			}
			s.pump()
			s.mu.Unlock()
		}}
	}
}
