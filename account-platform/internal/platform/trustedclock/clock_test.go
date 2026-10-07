package trustedclock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMonitorUsesMedianAndEnforcesAlertAndReadinessThresholds(t *testing.T) {
	if _, err := New([]string{"one", "two"}); err == nil {
		t.Fatal("fewer than three sources were accepted")
	}
	monitor, err := New([]string{"one", "two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]time.Duration{"one": 90 * time.Millisecond, "two": 260 * time.Millisecond, "three": 2 * time.Second}
	monitor.sample = func(_ context.Context, source string) (time.Duration, error) { return values[source], nil }
	if err := monitor.Check(context.Background()); err != nil {
		t.Fatalf("250ms alert must not remove readiness: %v", err)
	}
	monitor.checkedAt = time.Time{}
	values = map[string]time.Duration{"one": 1100 * time.Millisecond, "two": 1200 * time.Millisecond, "three": 1300 * time.Millisecond}
	if err := monitor.Check(context.Background()); !errors.Is(err, ErrClockUnsafe) {
		t.Fatalf("offset beyond one second must fail readiness: %v", err)
	}
}

func TestMonitorFailsClosedWithoutThreeResponses(t *testing.T) {
	monitor, _ := New([]string{"one", "two", "three"})
	monitor.sample = func(_ context.Context, source string) (time.Duration, error) {
		if source == "three" {
			return 0, errors.New("offline")
		}
		return 0, nil
	}
	if err := monitor.Check(context.Background()); err == nil {
		t.Fatal("monitor accepted fewer than three responses")
	}
}

func TestMonitorDoesNotReuseSafeSampleAfterClockMovesBackwards(t *testing.T) {
	monitor, err := New([]string{"one", "two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return now }
	monitor.sample = func(context.Context, string) (time.Duration, error) { return 0, nil }
	if err := monitor.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	monitor.sample = func(context.Context, string) (time.Duration, error) { return 2 * time.Second, nil }
	now = now.Add(10 * time.Second)
	if err := monitor.Check(context.Background()); err != nil {
		t.Fatalf("unexpired sample should remain cached: %v", err)
	}
	now = now.Add(-11 * time.Second)
	if err := monitor.Check(context.Background()); !errors.Is(err, ErrClockUnsafe) {
		t.Fatalf("clock rollback must resample and detect unsafe offset: %v", err)
	}
}
