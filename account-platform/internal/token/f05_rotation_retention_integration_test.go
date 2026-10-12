package token

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Each sequential case uses the existing isolated public-migration fixture.
// The old/active providers use real AEAD; no live KMS, financial service, key
// retirement, or deployed inventory is exercised by this controlled test.
func TestF05RotationRetainedContentBoundaries(t *testing.T) {
	for _, name := range []string{
		"cleanup-stubs-and-live-ciphertext",
		"case-copy-keeps-original-old-key",
		"cleanup-after-snapshot-cannot-resurrect-body",
		"acquired-null-and-legacy-null-envelope",
		"nonempty-bad-content-stays-unconfirmed",
	} {
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedManagementTokenRepository(t)
			clock := time.Now().UTC().Truncate(time.Microsecond)
			r.now = func() time.Time { return clock }
			old := r.secrets
			active, err := NewLocalAESProvider("controlled-retention-new-key", bytes.Repeat([]byte{9}, 32))
			if err != nil {
				t.Fatal(err)
			}
			provider := &rotationFixtureSecrets{old: old, active: active}
			// The fixture's provider credential/preparation use the active key;
			// only the actual replay API content under test uses the previous key.
			newContent := func() (uuid.UUID, ReplayResult) {
				t.Helper()
				r.secrets = active
				id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "STREAMING")
				r.secrets = old
				if _, err := r.AppendReplayEvent(ctx, id, "DELTA", []byte(`{"text":"original frame"}`)); err != nil {
					t.Fatal(err)
				}
				result := ReplayResult{Kind: "JSON", HTTPStatus: http.StatusOK, ContentType: "application/json", Payload: []byte(`{"text":"original result"}`), LastSequence: 1}
				if err := r.SaveReplayResult(ctx, id, result); err != nil {
					t.Fatal(err)
				}
				if err := r.MarkReview(ctx, id, "usage_missing", map[string]any{"usage_pending": true}); err != nil {
					t.Fatal(err)
				}
				r.secrets = provider
				return id, result
			}
			contentFacts := func(id uuid.UUID) string {
				t.Helper()
				var facts string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY sequence) FROM usage_stream_events e WHERE usage_request_id=$1),
 'result',(SELECT to_jsonb(v) FROM usage_replay_results v WHERE usage_request_id=$1))::text`, id).Scan(&facts); err != nil {
					t.Fatal(err)
				}
				return facts
			}
			// Exclude only the ordinary ciphertext/key/integrity fields that this
			// operation may re-encrypt. Financial, receipt, first-clock, audit,
			// case metadata and the complete private encrypted copies are exact.
			businessFacts := func() string {
				t.Helper()
				var facts string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'accounts',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM token_accounts v),
 'credentials',(SELECT jsonb_agg(to_jsonb(v)-'ciphertext'-'key_id' ORDER BY id) FROM provider_credentials v),
 'usage',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM usage_requests v),
 'preparations',(SELECT jsonb_agg(to_jsonb(v)-'encrypted_envelope'-'key_id'-'envelope_integrity' ORDER BY usage_request_id) FROM usage_dispatch_preparations v),
 'intents',(SELECT jsonb_agg(to_jsonb(v)-'encrypted_envelope'-'key_id'-'envelope_integrity' ORDER BY usage_request_id) FROM usage_dispatch_intents v),
 'events',(SELECT jsonb_agg(to_jsonb(v)-'encrypted_payload'-'key_id'-'payload_integrity' ORDER BY usage_request_id,sequence) FROM usage_stream_events v),
 'results',(SELECT jsonb_agg(to_jsonb(v)-'encrypted_payload'-'key_id'-'payload_integrity' ORDER BY usage_request_id) FROM usage_replay_results v),
 'audit',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM token_audit_logs v),
 'convergence',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM usage_convergence_evidence v),
 'cases',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM usage_replay_retention_cases v),
 'private',(SELECT jsonb_agg(to_jsonb(v) ORDER BY retention_case_id) FROM usage_replay_case_payloads v))::text`).Scan(&facts); err != nil {
					t.Fatal(err)
				}
				return facts
			}
			remaining := func(want int64) {
				t.Helper()
				got, err := r.SecretRotationRemaining(ctx)
				if err != nil || got != want {
					t.Fatalf("ordinary ciphertext remaining=%d want=%d err=%v", got, want, err)
				}
			}
			sweep := func() SecretRotationResult {
				t.Helper()
				var total SecretRotationResult
				for call := 0; call < 20; call++ {
					result, err := r.RotateSecretKey(ctx, 1)
					if err != nil || result.Scanned > 1 {
						t.Fatal("bounded ordinary rotation failed", result, err)
					}
					total.Scanned += result.Scanned
					total.Rotated += result.Rotated
					total.Unconfirmed += result.Unconfirmed
					total.Remaining, total.SweepComplete = result.Remaining, result.SweepComplete
					if result.SweepComplete {
						return total
					}
				}
				t.Fatal("captured finite sweep did not end")
				return total
			}
			assertEmpty := func(id uuid.UUID) {
				t.Helper()
				var eventBytes, resultBytes int
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT sum(octet_length(encrypted_payload)) FROM usage_stream_events WHERE usage_request_id=$1),(SELECT octet_length(encrypted_payload) FROM usage_replay_results WHERE usage_request_id=$1)`, id).Scan(&eventBytes, &resultBytes); err != nil || eventBytes != 0 || resultBytes != 0 {
					t.Fatal("retained metadata unexpectedly contains a public body", eventBytes, resultBytes, err)
				}
				replay, err := r.LoadReplay(ctx, id, 0)
				if err != nil || replay.Result != nil || len(replay.Events) != 0 || replay.Usage.ExecutionStatus != "REVIEW" {
					t.Fatal("erased public body replayed or financial review changed", err)
				}
			}

			switch name {
			case "cleanup-stubs-and-live-ciphertext":
				expired, _ := newContent()
				clock = clock.Add(30 * 24 * time.Hour)
				if err := r.CleanupReplayRetention(ctx); err != nil {
					t.Fatal(err)
				}
				assertEmpty(expired)
				stubs := contentFacts(expired)
				live, result := newContent()
				facts := businessFacts()
				remaining(2)
				got := sweep()
				if got.Scanned != 2 || got.Rotated != 2 || got.Unconfirmed != 0 || got.Remaining != 0 {
					t.Fatal("stub selected or live ciphertext not rotated", got)
				}
				if contentFacts(expired) != stubs || businessFacts() != facts {
					t.Fatal("retention metadata, original money or immutable facts changed")
				}
				assertEmpty(expired)
				replay, err := r.LoadReplay(ctx, live, 0)
				if err != nil || len(replay.Events) != 1 || string(replay.Events[0].Payload) != `{"text":"original frame"}` || replay.Result == nil || !bytes.Equal(replay.Result.Payload, result.Payload) {
					t.Fatal("actual original AAD replay did not survive healthy rotation", err)
				}

			case "case-copy-keeps-original-old-key":
				id, result := newContent()
				caseID, err := r.CreateReplayRetentionCase(ctx, id, uuid.New(), "controlled approved case", "original ciphertext", clock.Add(time.Hour), clock.Add(10*24*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				assertEmpty(id)
				facts, stubs := businessFacts(), contentFacts(id)
				remaining(0)
				if got := sweep(); got.Scanned != 0 || got.Rotated != 0 || got.Unconfirmed != 0 || got.Remaining != 0 {
					t.Fatal("private case or public metadata stub treated as ordinary ciphertext", got)
				}
				if businessFacts() != facts || contentFacts(id) != stubs {
					t.Fatal("private case, original key or public stub rewritten")
				}
				var eventJSON, resultJSON []byte
				if err := r.pool.QueryRow(ctx, `SELECT encrypted_events,encrypted_result FROM usage_replay_case_payloads WHERE retention_case_id=$1`, caseID).Scan(&eventJSON, &resultJSON); err != nil {
					t.Fatal(err)
				}
				type heldCipher struct {
					Sequence   int64  `json:"sequence"`
					Last       int64  `json:"last_sequence"`
					EventType  string `json:"event_type"`
					Kind       string `json:"response_kind"`
					Ciphertext string `json:"ciphertext"`
					Key        string `json:"key_id"`
					Integrity  string `json:"integrity"`
				}
				var events []heldCipher
				var response heldCipher
				if err := json.Unmarshal(eventJSON, &events); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(resultJSON, &response); err != nil || len(events) != 1 {
					t.Fatal("missing actual private cipher copy", err)
				}
				for i, held := range append(events, response) {
					cipher, err := base64.StdEncoding.DecodeString(held.Ciphertext)
					if err != nil || len(cipher) == 0 || held.Key != old.KeyID() || replayDigest(cipher) != held.Integrity {
						t.Fatal("private copy lost original cipher/key/integrity", err)
					}
					kind, sequence, want := "event:"+held.EventType, held.Sequence, []byte(`{"text":"original frame"}`)
					if i == 1 {
						kind, sequence, want = "result:"+held.Kind, held.Last, result.Payload
					}
					plain, err := old.DecryptWithKeyID(held.Key, cipher, replayAAD(id, kind, sequence))
					matches := bytes.Equal(plain, want)
					clear(plain)
					if err != nil || !matches {
						t.Fatal("ordinary Remaining=0 did not remove need for held old decrypt key", err)
					}
					if plain, err := active.DecryptWithKeyID(held.Key, cipher, replayAAD(id, kind, sequence)); err == nil {
						clear(plain)
						t.Fatal("active local key alone unexpectedly decrypts held old key")
					} else {
						clear(plain)
					}
				}

			case "cleanup-after-snapshot-cannot-resurrect-body":
				id, _ := newContent()
				clock = clock.Add(30 * 24 * time.Hour)
				var stubs, facts string
				provider.beforeDecrypt = func() {
					// Rotate already read and closed both body snapshots. A real
					// cleanup transaction now wins before its conditional write.
					if err := r.CleanupReplayRetention(ctx); err != nil {
						t.Fatal(err)
					}
					stubs, facts = contentFacts(id), businessFacts()
				}
				got, err := r.RotateSecretKey(ctx, 10)
				if err != nil || stubs == "" || got.Scanned != 2 || got.Rotated != 0 || got.Unconfirmed != 2 || got.Remaining != 0 {
					t.Fatal("cleanup did not win the original ciphertext CAS", got, err)
				}
				if contentFacts(id) != stubs || businessFacts() != facts {
					t.Fatal("conditional re-encryption resurrected content or changed first facts")
				}
				assertEmpty(id)

			case "acquired-null-and-legacy-null-envelope":
				r.secrets = active
				id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "HOLD_RESERVED")
				if _, err := r.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				r.secrets = old
				envelope := DispatchEnvelope{BaseURL: "https://provider.invalid", Credential: []byte("controlled-credential"), RequestBody: []byte(`{"model":"model"}`), Price: PriceVersion{ID: uuid.New()}, MaximumCost: 100}
				if err := r.PersistDispatchIntent(ctx, id, envelope); err != nil {
					t.Fatal(err)
				}
				r.secrets = provider
				actual, acquired, err := r.AcquireDispatch(ctx, id, "controlled-worker", 10*time.Second)
				if err != nil || !acquired || !bytes.Equal(actual.RequestBody, envelope.RequestBody) || !bytes.Equal(actual.Credential, envelope.Credential) {
					t.Fatal("real immutable dispatch acquire failed", acquired, err)
				}
				var cleared bool
				if err := r.pool.QueryRow(ctx, `SELECT encrypted_envelope IS NULL AND key_id IS NULL AND envelope_integrity IS NULL FROM usage_dispatch_intents WHERE usage_request_id=$1`, id).Scan(&cleared); err != nil || !cleared {
					t.Fatal("real acquire did not erase all three dispatch cipher fields", cleared, err)
				}
				remaining(0)
				// This is explicitly a schema-legal legacy metadata condition,
				// not a claim that the current Acquire writer retains these keys.
				if _, err := r.pool.Exec(ctx, `UPDATE usage_dispatch_intents SET key_id=$2,envelope_integrity=$3 WHERE usage_request_id=$1 AND encrypted_envelope IS NULL`, id, old.KeyID(), "controlled-legacy-no-body"); err != nil {
					t.Fatal(err)
				}
				var before, after string
				query := `SELECT to_jsonb(v)::text FROM usage_dispatch_intents v WHERE usage_request_id=$1`
				if err := r.pool.QueryRow(ctx, query, id).Scan(&before); err != nil {
					t.Fatal(err)
				}
				facts := businessFacts()
				remaining(0)
				if got := sweep(); got.Scanned != 0 || got.Rotated != 0 || got.Unconfirmed != 0 || got.Remaining != 0 {
					t.Fatal("absent intent body selected or counted", got)
				}
				if err := r.pool.QueryRow(ctx, query, id).Scan(&after); err != nil || before != after || businessFacts() != facts {
					t.Fatal("NULL envelope metadata or dispatch facts mutated", err)
				}

			case "nonempty-bad-content-stays-unconfirmed":
				id, _ := newContent()
				bad := []byte("controlled-nonempty-invalid-AEAD")
				if _, err := r.pool.Exec(ctx, `UPDATE usage_stream_events SET encrypted_payload=$2,payload_integrity=$3 WHERE usage_request_id=$1`, id, bad, replayDigest(bad)); err != nil {
					t.Fatal(err)
				}
				facts := businessFacts()
				var before, after string
				query := `SELECT to_jsonb(v)::text FROM usage_stream_events v WHERE usage_request_id=$1`
				if err := r.pool.QueryRow(ctx, query, id).Scan(&before); err != nil {
					t.Fatal(err)
				}
				remaining(2)
				got := sweep()
				if got.Scanned != 2 || got.Rotated != 1 || got.Unconfirmed != 1 || got.Remaining != 1 {
					t.Fatal("nonempty bad body hidden from strict ordinary inventory", got)
				}
				remaining(1)
				if err := r.pool.QueryRow(ctx, query, id).Scan(&after); err != nil || before != after || businessFacts() != facts {
					t.Fatal("unconfirmed ciphertext or financial history changed", err)
				}
			}
		})
	}
}
