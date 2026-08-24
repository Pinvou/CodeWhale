package token

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestActiveUpstreamCancellationIsSingleUsageScoped(t *testing.T) {
	service := &Service{}
	first, second := uuid.New(), uuid.New()
	firstCancelled, secondCancelled := false, false
	removeFirst := service.registerActiveUpstream(first, func() { firstCancelled = true })
	defer removeFirst()
	removeSecond := service.registerActiveUpstream(second, func() { secondCancelled = true })
	defer removeSecond()

	if !service.abortActiveUpstream(first) {
		t.Fatal("registered usage was not aborted")
	}
	if !firstCancelled || secondCancelled {
		t.Fatalf("cancellation crossed usage boundary: first=%v second=%v", firstCancelled, secondCancelled)
	}
	removeFirst()
	if service.abortActiveUpstream(first) {
		t.Fatal("removed usage cancellation remained addressable")
	}
}

func TestStableEmergencyErrorDoesNotPersistDependencyDetails(t *testing.T) {
	if got := stableEmergencyError(context.DeadlineExceeded); got != "DEADLINE_EXCEEDED" {
		t.Fatalf("deadline classification = %q", got)
	}
	if got := stableEmergencyError(context.Canceled); got != "CONTEXT_CANCELLED" {
		t.Fatalf("cancellation classification = %q", got)
	}
	if got := stableEmergencyError(errors.New("provider secret response")); got != "DEPENDENCY_ERROR" {
		t.Fatalf("dependency detail leaked through classification: %q", got)
	}
}
