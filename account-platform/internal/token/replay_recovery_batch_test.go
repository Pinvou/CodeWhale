package token

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRecoverDispatchCandidatesContinuesAfterFailure(t *testing.T) {
	first := errors.New("controlled first failure")
	last := errors.New("controlled later failure")
	candidates := []uncertainDispatchCandidate{
		{id: uuid.New(), owner: "first", until: time.Unix(100, 0).UTC()},
		{id: uuid.New(), owner: "healthy", until: time.Unix(200, 0).UTC()},
		{id: uuid.New(), owner: "last", until: time.Unix(300, 0).UTC()},
	}
	var observed []uncertainDispatchCandidate
	err := recoverDispatchCandidates(context.Background(), candidates, func(_ context.Context, id uuid.UUID, owner string, until time.Time) error {
		observed = append(observed, uncertainDispatchCandidate{id: id, owner: owner, until: until})
		switch owner {
		case "first":
			return first
		case "last":
			return last
		default:
			return nil
		}
	})
	if !reflect.DeepEqual(observed, candidates) {
		t.Fatal("candidate identity or lease changed, or a healthy row was starved", observed)
	}
	if !errors.Is(err, first) || !strings.Contains(err.Error(), "2 uncertain dispatch recoveries failed") {
		t.Fatal("batch must report failures while keeping the first cause", err)
	}
}

func TestRecoverDispatchCandidatesAllHealthyOrEmpty(t *testing.T) {
	for _, count := range []int{0, 2} {
		candidates := make([]uncertainDispatchCandidate, count)
		calls := 0
		err := recoverDispatchCandidates(context.Background(), candidates, func(context.Context, uuid.UUID, string, time.Time) error {
			calls++
			return nil
		})
		if err != nil || calls != count {
			t.Fatalf("count=%d calls=%d err=%v", count, calls, err)
		}
	}
}

func TestRecoverDispatchCandidatesStopsOnCancellationAndKeepsCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := errors.New("controlled candidate failure")
	calls := 0
	err := recoverDispatchCandidates(ctx, make([]uncertainDispatchCandidate, 3), func(context.Context, uuid.UUID, string, time.Time) error {
		calls++
		cancel()
		return first
	})
	if calls != 1 || !errors.Is(err, first) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled batch continued or lost its failure: calls=%d err=%v", calls, err)
	}
}

func TestRecoverDispatchCandidatesInitiallyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := recoverDispatchCandidates(ctx, make([]uncertainDispatchCandidate, 1), func(context.Context, uuid.UUID, string, time.Time) error {
		t.Fatal("canceled batch must not start a transaction")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("missing cancellation", err)
	}
}
