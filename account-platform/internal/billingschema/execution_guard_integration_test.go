package billingschema

// Uses the independent before probe's genuine Create/Submit/Decide/relay/Begin chain.
// The broker publication ACK is controlled; both databases and all roles are owned.
import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"testing"
	"time"
)

type frozenFactsActivationReader struct{ c *pgx.Conn }

func (v frozenFactsActivationReader) Snapshot(ctx context.Context) (billinggovernance.Snapshot, error) {
	s := billinggovernance.Snapshot{SchemaVersion: 1}
	rows, err := v.c.Query(ctx, `SELECT command_type,policy_version,snapshot_schema_version,state,version,updated_at FROM billing_approval_command_activations ORDER BY command_type`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var a billinggovernance.Activation
		if err = rows.Scan(&a.CommandType, &a.PolicyVersion, &a.SnapshotSchemaVersion, &a.State, &a.Version, &a.UpdatedAt); err != nil {
			return s, err
		}
		s.Activations = append(s.Activations, a)
	}
	return s, rows.Err()
}
func TestApprovalExecutionFrozenFinancialFacts(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_SCHEMA_DATABASE_URL required for owned actual PostgreSQL")
	}
	ctx, stop := context.WithTimeout(context.Background(), 90*time.Second)
	defer stop()
	f := newStageFixture(t, ctx, raw)
	aa, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal("load Account source")
	}
	c := f.database(t, ctx, "account", "account_service")
	if err = accountschema.Up(ctx, c, aa); err != nil {
		t.Fatal("install current real Account schema")
	}
	migrations.Unlock(ctx, c)
	bb, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal("load Billing source")
	}
	b := f.database(t, ctx, "billing", "billing_schema_owner")
	if err = Up(ctx, b, bb); err != nil {
		t.Fatal("install current real Billing schema")
	}
	migrations.Unlock(ctx, b)
	_, err = c.Exec(ctx, `
INSERT INTO users(id,status,display_name,username,user_code,created_at,updated_at) VALUES
('11111111-1111-4111-8111-111111111111','ACTIVE','owned requester','own-requester','USR1111111111111111'||user_code_checksum('USR1111111111111111'),now(),now()),
('22222222-2222-4222-8222-222222222222','ACTIVE','owned approver','own-approver','USR2222222222222222'||user_code_checksum('USR2222222222222222'),now(),now());
INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES
('41111111-1111-4111-8111-111111111111','11111111-1111-4111-8111-111111111111','PLATFORM_BILLING_OPERATOR','ACCOUNT',now()-interval '1 day',now()),
('42222222-2222-4222-8222-222222222222','22222222-2222-4222-8222-222222222222','PLATFORM_BILLING_APPROVER','ACCOUNT',now()-interval '1 day',now());
INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES
('https://owned.invalid','owned-requester','11111111-1111-4111-8111-111111111111',now(),now()),
('https://owned.invalid','owned-approver','22222222-2222-4222-8222-222222222222',now(),now());`)
	if err != nil {
		t.Fatal("seed two distinct genuine permission-bound actors")
	}
	repo, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal("open real Account repository")
	}
	defer repo.Close()
	repo.WithBillingGovernanceActivation(frozenFactsActivationReader{b})
	if err = repo.ConfigureApprovalWriter(ctx, f.serviceURL(t, c, "account_approval_writer")); err != nil {
		t.Fatal("real isolated writer readiness")
	}
	principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-requester", SessionID: "owned-requester-session", AuthenticatedAt: time.Now()}
	approver := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-approver", SessionID: "owned-approver-session", AuthenticatedAt: time.Now()}
	draft := account.ApprovalDraft{CommandType: "WALLET_ADJUSTMENT", IdempotencyKey: "owned-independent-financial-freeze", NormalizedCommand: json.RawMessage(`{"adjustment_id":"0198f110-f538-7f30-a8b1-f3984fd459a0","wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","amount_microcredits":100,"requested_by":"11111111-1111-4111-8111-111111111111"}`), TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: "0198f110-f538-7f30-a8b1-f3984fd459a0", Reason: "owned independent financial freeze", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{"delta":100}`), CriticalResourceVersions: json.RawMessage(`{"adjustment_version":1}`), FinancialSnapshot: json.RawMessage(`{"wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","delta_microcredits":100}`)}
	item, err := repo.CreateApprovalDraft(ctx, principal, draft)
	if err != nil {
		t.Fatal("real CreateApprovalDraft failed")
	}
	item, err = repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version)
	if err != nil {
		t.Fatal("real SubmitApprovalRequest failed")
	}
	item, err = repo.DecideApprovalRequest(ctx, approver, item.ID, item.Version, "APPROVED", "owned independent decision", draft.CriticalResourceVersions)
	if err != nil || item.Status != "APPROVED" {
		t.Fatal("real independent DecideApprovalRequest failed")
	}
	relay, err := account.NewApprovalRelayStore(ctx, f.serviceURL(t, c, "account_approval_relay"))
	if err != nil {
		t.Fatal("real relay readiness")
	}
	defer relay.Close()
	claims, err := relay.ClaimFrozen(ctx, "owned-independent-relay", 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatal("real relay claim failed")
	}
	disposition, err := relay.ProcessFrozen(ctx, "owned-independent-relay", claims[0], 12, time.Now(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	})
	if err != nil || disposition != events.FrozenPublished {
		t.Fatal("real relay persistence with controlled broker ACK failed")
	}
	execution, err := repo.BeginApprovalExecution(ctx, item.ID, "billing-command-executor", claims[0].EventID.String(), draft.CriticalResourceVersions, draft.FinancialSnapshot)
	if err != nil || execution.Status != "EXECUTING" {
		t.Fatal("real BeginApprovalExecution failed")
	}
	runtime, err := pgx.Connect(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal("open runtime role")
	}
	defer runtime.Close(context.Background())
	var user string
	if err = runtime.QueryRow(ctx, `SELECT session_user`).Scan(&user); err != nil || user != "account_service" {
		t.Fatal("runtime role identity mismatch")
	}
	for _, column := range []string{"financial_snapshot", "critical_resource_versions"} {
		t.Run(column, func(t *testing.T) {
			tx, err := runtime.Begin(ctx)
			if err != nil {
				t.Fatal("begin owned mutation")
			}
			defer tx.Rollback(context.Background())
			var prior string
			if err = tx.QueryRow(ctx, `SELECT `+column+`::text FROM approval_executions WHERE approval_request_id=$1`, item.ID).Scan(&prior); err != nil {
				t.Fatal("read frozen prior")
			}
			tag, err := tx.Exec(ctx, `UPDATE approval_executions SET `+column+`='{"owned_mutated":999}'::jsonb WHERE approval_request_id=$1`, item.ID)
			if err == nil && tag.RowsAffected() == 1 {
				var after string
				if err = tx.QueryRow(ctx, `SELECT `+column+`::text FROM approval_executions WHERE approval_request_id=$1`, item.ID).Scan(&after); err != nil {
					t.Fatal("read mutation")
				}
				if after != prior {
					t.Fatal("confirmed: legal execution financial fact changed under account_service")
				}
			}
			if err == nil {
				t.Fatal("runtime mutation not explicitly rejected")
			}
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "55000" {
				t.Fatal("mutation did not reach the immutable guard")
			}
		})
	}
	t.Run("same_facts_and_failed_retry", func(t *testing.T) {
		tx, err := runtime.Begin(ctx)
		if err != nil {
			t.Fatal("begin retry fixture")
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(ctx, `UPDATE approval_executions SET financial_snapshot=financial_snapshot,critical_resource_versions=critical_resource_versions,status='FAILED' WHERE approval_request_id=$1`, item.ID); err != nil {
			t.Fatal("guard rejected unchanged facts or legal failure")
		}
		if _, err = tx.Exec(ctx, `UPDATE approval_executions SET status='EXECUTING' WHERE approval_request_id=$1`, item.ID); err != nil {
			t.Fatal("guard rejected legal retry")
		}
	})
}
