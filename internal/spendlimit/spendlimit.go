// Package spendlimit names the monetary limit subjects, periods and refusal
// reasons, and computes the UTC calendar windows they reset on. It is a leaf
// package so the throttle, the router and the HTTP layer agree on one contract.
package spendlimit

import (
	"strings"
	"time"
)

// Subject types a limit can apply to.
const (
	SubjectKey  = "key"  // api key name
	SubjectUser = "user" // router.UserID: attributes["user_id"] or svc:<key_name>
	SubjectTeam = "team" // reserved in the schema; not enforced yet
)

// Periods are UTC calendar windows.
const (
	PeriodDay   = "day"
	PeriodMonth = "month"
)

// Wildcard as a subject id is the default limit for every subject of a type.
const Wildcard = "*"

// ReasonUnavailable is reported when the counters cannot be read and
// SPEND_LIMITS_FAIL_MODE=closed.
const ReasonUnavailable = "spend_unavailable"

const reasonPrefix = "spend_"

// Enforced lists the (subject, period) pairs checked on every call, in the
// order a refusal names them.
var Enforced = []struct{ Subject, Period string }{
	{SubjectKey, PeriodDay},
	{SubjectKey, PeriodMonth},
	{SubjectUser, PeriodDay},
	{SubjectUser, PeriodMonth},
}

// ValidSubject reports a subject type that is enforced.
func ValidSubject(s string) bool { return s == SubjectKey || s == SubjectUser }

// ValidPeriod reports a known period.
func ValidPeriod(p string) bool { return p == PeriodDay || p == PeriodMonth }

// Reason is the throttle kind for a tripped limit, e.g. "spend_key_daily".
func Reason(subject, period string) string {
	adj := "daily"
	if period == PeriodMonth {
		adj = "monthly"
	}
	return reasonPrefix + subject + "_" + adj
}

// IsReason reports a refusal caused by an exhausted spend limit.
func IsReason(kind string) bool {
	return strings.HasPrefix(kind, reasonPrefix) && kind != ReasonUnavailable
}

// Window returns the id of the period containing now and the instant it ends.
func Window(period string, now time.Time) (id string, end time.Time) {
	now = now.UTC()
	if period == PeriodMonth {
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return "m" + now.Format("200601"), start.AddDate(0, 1, 0)
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return "d" + now.Format("20060102"), start.AddDate(0, 0, 1)
}

// RetryAfter is the time until the period behind a spend refusal resets,
// rounded up to whole seconds. ok is false for any other kind.
func RetryAfter(kind string, now time.Time) (d time.Duration, ok bool) {
	if !IsReason(kind) {
		return 0, false
	}
	period := PeriodDay
	if strings.HasSuffix(kind, "_monthly") {
		period = PeriodMonth
	}
	_, end := Window(period, now)
	d = end.Sub(now.UTC())
	if rem := d % time.Second; rem != 0 {
		d += time.Second - rem
	}
	if d < time.Second {
		d = time.Second
	}
	return d, true
}
