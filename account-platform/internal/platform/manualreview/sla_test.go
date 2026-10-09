package manualreview

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDefaultDeadlines(t *testing.T) {
	created := time.Date(2026, time.August, 24, 1, 0, 0, 0, time.UTC)
	p0, err := DeadlinesFor(created, "P0")
	if err != nil || p0.Claim.Sub(created) != 15*time.Minute || p0.FirstResponse.Sub(created) != 30*time.Minute || p0.Mitigation.Sub(created) != 2*time.Hour || p0.Resolution.Sub(created) != 4*time.Hour {
		t.Fatalf("P0 deadlines = %#v, %v", p0, err)
	}
	p1, err := DeadlinesFor(created, "P1")
	if err != nil || p1.Claim.Sub(created) != 30*time.Minute || p1.FirstResponse.Sub(created) != time.Hour || p1.Mitigation.Sub(created) != 4*time.Hour || p1.Resolution.Sub(created) != 24*time.Hour {
		t.Fatalf("P1 deadlines = %#v, %v", p1, err)
	}
}

func TestP2DeadlinesUseBusinessHours(t *testing.T) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	created := time.Date(2026, time.August, 21, 16, 30, 0, 0, location)
	deadlines, err := DeadlinesFor(created, "P2")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, time.August, 24, 12, 30, 0, 0, location); !deadlines.Claim.Equal(want) {
		t.Fatalf("claim = %s, want %s", deadlines.Claim.In(location), want)
	}
	if want := time.Date(2026, time.August, 25, 16, 30, 0, 0, location); !deadlines.Resolution.Equal(want) {
		t.Fatalf("resolution = %s, want %s", deadlines.Resolution.In(location), want)
	}
}

func TestOnCallScheduleEscalatesAndSaturates(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	schedule, err := ParseOnCallSchedule(first.String()+","+second.String(), true)
	if err != nil {
		t.Fatal(err)
	}
	for level, want := range []uuid.UUID{first, second, second} {
		got, ok := schedule.Next(level)
		if !ok || got != want {
			t.Fatalf("level %d owner = %s, %v; want %s", level, got, ok, want)
		}
	}
	if !RequiresStopLoss("P1", "MITIGATION_OVERDUE") || RequiresStopLoss("P2", "MITIGATION_OVERDUE") {
		t.Fatal("stop-loss classification mismatch")
	}
}
