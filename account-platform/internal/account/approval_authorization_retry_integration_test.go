package account

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestApprovalAuthorizationCommitAbortRetriesWholeTransactionIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	// This isolated fixture's nontransactional sequence observes attempts even
	// when the deferred trigger aborts COMMIT. No production fault hook is added.
	if _, err := db.adminPool.Exec(ctx, `CREATE SEQUENCE public.authorization_retry_test_attempts;
CREATE FUNCTION public.authorization_retry_test_commit_fault() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE attempt BIGINT;
BEGIN
  IF (SELECT count(*) FROM public.approval_decisions WHERE approval_request_id=NEW.id)<>1
     OR (SELECT count(*) FROM public.approval_state_events WHERE approval_request_id=NEW.id AND to_status='APPROVED')<>1
     OR (SELECT count(*) FROM public.outbox_events WHERE aggregate_id=NEW.id AND event_type='account.approval-command.authorized.v1')<>1
     OR (SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes s JOIN public.outbox_events o USING(event_id) WHERE o.aggregate_id=NEW.id)<>1 THEN
    RAISE EXCEPTION 'test transaction did not contain all approval facts' USING ERRCODE='23514';
  END IF;
  attempt:=nextval('public.authorization_retry_test_attempts');
  IF attempt<=TG_ARGV[0]::bigint THEN
    RAISE EXCEPTION 'test confirmed commit abort' USING ERRCODE=TG_ARGV[1];
  END IF;
  RETURN NEW;
END $$;`); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"40001", "40P01"} {
		for failures := 1; failures <= 3; failures++ {
			t.Run(fmt.Sprintf("%s_failures_%d", code, failures), func(t *testing.T) {
				request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
				approver := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "retry-"+uuid.NewString())
				if _, err := db.adminPool.Exec(ctx, fmt.Sprintf(`ALTER SEQUENCE public.authorization_retry_test_attempts RESTART WITH 1;
CREATE CONSTRAINT TRIGGER authorization_retry_test_fault AFTER UPDATE ON public.approval_requests
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.status='APPROVED')
EXECUTE FUNCTION public.authorization_retry_test_commit_fault('%d','%s');`, failures, code)); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := db.adminPool.Exec(ctx, `DROP TRIGGER authorization_retry_test_fault ON public.approval_requests`); err != nil {
						t.Error(err)
					}
				}()
				repository := (&PostgresRepository{pool: db.runtimePool, approvalWriterPool: db.writerPool, now: func() time.Time { return time.Now().UTC() }}).WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")})
				result, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(approver), request.ID, request.Version, "APPROVED", "retry fault validation", request.CriticalResourceVersions)
				if failures < 3 && (err != nil || result.Status != "APPROVED") {
					t.Fatalf("confirmed abort was not retried: %v", err)
				}
				if failures == 3 && !errors.Is(err, ErrApprovalWriterUnavailable) {
					t.Fatalf("retry exhaustion did not fail closed: %v", err)
				}
				var status string
				var version, decisions, states, outbox, sidecars, attempts int64
				if err := db.adminPool.QueryRow(ctx, `SELECT status,version,
(SELECT count(*) FROM public.approval_decisions WHERE approval_request_id=$1),
(SELECT count(*) FROM public.approval_state_events WHERE approval_request_id=$1 AND to_status='APPROVED'),
(SELECT count(*) FROM public.outbox_events WHERE aggregate_id=$1),
(SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes s JOIN public.outbox_events o USING(event_id) WHERE o.aggregate_id=$1),
(SELECT last_value FROM public.authorization_retry_test_attempts)
FROM public.approval_requests WHERE id=$1`, request.ID).Scan(&status, &version, &decisions, &states, &outbox, &sidecars, &attempts); err != nil {
					t.Fatal(err)
				}
				expectedFacts, expectedAttempts := int64(1), int64(failures+1)
				if failures == 3 {
					expectedFacts, expectedAttempts = 0, 3
				}
				if decisions != expectedFacts || states != expectedFacts || outbox != expectedFacts || sidecars != expectedFacts || attempts != expectedAttempts || version != request.Version+expectedFacts {
					t.Fatalf("transaction retry leaked/duplicated facts: status=%s version=%d decisions=%d states=%d outbox=%d sidecars=%d attempts=%d", status, version, decisions, states, outbox, sidecars, attempts)
				}
				if failures == 3 && status != "SUBMITTED" {
					t.Fatal("exhausted retry changed original request")
				}
			})
		}
	}
}
