package token

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The activation transition is an explicit controlled operator fixture through
// the immutable public trigger. The runtime guard and all command/CAS/audit
// queries use the genuine runtime role, not a fake guard or bypassed trigger.
// This is not an offline release-issuance or deployed authorization proof.
func TestPersonalUsageGovernancePublicPostgres(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	handler, err := TokenCommandHandlerDigest()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := approvalpolicy.PolicySetDigest()
	if err != nil {
		t.Fatal(err)
	}
	store, err := r.GovernanceStore(handler, policy)
	if err != nil {
		t.Fatal(err)
	}
	r.WithTokenRouteActivationTransactionGuard(store)
	config, err := pgx.ParseConfig(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.Database = r.pool.Config().ConnConfig.Database
	operator, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close(context.Background())
	if _, err = operator.Exec(ctx, `SET ROLE token_schema_owner`); err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	makeInput := func(t *testing.T) runtimeTokenCommandBegin {
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "REVIEW")
		var updated time.Time
		if err := r.pool.QueryRow(ctx, `UPDATE usage_requests SET status='REVIEW',execution_status='REVIEW',cancel_status='NOT_REQUESTED',cancel_requested_at=NULL,updated_at=clock_timestamp() WHERE id=$1 RETURNING updated_at`, id).Scan(&updated); err != nil {
			t.Fatal(err)
		}
		command, _ := json.Marshal(map[string]any{"usage_request_id": id.String(), "expected_usage_version": updated.UTC().Format(time.RFC3339Nano), "evidence_high_watermark": 0, "replay_model_request": false})
		envelope := TokenCommandEnvelope{CommandType: "TOKEN_USAGE_RECONCILIATION_RETRY", Reason: "controlled genuine reconciliation retry", TargetResourceType: "USAGE_REQUEST", TargetResourceID: id.String(), Command: command}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expected, fingerprint, err := runtimeCommandPrecheckInTx(ctx, tx, envelope)
		tx.Rollback(ctx)
		if err != nil {
			t.Fatal(err)
		}
		envelope.ExpectedVersions = expected
		envelope.PrecheckFingerprint = fingerprint
		return runtimeTokenCommandBegin{tokenCommandBegin: tokenCommandBegin{Envelope: envelope, ActorUserID: actor, ActorPermission: "platform.usage.reconcile.retry", AuthorizationVersion: 1, IdempotencyKey: uuid.NewString(), AuditRequestID: uuid.NewString()}, RequestDigest: strings.Repeat("a", 64), KeyDigest: digestString([]byte(id.String())), IdempotencyFingerprint: digestString(command)}
	}
	selected := func(state string, epoch int64) context.Context {
		return context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: TokenCommandGovernanceActivation{State: state, Epoch: epoch}, transactional: true})
	}
	transition := func(state string, epoch int64) {
		if _, err := operator.Exec(ctx, `UPDATE public.token_command_governance_activation SET state=$1,epoch=$2,policy_set_digest=$3,handler_set_digest=$4,approved_runtime_roles=ARRAY['token_gateway'],operator_id='controlled-fixture',reason='current release fixture',request_id=$5,updated_at=clock_timestamp() WHERE singleton=TRUE`, state, epoch, policy, handler, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("preactivation-rejects-new-active-selection", func(t *testing.T) {
		input := makeInput(t)
		if _, _, err := r.ExecuteRuntimeTokenCommand(selected("ACTIVE", 1), input); !errors.Is(err, ErrTokenRouteActivationChanged) {
			t.Fatalf("inactive command accepted: %v", err)
		}
	})
	transition("ACTIVE", 1)
	t.Run("current-release-readiness-and-result-replay", func(t *testing.T) {
		service := (&Service{}).WithTokenCommandGovernanceActivation(store)
		response := httptest.NewRecorder()
		service.adminUsageReconciliationCommandReadiness(response, httptest.NewRequest(http.MethodGet, "/readiness", nil).WithContext(ctx))
		var readiness usageReconciliationCommandReadiness
		if json.Unmarshal(response.Body.Bytes(), &readiness) != nil || !readiness.CommandsEnabled || readiness.GovernanceEpoch != 1 {
			t.Fatalf("current release readiness missing: %s", response.Body.String())
		}
		input := makeInput(t)
		result, created, err := r.ExecuteRuntimeTokenCommand(selected("ACTIVE", 1), input)
		if err != nil || !created || result.Status != "SUCCEEDED" {
			t.Fatalf("genuine command failed: created=%v err=%v", created, err)
		}
		replay, again, err := r.ExecuteRuntimeTokenCommand(selected("ACTIVE", 1), input)
		if err != nil || again || replay.CommandReference != result.CommandReference {
			t.Fatalf("same key did not return immutable result: %v", err)
		}
		changed := input
		changed.IdempotencyFingerprint = strings.Repeat("f", 64)
		if _, _, err = r.ExecuteRuntimeTokenCommand(selected("ACTIVE", 1), changed); !errors.Is(err, ErrTokenCommandResultMismatch) {
			t.Fatalf("foreign same-key facts accepted: %v", err)
		}
		var domain, generic, audits, outbox int
		if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_reconciliation_commands WHERE usage_request_id=$1),(SELECT count(*) FROM token_command_results WHERE command_reference=$2),(SELECT count(*) FROM token_audit_logs WHERE summary->>'command_reference'=$3),(SELECT count(*) FROM outbox_events WHERE payload->>'command_reference'=$3)`, input.Envelope.TargetResourceID, result.CommandReference, result.CommandReference.String()).Scan(&domain, &generic, &audits, &outbox); err != nil || domain != 1 || generic != 1 || audits != 2 || outbox != 1 {
			t.Fatalf("atomic receipt/audit differs: domain=%d generic=%d audit=%d outbox=%d err=%v", domain, generic, audits, outbox, err)
		}
	})
	t.Run("cas-conflict-rolls-back-result-and-domain", func(t *testing.T) {
		input := makeInput(t)
		if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET updated_at=updated_at+interval '1 second' WHERE id=$1`, input.Envelope.TargetResourceID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.ExecuteRuntimeTokenCommand(selected("ACTIVE", 1), input); !errors.Is(err, ErrCatalogVersionConflict) {
			t.Fatalf("stale CAS accepted: %v", err)
		}
		assertNoUsageGovernanceMutation(t, r, ctx, input)
	})
	t.Run("customer-debit-prohibited-cannot-queue", func(t *testing.T) {
		input := makeInput(t)
		if _, err := r.pool.Exec(ctx, `INSERT INTO usage_platform_loss_reviews(id,usage_request_id,cause_code,created_at) VALUES($1,$2,'CONTROLLED_NO_CUSTOMER_DEBIT',clock_timestamp())`, uuid.New(), input.Envelope.TargetResourceID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.ExecuteRuntimeTokenCommand(selected("ACTIVE", 1), input); !errors.Is(err, ErrCatalogVersionConflict) {
			t.Fatalf("prohibited usage queued: %v", err)
		}
		assertNoUsageGovernanceMutation(t, r, ctx, input)
	})
	t.Run("safe-release-denies-old-selection", func(t *testing.T) {
		input := makeInput(t)
		transition("SAFE_ROLLBACK", 2)
		if _, _, err := r.ExecuteRuntimeTokenCommand(selected("ACTIVE", 1), input); !errors.Is(err, ErrTokenRouteActivationChanged) {
			t.Fatalf("old epoch mutation accepted: %v", err)
		}
		assertNoUsageGovernanceMutation(t, r, ctx, input)
	})
}
func assertNoUsageGovernanceMutation(t *testing.T, r *PostgresRepository, ctx context.Context, input runtimeTokenCommandBegin) {
	t.Helper()
	var domain, generic int
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_reconciliation_commands WHERE usage_request_id=$1),(SELECT count(*) FROM token_command_results WHERE actor_user_id=$2 AND idempotency_key_digest=$3)`, input.Envelope.TargetResourceID, input.ActorUserID, input.KeyDigest).Scan(&domain, &generic); err != nil || domain != 0 || generic != 0 {
		t.Fatalf("rejected command left mutation: domain=%d generic=%d err=%v", domain, generic, err)
	}
}
