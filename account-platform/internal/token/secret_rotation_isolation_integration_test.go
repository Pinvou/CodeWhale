package token

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Healthy ciphertext uses real AEAD. Provider failures/hooks are controlled;
// this fixture neither contacts a live KMS nor proves deployed key retirement.
type rotationFixtureSecrets struct {
	old, active            SecretProvider
	decryptErr, encryptErr error
	beforeDecrypt          func()
}

func (p *rotationFixtureSecrets) KeyID() string { return p.active.KeyID() }
func (p *rotationFixtureSecrets) Encrypt(value, aad []byte) ([]byte, error) {
	if p.encryptErr != nil {
		return nil, p.encryptErr
	}
	return p.active.Encrypt(value, aad)
}
func (p *rotationFixtureSecrets) Decrypt(value, aad []byte) ([]byte, error) {
	return p.DecryptWithKeyID(p.KeyID(), value, aad)
}
func (p *rotationFixtureSecrets) DecryptWithKeyID(key string, value, aad []byte) ([]byte, error) {
	if p.beforeDecrypt != nil {
		f := p.beforeDecrypt
		p.beforeDecrypt = nil
		f()
	}
	if p.decryptErr != nil {
		return nil, p.decryptErr
	}
	if key == p.old.KeyID() {
		return p.old.DecryptWithKeyID(key, value, aad)
	}
	return p.active.DecryptWithKeyID(key, value, aad)
}

func TestF04SecretRotationUnconfirmedRowsDoNotStarveHealthyCiphertext(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	old := r.secrets
	active, err := NewLocalAESProvider("controlled-new-key", bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	provider := &rotationFixtureSecrets{old: old, active: active}
	seedCredential := func(n int, bad bool) uuid.UUID {
		t.Helper()
		id := uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", n))
		account, actor := uuid.New(), uuid.New()
		if _, err := r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$4,'ACTIVE',100,$2,$5,$5,'PLATFORM')`, account, actor, uuid.New(), id.String(), time.Now()); err != nil {
			t.Fatal(err)
		}
		cipher, err := old.Encrypt([]byte("controlled-rotation-plaintext"), []byte(account.String()+":"+id.String()))
		if err != nil {
			t.Fatal(err)
		}
		if bad {
			cipher = []byte("unconfirmed-authenticated-ciphertext")
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',ARRAY['model'],$6,$7)`, id, account, cipher, old.KeyID(), strings.Repeat("a", 64), actor, time.Now()); err != nil {
			t.Fatal(err)
		}
		return id
	}
	credentialFacts := func(id uuid.UUID) string {
		t.Helper()
		var facts string
		if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(credential)::text FROM provider_credentials credential WHERE id=$1`, id).Scan(&facts); err != nil {
			t.Fatal(err)
		}
		return facts
	}
	businessFacts := func() string {
		t.Helper()
		var facts string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'accounts',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM token_accounts t),
 'credentials',(SELECT jsonb_agg(to_jsonb(t)-'ciphertext'-'key_id' ORDER BY id) FROM provider_credentials t),
 'usage',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM usage_requests t),
 'preparations',(SELECT jsonb_agg(to_jsonb(t)-'encrypted_envelope'-'key_id'-'envelope_integrity' ORDER BY usage_request_id) FROM usage_dispatch_preparations t),
 'intents',(SELECT jsonb_agg(to_jsonb(t)-'encrypted_envelope'-'key_id'-'envelope_integrity' ORDER BY usage_request_id) FROM usage_dispatch_intents t),
 'events',(SELECT jsonb_agg(to_jsonb(t)-'encrypted_payload'-'key_id'-'payload_integrity' ORDER BY usage_request_id,sequence) FROM usage_stream_events t),
 'results',(SELECT jsonb_agg(to_jsonb(t)-'encrypted_payload'-'key_id'-'payload_integrity' ORDER BY usage_request_id) FROM usage_replay_results t),
 'audit',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM token_audit_logs t),
 'convergence',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM usage_convergence_evidence t))::text`).Scan(&facts); err != nil {
			t.Fatal(err)
		}
		return facts
	}
	rotateSweep := func(batch int) int {
		t.Helper()
		rotated := 0
		for calls := 0; calls < 100; calls++ {
			result, err := r.RotateSecretKey(ctx, batch)
			if err != nil {
				t.Fatal(err)
			}
			if result.Scanned > batch {
				t.Fatal("scan attempt budget exceeded", result)
			}
			rotated += result.Rotated
			if result.SweepComplete {
				return rotated
			}
		}
		t.Fatal("finite captured sweep did not terminate")
		return 0
	}

	t.Run("bad-prefix-multiple-batches-all-five-tables", func(t *testing.T) {
		bad := []uuid.UUID{seedCredential(1, true), seedCredential(2, true), seedCredential(3, true)}
		seedCredential(4, false)
		streamID := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
		// F04 owns the ciphertext storage/rotation tables and AAD contract;
		// the model invocation Replay API is delivered by F05, not this fixture.
		frame, err := old.Encrypt([]byte(`{"text":"controlled-frame"}`), replayAAD(streamID, "event:DELTA", 1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO usage_stream_events(usage_request_id,sequence,event_type,encrypted_payload,key_id,payload_integrity,created_at,expires_at) VALUES($1,1,'DELTA',$2,$3,$4,$5,NULL)`, streamID, frame, old.KeyID(), replayDigest(frame), time.Now()); err != nil {
			t.Fatal(err)
		}
		response, err := old.Encrypt([]byte(`{"text":"controlled-result"}`), replayAAD(streamID, "result:JSON", 1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO usage_replay_results(usage_request_id,response_kind,http_status,content_type,encrypted_payload,key_id,payload_integrity,final_usage,last_sequence,created_at,expires_at) VALUES($1,'JSON',200,'application/json',$2,$3,$4,NULL,1,$5,NULL)`, streamID, response, old.KeyID(), replayDigest(response), time.Now()); err != nil {
			t.Fatal(err)
		}
		intentID := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "HOLD_RESERVED")
		if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='NOT_REQUESTED' WHERE id=$1`, intentID); err != nil {
			t.Fatal(err)
		}
		sealed, err := old.Encrypt([]byte(`{"controlled":"original-intent"}`), replayAAD(intentID, "dispatch", 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO usage_dispatch_intents(usage_request_id,encrypted_envelope,key_id,envelope_integrity,upstream_idempotency_key,created_at) VALUES($1,$2,$3,$4,$5,$6)`, intentID, sealed, old.KeyID(), replayDigest(sealed), "controlled-rotation-intent", time.Now()); err != nil {
			t.Fatal(err)
		}
		before := businessFacts()
		badFacts := map[uuid.UUID]string{}
		for _, id := range bad {
			badFacts[id] = credentialFacts(id)
		}
		r.secrets = provider
		first, err := r.RotateSecretKey(ctx, 1)
		if err != nil || first.Rotated != 0 || first.Scanned != 1 || first.Unconfirmed != 1 || first.Remaining == 0 {
			t.Fatal("bad first row did not preserve explicit uncertainty", first, err)
		}
		if rotated := rotateSweep(1); rotated != 8 {
			t.Fatal("later healthy rows/tables starved", rotated)
		}
		if remaining, err := r.SecretRotationRemaining(ctx); err != nil || remaining != 3 {
			t.Fatal("bad rows disappeared from strict readiness count", remaining, err)
		}
		for _, id := range bad {
			if credentialFacts(id) != badFacts[id] {
				t.Fatal("unconfirmed row identity or ciphertext changed")
			}
		}
		if businessFacts() != before {
			t.Fatal("rotation changed financial, audit or original noncrypto facts")
		}
		var got []byte
		var key, integrity string
		var sequence int64
		var eventType string
		if err := r.pool.QueryRow(ctx, `SELECT sequence,event_type,encrypted_payload,key_id,payload_integrity FROM usage_stream_events WHERE usage_request_id=$1`, streamID).Scan(&sequence, &eventType, &got, &key, &integrity); err != nil {
			t.Fatal(err)
		}
		plain, err := active.DecryptWithKeyID(key, got, replayAAD(streamID, "event:"+eventType, sequence))
		if err != nil || key != active.KeyID() || sequence != 1 || eventType != "DELTA" || string(plain) != `{"text":"controlled-frame"}` || replayDigest(got) != integrity {
			t.Fatal("original stored frame/AAD/integrity did not survive real AEAD rotation", err)
		}
		clear(plain)
		var kind, contentType string
		var httpStatus int
		if err := r.pool.QueryRow(ctx, `SELECT response_kind,http_status,content_type,last_sequence,encrypted_payload,key_id,payload_integrity FROM usage_replay_results WHERE usage_request_id=$1`, streamID).Scan(&kind, &httpStatus, &contentType, &sequence, &got, &key, &integrity); err != nil {
			t.Fatal(err)
		}
		plain, err = active.DecryptWithKeyID(key, got, replayAAD(streamID, "result:"+kind, sequence))
		if err != nil || key != active.KeyID() || kind != "JSON" || httpStatus != 200 || contentType != "application/json" || sequence != 1 || string(plain) != `{"text":"controlled-result"}` || replayDigest(got) != integrity {
			t.Fatal("original stored result/AAD/integrity did not survive real AEAD rotation", err)
		}
		clear(plain)
		if err := r.pool.QueryRow(ctx, `SELECT encrypted_envelope,key_id,envelope_integrity FROM usage_dispatch_intents WHERE usage_request_id=$1`, intentID).Scan(&got, &key, &integrity); err != nil {
			t.Fatal(err)
		}
		plain, err = active.DecryptWithKeyID(key, got, replayAAD(intentID, "dispatch", 0))
		if err != nil || string(plain) != `{"controlled":"original-intent"}` || replayDigest(got) != integrity {
			t.Fatal("intent original authenticated bytes changed", err)
		}
		clear(plain)
	})

	t.Run("provider-decrypt-outage-is-explicit-and-retryable", func(t *testing.T) {
		id := seedCredential(100, false)
		before := credentialFacts(id)
		provider.decryptErr = errors.New("synthetic-provider-unavailable")
		result, err := r.RotateSecretKey(ctx, 100)
		provider.decryptErr = nil
		if err != nil || result.Unconfirmed != 4 || result.Rotated != 0 || result.Remaining != 4 || !result.SweepComplete || credentialFacts(id) != before {
			t.Fatal("provider failure falsely completed/modified records or spun", result, err)
		}
		if rotateSweep(100) != 1 {
			t.Fatal("provider recovery did not retry original row")
		}
	})

	t.Run("encrypt-failure-does-not-commit-or-advance-scan", func(t *testing.T) {
		id := seedCredential(110, false)
		before := credentialFacts(id)
		scan := r.secretRotation.scan
		failure := errors.New("synthetic-encrypt-failure")
		provider.encryptErr = failure
		_, err := r.RotateSecretKey(ctx, 100)
		provider.encryptErr = nil
		if !errors.Is(err, failure) || credentialFacts(id) != before || r.secretRotation.scan != scan {
			t.Fatal("encrypt failure swallowed or changed cursor/data", err)
		}
		if rotateSweep(100) != 1 {
			t.Fatal("encrypt retry did not recover")
		}
	})

	t.Run("crypto-outside-row-lock-and-concurrent-byte-CAS", func(t *testing.T) {
		id := seedCredential(120, false)
		var replacement []byte
		provider.beforeDecrypt = func() {
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			var account uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT token_account_id FROM provider_credentials WHERE id=$1 FOR UPDATE NOWAIT`, id).Scan(&account); err != nil {
				t.Fatal("rotation held SQL row lock across KMS", err)
			}
			replacement, err = old.Encrypt([]byte("controlled-rotation-plaintext"), []byte(account.String()+":"+id.String()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET ciphertext=$2 WHERE id=$1`, id, replacement); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		result, err := r.RotateSecretKey(ctx, 100)
		var cipher []byte
		var key string
		if readErr := r.pool.QueryRow(ctx, `SELECT ciphertext,key_id FROM provider_credentials WHERE id=$1`, id).Scan(&cipher, &key); readErr != nil {
			t.Fatal(readErr)
		}
		if err != nil || result.Rotated != 0 || result.Unconfirmed != 4 || key != old.KeyID() || !bytes.Equal(cipher, replacement) {
			t.Fatal("stale snapshot overwrote changed ciphertext", result, err)
		}
		if rotateSweep(100) != 1 {
			t.Fatal("concurrent changed row not retried next sweep")
		}
	})

	t.Run("locked-inflight-row-skipped-and-retried", func(t *testing.T) {
		id := seedCredential(125, false)
		before := credentialFacts(id)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM provider_credentials WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		result, err := r.RotateSecretKey(bounded, 100)
		if err != nil || result.Rotated != 0 || result.Unconfirmed != 4 || credentialFacts(id) != before {
			t.Fatal("inflight row lock blocked or changed original ciphertext", result, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if rotateSweep(100) != 1 {
			t.Fatal("unlocked original row not retried")
		}
	})

	t.Run("SQL-error-rolls-back-healthy-prefix-and-keeps-cursor", func(t *testing.T) {
		prefix, id := seedCredential(129, false), seedCredential(130, false)
		before, prefixBefore := credentialFacts(id), credentialFacts(prefix)
		scan := r.secretRotation.scan
		config := r.pool.Config().ConnConfig.Copy()
		config.User = "token_migrator"
		owner, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close(context.Background())
		if _, err := owner.Exec(ctx, `SET ROLE token_schema_owner; CREATE FUNCTION public.rotation_fixture_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled rotation SQL failure' USING ERRCODE='P0001'; END $$`); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `CREATE TRIGGER rotation_fixture_failure BEFORE UPDATE ON public.provider_credentials FOR EACH ROW WHEN (NEW.id='`+id.String()+`'::uuid) EXECUTE FUNCTION public.rotation_fixture_failure()`); err != nil {
			t.Fatal(err)
		}
		defer owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS rotation_fixture_failure ON public.provider_credentials; DROP FUNCTION IF EXISTS public.rotation_fixture_failure()`)
		_, err = r.RotateSecretKey(ctx, 100)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "P0001" || credentialFacts(id) != before || credentialFacts(prefix) != prefixBefore || r.secretRotation.scan != scan {
			t.Fatal("SQL failure swallowed or partially committed/advanced scan", err)
		}
		if _, err := owner.Exec(ctx, `DROP TRIGGER rotation_fixture_failure ON public.provider_credentials; DROP FUNCTION public.rotation_fixture_failure()`); err != nil {
			t.Fatal(err)
		}
		if rotateSweep(100) != 2 {
			t.Fatal("SQL retry did not recover both original rows")
		}
	})

	t.Run("context-cancel-keeps-original-record-and-scan", func(t *testing.T) {
		id := seedCredential(140, false)
		before := credentialFacts(id)
		scan := r.secretRotation.scan
		cancelled, cancel := context.WithCancel(ctx)
		defer cancel()
		provider.beforeDecrypt = cancel
		_, err := r.RotateSecretKey(cancelled, 100)
		if !errors.Is(err, context.Canceled) || credentialFacts(id) != before || r.secretRotation.scan != scan {
			t.Fatal("cancelled crypto work committed/advanced scan", err)
		}
		if rotateSweep(100) != 1 {
			t.Fatal("cancel retry did not recover")
		}
	})
}
