package token

import (
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

// This predecessor regression runs HEAD plus its narrow SQL fix. It requires
// no unpublished activation code or modified application integration helpers.
func TestApprovedCredentialRevokeAuditPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repo := newTokenQueryIntegrationRepository(t, ctx, tokenPaginationIntegrationDatabaseURL(t))
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo.now = func() time.Time { return now }
	for _, scenario := range []string{"ACTIVE", "RETIRED", "late_outbox_failure"} {
		t.Run(scenario, func(t *testing.T) {
			actor, account, credential, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			status := scenario
			if scenario == "late_outbox_failure" {
				status = "RETIRED"
			}
			if _, err := repo.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Revoke audit test','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($6,$1,'INTEGRATION','https://provider.example','fixture'::bytea,'fixture-key',$7,1,$8,ARRAY['model-a'],$5,$4,$5,'VALID');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($9,$1,'INTEGRATION','https://provider.example','other'::bytea,'fixture-key',$10,2,'RETIRED',ARRAY['model-a'],$5,$4,$5,'VALID')`, pgx.QueryExecModeSimpleProtocol, account, uuid.New(), uuid.New(), actor, now, credential, strings.Repeat("b", 64), status, other, strings.Repeat("c", 64)); err != nil {
				t.Fatal(err)
			}
			evidence, err := repo.TokenCredentialImpact(ctx, account, &credential)
			if err != nil {
				t.Fatal(err)
			}
			critical, err := credentialRevokeCriticalFromEvidence(account, credential, evidence)
			if err != nil {
				t.Fatal(err)
			}
			expected, _ := json.Marshal(critical)
			command, _ := json.Marshal(credentialRevokeCommand{TokenAccountID: account, ExpectedAccountVersion: 1, CredentialID: credential, CredentialVersion: 1})
			const originalReason = "revoke after independent original approval review"
			envelope := TokenCommandEnvelope{CommandType: "TOKEN_CREDENTIAL_REVOKE", Reason: originalReason, TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credential.String(), ExpectedVersions: expected, PrecheckFingerprint: strings.Repeat("9", 64), Command: command}
			approvalID := uuid.New()
			fingerprint := strings.Repeat("8", 64)
			pending, created, err := repo.BeginTokenCommand(ctx, tokenCommandBegin{Envelope: envelope, ActorUserID: actor, ActorPermission: "platform.tokens.credentials.revoke.request", AuthorizationVersion: 3, IdempotencyKey: "approval:" + approvalID.String() + ":TOKEN_CREDENTIAL_REVOKE:" + fingerprint, AuditRequestID: "revoke-audit-integration", ApprovalRequestID: approvalID, ApprovalFingerprint: fingerprint, BeforeState: expected})
			if err != nil || !created {
				t.Fatalf("begin created=%v err=%v", created, err)
			}
			executor := NewPostgresApprovedCredentialExecutor(repo)
			if _, err = executor.PrecheckApprovedTokenCommand(ctx, envelope.CommandType, envelope); err != nil {
				t.Fatal(err)
			}
			before := revokeAuditFacts(t, ctx, repo, account, pending.CommandReference)
			var otherBefore string
			if err = repo.pool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM provider_credentials c WHERE id=$1`, other).Scan(&otherBefore); err != nil {
				t.Fatal(err)
			}
			if _, err = executor.ExecuteApprovedCredentialRevoke(ctx, pending.CommandReference, uuid.New(), envelope); !errors.Is(err, ErrForbidden) {
				t.Fatalf("other actor=%v", err)
			}
			if revokeAuditFacts(t, ctx, repo, account, pending.CommandReference) != before {
				t.Fatal("other actor changed facts")
			}
			for _, update := range []string{`status='REVOKED',revoked_at=$2,revocation_reason='valid reason'`, `status='REVOKED',revoked_at=$2,revoked_by=$3,revocation_reason=' '`} {
				tx, err := repo.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var writeErr error
				if strings.Contains(update, "$3") {
					_, writeErr = tx.Exec(ctx, `UPDATE provider_credentials SET `+update+` WHERE id=$1`, credential, now, actor)
				} else {
					_, writeErr = tx.Exec(ctx, `UPDATE provider_credentials SET `+update+` WHERE id=$1`, credential, now)
				}
				_ = tx.Rollback(ctx)
				var pgError *pgconn.PgError
				if !errors.As(writeErr, &pgError) || pgError.Code != "23514" || pgError.ConstraintName != "provider_credentials_revocation_audit" {
					t.Fatalf("original revocation constraint not enforced: %v", writeErr)
				}
			}
			if scenario == "late_outbox_failure" {
				if _, err = repo.pool.Exec(ctx, `CREATE FUNCTION reject_revoke_test_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='token.management-command.executed.v1' THEN RAISE EXCEPTION 'isolated late revoke outbox failure' USING ERRCODE='P0001'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_revoke_test_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION reject_revoke_test_outbox()`); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := repo.pool.Exec(ctx, `DROP TRIGGER reject_revoke_test_outbox ON outbox_events; DROP FUNCTION reject_revoke_test_outbox()`); err != nil {
						t.Error(err)
					}
				}()
				_, err = executor.ExecuteApprovedCredentialRevoke(ctx, pending.CommandReference, actor, envelope)
				var lateFailure *pgconn.PgError
				if !errors.As(err, &lateFailure) || lateFailure.Code != "P0001" || lateFailure.Message != "isolated late revoke outbox failure" {
					t.Fatalf("expected exact late Outbox failure, got %v", err)
				}
				if revokeAuditFacts(t, ctx, repo, account, pending.CommandReference) != before {
					t.Fatal("late failure left partial credential/version/result/audit/outbox facts")
				}
				return
			}
			// Even an internal retry supplying new text cannot replace the reason
			// frozen on the already locked durable command.
			changedReason := envelope
			changedReason.Reason = "retry text must not replace the original decision"
			result, err := executor.ExecuteApprovedCredentialRevoke(ctx, pending.CommandReference, actor, changedReason)
			if err != nil || result.Status != "SUCCEEDED" {
				t.Fatalf("revoke status=%s err=%v", result.Status, err)
			}
			var revokedBy uuid.UUID
			var revokedAt time.Time
			var reason, gotStatus, otherStatus string
			var version, domainAudits, commandAudits, outbox int
			if err = repo.pool.QueryRow(ctx, `SELECT p.status,p.revoked_at,p.revoked_by,p.revocation_reason,a.management_version,(SELECT status FROM provider_credentials WHERE id=$3),(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_CREDENTIAL_REVOKED' AND resource_id=$1),(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_CREDENTIAL_REVOKE' AND summary->>'command_reference'=($4::uuid)::text),(SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1' AND payload->>'command_reference'=($4::uuid)::text) FROM provider_credentials p JOIN token_accounts a ON a.id=p.token_account_id WHERE p.id=$1 AND a.id=$2`, credential, account, other, pending.CommandReference).Scan(&gotStatus, &revokedAt, &revokedBy, &reason, &version, &otherStatus, &domainAudits, &commandAudits, &outbox); err != nil {
				t.Fatal(err)
			}
			if gotStatus != "REVOKED" || revokedBy != actor || !revokedAt.Equal(now) || reason != originalReason || version != 2 || otherStatus != "RETIRED" || domainAudits != 1 || commandAudits != 1 || outbox != 1 {
				t.Fatalf("revoke facts status=%s actorMatch=%v timeMatch=%v reasonMatch=%v version=%d other=%s audit=%d/%d outbox=%d", gotStatus, revokedBy == actor, revokedAt.Equal(now), reason == originalReason, version, otherStatus, domainAudits, commandAudits, outbox)
			}
			succeeded := revokeAuditFacts(t, ctx, repo, account, pending.CommandReference)
			var otherAfter string
			if err = repo.pool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM provider_credentials c WHERE id=$1`, other).Scan(&otherAfter); err != nil || otherBefore != otherAfter {
				t.Fatalf("other credential changed: %v", err)
			}
			replayed, err := executor.ExecuteApprovedCredentialRevoke(ctx, pending.CommandReference, actor, envelope)
			if err != nil || replayed.CommandReference != result.CommandReference || replayed.Status != "SUCCEEDED" {
				t.Fatalf("replay=%+v err=%v", replayed, err)
			}
			if revokeAuditFacts(t, ctx, repo, account, pending.CommandReference) != succeeded {
				t.Fatal("replay changed original persisted facts")
			}
		})
	}
}

func revokeAuditFacts(t *testing.T, ctx context.Context, r *PostgresRepository, account, reference uuid.UUID) string {
	t.Helper()
	var facts string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('account',(SELECT to_jsonb(a) FROM token_accounts a WHERE id=$1),'credentials',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM provider_credentials c WHERE token_account_id=$1),'command',(SELECT to_jsonb(c) FROM token_command_results c WHERE command_reference=$2),'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM token_audit_logs a),'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM outbox_events o))::text`, account, reference).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}
