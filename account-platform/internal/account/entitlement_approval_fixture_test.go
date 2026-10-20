package account

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Installation reuses the owned Account fixture and its official Up/Verify;
// runtime, writer, relay and operator each connect as their actual login role.
func newEntitlementApprovalFixture(t *testing.T, ctx context.Context, raw string) (*PostgresRepository, *pgx.Conn, *ApprovalRelayStore, string) {
	t.Helper()
	t.Setenv("PINVOU_TEST_ACCOUNT_DATABASE_URL", raw)
	f := retainedWalletFenceActors(t)
	repository := f.repository(t)
	repository.approvalPresenterReady = approvalPolicyPresenterReady
	config := repository.pool.Config().ConnConfig.Copy()
	config.User = "account_migrator"
	actorURL := func(role string) string {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
			t.Fatal("owned approval fixture requires PostgreSQL URL")
		}
		parsed.User = url.User(role)
		parsed.Path = "/" + config.Database
		return parsed.String()
	}
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("open genuine Account operator")
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	if _, err := approvalpolicy.VerifyPolicySet(ctx, repository.pool); err != nil {
		t.Fatalf("official policy verification: %v", err)
	}
	version := int64(1)
	for _, step := range []FreshApprovalStep{FreshApprovalQuiesce, FreshApprovalFence, FreshApprovalResources, FreshApprovalActivate, FreshApprovalResume} {
		control, e := RunFreshApprovalStep(ctx, actorURL("account_migrator"), version, step, func(context.Context, AdminApprovalControl) (json.RawMessage, error) {
			return json.RawMessage(`{"controlled_broker_check":true}`), nil
		})
		if e != nil {
			t.Fatalf("official fresh approval transition failed: step=%s version=%d", step, version)
		}
		version = control.Version
	}
	if err := repository.ConfigureApprovalWriter(ctx, actorURL("account_approval_writer")); err != nil {
		t.Fatal("genuine writer readiness failed")
	}
	t.Cleanup(func() {
		if repository.approvalWriterPool != nil {
			repository.approvalWriterPool.Close()
		}
	})
	relay, err := NewApprovalRelayStore(ctx, actorURL("account_approval_relay"))
	if err != nil {
		t.Fatal("genuine relay readiness failed")
	}
	t.Cleanup(relay.Close)
	if _, err := connection.Exec(ctx, "SET ROLE account_service"); err != nil {
		t.Fatal(err)
	}
	return repository, connection, relay, actorURL("account_migrator")
}

func signEntitlementFixtureJWT(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "approval-executor"})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

type entitlementFixtureActor struct {
	ID      uuid.UUID
	Subject string
}

func seedEntitlementApprovalActor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role, label string) entitlementFixtureActor {
	t.Helper()
	actor := entitlementFixtureActor{uuid.New(), label + "-" + uuid.NewString()}
	var digits strings.Builder
	digits.WriteString("USR")
	for _, b := range actor.ID {
		digits.WriteByte('1' + b%9)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO users(id,status,display_name,username,user_code,created_at,updated_at) VALUES($1,'ACTIVE',$2,$2,$3||user_code_checksum($3),now(),now())`, actor.ID, actor.Subject, digits.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('https://entitlement-owned.invalid',$1,$2,now(),now())`, actor.Subject, actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,$3,'ACCOUNT',now()-interval '1 day',now())`, uuid.New(), actor.ID, role); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return actor
}

func entitlementApprovalPrincipal(actor entitlementFixtureActor) identity.Principal {
	return identity.Principal{Issuer: "https://entitlement-owned.invalid", Subject: actor.Subject, SessionID: actor.Subject + "-session", AuthenticatedAt: time.Now().UTC()}
}

func assertEntitlementRequestAbsent(t *testing.T, ctx context.Context, repo *PostgresRepository, user uuid.UUID, key string) {
	t.Helper()
	var count int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM approval_requests WHERE requester_user_id=$1 AND idempotency_key=$2`, user, key).Scan(&count); err != nil || count != 0 {
		t.Fatal("inactive activation created approval facts")
	}
}

func seedEntitlementAdvancementRequest(t *testing.T, ctx context.Context, repo *PostgresRepository, user uuid.UUID, policy approvalpolicy.Policy, status string, frozen approvalencoding.LegacyProofInput) uuid.UUID {
	t.Helper()
	id := uuid.New()
	binding, authorizationVersion, _, err := strongPlatformAuthorization(ctx, repo.pool, user, policy.RequesterPermission)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.pool.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,expires_at,submitted_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$9,$6,$10,decode(repeat('00',32),'hex'),'ENTITLEMENT_PACKAGE',$7,'controlled historical draft','[]','{}',$8,now()+interval '1 hour',CASE WHEN $8='SUBMITTED' THEN now() ELSE NULL END,now(),now())`, id, policy.CommandType, policy.PolicyVersion, user, binding, "historical-"+id.String(), frozen.TargetResourceID, status, authorizationVersion, frozen.NormalizedCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO approval_request_snapshots(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at) VALUES($1,$2,$3,$4,'{}','{}',now())`, id, policy.SnapshotSchemaVersion, frozen.CriticalResourceVersions, frozen.FinancialSnapshot); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertEntitlementRequestState(t *testing.T, ctx context.Context, repo *PostgresRepository, id uuid.UUID, expected string, version int64) {
	t.Helper()
	var status string
	var actual int64
	if err := repo.pool.QueryRow(ctx, `SELECT status,version FROM approval_requests WHERE id=$1`, id).Scan(&status, &actual); err != nil || status != expected || actual != version {
		t.Fatal("inactive gate changed historical approval")
	}
}
