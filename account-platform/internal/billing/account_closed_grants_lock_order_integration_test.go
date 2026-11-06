package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func closedGrantWalletFirstFixture(t *testing.T) (*PostgresRepository, context.Context, Wallet, MemberQuotaGrant, uuid.UUID) {
	t.Helper()
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil {
		t.Fatal(err)
	}
	member, actor := uuid.New(), uuid.New()
	if _, err = r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	allocation := MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: actor, IdempotencyKey: uuid.NewString()}
	if err = r.pool.QueryRow(ctx, `SELECT component.id,package.id FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.source_payment_transaction_id=$1 AND component.component_type='BASE'`, transaction).Scan(&allocation.ComponentID, &allocation.PackageID); err != nil {
		t.Fatal(err)
	}
	grant, _, err := r.AllocateMemberQuota(ctx, allocation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.IsolateAccountClosedGrants(ctx, uuid.New(), member, 2); err != nil {
		t.Fatal(err)
	}
	jobctx, err := serviceauth.ContextForJob(ctx, "token-gateway", "closed-grant-reclaim", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	jobctx = humandelegation.WithClaims(jobctx, humandelegation.Claims{DelegationID: uuid.New(), HumanUserID: actor, Permission: "org.grants.reclaim_closed", AuthorizationVersion: 1})
	return r, jobctx, wallet, grant, actor
}

func TestF08ClosedGrantReclaimWalletFirstPostgres(t *testing.T) {
	r, fixturectx, wallet, grant, actor := closedGrantWalletFirstFixture(t)
	ctx, cancelCalls := context.WithCancel(fixturectx)
	defer cancelCalls()
	financialFacts := func() string {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT to_jsonb(w) FROM wallets w WHERE id=$1),(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM ledger_postings p))::text`, wallet.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	beforeFinancial := financialFacts()
	blocker, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, `SELECT id FROM wallets WHERE id=$1 FOR UPDATE`, wallet.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.Exec(ctx, `SELECT id FROM entitlement_components WHERE id=$1 FOR UPDATE`, grant.ComponentID); err != nil {
		t.Fatal(err)
	}
	app := "f08_wallet_first_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	configuration := r.pool.Config().Copy()
	configuration.ConnConfig.RuntimeParams["application_name"] = app
	configuration.MaxConns, configuration.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancelCalls(); pool.Close() }()
	repo := NewPostgresRepositoryWithPool(pool)
	key, reason := uuid.NewString(), "controlled wallet-first closed-grant reclaim"
	type outcome struct {
		value AccountClosedGrantReclaim
		err   error
	}
	result := make(chan outcome, 1)
	go func() {
		value, err := repo.ReclaimAccountClosedGrant(ctx, wallet.OwnerID, grant.ID, actor, 2, reason, key)
		result <- outcome{value, err}
	}()
	waitctx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err = r.pool.QueryRow(waitctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0 AND query ILIKE '%wallets%FOR UPDATE%')`, app).Scan(&waiting); err != nil {
			t.Fatal("observe actual wallet lock wait", err)
		}
		if waiting {
			break
		}
		select {
		case <-waitctx.Done():
			t.Fatal("production reclaim did not wait on the wallet first")
		case <-ticker.C:
		}
	}
	probe, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Rollback(context.Background())
	if _, err = probe.Exec(ctx, `SELECT id FROM member_quota_grants WHERE id=$1 FOR UPDATE NOWAIT`, grant.ID); err != nil {
		t.Fatal("wallet-blocked reclaim already held the grant", err)
	}
	if err = probe.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result:
		t.Fatal("reclaim completed while its wallet lock was held")
	default:
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var completed outcome
	select {
	case completed = <-result:
	case <-waitctx.Done():
		t.Fatal("reclaim did not finish after the wallet was released")
	}
	if completed.err != nil || completed.value.Replayed || completed.value.AmountMicrocredits != 500000 || completed.value.Grant.AvailableMicrocredits != 0 || completed.value.Grant.RevokedMicrocredits != 500000 || completed.value.Grant.Status != "RECLAIMED_ACCOUNT_CLOSED" {
		t.Fatal("wallet-first reclaim changed its financial result", completed.value, completed.err)
	}
	var available, suspended int64
	var audit, commands int
	if err = r.pool.QueryRow(ctx, `SELECT available_microcredits,suspended_grant_microcredits,(SELECT count(*) FROM member_quota_grant_audits WHERE grant_id=$2 AND action='RECLAIMED_ACCOUNT_CLOSED'),(SELECT count(*) FROM account_closed_grant_reclaim_commands WHERE organization_id=$3 AND idempotency_key=$4) FROM entitlement_components WHERE id=$1`, grant.ComponentID, grant.ID, wallet.OwnerID, key).Scan(&available, &suspended, &audit, &commands); err != nil || available != 1000000 || suspended != 0 || audit != 1 || commands != 1 || financialFacts() != beforeFinancial {
		t.Fatal("reclaim changed total funds/ledger or duplicated audit/receipt", available, suspended, audit, commands, err)
	}
	replayBlocker, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer replayBlocker.Rollback(context.Background())
	if _, err = replayBlocker.Exec(ctx, `SELECT id FROM wallets WHERE id=$1 FOR UPDATE`, wallet.ID); err != nil {
		t.Fatal(err)
	}
	// Existing receipts must finish even while another transaction owns the
	// wallet. This proves the added lock remains after immutable readback.
	replayctx, stopReplay := context.WithTimeout(ctx, 2*time.Second)
	defer stopReplay()
	replayed, err := r.ReclaimAccountClosedGrant(replayctx, wallet.OwnerID, grant.ID, actor, 2, reason, key)
	if err != nil || !replayed.Replayed || replayed.AmountMicrocredits != completed.value.AmountMicrocredits {
		t.Fatal("old command receipt tried to reacquire the wallet", replayed, err)
	}
	if err = replayBlocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var auditsAfter, commandsAfter int
	if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM member_quota_grant_audits WHERE grant_id=$1 AND action='RECLAIMED_ACCOUNT_CLOSED'),(SELECT count(*) FROM account_closed_grant_reclaim_commands WHERE organization_id=$2 AND idempotency_key=$3)`, grant.ID, wallet.OwnerID, key).Scan(&auditsAfter, &commandsAfter); err != nil || auditsAfter != audit || commandsAfter != commands || financialFacts() != beforeFinancial {
		t.Fatal("receipt replay duplicated financial effects", err)
	}
	t.Log("SUMMARY actual_wallet_wait=true grant_NOWAIT_available=true reclaimed=500000 one_audit=true one_receipt=true old_receipt_without_wallet_lock=true wallet_and_ledger_unchanged=true")
}

func TestF08ClosedGrantReclaimRejectsForeignOwnershipPostgres(t *testing.T) {
	for _, mismatch := range []string{"component-package", "package-wallet"} {
		t.Run(mismatch, func(t *testing.T) {
			r, ctx, wallet, grant, actor := closedGrantWalletFirstFixture(t)
			_, otherOrder, otherTransaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			var otherPackage uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_packages WHERE source_payment_transaction_id=$1`, otherTransaction).Scan(&otherPackage); err != nil {
				t.Fatal(err)
			}
			statement := fmt.Sprintf(`UPDATE member_quota_grants SET package_id='%s' WHERE id='%s'`, otherPackage, grant.ID)
			if mismatch == "package-wallet" {
				statement = fmt.Sprintf(`UPDATE entitlement_packages SET wallet_id='%s' WHERE id='%s'`, otherOrder.WalletID, grant.PackageID)
			}
			snapshot := func() string {
				var value string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),(SELECT jsonb_agg(to_jsonb(g) ORDER BY id) FROM member_quota_grants g),(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_components c),(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM entitlement_packages p),(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM member_quota_grant_audits a),(SELECT jsonb_agg(to_jsonb(c) ORDER BY organization_id,idempotency_key) FROM account_closed_grant_reclaim_commands c),(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM outbox_events o),(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM ledger_postings p))::text`).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := snapshot()
			_, setupErr := retainedFinanceOwnerExec(t, r, ctx, statement)
			if mismatch == "component-package" {
				// The installed composite FK already makes this tuple impossible.
				// Do not disable it to invent runtime branch coverage.
				var pgerr *pgconn.PgError
				if !errors.As(setupErr, &pgerr) || pgerr.Code != "23503" || pgerr.ConstraintName != "member_quota_grants_component_id_package_id_component_type_fkey" || snapshot() != before {
					t.Fatal("installed component/package FK did not reject mismatched facts without effects", setupErr)
				}
				return
			}
			if setupErr != nil {
				t.Fatal("create controlled foreign-wallet fact only in owned UUID fixture", setupErr)
			}
			before = snapshot()
			_, err := r.ReclaimAccountClosedGrant(ctx, wallet.OwnerID, grant.ID, actor, 2, "controlled foreign ownership must fail closed", uuid.NewString())
			if !errors.Is(err, ErrConflict) || snapshot() != before {
				t.Fatal("foreign package/component/wallet ownership produced an effect", err)
			}
		})
	}
}
