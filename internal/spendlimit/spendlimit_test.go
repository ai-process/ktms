package spendlimit

import (
	"testing"
	"time"
)

func TestReason(t *testing.T) {
	cases := map[[2]string]string{
		{SubjectKey, PeriodDay}:    "spend_key_daily",
		{SubjectKey, PeriodMonth}:  "spend_key_monthly",
		{SubjectUser, PeriodDay}:   "spend_user_daily",
		{SubjectUser, PeriodMonth}: "spend_user_monthly",
	}
	for in, want := range cases {
		if got := Reason(in[0], in[1]); got != want {
			t.Errorf("Reason(%s, %s) = %q, want %q", in[0], in[1], got, want)
		}
		if !IsReason(want) {
			t.Errorf("IsReason(%q) = false", want)
		}
	}
	for _, other := range []string{"rpm", "budget_key", ReasonUnavailable, ""} {
		if IsReason(other) {
			t.Errorf("IsReason(%q) = true", other)
		}
	}
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 12, 31, 23, 59, 30, 0, time.UTC)

	id, end := Window(PeriodDay, now)
	if id != "d20261231" || !end.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day window = %s ending %s", id, end)
	}
	id, end = Window(PeriodMonth, now)
	if id != "m202612" || !end.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month window = %s ending %s", id, end)
	}

	// Non-UTC input lands in the UTC window.
	tokyo := time.FixedZone("JST", 9*3600)
	id, _ = Window(PeriodDay, time.Date(2027, 1, 1, 8, 0, 0, 0, tokyo))
	if id != "d20261231" {
		t.Fatalf("08:00 JST on Jan 1 is Dec 31 UTC, got %s", id)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 15, 23, 0, 0, 500_000_000, time.UTC)

	d, ok := RetryAfter("spend_key_daily", now)
	if !ok || d != time.Hour {
		t.Fatalf("daily retry = %s ok=%v, want 1h (rounded up to whole seconds)", d, ok)
	}
	d, ok = RetryAfter("spend_user_monthly", now)
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Sub(now).Round(time.Second)
	if !ok || d != want {
		t.Fatalf("monthly retry = %s, want %s", d, want)
	}
	if _, ok := RetryAfter("budget_key", now); ok {
		t.Fatal("token budgets carry no period reset")
	}
	// Exactly at the boundary the next window starts: a full period away, never 0.
	d, _ = RetryAfter("spend_key_daily", time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC))
	if d != 24*time.Hour {
		t.Fatalf("retry at midnight = %s, want 24h", d)
	}
}
