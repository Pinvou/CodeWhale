package account

import (
	"context"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// receivableWriteOffCrossDomainFacts names the seeded open overage receivable so
// the test can assert the frozen projection, the presenter and the committed
// write-off against the same identifiers.
type receivableWriteOffCrossDomainFacts struct {
	WalletID                uuid.UUID
	ReceivableID            uuid.UUID
	SourceReference         string
	OriginalMicrocredits    int64
	WriteOffMicrocredits    int64
	WalletReceivableCredits int64
}

// newReceivableWriteOffCrossDomainBillingFixture provisions a production-shaped
// Billing database for the cross-domain write-off gate: the database and public
// schema belong to billing_schema_owner, migrations run as billing_migrator with
// MIGRATION_ROLE=billing_service, and the runtime pool runs as the non-superuser
// billing_service identity. Activation stays exactly as deployed: the command
// remains the original INACTIVE v1/s1 row.
func newReceivableWriteOffCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, receivableWriteOffCrossDomainFacts) {
	t.Helper()
	repository, pool, operator := newEntitlementCrossDomainBillingFixture(t, ctx, databaseURL)
	facts := seedReceivableWriteOffCrossDomainEvidence(t, ctx, pool)
	return repository, pool, operator, facts
}

// seedReceivableWriteOffCrossDomainEvidence writes one wallet with an open
// USAGE_OVERAGE receivable of 30 microcredits and the wallet-side receivable
// projection the real settlement path maintains.
func seedReceivableWriteOffCrossDomainEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool) receivableWriteOffCrossDomainFacts {
	t.Helper()
	facts := receivableWriteOffCrossDomainFacts{
		WalletID: uuid.New(), ReceivableID: uuid.New(), OriginalMicrocredits: 30, WriteOffMicrocredits: 30,
	}
	facts.SourceReference = "usage:" + uuid.NewString()
	facts.WalletReceivableCredits = 30
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,receivable_microcredits,financial_status,financial_freeze_reason,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',100,30,'RECEIVABLE_OPEN','USAGE_OVERAGE',2,$3,$3)`, facts.WalletID, uuid.New(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at)
VALUES($1,$2,'USAGE_OVERAGE',$3,$4,$4,'OPEN',$5,$5)`, facts.ReceivableID, facts.WalletID, facts.SourceReference, facts.OriginalMicrocredits, now); err != nil {
		t.Fatal(err)
	}
	return facts
}

// Seed only the controlled historical draft with this owner's actual frozen facts.
func seedReceivableAdvancementRequest(t *testing.T, ctx context.Context, repo *PostgresRepository, user uuid.UUID, policy approvalpolicy.Policy, status string, frozen approvalencoding.LegacyProofInput) uuid.UUID {
	t.Helper()
	id := uuid.New()
	binding, authorizationVersion, _, err := strongPlatformAuthorization(ctx, repo.pool, user, policy.RequesterPermission)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.pool.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,expires_at,submitted_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$9,$6,$10,decode(repeat('00',32),'hex'),'RECEIVABLE',$7,'controlled historical draft','[]','{}',$8,now()+interval '1 hour',CASE WHEN $8='SUBMITTED' THEN now() ELSE NULL END,now(),now())`, id, policy.CommandType, policy.PolicyVersion, user, binding, "historical-"+id.String(), frozen.TargetResourceID, status, authorizationVersion, frozen.NormalizedCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO approval_request_snapshots(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at) VALUES($1,$2,$3,$4,'{}','{}',now())`, id, policy.SnapshotSchemaVersion, frozen.CriticalResourceVersions, frozen.FinancialSnapshot); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedReceivableApprovalActor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role, label string) integrationActor {
	actor := seedEntitlementApprovalActor(t, ctx, pool, role, label)
	return integrationActor{ID: actor.ID, Issuer: "https://entitlement-owned.invalid", Subject: actor.Subject}
}
