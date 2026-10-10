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

// ledgerRepairCrossDomainFacts names the seeded attribution error so the test
// can assert the frozen projection, the presenter and the committed repair
// against the same identifiers.
type ledgerRepairCrossDomainFacts struct {
	WalletID           uuid.UUID
	JournalEntryID     uuid.UUID
	TargetPostingID    uuid.UUID
	WrongAccountID     uuid.UUID
	CorrectAccountID   uuid.UUID
	WalletAccountID    uuid.UUID
	AmountMicrocredits int64
}

// newLedgerRepairCrossDomainBillingFixture provisions a production-shaped
// Billing database for the cross-domain ledger-repair gate: the database and
// public schema belong to billing_schema_owner, migrations run as
// billing_migrator with MIGRATION_ROLE=billing_service, and the runtime pool
// runs as the non-superuser billing_service identity. Activation stays exactly
// as deployed: LEDGER_REPAIR remains the original INACTIVE v1/s1 row.
func newLedgerRepairCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, ledgerRepairCrossDomainFacts) {
	t.Helper()
	repository, pool, operator := newEntitlementCrossDomainBillingFixture(t, ctx, databaseURL)
	facts := seedLedgerRepairCrossDomainEvidence(t, ctx, pool)
	return repository, pool, operator, facts
}

// seedLedgerRepairCrossDomainEvidence writes one USAGE journal whose revenue
// leg was attributed to the platform adjustment account instead of the usage
// revenue account. That is exactly the attribution error the first release of
// LEDGER_REPAIR may correct, and the whole journal is written in one deferred
// balancing transaction like the real writer.
func seedLedgerRepairCrossDomainEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool) ledgerRepairCrossDomainFacts {
	t.Helper()
	facts := ledgerRepairCrossDomainFacts{
		WalletID: uuid.New(), JournalEntryID: uuid.New(), TargetPostingID: uuid.New(), WalletAccountID: uuid.New(),
		AmountMicrocredits: 30,
	}
	now := time.Now().UTC()
	if err := pool.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code='platform:adjustment' AND wallet_id IS NULL`).Scan(&facts.WrongAccountID); err != nil {
		t.Fatalf("platform adjustment account: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code='platform:usage-revenue' AND wallet_id IS NULL`).Scan(&facts.CorrectAccountID); err != nil {
		t.Fatalf("platform usage revenue account: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for index, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',100,1,$3,$3)`, []any{facts.WalletID, uuid.New(), now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, []any{facts.WalletAccountID, facts.WalletID, "ledger-repair-wallet:" + facts.WalletID.String(), now}},
		{`INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'USAGE',$2,'attribution error',$3)`, []any{facts.JournalEntryID, "usage:" + facts.JournalEntryID.String(), now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,30,$4)`, []any{facts.TargetPostingID, facts.JournalEntryID, facts.WrongAccountID, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-30,$4)`, []any{uuid.New(), facts.JournalEntryID, facts.WalletAccountID, now}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("Billing evidence statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit Billing evidence: %v", err)
	}
	return facts
}

func seedLedgerAdvancementRequest(t *testing.T, ctx context.Context, repo *PostgresRepository, user uuid.UUID, policy approvalpolicy.Policy, status string, frozen approvalencoding.LegacyProofInput) uuid.UUID {
	t.Helper()
	id := uuid.New()
	binding, authorizationVersion, _, err := strongPlatformAuthorization(ctx, repo.pool, user, policy.RequesterPermission)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.pool.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,expires_at,submitted_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$9,$6,$10,decode(repeat('00',32),'hex'),'LEDGER_POSTING',$7,'controlled historical draft','[]','{}',$8,now()+interval '1 hour',CASE WHEN $8='SUBMITTED' THEN now() ELSE NULL END,now(),now())`, id, policy.CommandType, policy.PolicyVersion, user, binding, "historical-"+id.String(), frozen.TargetResourceID, status, authorizationVersion, frozen.NormalizedCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO approval_request_snapshots(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at) VALUES($1,$2,$3,$4,'{}','{}',now())`, id, policy.SnapshotSchemaVersion, frozen.CriticalResourceVersions, frozen.FinancialSnapshot); err != nil {
		t.Fatal(err)
	}
	return id
}
