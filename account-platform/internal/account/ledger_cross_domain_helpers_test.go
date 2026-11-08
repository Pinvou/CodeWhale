package account

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
	"time"
)

func approvalIntegrationPrincipal(actor integrationActor) identity.Principal {
	return identity.Principal{Issuer: actor.Issuer, Subject: actor.Subject, SessionID: "snapshot-integration-session", AuthenticatedAt: time.Now().UTC()}
}

type integrationActor struct {
	ID      uuid.UUID
	Issuer  string
	Subject string
}

func assertApprovalRequestAbsent(t *testing.T, ctx context.Context, repository *PostgresRepository, requester uuid.UUID, key string) {
	t.Helper()
	var count int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM approval_requests WHERE requester_user_id=$1 AND idempotency_key=$2`, requester, key).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed create persisted approval request: count=%d err=%v", count, err)
	}
}

func assertApprovalRequestState(t *testing.T, ctx context.Context, repository *PostgresRepository, id uuid.UUID, status string, version int64) {
	t.Helper()
	var actualStatus string
	var actualVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT status,version FROM approval_requests WHERE id=$1`, id).Scan(&actualStatus, &actualVersion); err != nil || actualStatus != status || actualVersion != version {
		t.Fatalf("request state changed: %s/%d want %s/%d err=%v", actualStatus, actualVersion, status, version, err)
	}
}

// The Ledger fixture reuses the installed Account role and identity producers.
func seedLedgerApprovalActor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role, label string) integrationActor {
	actor := seedEntitlementApprovalActor(t, ctx, pool, role, label)
	return integrationActor{ID: actor.ID, Issuer: "https://entitlement-owned.invalid", Subject: actor.Subject}
}
