package billing

// Controlled concurrent PostgreSQL calls exercise the production transactions.
// Existing retained fixtures supply paid entitlement facts and owned UUID DBs;
// provider delivery and the HTTP authentication chain are outside this test.
import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestF08ClosedGrantConcurrentReceiptReplayPostgres(t *testing.T) {
	for _, operation := range []string{"isolate", "reclaim"} {
		t.Run(operation, func(t *testing.T) {
			r, fixturectx := retainedFinanceRepository(t)
			ctx, cancelCalls := context.WithCancel(fixturectx)
			defer cancelCalls()
			_, order, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			wallet, err := r.GetWallet(ctx, order.WalletID)
			if err != nil {
				t.Fatal(err)
			}
			member, actor, closure := uuid.New(), uuid.New(), uuid.New()
			if _, err = r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); err != nil {
				t.Fatal(err)
			}
			request := MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: actor, IdempotencyKey: uuid.NewString()}
			if err = r.pool.QueryRow(ctx, `SELECT component.id,package.id FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.source_payment_transaction_id=$1 AND component.component_type='BASE'`, transaction).Scan(&request.ComponentID, &request.PackageID); err != nil {
				t.Fatal(err)
			}
			grant, _, err := r.AllocateMemberQuota(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "reclaim" {
				if _, err = r.IsolateAccountClosedGrants(ctx, closure, member, 2); err != nil {
					t.Fatal(err)
				}
			}
			jobctx, err := serviceauth.ContextForJob(ctx, "token-gateway", "closed-grant-reclaim", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			jobctx = humandelegation.WithClaims(jobctx, humandelegation.Claims{DelegationID: uuid.New(), HumanUserID: actor, Permission: "org.grants.reclaim_closed", AuthorizationVersion: 1})
			key, reason := uuid.NewString(), "controlled same-key concurrent reclaim"
			invoke := func(repo *PostgresRepository) (bool, error) {
				if operation == "isolate" {
					result, err := repo.IsolateAccountClosedGrants(ctx, closure, member, 2)
					return result.Replayed, err
				}
				result, err := repo.ReclaimAccountClosedGrant(jobctx, wallet.OwnerID, grant.ID, actor, 2, reason, key)
				return result.Replayed, err
			}
			// The blocker changes no row/counter; actual PostgreSQL lock waits
			// prove both production calls have already executed their receipt read.
			blocker, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if operation == "isolate" {
				_, err = blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "account-closed-grants:"+member.String())
			} else {
				_, err = blocker.Exec(ctx, `SELECT id FROM member_quota_grants WHERE id=$1 FOR UPDATE`, grant.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				replay bool
				err    error
			}
			results := make(chan outcome, 2)
			apps := []string{"f08_diag_a_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "f08_diag_b_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
			for _, app := range apps {
				config := r.pool.Config().Copy()
				config.ConnConfig.RuntimeParams["application_name"] = app
				config.MaxConns, config.MinConns = 1, 0
				pool, err := pgxpool.NewWithConfig(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { cancelCalls(); pool.Close() }()
				repo := NewPostgresRepositoryWithPool(pool)
				go func() { replay, err := invoke(repo); results <- outcome{replay, err} }()
			}
			waitctx, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			queryFragment := "%pg_advisory_xact_lock%"
			if operation == "reclaim" {
				queryFragment = "%member_quota_grants%FOR UPDATE%"
			}
			for {
				var waiting int
				if err = r.pool.QueryRow(waitctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=ANY($1) AND wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0 AND query ILIKE $2`, apps, queryFragment).Scan(&waiting); err != nil {
					t.Fatal("real SQL wait observation", err)
				}
				if waiting == 2 {
					t.Log("LOCK_BARRIER both actual production receipt reads precede two observed SQL lock waits")
					break
				}
				select {
				case <-waitctx.Done():
					t.Fatal("both actual SQL lock waits were not observed")
				case <-ticker.C:
				}
			}
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			success, replayedCalls := 0, 0
			for range 2 {
				result := <-results
				if result.err != nil {
					t.Fatalf("same-key concurrent call did not recover its definite abort: %v", result.err)
				}
				success++
				if result.replay {
					replayedCalls++
				}
			}
			if success != 2 || replayedCalls != 1 {
				t.Fatalf("same-key calls: success=%d replayed=%d", success, replayedCalls)
			}
			facts := func() string {
				var value string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),(SELECT jsonb_agg(to_jsonb(g) ORDER BY id) FROM member_quota_grants g),(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_components c),(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM member_quota_grant_audits a),(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM outbox_events o),(SELECT jsonb_agg(to_jsonb(c) ORDER BY organization_id,idempotency_key) FROM account_closed_grant_reclaim_commands c),(SELECT jsonb_agg(to_jsonb(i) ORDER BY closure_request_id) FROM account_closure_grant_isolations i),(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j))::text`).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			beforeReplay := facts()
			replayed, err := invoke(r)
			if err != nil || !replayed || facts() != beforeReplay {
				t.Fatal("subsequent same-key replay did not preserve all durable effects", err)
			}
			var audit, commands int
			if operation == "isolate" {
				if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM member_quota_grant_audits WHERE grant_id=$1 AND action='ACCOUNT_CLOSED_ISOLATED'),(SELECT count(*) FROM account_closure_grant_isolations WHERE closure_request_id=$2)`, grant.ID, closure).Scan(&audit, &commands); err != nil {
					t.Fatal(err)
				}
			} else {
				if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM member_quota_grant_audits WHERE grant_id=$1 AND action='RECLAIMED_ACCOUNT_CLOSED'),(SELECT count(*) FROM account_closed_grant_reclaim_commands WHERE organization_id=$2 AND idempotency_key=$3)`, grant.ID, wallet.OwnerID, key).Scan(&audit, &commands); err != nil {
					t.Fatal(err)
				}
			}
			if audit != 1 || commands != 1 {
				t.Fatalf("durable effects duplicated: audit=%d commands=%d", audit, commands)
			}
			t.Logf("SUMMARY operation=%s success=%d replayed=%d one_audit=%d one_receipt=%d later_original_key_replay=true no_extra_effects=true", operation, success, replayedCalls, audit, commands)
		})
	}
}
