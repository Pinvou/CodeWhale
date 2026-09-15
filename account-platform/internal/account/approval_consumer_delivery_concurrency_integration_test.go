package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
)

// Concurrent physical deliveries must share one durable attempt ledger. The
// database CAS, not the transport delivery count, owns dispatch permission.
func TestApprovalDeliveryConcurrentAttemptCASIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	initial := observeApprovalDelivery(t, ctx, repository, binding, 41)
	const workers = 8
	type outcome struct {
		state ApprovalDeliveryState
		err   error
	}
	start := make(chan struct{})
	results := make(chan outcome, workers)
	for index := 0; index < workers; index++ {
		go func() {
			<-start
			state, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, initial.Version)
			results <- outcome{state: state, err: err}
		}()
	}
	close(start)
	successes := 0
	want := ApprovalDeliveryState{DomainAttemptCount: 1, State: "DOMAIN_INFLIGHT", Version: initial.Version + 1}
	for index := 0; index < workers; index++ {
		result := <-results
		if result.err == nil {
			successes++
			if result.state != want {
				t.Errorf("successful CAS state=%+v want=%+v", result.state, want)
			}
		} else if !errors.Is(result.err, ErrApprovalConsumerBindingInvalid) {
			t.Errorf("competing CAS returned unexpected error: %v", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent grants=%d want=1", successes)
	}
	// The same event may reappear outside the broker deduplication window with
	// another sequence; it must neither gain another grant nor reset the anchor.
	for _, sequence := range []approvalconsumer.Sequence{42, 99, 100} {
		if got := observeApprovalDelivery(t, ctx, repository, binding, sequence); got != want {
			t.Fatalf("physical redelivery reset state: %+v", got)
		}
	}
	var rows, sequence, attempts, version int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*),min(stream_sequence),min(domain_attempt_count),min(version)
FROM account_approval_authorization.approval_delivery_states WHERE original_event_id=$1 AND durable=$2`, binding.EventID, binding.Durable).
		Scan(&rows, &sequence, &attempts, &version); err != nil || rows != 1 || sequence != 41 || attempts != 1 || version != want.Version {
		t.Fatalf("ledger rows=%d sequence=%d attempts=%d version=%d err=%v", rows, sequence, attempts, version, err)
	}
}
