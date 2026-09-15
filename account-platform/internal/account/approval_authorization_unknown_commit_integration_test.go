package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type approvalAuthorizationCommittedFacts struct {
	eventID              uuid.UUID
	requestVersion       int64
	eventDigest          string
	serialization        string
	approvalEpoch        int64
	decisionID           uuid.UUID
	approvalStateEventID uuid.UUID
}

func TestApprovalAuthorizationUnknownCommitReadbackIntegration(t *testing.T) {
	t.Run("approved survives later execution advancement", func(t *testing.T) {
		database := newApprovalAuthorizationIntegrationDatabase(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		request := seedSubmittedApprovalForAuthorization(t, database, "WALLET_ADJUSTMENT")
		approver := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_SUPER_ADMIN", "unknown-approved-"+uuid.NewString())
		commitCalls := 0
		repository := approvalAuthorizationTestRepository(database)
		repository.approvalWriterCommit = func(commitContext context.Context, tx pgx.Tx) error {
			commitCalls++
			if err := tx.Commit(commitContext); err != nil {
				return err
			}
			advanceContext, advanceCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer advanceCancel()
			if _, err := database.adminPool.Exec(advanceContext, `UPDATE public.approval_requests
				SET status='EXECUTING',version=version+1,updated_at=transaction_timestamp()
				WHERE id=$1 AND status='APPROVED'`, request.ID); err != nil {
				return err
			}
			return errors.New("simulated transport failure after server commit")
		}

		current, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(approver), request.ID, request.Version, "APPROVED", "unknown committed approval", request.CriticalResourceVersions)
		if err != nil {
			t.Fatal(err)
		}
		if commitCalls != 1 || current.Status != "EXECUTING" || current.Version != request.Version+2 {
			t.Fatalf("unknown approval outcome was not recognized exactly once: calls=%d status=%s version=%d", commitCalls, current.Status, current.Version)
		}

		facts := readApprovalAuthorizationCommittedFacts(t, ctx, database, request.ID)
		if !approvalAuthorizationOutcome(t, ctx, database, facts, request.ID) {
			t.Fatal("exact immutable approval facts were not recognized after request advancement")
		}
		wrongDigest := "0" + facts.eventDigest[1:]
		if wrongDigest == facts.eventDigest {
			wrongDigest = "1" + facts.eventDigest[1:]
		}
		for _, test := range []struct {
			name          string
			eventID       uuid.UUID
			requestID     uuid.UUID
			version       int64
			digest        string
			serialization string
			epoch         int64
			decisionID    uuid.UUID
			stateEventID  uuid.UUID
		}{
			{name: "event id", eventID: uuid.New(), requestID: request.ID, version: facts.requestVersion, digest: facts.eventDigest, serialization: facts.serialization, epoch: facts.approvalEpoch, decisionID: facts.decisionID, stateEventID: facts.approvalStateEventID},
			{name: "request id", eventID: facts.eventID, requestID: uuid.New(), version: facts.requestVersion, digest: facts.eventDigest, serialization: facts.serialization, epoch: facts.approvalEpoch, decisionID: facts.decisionID, stateEventID: facts.approvalStateEventID},
			{name: "approved version", eventID: facts.eventID, requestID: request.ID, version: facts.requestVersion + 1, digest: facts.eventDigest, serialization: facts.serialization, epoch: facts.approvalEpoch, decisionID: facts.decisionID, stateEventID: facts.approvalStateEventID},
			{name: "digest", eventID: facts.eventID, requestID: request.ID, version: facts.requestVersion, digest: wrongDigest, serialization: facts.serialization, epoch: facts.approvalEpoch, decisionID: facts.decisionID, stateEventID: facts.approvalStateEventID},
			{name: "serialization", eventID: facts.eventID, requestID: request.ID, version: facts.requestVersion, digest: facts.eventDigest, serialization: "APPROVAL_RFC8785_DECIMAL_V1", epoch: facts.approvalEpoch, decisionID: facts.decisionID, stateEventID: facts.approvalStateEventID},
			{name: "epoch", eventID: facts.eventID, requestID: request.ID, version: facts.requestVersion, digest: facts.eventDigest, serialization: facts.serialization, epoch: facts.approvalEpoch + 1, decisionID: facts.decisionID, stateEventID: facts.approvalStateEventID},
			{name: "decision id", eventID: facts.eventID, requestID: request.ID, version: facts.requestVersion, digest: facts.eventDigest, serialization: facts.serialization, epoch: facts.approvalEpoch, decisionID: uuid.New(), stateEventID: facts.approvalStateEventID},
			{name: "state event id", eventID: facts.eventID, requestID: request.ID, version: facts.requestVersion, digest: facts.eventDigest, serialization: facts.serialization, epoch: facts.approvalEpoch, decisionID: facts.decisionID, stateEventID: uuid.New()},
		} {
			t.Run("wrong "+test.name, func(t *testing.T) {
				var found bool
				if err := database.writerPool.QueryRow(ctx,
					`SELECT account_approval_authorization.authorization_commit_outcome($1,$2,$3,$4,$5,$6,$7,$8)`,
					test.eventID, test.requestID, test.version, test.digest, test.serialization, test.epoch, test.decisionID, test.stateEventID,
				).Scan(&found); err != nil {
					t.Fatal(err)
				}
				if found {
					t.Fatalf("outcome accepted wrong %s", test.name)
				}
			})
		}
	})

	t.Run("conflict survives later request advancement", func(t *testing.T) {
		database := newApprovalAuthorizationIntegrationDatabase(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		request := seedSubmittedApprovalForAuthorization(t, database, "WALLET_ADJUSTMENT")
		approver := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_SUPER_ADMIN", "unknown-conflict-"+uuid.NewString())
		commitCalls := 0
		repository := approvalAuthorizationTestRepository(database)
		repository.approvalWriterCommit = func(commitContext context.Context, tx pgx.Tx) error {
			commitCalls++
			if err := tx.Commit(commitContext); err != nil {
				return err
			}
			advanceContext, advanceCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer advanceCancel()
			if _, err := database.adminPool.Exec(advanceContext, `UPDATE public.approval_requests
				SET status='REJECTED',version=version+1,updated_at=transaction_timestamp()
				WHERE id=$1 AND status='SUBMITTED'`, request.ID); err != nil {
				return err
			}
			return errors.New("simulated transport failure after terminal event commit")
		}

		_, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(approver), request.ID, request.Version+1, "APPROVED", "stale approval conflict", request.CriticalResourceVersions)
		if !errors.Is(err, ErrConflict) || commitCalls != 1 {
			t.Fatalf("terminal unknown outcome mismatch: calls=%d err=%v", commitCalls, err)
		}
		current, readErr := approvalRequestByID(ctx, database.adminPool, request.ID)
		if readErr != nil || current.Status != "REJECTED" || current.Version != request.Version+1 {
			t.Fatalf("later request state was not preserved: status=%s version=%d err=%v", current.Status, current.Version, readErr)
		}
	})

	for _, test := range []struct {
		name               string
		commitThenFailRead bool
		wantCommittedFacts bool
	}{
		{name: "definite rollback returns unavailable without retry"},
		{name: "readback failure after commit returns unavailable without retry", commitThenFailRead: true, wantCommittedFacts: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := newApprovalAuthorizationIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			request := seedSubmittedApprovalForAuthorization(t, database, "WALLET_ADJUSTMENT")
			approver := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_SUPER_ADMIN", "unknown-failure-"+uuid.NewString())
			commitCalls := 0
			repository := approvalAuthorizationTestRepository(database)
			repository.approvalWriterCommit = func(commitContext context.Context, tx pgx.Tx) error {
				commitCalls++
				if test.commitThenFailRead {
					if err := tx.Commit(commitContext); err != nil {
						return err
					}
					database.writerPool.Close()
				} else if err := tx.Rollback(commitContext); err != nil {
					return err
				}
				return errors.New("simulated transport failure with unprovable outcome")
			}

			_, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(approver), request.ID, request.Version, "APPROVED", "unknown approval failure", request.CriticalResourceVersions)
			if !errors.Is(err, ErrApprovalWriterUnavailable) || commitCalls != 1 {
				t.Fatalf("unknown failure mismatch: calls=%d err=%v", commitCalls, err)
			}
			facts := countApprovalAuthorizationFacts(t, ctx, database, request.ID)
			if test.wantCommittedFacts && facts != 5 {
				t.Fatalf("server commit facts were lost after readback failure: %d", facts)
			}
			if !test.wantCommittedFacts && facts != 0 {
				t.Fatalf("rolled-back unknown attempt persisted facts: %d", facts)
			}
		})
	}
}

func approvalAuthorizationTestRepository(database *approvalAuthorizationIntegrationDatabase) *PostgresRepository {
	return (&PostgresRepository{
		pool:               database.runtimePool,
		approvalWriterPool: database.writerPool,
		now:                func() time.Time { return time.Now().UTC() },
	}).WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")})
}

func readApprovalAuthorizationCommittedFacts(t *testing.T, ctx context.Context, database *approvalAuthorizationIntegrationDatabase, requestID uuid.UUID) approvalAuthorizationCommittedFacts {
	t.Helper()
	var facts approvalAuthorizationCommittedFacts
	if err := database.adminPool.QueryRow(ctx, `SELECT o.event_id,o.aggregate_version,s.event_digest,s.serialization_version,
		o.approval_control_epoch,d.id,e.id
		FROM public.outbox_events o
		JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
		JOIN public.approval_decisions d ON d.approval_request_id=o.aggregate_id AND d.decision='APPROVED'
		JOIN public.approval_state_events e ON e.approval_request_id=o.aggregate_id AND e.to_status='APPROVED'
		WHERE o.aggregate_id=$1 AND o.event_type='account.approval-command.authorized.v1'`, requestID).Scan(
		&facts.eventID, &facts.requestVersion, &facts.eventDigest, &facts.serialization,
		&facts.approvalEpoch, &facts.decisionID, &facts.approvalStateEventID,
	); err != nil {
		t.Fatal(err)
	}
	return facts
}

func approvalAuthorizationOutcome(t *testing.T, ctx context.Context, database *approvalAuthorizationIntegrationDatabase, facts approvalAuthorizationCommittedFacts, requestID uuid.UUID) bool {
	t.Helper()
	var found bool
	if err := database.writerPool.QueryRow(ctx,
		`SELECT account_approval_authorization.authorization_commit_outcome($1,$2,$3,$4,$5,$6,$7,$8)`,
		facts.eventID, requestID, facts.requestVersion, facts.eventDigest, facts.serialization,
		facts.approvalEpoch, facts.decisionID, facts.approvalStateEventID,
	).Scan(&found); err != nil {
		t.Fatal(err)
	}
	return found
}

func countApprovalAuthorizationFacts(t *testing.T, ctx context.Context, database *approvalAuthorizationIntegrationDatabase, requestID uuid.UUID) int64 {
	t.Helper()
	var count int64
	if err := database.adminPool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM public.approval_requests WHERE id=$1 AND status IN ('APPROVED','EXECUTING','EXECUTED','EXECUTION_FAILED'))+
		(SELECT count(*) FROM public.approval_decisions WHERE approval_request_id=$1)+
		(SELECT count(*) FROM public.approval_state_events WHERE approval_request_id=$1 AND to_status='APPROVED')+
		(SELECT count(*) FROM public.outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1')+
		(SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes s JOIN public.outbox_events o USING(event_id) WHERE o.aggregate_id=$1)+
		(SELECT count(*) FROM account_approval_authorization.authorization_event_pending p JOIN public.outbox_events o USING(event_id) WHERE o.aggregate_id=$1)`, requestID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
