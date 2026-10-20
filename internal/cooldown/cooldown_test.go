package cooldown

import (
	"fmt"
	"testing"
	"time"
)

func newTestStore(max time.Duration) (*Store, *time.Time) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := New(max)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestPauseExpires(t *testing.T) {
	s, now := newTestStore(time.Minute)
	if got := s.Pause("k", 5*time.Second); got != 5*time.Second {
		t.Fatalf("Pause = %v, want 5s", got)
	}
	*now = now.Add(2 * time.Second)
	if got := s.Remaining("k"); got != 3*time.Second {
		t.Fatalf("Remaining = %v, want 3s", got)
	}
	*now = now.Add(3 * time.Second)
	if got := s.Remaining("k"); got != 0 {
		t.Fatalf("Remaining after expiry = %v, want 0", got)
	}
	if got := s.Remaining("other"); got != 0 {
		t.Fatalf("unknown key Remaining = %v, want 0", got)
	}
}

func TestPauseIsCappedAndNeverShortened(t *testing.T) {
	s, _ := newTestStore(10 * time.Second)
	if got := s.Pause("k", time.Hour); got != 10*time.Second {
		t.Fatalf("capped Pause = %v, want 10s", got)
	}
	if got := s.Pause("k", time.Second); got != 10*time.Second {
		t.Fatalf("shorter Pause = %v, want the running 10s", got)
	}
}

func TestDisabledStoreIsNoop(t *testing.T) {
	s := New(0)
	if s != nil {
		t.Fatal("New(0) must disable the store")
	}
	if got := s.Pause("k", time.Second); got != 0 {
		t.Fatalf("nil Pause = %v", got)
	}
	if got := s.Remaining("k"); got != 0 {
		t.Fatalf("nil Remaining = %v", got)
	}
}

func TestSweepDropsExpired(t *testing.T) {
	s, now := newTestStore(time.Minute)
	for i := 0; i <= sweepAt; i++ {
		s.Pause(fmt.Sprint(i), time.Second)
	}
	*now = now.Add(2 * time.Second)
	s.Pause("fresh", time.Second)
	if n := len(s.until); n != 1 {
		t.Fatalf("after sweep %d entries remain, want 1", n)
	}
}
