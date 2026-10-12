package account

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
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

func seedTokenOwnerCandidateActor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role, suffix string) integrationActor {
	t.Helper()
	actor := integrationActor{ID: uuid.New(), Issuer: "https://issuer.integration.example", Subject: "owner-candidate-" + suffix}
	now := time.Now().UTC().Add(-time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,created_at,updated_at) VALUES($1,'ACTIVE',$2,generate_unique_user_code(),$3,$3)`, actor.ID, suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, actor.Issuer, actor.Subject, actor.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,grant_source,starts_at,created_at) VALUES($1,$2,$3,'INTEGRATION_TEST','INTEGRATION_TEST',$4,$4)`, uuid.New(), actor.ID, role, now); err != nil {
		t.Fatal(err)
	}
	return actor
}

func approvalAdvancementClosedSnapshot(commandType string) (string, string, string, string, string, string, bool) {
	raw, err := os.ReadFile("../platform/approvalencoding/testdata/golden.json")
	if err != nil {
		return "", "", "", "", "", "", false
	}
	var fixture struct {
		Vectors []struct {
			CommandType string `json:"command_type"`
			Legacy      struct {
				TargetResourceType string `json:"target_resource_type"`
				TargetResourceID   string `json:"target_resource_id"`
				Normalized         string `json:"normalized_json"`
				Critical           string `json:"critical_json"`
				Financial          string `json:"financial_json"`
			} `json:"legacy"`
			CommandDigest string `json:"command_digest"`
		} `json:"proof_vectors"`
	}
	if json.Unmarshal(raw, &fixture) != nil {
		return "", "", "", "", "", "", false
	}
	for _, vector := range fixture.Vectors {
		if vector.CommandType != commandType {
			continue
		}
		return vector.Legacy.TargetResourceType, vector.Legacy.TargetResourceID, vector.Legacy.Normalized,
			vector.Legacy.Critical, vector.Legacy.Financial, vector.CommandDigest, true
	}
	if commandType != "ENTITLEMENT_COMPONENT_ADJUSTMENT" {
		return "", "", "", "", "", "", false
	}
	packageID, orderID := uuid.New(), uuid.New()
	normalized := fmt.Sprintf(`{"package_id":%q,"payment_order_id":%q,"reason_code":"ORIGINAL_SPLIT_MISMATCH"}`, packageID, orderID)
	critical := fmt.Sprintf(`{"package_id":%q,"wallet_id":%q,"wallet_version":1,"payment_order_id":%q,"payment_transaction_id":%q,"source_component_id":%q,"source_lot_id":%q,"target_component_id":null,"target_lot_id":null,"source_type":"BASE","target_type":"BONUS","amount_microcredits":20,"source_evidence_digest":%q,"facts_digest":%q,"policy_version":2}`,
		packageID, uuid.New(), orderID, uuid.New(), uuid.New(), uuid.New(), strings.Repeat("a", 64), strings.Repeat("b", 64))
	financial := `{"package_total_microcredits":100,"source_original_microcredits":100,"target_original_microcredits":0,"effective_total_microcredits":100}`
	return "ENTITLEMENT_PACKAGE", packageID.String(), normalized, critical, financial, strings.Repeat("cd", 32), true
}

func seedAdvancementRequest(t *testing.T, ctx context.Context, repository *PostgresRepository, requester uuid.UUID, policy approvalpolicy.Policy, status string) uuid.UUID {
	return seedAdvancementRequestWithSnapshot(t, ctx, repository, requester, policy, status, false)
}

func seedAdvancementRequestWithSnapshot(t *testing.T, ctx context.Context, repository *PostgresRepository, requester uuid.UUID, policy approvalpolicy.Policy, status string, forceClosedSnapshot bool) uuid.UUID {
	t.Helper()
	binding, version, _, err := strongPlatformAuthorization(ctx, repository.pool, requester, policy.RequesterPermission)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	now := time.Now().UTC()
	var submitted, deadline *time.Time
	if status != "DRAFT" {
		submitted = &now
	}
	if status == "APPROVED" || status == "EXECUTING" || status == "EXECUTED" || status == "EXECUTION_FAILED" {
		value := now.Add(5 * time.Minute)
		deadline = &value
	}
	targetType, targetID := "TEST", id.String()
	normalized, critical, financial := "{}", "{}", "{}"
	fingerprint := strings.Repeat("ab", 32)
	if (status == "APPROVED" || forceClosedSnapshot) && repository.approvalWriterPool != nil {
		if closedTargetType, closedTargetID, closedNormalized, closedCritical, closedFinancial, closedFingerprint, supported := approvalAdvancementClosedSnapshot(policy.CommandType); supported {
			targetType, targetID, normalized, critical, financial, fingerprint = closedTargetType, closedTargetID, closedNormalized, closedCritical, closedFinancial, closedFingerprint
		}
	}
	_, err = repository.pool.Exec(ctx, `INSERT INTO approval_requests
		(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,
		idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,
		redacted_evidence,expected_impact,status,version,expires_at,submitted_at,execution_deadline,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,decode($9,'hex'),$10,$11,'historical approval containment',
		'[]','{}',$12,1,$13,$14,$15,$16,$16)`, id, policy.CommandType, policy.PolicyVersion, requester, binding, version,
		id.String(), normalized, fingerprint, targetType, targetID, status, now.Add(time.Hour), submitted, deadline, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.pool.Exec(ctx, `INSERT INTO approval_request_snapshots
		(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at)
		VALUES($1,$2,$3,$4,'{}','{}',$5)`, id, policy.SnapshotSchemaVersion, critical, financial, now)
	if err != nil {
		t.Fatal(err)
	}
	if status == "APPROVED" && repository.approvalWriterPool != nil {
		if _, _, _, _, _, _, supported := approvalAdvancementClosedSnapshot(policy.CommandType); !supported {
			return id
		}
		tx, err := repository.approvalWriterPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		control, err := LockAdminApprovalControlShared(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		authorization, err := appendLegacyApprovalAuthorization(ctx, tx, control, id, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		publishLegacyAuthorizationEvent(t, ctx, repository, authorization.EventID)
	}
	return id
}

func publishLegacyAuthorizationEvent(t *testing.T, ctx context.Context, repository *PostgresRepository, eventID uuid.UUID) {
	t.Helper()
	if _, err := repository.pool.Exec(ctx, `UPDATE outbox_events SET locked_by='legacy-fixture-relay',locked_until=clock_timestamp()+interval '30 seconds'
WHERE event_id=$1 AND published_at IS NULL`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE outbox_events SET locked_by=NULL,locked_until=NULL,attempts=attempts+1,published_at=occurred_at,last_error=NULL
WHERE event_id=$1 AND published_at IS NULL`, eventID); err != nil {
		t.Fatal(err)
	}
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
