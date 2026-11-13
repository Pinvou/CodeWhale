package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMigrationTimeoutBounds(t *testing.T) {
	for _, raw := range []string{"", "0", "0s", "-1s", "24h1ns", "999999999999999999h", "bad"} {
		if _, err := migrationTimeout(raw); err == nil {
			t.Errorf("accepted invalid bounded budget %q", raw)
		}
	}
	for raw, want := range map[string]time.Duration{"1ns": time.Nanosecond, "2m": 2 * time.Minute, "1h30m": 90 * time.Minute, "24h": 24 * time.Hour} {
		if got, err := migrationTimeout(raw); err != nil || got != want {
			t.Errorf("budget %q: got %s, err %v", raw, got, err)
		}
	}
	if got, _ := migrationTimeout(defaultMigrationTimeout); got != 2*time.Minute {
		t.Fatal("legacy default changed")
	}
}

func TestMigrationFailureClassification(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if got := migrationFailureCode(ctx, errors.New("private driver details"), "OLD_CODE"); got != "MIGRATION_DEADLINE_EXCEEDED" {
		t.Fatal(got)
	}
	if got := migrationFailureCode(context.Background(), context.DeadlineExceeded, "OLD_CODE"); got != "MIGRATION_DEADLINE_EXCEEDED" {
		t.Fatal(got)
	}
	if got := migrationFailureCode(context.Background(), errors.New("private driver details"), "OLD_CODE"); got != "OLD_CODE" {
		t.Fatal(got)
	}
}
