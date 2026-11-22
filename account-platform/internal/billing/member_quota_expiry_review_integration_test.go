package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func reviewExpiryGrant(t *testing.T, r *PostgresRepository, ctx context.Context, order PaymentOrder, packageID, member uuid.UUID, amount int64, types ...string) MemberQuotaGrant {
	t.Helper()
	componentType := "BASE"
	if len(types) == 1 {
		componentType = types[0]
	}
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, *order.BeneficiaryOrganizationID, member, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	var component uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type=$2`, packageID, componentType).Scan(&component); err != nil {
		t.Fatal(err)
	}
	grant, _, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: *order.BeneficiaryOrganizationID, MemberUserID: member, PackageID: packageID, ComponentID: component, ComponentType: componentType, AmountMicrocredits: amount, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func reviewExpiryBaseBonusOrder(t *testing.T, r *PostgresRepository, ctx context.Context) (PaymentOrder, uuid.UUID, uuid.UUID) {
	t.Helper()
	c, order, _, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, false)
	// Change only the owned fixture's unpaid product fact; issuance still runs
	// the production split/lot/package/journal transaction with both components.
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET product_snapshot=product_snapshot||'{"base_points":1,"bonus_points":1}'::jsonb WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	provider := c.provider.(*retainedFinancialProvider)
	if _, _, err := c.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", "fixture-base-bonus-"+uuid.NewString(), []byte(`{"controlled":true}`), provider.result, false, r.now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var transaction, pack uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT t.id,p.id FROM payment_transactions t JOIN entitlement_packages p ON p.source_payment_transaction_id=t.id WHERE t.payment_order_id=$1`, order.ID).Scan(&transaction, &pack); err != nil {
		t.Fatal(err)
	}
	return order, transaction, pack
}

func reviewExpiryPackageSnapshot(t *testing.T, r *PostgresRepository, ctx context.Context, packageID uuid.UUID) string {
	t.Helper()
	var value string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(p),to_jsonb(w),
 (SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id) FROM entitlement_components c WHERE c.package_id=p.id),
 (SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id) FROM credit_lots l JOIN entitlement_components c ON c.credit_lot_id=l.id WHERE c.package_id=p.id),
 (SELECT jsonb_agg(to_jsonb(g) ORDER BY g.id) FROM member_quota_grants g WHERE g.package_id=p.id),
 (SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM usage_holds h WHERE h.wallet_id=w.id),
 (SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM member_quota_grant_audits a JOIN member_quota_grants g ON g.id=a.grant_id WHERE g.package_id=p.id),
 (SELECT jsonb_agg(to_jsonb(e) ORDER BY e.event_id) FROM outbox_events e JOIN member_quota_grants g ON g.id=e.aggregate_id WHERE g.package_id=p.id),
 (SELECT jsonb_agg(to_jsonb(lp) ORDER BY lp.id) FROM ledger_postings lp JOIN ledger_accounts la ON la.id=lp.ledger_account_id WHERE la.wallet_id=w.id))::text
 FROM entitlement_packages p JOIN wallets w ON w.id=p.wallet_id WHERE p.id=$1`, packageID).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func reviewExpirySnapshot(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var snapshot string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM wallets t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM credit_lots t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM entitlement_packages t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM entitlement_components t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM member_quota_grants t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM usage_holds t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY hold_id,component_id) FROM usage_hold_allocations t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM journal_entries t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM ledger_postings t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY organization_id,user_id) FROM organization_member_lifecycle_states t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM member_quota_grant_audits t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY event_id) FROM outbox_events t))::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func reviewExpiryDiagnostics(t *testing.T) [2]string {
	t.Helper()
	doc, err := os.ReadFile("../../docs/member-quota-expiry-recovery.md")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.ReplaceAll(string(doc), "\r\n", "\n"), "```sql\n")
	if len(parts) != 2 {
		t.Fatal("diagnostic SQL block missing")
	}
	statements := strings.Split(strings.TrimSpace(strings.Split(parts[1], "```")[0]), ";")
	if len(statements) != 5 || strings.TrimSpace(statements[0]) != "BEGIN READ ONLY" || strings.TrimSpace(statements[3]) != "ROLLBACK" || strings.TrimSpace(statements[4]) != "" {
		t.Fatal("expected the original two SELECTs inside the read-only runbook transaction")
	}
	return [2]string{strings.TrimSpace(statements[1]), strings.TrimSpace(statements[2])}
}

func TestF06MemberQuotaRestoreUsesOrdinaryPackageExpiry(t *testing.T) {
	for _, scenario := range []string{"restore-first", "worker-first", "concurrent", "zero-restore", "zero-worker", "active-hold", "review-all", "review-mixed", "package-not-due", "package-frozen", "base-bonus", "review-queue"} {
		t.Run(scenario, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			var order PaymentOrder
			var transactionID, packageID uuid.UUID
			if scenario == "base-bonus" {
				order, transactionID, packageID = reviewExpiryBaseBonusOrder(t, r, ctx)
			} else {
				_, order, transactionID, packageID = retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			}
			member := uuid.New()
			amount := int64(500000)
			if scenario == "review-all" || scenario == "review-mixed" || scenario == "active-hold" || scenario == "zero-restore" || scenario == "zero-worker" || scenario == "review-queue" {
				amount = 1000000
			}
			grant := reviewExpiryGrant(t, r, ctx, order, packageID, member, amount)
			grants := []MemberQuotaGrant{grant}
			if scenario == "base-bonus" {
				grants = append(grants, reviewExpiryGrant(t, r, ctx, order, packageID, member, 500000, "BONUS"))
			}
			if scenario == "restore-first" {
				grants = append(grants, reviewExpiryGrant(t, r, ctx, order, packageID, member, 200000))
				// The whole package also owns another member's live quota.
				grants = append(grants, reviewExpiryGrant(t, r, ctx, order, packageID, uuid.New(), 100000))
			}
			var reviewUsage, reviewHold uuid.UUID
			if scenario == "review-all" || scenario == "review-mixed" || scenario == "active-hold" || scenario == "zero-restore" || scenario == "zero-worker" || scenario == "review-queue" {
				usage := uuid.New()
				max := int64(1000000)
				if scenario == "review-mixed" {
					max = 800000
				}
				hold, err := r.ReserveMemberQuota(ctx, usage, order.WalletID, *order.BeneficiaryOrganizationID, member, 1, max, nil, r.now().Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "zero-restore" || scenario == "zero-worker" {
					if _, err := r.Commit(ctx, usage, hold.ID, max, map[string]any{"fixture": "complete-consumption"}); err != nil {
						t.Fatal(err)
					}
				} else if scenario == "review-all" || scenario == "review-mixed" || scenario == "review-queue" {
					// Inject only the existing REVIEW hold state in this owned database;
					// reservations themselves are created by the production API above.
					if _, err := r.pool.Exec(ctx, `UPDATE usage_holds SET status='REVIEW' WHERE id=$1`, hold.ID); err != nil {
						t.Fatal(err)
					}
					reviewUsage, reviewHold = usage, hold.ID
					if scenario == "review-mixed" {
						if _, err := r.ReserveMemberQuota(ctx, uuid.New(), order.WalletID, *order.BeneficiaryOrganizationID, member, 1, 200000, nil, r.now().Add(time.Hour)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if _, err := r.ApplyOrganizationMemberLifecycle(ctx, *order.BeneficiaryOrganizationID, member, "SUSPENDED", 2); err != nil {
				t.Fatal(err)
			}
			var deadline time.Time
			if err := r.pool.QueryRow(ctx, `SELECT valid_until FROM entitlement_packages WHERE id=$1`, packageID).Scan(&deadline); err != nil {
				t.Fatal(err)
			}
			var nextPackage uuid.UUID
			if scenario == "review-queue" {
				_, _, _, nextPackage = retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
				if err := r.pool.QueryRow(ctx, `SELECT valid_until FROM entitlement_packages WHERE id=$1`, nextPackage).Scan(&deadline); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "package-not-due" {
				// Constructor equality is not a database constraint. A malformed grant
				// deadline cannot authorize expiring its still-valid whole package.
				if _, err := r.pool.Exec(ctx, `UPDATE member_quota_grants SET expires_at=$2 WHERE id=$1`, grant.ID, r.now().Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
			} else {
				r.now = func() time.Time { return deadline.Add(time.Second) }
			}
			if scenario == "package-frozen" {
				// Use the existing financial-review API to freeze the package.
				if _, err := r.RecordAndFreezeForcedRefund(ctx, transactionID, "controlled-expiry-frozen-package", 5, nil); err != nil {
					t.Fatal(err)
				}
			}
			restore := func() error {
				_, err := r.ApplyOrganizationMemberLifecycle(ctx, *order.BeneficiaryOrganizationID, member, "ACTIVE", 3)
				return err
			}
			expire := func() error { _, err := r.ExpirePackages(ctx, 100); return err }
			if scenario == "review-queue" {
				before := reviewExpiryPackageSnapshot(t, r, ctx, packageID)
				if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 1 {
					t.Fatal("REVIEW package starved later due package", count, err)
				}
				if before != reviewExpiryPackageSnapshot(t, r, ctx, packageID) {
					t.Fatal("deferring REVIEW changed protected package facts")
				}
				var nextStatus string
				if err := r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, nextPackage).Scan(&nextStatus); err != nil || nextStatus != "EXPIRED" {
					t.Fatal("later package did not expire", nextStatus, err)
				}
				// Remove the same controlled REVIEW injection; the existing release
				// API performs all actual reservation and grant/component changes.
				// This is not a production review-management acceptance test.
				if _, err := r.pool.Exec(ctx, `UPDATE usage_holds SET status='ACTIVE' WHERE id=$1`, reviewHold); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Release(ctx, reviewUsage, reviewHold, "controlled-review-cleared"); err != nil {
					t.Fatal(err)
				}
				if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 1 {
					t.Fatal("cleared review package did not reenter expiry", count, err)
				}
			}
			if scenario == "review-all" || scenario == "review-mixed" || scenario == "package-not-due" || scenario == "package-frozen" {
				before := reviewExpirySnapshot(t, r, ctx)
				if err := restore(); !errors.Is(err, ErrConflict) {
					t.Fatal("unsafe restoration was not rejected", err)
				}
				if before != reviewExpirySnapshot(t, r, ctx) {
					t.Fatal("failed restoration changed funds, projection, holds, or evidence")
				}
				if scenario == "review-all" || scenario == "review-mixed" {
					if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 0 || before != reviewExpirySnapshot(t, r, ctx) {
						t.Fatal("worker finalized or released REVIEW reservations", err)
					}
				}
				return
			}
			if scenario == "concurrent" {
				var group sync.WaitGroup
				failures := make(chan error, 2)
				start := make(chan struct{})
				for _, operation := range []func() error{restore, expire} {
					group.Add(1)
					go func(operation func() error) { defer group.Done(); <-start; failures <- operation() }(operation)
				}
				close(start)
				group.Wait()
				close(failures)
				for err := range failures {
					if err != nil {
						t.Fatal("wallet-first expiry/restoration did not converge", err)
					}
				}
			} else {
				if scenario == "worker-first" || scenario == "zero-worker" {
					if err := expire(); err != nil {
						t.Fatal(err)
					}
				}
				if err := restore(); err != nil {
					t.Fatal(err)
				}
			}
			var posted, reserved, lotRemaining, journals, balance, postings int64
			var packageStatus string
			if err := r.pool.QueryRow(ctx, `SELECT w.posted_microcredits,w.reserved_microcredits,p.status,
 (SELECT sum(l.remaining_microcredits) FROM credit_lots l JOIN entitlement_components c ON c.credit_lot_id=l.id WHERE c.package_id=p.id),
 (SELECT count(*) FROM journal_entries j WHERE j.business_reference='expire:'||p.id::text),
 (SELECT COALESCE(sum(lp.delta_microcredits),0) FROM ledger_postings lp JOIN journal_entries j ON j.id=lp.journal_entry_id WHERE j.business_reference='expire:'||p.id::text),
 (SELECT count(*) FROM ledger_postings lp JOIN journal_entries j ON j.id=lp.journal_entry_id WHERE j.business_reference='expire:'||p.id::text)
 FROM entitlement_packages p JOIN wallets w ON w.id=p.wallet_id WHERE p.id=$1`, packageID).Scan(&posted, &reserved, &packageStatus, &lotRemaining, &journals, &balance, &postings); err != nil {
				t.Fatal(err)
			}
			zero := scenario == "zero-restore" || scenario == "zero-worker"
			wantedPostings := int64(2)
			if scenario == "base-bonus" {
				wantedPostings = 3
			}
			if posted != 0 || reserved != 0 || lotRemaining != 0 || balance != 0 || (!zero && (journals != 1 || postings != wantedPostings || packageStatus != "EXPIRED")) || (zero && (journals != 0 || postings != 0 || packageStatus != "CONSUMED")) {
				t.Fatal("expiry did not conserve the original wallet/lot/balanced journal facts", posted, reserved, lotRemaining, journals, balance, postings, packageStatus)
			}
			for _, expected := range grants {
				var status string
				var version, expiryAudits, expiryEvents int64
				if err := r.pool.QueryRow(ctx, `SELECT g.status,g.membership_version,
 (SELECT count(*) FROM member_quota_grant_audits a WHERE a.grant_id=g.id AND a.action='EXPIRED'),
 (SELECT count(*) FROM outbox_events e WHERE e.aggregate_id=g.id AND e.event_type='billing.member-quota.expired.v1') FROM member_quota_grants g WHERE id=$1`, expected.ID).Scan(&status, &version, &expiryAudits, &expiryEvents); err != nil || status != "EXPIRED" || expiryAudits != 1 || expiryEvents != 1 {
					t.Fatal("grant expiry was not exactly once, including zero capacity", status, expiryAudits, expiryEvents, err)
				}
				if scenario == "restore-first" && expected.MemberUserID == member && version != 3 {
					t.Fatal("expiry audit lost authoritative restoration membership version", version)
				}
			}
			before := reviewExpirySnapshot(t, r, ctx)
			if err := restore(); err != nil {
				t.Fatal(err)
			}
			if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 0 || before != reviewExpirySnapshot(t, r, ctx) {
				t.Fatal("same-version restore or repeated worker repeated financial effects", count, err)
			}
			if _, err := r.ApplyOrganizationLifecycle(ctx, *order.BeneficiaryOrganizationID, "DISABLED", 2, false); err != nil {
				t.Fatal(err)
			}
			assessment, err := r.ApplyOrganizationLifecycle(ctx, *order.BeneficiaryOrganizationID, "DELETED", 3, true)
			if err != nil || !assessment.Ready {
				t.Fatal("completed expiry stranded organization deletion", assessment.Blockers, err)
			}
		})
	}
	for _, amount := range []int64{500000, 1000000} {
		name := "legacy-partial-expiry"
		if amount == 1000000 {
			name = "legacy-consumed"
		}
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, _, packageID := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			member := uuid.New()
			grant := reviewExpiryGrant(t, r, ctx, order, packageID, member, amount)
			if _, err := r.ApplyOrganizationMemberLifecycle(ctx, *order.BeneficiaryOrganizationID, member, "SUSPENDED", 2); err != nil {
				t.Fatal(err)
			}
			var deadline time.Time
			if err := r.pool.QueryRow(ctx, `SELECT valid_until FROM entitlement_packages WHERE id=$1`, packageID).Scan(&deadline); err != nil {
				t.Fatal(err)
			}
			r.now = func() time.Time { return deadline.Add(time.Second) }
			// Reproduce only the old, non-financial bucket transition inside this
			// UUID fixture. No migration or immutable posting is rewritten.
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `UPDATE entitlement_components SET suspended_grant_microcredits=suspended_grant_microcredits-$2,expired_microcredits=expired_microcredits+$2 WHERE id=$1`, grant.ComponentID, amount); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `UPDATE member_quota_grants SET available_microcredits=0,expired_microcredits=expired_microcredits+$2,status='EXPIRED',suspension_reason=NULL WHERE id=$1`, grant.ID, amount); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 1 {
				t.Fatal(count, err)
			}
			var posted, lotRemaining, journals int64
			var status string
			if err := r.pool.QueryRow(ctx, `SELECT w.posted_microcredits,p.status,l.remaining_microcredits,(SELECT count(*) FROM journal_entries WHERE business_reference='expire:'||p.id::text) FROM entitlement_packages p JOIN wallets w ON w.id=p.wallet_id JOIN entitlement_components c ON c.package_id=p.id JOIN credit_lots l ON l.id=c.credit_lot_id WHERE p.id=$1`, packageID).Scan(&posted, &status, &lotRemaining, &journals); err != nil {
				t.Fatal(err)
			}
			if posted != amount || lotRemaining != amount || (amount == 500000 && (status != "EXPIRED" || journals != 1)) || (amount == 1000000 && (status != "CONSUMED" || journals != 0)) {
				t.Fatal("legacy counters were blindly burned again", posted, lotRemaining, status, journals)
			}
			before := reviewExpirySnapshot(t, r, ctx)
			if count, err := r.ExpirePackages(ctx, 100); err != nil || count != 0 || before != reviewExpirySnapshot(t, r, ctx) {
				t.Fatal("historical residual was automatically rewritten", count, err)
			}
			// Execute the actual runbook's first diagnostic through a read-only tx.
			diagnostic := reviewExpiryDiagnostics(t)[0]
			read, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer read.Rollback(ctx)
			rows, err := read.Query(ctx, diagnostic)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for rows.Next() {
				if _, err := rows.Values(); err != nil {
					t.Fatal(err)
				}
				count++
			}
			rows.Close()
			if rows.Err() != nil || count != 1 {
				t.Fatal("read-only diagnostic missed terminal historical residual", count, rows.Err())
			}
			if err := read.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if before != reviewExpirySnapshot(t, r, ctx) {
				t.Fatal("read-only diagnostic changed historical data")
			}
		})
	}
	for _, status := range []string{"EXPIRED", "CONSUMED", "ACTIVE"} {
		t.Run("terminal-suspended-diagnostic-"+status, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, _, packageID := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			member := uuid.New()
			grant := reviewExpiryGrant(t, r, ctx, order, packageID, member, 500000)
			if _, err := r.ApplyOrganizationMemberLifecycle(ctx, *order.BeneficiaryOrganizationID, member, "SUSPENDED", 2); err != nil {
				t.Fatal(err)
			}
			if status != "ACTIVE" {
				// Represent the historical terminal-package/suspended-grant
				// mismatch only in this UUID fixture, without changing postings.
				if _, err := r.pool.Exec(ctx, `UPDATE entitlement_packages SET status=$2 WHERE id=$1`, packageID, status); err != nil {
					t.Fatal(err)
				}
			}
			before := reviewExpirySnapshot(t, r, ctx)
			read, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer read.Rollback(ctx)
			var readOnly string
			if err := read.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil || readOnly != "on" {
				t.Fatal("diagnostic transaction is not read-only", err)
			}
			rows, err := read.Query(ctx, reviewExpiryDiagnostics(t)[1])
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for rows.Next() {
				var pack, grantID uuid.UUID
				var actualStatus string
				var available, reserved int64
				if err := rows.Scan(&pack, &actualStatus, &grantID, &available, &reserved); err != nil || pack != packageID || actualStatus != status || grantID != grant.ID || available != 500000 || reserved != 0 {
					rows.Close()
					t.Fatal("diagnostic returned another grant or incorrect financial facts", err)
				}
				count++
			}
			rows.Close()
			want := 1
			if status == "ACTIVE" {
				want = 0
			}
			if rows.Err() != nil || count != want {
				t.Fatal("terminal suspended diagnostic hit/miss differs", count, want, rows.Err())
			}
			if err := read.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if before != reviewExpirySnapshot(t, r, ctx) {
				t.Fatal("second read-only diagnostic changed financial facts")
			}
		})
	}
}
