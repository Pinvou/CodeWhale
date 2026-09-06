package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This regression deliberately uses the default extended query protocol for
// the production claim UPDATE; simple protocol would hide parameter inference.
func TestCredentialProbeUnknownCodeIsExact(t *testing.T) {
	if !validStableTokenErrorCode("CREDENTIAL_PROBE_OUTCOME_UNKNOWN") {
		t.Fatal("fixed unknown probe outcome was rejected")
	}
	for _, code := range []string{"credential_probe_outcome_unknown", " CREDENTIAL_PROBE_OUTCOME_UNKNOWN", "CREDENTIAL_PROBE_OUTCOME_UNKNOWN ", "PREFIX_CREDENTIAL_PROBE_OUTCOME_UNKNOWN", "CREDENTIAL_PROBE_OUTCOME_UNKNOWN_SUFFIX", "CREDENTIAL_CHECK_FAILED", "CREDENTIAL_PRIVATE_VALUE", "UPSTREAM_TOKEN_ABC123", "PROVIDER_KEY_ABC123", "SECRET_VALUE", "PASSWORD_VALUE"} {
		if validStableTokenErrorCode(code) {
			t.Fatalf("noncanonical or sensitive error code accepted: %q", code)
		}
	}
}

func TestCredentialProbeLeasePostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, tokenPaginationIntegrationDatabaseURL(t))
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	var migration migrations.Migration
	for _, item := range loaded {
		if item.Version == 30 && item.Name == "credential_probe_unknown_code" {
			migration = item
		}
	}
	if migration.UpSQL == "" || migration.DownSQL == "" {
		t.Fatal("credential probe compatibility migration missing")
	}
	apply := func(sql string) error {
		t.Helper()
		tx, err := repository.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, sql); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err = apply(migration.DownSQL); err != nil {
		t.Fatalf("empty history down: %v", err)
	}
	if err = apply(migration.UpSQL); err != nil {
		t.Fatalf("forward reapply: %v", err)
	}
	secrets, err := NewLocalAESProvider("probe-lease-test-key", bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	repository.secrets = secrets
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository.now = func() time.Time { return now }
	actor, account, credential := uuid.New(), uuid.New(), uuid.New()
	if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Probe lease test','ACTIVE',$4,$5,$5,'PLATFORM')`, account, uuid.New(), uuid.New(), actor, now); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("isolated-probe-lease-test-secret")
	ciphertext, err := secrets.Encrypt(plaintext, []byte(account.String()+":"+credential.String()))
	clear(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status,activated_at) VALUES($1,$2,'NEW_API','https://provider.example',$3,$4,$7,1,'ACTIVE',ARRAY['model-a'],$5,$6,$5,'VALID',$5)`, credential, account, ciphertext, secrets.KeyID(), now, actor, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	newCommand := func() uuid.UUID {
		t.Helper()
		reference := uuid.New()
		if _, err := repository.pool.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,audit_request_id) VALUES($1,'TOKEN_CREDENTIAL_VALIDATE',2,$2,'platform.tokens.credentials.validate',7,$3,$4,$4,'PROVIDER_CREDENTIAL',$5,'verify one durable credential probe',$6,$4,'PENDING','{}','probe-lease-test')`, reference, actor, strings.ReplaceAll(reference.String(), "-", "")+strings.ReplaceAll(reference.String(), "-", ""), strings.Repeat("a", 64), credential.String(), []byte(`{"expected_account_version":1,"credential_version":1}`)); err != nil {
			t.Fatal(err)
		}
		return reference
	}
	claim := func(reference, caller uuid.UUID) (bool, claimedCredentialValidation, []byte, error) {
		return repository.claimCredentialValidationProbe(ctx, reference, account, credential, caller, 1, 1)
	}
	reference := newCommand()
	for _, code := range []string{"CREDENTIAL_PROBE_OUTCOME_UNKNOWN_SUFFIX", "PREFIX_CREDENTIAL_PROBE_OUTCOME_UNKNOWN", "CREDENTIAL_CHECK_FAILED", "PROVIDER_KEY_ABC123", "SECRET_VALUE", "PASSWORD_VALUE", "lowercase", strings.Repeat("A", 121)} {
		tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `UPDATE token_command_results SET status='FAILED',stable_error_code=$2,completed_at=transaction_timestamp() WHERE command_reference=$1`, reference, code)
		_ = tx.Rollback(ctx)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "token_command_results_stable_error_code_check" {
			t.Fatalf("database did not enforce bounded error code: %v", err)
		}
	}
	if _, _, secret, err := claim(reference, uuid.New()); !errors.Is(err, ErrForbidden) || len(secret) != 0 {
		clear(secret)
		t.Fatalf("foreign actor claim err=%v", err)
	}
	var claimedAt, leaseUntil *time.Time
	if err := repository.pool.QueryRow(ctx, `SELECT credential_probe_claimed_at,credential_probe_lease_until FROM token_command_results WHERE command_reference=$1`, reference).Scan(&claimedAt, &leaseUntil); err != nil || claimedAt != nil || leaseUntil != nil {
		t.Fatalf("foreign actor occupied lease: %v", err)
	}
	claimed, _, secret, err := claim(reference, actor)
	secretPresent := len(secret) > 0
	clear(secret)
	if err != nil || !claimed || !secretPresent {
		t.Fatalf("first claim=%v secret present=%v err=%v", claimed, secretPresent, err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT credential_probe_claimed_at,credential_probe_lease_until FROM token_command_results WHERE command_reference=$1`, reference).Scan(&claimedAt, &leaseUntil); err != nil || claimedAt == nil || leaseUntil == nil || !claimedAt.Equal(now) || !leaseUntil.Equal(now.Add(30*time.Second)) {
		t.Fatalf("lease times claimed=%v until=%v err=%v", claimedAt, leaseUntil, err)
	}
	claimed, pending, secret, err := claim(reference, actor)
	secretPresent = len(secret) > 0
	clear(secret)
	if err != nil || claimed || secretPresent || pending.CommandResult.Status != "PENDING" {
		t.Fatalf("duplicate claim=%v pending=%s err=%v", claimed, pending.CommandResult.Status, err)
	}
	completed, err := repository.recordGovernedCredentialValidation(ctx, reference, account, credential, actor, 1, 1, true, "")
	if err != nil || completed.Status != "SUCCEEDED" {
		t.Fatalf("complete status=%s err=%v", completed.Status, err)
	}
	counts := func() [3]int {
		t.Helper()
		var value [3]int
		if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_CREDENTIAL_VALIDATED'),(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_CREDENTIAL_VALIDATE'),(SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1')`).Scan(&value[0], &value[1], &value[2]); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := counts(); got != [3]int{1, 1, 1} {
		t.Fatalf("completion evidence=%v", got)
	}
	claimed, replay, secret, err := claim(reference, actor)
	secretPresent = len(secret) > 0
	clear(secret)
	if err != nil || claimed || secretPresent || replay.CommandResult.Status != "SUCCEEDED" || counts() != [3]int{1, 1, 1} {
		t.Fatalf("terminal replay claimed=%v status=%s err=%v", claimed, replay.CommandResult.Status, err)
	}
	expired := newCommand()
	if _, _, secret, err = claim(expired, actor); err != nil {
		clear(secret)
		t.Fatal(err)
	}
	clear(secret)
	now = now.Add(30 * time.Second)
	validator := &countingCredentialValidator{}
	service := &Service{repository: repository, validator: validator}
	envelope := TokenCommandEnvelope{Command: json.RawMessage(`{"token_account_id":"` + account.String() + `","credential_id":"` + credential.String() + `"}`), ExpectedVersions: json.RawMessage(`{"expected_account_version":1,"credential_version":1}`)}
	for range 2 {
		result, err := service.executeGovernedCredentialValidation(ctx, repository, expired, actor, envelope)
		if err != nil || result.CommandReference != expired || result.Status != "FAILED" || result.StableErrorCode == nil || *result.StableErrorCode != "CREDENTIAL_PROBE_OUTCOME_UNKNOWN" || validator.calls.Load() != 0 {
			t.Fatalf("expired replay status=%s calls=%d err=%v", result.Status, validator.calls.Load(), err)
		}
	}
	if got := counts(); got != [3]int{1, 2, 2} {
		t.Fatalf("expired terminal evidence=%v", got)
	}
	var pgErr *pgconn.PgError
	if err = apply(migration.DownSQL); !errors.As(err, &pgErr) || pgErr.Code != "55000" {
		t.Fatalf("referenced compatibility down was not blocked: %v", err)
	}
	retained, err := repository.TokenCommandResult(ctx, expired, actor)
	if err != nil || retained.Status != "FAILED" || retained.StableErrorCode == nil || *retained.StableErrorCode != "CREDENTIAL_PROBE_OUTCOME_UNKNOWN" || counts() != [3]int{1, 2, 2} {
		t.Fatalf("history changed after blocked down: status=%s err=%v", retained.Status, err)
	}
}
