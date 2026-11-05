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
		minimum      time.Duration
	}{
		{"fresh owner group", 0, 415 * time.Second, 150 * time.Second},
		{"full setup owner group", 150 * time.Second, 265 * time.Second, 150 * time.Second},
		{"full setup financial group", 150 * time.Second, 265 * time.Second, 240 * time.Second},
		{"full setup membership group", 150 * time.Second, 265 * time.Second, 180 * time.Second},
		{"minimum financial group", 175 * time.Second, 240 * time.Second, 240 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := start.Add(tc.setupElapsed)
			commandDeadline, timeout, err := closedGrantChildBudget(now, deadline, true, tc.minimum)
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
		{"one nanosecond below group minimum", now.Add(305*time.Second - time.Nanosecond), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commandDeadline, timeout, err := closedGrantChildBudget(now, tc.deadline, tc.hasDeadline, 240*time.Second)
			if err == nil || !commandDeadline.IsZero() || timeout != 0 {
				t.Fatal("unbounded or insufficient budget was accepted")
			}
		})
	}
}

func TestClosedGrantChildBudgetRejectsSingleRootAllowanceForCompleteGroups(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	// The old admission accepted a 90s child even though the financial group's
	// observed root total was ~190s. The process/cleanup reserves still fit, but
	// admitting this incomplete execution window must fail before any child runs.
	deadline := now.Add(90*time.Second + closedGrantCleanupReserve + closedGrantWaitDelay + closedGrantExitGrace)
	for _, minimum := range []time.Duration{150 * time.Second, 240 * time.Second, 180 * time.Second, 0, -time.Second} {
		commandDeadline, timeout, err := closedGrantChildBudget(now, deadline, true, minimum)
		if err == nil || !commandDeadline.IsZero() || timeout != 0 {
			t.Fatal("single-root or invalid group allowance was accepted")
		}
	}
}
