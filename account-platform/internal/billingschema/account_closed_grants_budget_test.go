package billingschema

import (
	"testing"
	"time"
)

func TestClosedGrantChildBudget(t *testing.T) {
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	deadline := start.Add(8 * time.Minute)
	for _, tc := range []struct {
		name         string
		setupElapsed time.Duration
		wantTimeout  time.Duration
	}{
		{"fresh enclosing budget", 0, 415 * time.Second},
		{"full setup budget", 150 * time.Second, 265 * time.Second},
		{"minimum existing root budget", 325 * time.Second, 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := start.Add(tc.setupElapsed)
			commandDeadline, timeout, err := closedGrantChildBudget(now, deadline, true)
			if err != nil {
				t.Fatal(err)
			}
			if timeout != tc.wantTimeout {
				t.Fatalf("child timeout: got %s want %s", timeout, tc.wantTimeout)
			}
			if deadline.Sub(commandDeadline) != 50*time.Second || commandDeadline.Sub(now.Add(timeout)) != 15*time.Second {
				t.Fatal("cleanup or child exit reserve was consumed")
			}
			if deadline.Sub(commandDeadline.Add(closedGrantWaitDelay)) != 45*time.Second {
				t.Fatal("command wait delay consumed fixture cleanup time")
			}
		})
	}
}

func TestClosedGrantChildBudgetRejectsUnboundedOrInsufficientTime(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		deadline    time.Time
		hasDeadline bool
	}{
		{"no Go test timeout", time.Time{}, false},
		{"missing deadline", time.Time{}, true},
		{"expired deadline", now.Add(-time.Second), true},
		{"cleanup reserve only", now.Add(45 * time.Second), true},
		{"one nanosecond below root minimum", now.Add(155*time.Second - time.Nanosecond), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commandDeadline, timeout, err := closedGrantChildBudget(now, tc.deadline, tc.hasDeadline)
			if err == nil || !commandDeadline.IsZero() || timeout != 0 {
				t.Fatal("unbounded or insufficient budget was accepted")
			}
		})
	}
}
