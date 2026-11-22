package billing

import (
	"testing"
	"time"
)

func TestP2ReviewDeadlinesUseShanghaiBusinessHoursWithoutReset(t *testing.T) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	created := time.Date(2026, time.August, 21, 15, 0, 0, 0, location) // Friday.
	claim, response, mitigation, resolution := p2ReviewDeadlines(created)
	assertShanghaiTime(t, claim, 2026, time.August, 24, 11)
	assertShanghaiTime(t, response, 2026, time.August, 24, 15)
	assertShanghaiTime(t, mitigation, 2026, time.August, 24, 15)
	assertShanghaiTime(t, resolution, 2026, time.August, 25, 15)
}

func TestBusinessHoursDoNotCountTimeAfterClosing(t *testing.T) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	created := time.Date(2026, time.August, 21, 16, 30, 0, 0, location) // Friday.
	deadline := addBusinessHours(created, 1)
	want := time.Date(2026, time.August, 24, 9, 30, 0, 0, location)
	if !deadline.Equal(want) {
		t.Fatalf("deadline = %s, want %s", deadline.In(location), want)
	}
}

func TestLatePaymentReviewCadenceChangesAfterSevenDays(t *testing.T) {
	created := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	withinFirstWeek := created.Add(6 * 24 * time.Hour)
	if got := nextLatePaymentReviewAt(created, withinFirstWeek); !got.Equal(withinFirstWeek.Add(24 * time.Hour)) {
		t.Fatalf("first-week cadence = %s", got)
	}
	afterFirstWeek := created.Add(8 * 24 * time.Hour)
	if got := nextLatePaymentReviewAt(created, afterFirstWeek); !got.Equal(afterFirstWeek.Add(7 * 24 * time.Hour)) {
		t.Fatalf("later cadence = %s", got)
	}
}

func assertShanghaiTime(t *testing.T, value time.Time, year int, month time.Month, day, hour int) {
	t.Helper()
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	want := time.Date(year, month, day, hour, 0, 0, 0, location)
	if !value.Equal(want) {
		t.Fatalf("deadline = %s, want %s", value.In(location), want)
	}
}
