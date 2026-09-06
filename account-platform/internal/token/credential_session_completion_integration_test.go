package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This narrow predecessor regression uses only committed repository helpers.
// It does not install the APT-138 production activation store or bypass any
// production database trigger. External validation/identity are fixture seams.
func TestCredentialSessionCompletionPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, tokenPaginationIntegrationDatabaseURL(t))
	secrets, err := NewLocalAESProvider("completion-test-key", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	repository.secrets = secrets
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository.now = func() time.Time { return now }
	actor := uuid.New()
	authority := CredentialIsolationAuthoritySnapshot{ActorID: actor, Issuer: "https://identity.example", Subject: actor.String(), AuthorizationVersion: 7, GateEpoch: 11}
	repository.WithCredentialIsolationAuthorityGuard(credentialAuthorityGuardFunc(func(_ context.Context, _ pgx.Tx, observed CredentialIsolationAuthoritySnapshot) error {
		if observed != authority {
			return ErrTokenRouteActivationChanged
		}
		return nil
	}))
	ctx = withCredentialIsolationAuthority(ctx, authority)
	policy, err := NewProviderURLPolicy("test", []string{"provider.example"})
	if err != nil {
		t.Fatal(err)
	}
	const origin = "https://credentials.example"
	browser, csrf := strings.Repeat("b", 64), strings.Repeat("c", 64)
	ready := func(t *testing.T, action string) (credentialIsolationSession, uuid.UUID) {
		t.Helper()
		account, owner, wallet, oldCredential := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		if _, err := repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Completion test','ACTIVE',$4,$5,$5,'PLATFORM'); INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5);`, pgx.QueryExecModeSimpleProtocol, account, owner, wallet, actor, now); err != nil {
			t.Fatal(err)
		}
		version := 0
		if action == "ROTATE" {
			version = 1
			if _, err := repository.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status,activated_at) VALUES($1,$2,'NEW_API','https://provider.example','old'::bytea,'old-key','old-fingerprint',1,'ACTIVE',ARRAY['model-a'],$3,$4,$3,'VALID',$3)`, oldCredential, account, now, actor); err != nil {
				t.Fatal(err)
			}
		}
		launch, err := repository.CreateCredentialLaunch(ctx, account, actor, CredentialLaunchRequest{Action: action, ExpectedAccountVersion: 1, ExpectedCredentialVersion: version, Slot: "PRIMARY", Reason: "verify isolated session completion transaction"}, "completion-"+uuid.NewString(), "completion-request")
		if err != nil {
			t.Fatal(err)
		}
		session, err := repository.ConsumeCredentialLaunch(ctx, launch.LaunchID, origin, browser, strings.Repeat("s", 64), strings.Repeat("n", 64), strings.Repeat("v", 64), csrf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repository.ConfirmCredentialSession(ctx, session.ID, browser); err != nil {
			t.Fatal(err)
		}
		if err = repository.AuthenticateCredentialSession(ctx, session.ID, actor, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		return session, oldCredential
	}
	sessionJSON := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		var value string
		if err := repository.pool.QueryRow(ctx, `SELECT to_jsonb(s)::text FROM credential_isolation_sessions s WHERE id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	allFacts := func(t *testing.T) [6]string {
		t.Helper()
		var result [6]string
		for index, table := range []string{"token_accounts", "provider_credentials", "credential_isolation_sessions", "credential_launches", "credential_isolation_results", "token_audit_logs"} {
			if err := repository.pool.QueryRow(ctx, "SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM "+pgx.Identifier{table}.Sanitize()+" t").Scan(&result[index]); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	complete := func(t *testing.T, session credentialIsolationSession) (CredentialIsolationResult, error) {
		t.Helper()
		secret := []byte("isolated-completion-test-secret")
		result, err := repository.CompleteCredentialSession(ctx, session.ID, origin, browser, csrf, "NEW_API", "https://provider.example", []string{"model-a"}, secret, acceptingCredentialValidator{}, policy)
		for _, value := range secret {
			if value != 0 {
				t.Fatal("caller secret buffer was not cleared")
			}
		}
		return result, err
	}
	for _, action := range []string{"ENROLL", "ROTATE"} {
		t.Run(action, func(t *testing.T) {
			session, oldCredential := ready(t, action)
			other, _ := ready(t, "ENROLL")
			otherBefore := sessionJSON(t, other.ID)
			result, err := complete(t, session)
			expectedVersion, expectedCredentials, expectedAudits := 1, 1, 3
			if action == "ROTATE" {
				expectedVersion, expectedCredentials, expectedAudits = 2, 2, 5
			}
			if err != nil || result.Status != "SUCCEEDED" || result.CredentialID == nil || result.CredentialVersion == nil || *result.CredentialVersion != expectedVersion {
				t.Fatalf("%s completion failed: %v", action, err)
			}
			var sessionStatus, launchStatus, keyID string
			var ciphertextLength, accountVersion, credentialCount, activeCount, completionAudits, domainAudits int
			if err = repository.pool.QueryRow(ctx, `SELECT s.status,l.status,octet_length(s.pkce_verifier_ciphertext),s.key_id,a.management_version,
			 (SELECT count(*) FROM provider_credentials WHERE token_account_id=a.id),
			 (SELECT count(*) FROM provider_credentials WHERE token_account_id=a.id AND status='ACTIVE'),
			 (SELECT count(*) FROM token_audit_logs WHERE action=$2::text AND summary->>'token_account_id'=(a.id)::text),
			 (SELECT count(*) FROM token_audit_logs WHERE action IN ('TOKEN_CREDENTIAL_CANDIDATE_CREATED','TOKEN_CREDENTIAL_RETIRING','TOKEN_CREDENTIAL_ACTIVATED','TOKEN_CREDENTIAL_RETIRED','TOKEN_CREDENTIAL_ENROLL_COMPLETED','TOKEN_CREDENTIAL_ROTATE_COMPLETED') AND summary->>'token_account_id'=(a.id)::text)
			 FROM credential_isolation_sessions s JOIN credential_launches l ON l.id=s.launch_id JOIN token_accounts a ON a.id=l.token_account_id WHERE s.id=$1`, session.ID, "TOKEN_CREDENTIAL_"+action+"_COMPLETED").Scan(&sessionStatus, &launchStatus, &ciphertextLength, &keyID, &accountVersion, &credentialCount, &activeCount, &completionAudits, &domainAudits); err != nil || sessionStatus != "COMPLETED" || launchStatus != "COMPLETED" || ciphertextLength != 0 || keyID != "" || accountVersion != 2 || credentialCount != expectedCredentials || activeCount != 1 || completionAudits != 1 || domainAudits != expectedAudits {
				t.Fatalf("%s completion facts invalid: session=%s launch=%s pkce_length=%d account_version=%d credentials=%d active=%d completion_audits=%d domain_audits=%d err=%v", action, sessionStatus, launchStatus, ciphertextLength, accountVersion, credentialCount, activeCount, completionAudits, domainAudits, err)
			}
			if action == "ROTATE" {
				var status string
				if err = repository.pool.QueryRow(ctx, `SELECT status FROM provider_credentials WHERE id=$1`, oldCredential).Scan(&status); err != nil || status != "RETIRED" {
					t.Fatal("old credential history was not retained as RETIRED")
				}
			}
			if sessionJSON(t, other.ID) != otherBefore {
				t.Fatal("completing one session changed another READY session")
			}
			beforeReplay := allFacts(t)
			if _, err = complete(t, session); !errors.Is(err, ErrForbidden) {
				t.Fatalf("completed one-time session resubmission must remain forbidden: %v", err)
			}
			queried, err := repository.CredentialIsolationResult(ctx, actor, result.ResultRef)
			originalJSON, _ := json.Marshal(result)
			queriedJSON, _ := json.Marshal(queried)
			if err != nil || !bytes.Equal(originalJSON, queriedJSON) {
				t.Fatal("read-only result recovery changed the original safe result")
			}
			if allFacts(t) != beforeReplay {
				t.Fatal("completed resubmission or result lookup changed persisted facts")
			}
		})
	}
	t.Run("late_failure_rolls_back_all_effects", func(t *testing.T) {
		session, _ := ready(t, "ENROLL")
		before := allFacts(t)
		// A fixture-only final-audit failure occurs after the session clearing
		// statement. Existing production constraints/triggers remain enabled.
		if _, err := repository.pool.Exec(ctx, `CREATE FUNCTION test_fail_completion_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='TOKEN_CREDENTIAL_ENROLL_COMPLETED' THEN RAISE EXCEPTION 'fixture final audit failure' USING ERRCODE='P0001'; END IF; RETURN NEW; END $$; CREATE TRIGGER test_completion_failure BEFORE INSERT ON token_audit_logs FOR EACH ROW EXECUTE FUNCTION test_fail_completion_audit();`, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatal(err)
		}
		_, err := complete(t, session)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("expected the fixture final-audit failure, got %v", err)
		}
		if allFacts(t) != before {
			t.Fatal("late failure did not roll back credential/session/account/result/audit facts")
		}
	})
}
