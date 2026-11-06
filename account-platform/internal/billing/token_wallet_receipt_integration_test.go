package billing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

func tokenWalletWorkload(t *testing.T) func(string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "wallet-fixture", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(jwks.Close)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: jwks.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := jwks.URL + "/realms/workload"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", jwks.URL)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "wallet-fixture")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"token","service":"token-gateway","permissions":["internal.billing.token-account-wallet-provision.execute","internal.billing.token-account-wallet-provision.read"]},{"subject":"read","service":"token-gateway","permissions":["internal.billing.token-account-wallet-provision.read"]},{"subject":"execute","service":"token-gateway","permissions":["internal.billing.token-account-wallet-provision.execute"]},{"subject":"wrong","service":"account-service","permissions":["internal.billing.wallet.ensure"]}]`)
	return func(subject string) string {
		if subject == "" {
			return ""
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "wallet-fixture"))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "wallet-fixture", "sub": subject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
		v, err := signer.Sign(raw)
		if err != nil {
			t.Fatal(err)
		}
		s, err := v.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
}

func tokenWalletSnapshot(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var s string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('wallets',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),'accounts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM ledger_accounts a),'receipts',(SELECT jsonb_agg(to_jsonb(v) ORDER BY command_reference) FROM token_account_wallet_provisioning_receipts v),'journals',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),'postings',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM ledger_postings p))::text`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTokenWalletReceiptSignedClientRecoveryPostgres(t *testing.T) {
	f06WalletActors(t)
	r, ctx := retainedFinanceRepository(t)
	signed := tokenWalletWorkload(t)
	handler := NewService(r).Handler("")
	owner, ref := uuid.New(), uuid.New()
	digest := tokenAccountWalletRequestDigest("PERSONAL", owner)
	t.Run("anonymous-wrong-workload-and-split-scopes", func(t *testing.T) {
		before := tokenWalletSnapshot(t, r, ctx)
		for _, c := range []struct {
			method, subject string
			want            int
		}{{"POST", "", 401}, {"POST", "wrong", 403}, {"POST", "read", 403}, {"GET", "execute", 403}, {"GET", "unregistered", 403}} {
			body, _ := json.Marshal(map[string]any{"command_reference": ref, "owner_type": "PERSONAL", "owner_id": owner, "request_digest": digest})
			path := "/internal/v1/token-account-wallet-provisioning-receipts"
			if c.method == "GET" {
				path += "/" + ref.String()
			}
			req := httptest.NewRequest(c.method, path, bytes.NewReader(body))
			if v := signed(c.subject); v != "" {
				req.Header.Set("Authorization", "Bearer "+v)
			}
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != c.want {
				t.Fatalf("%s/%s status%d want%d", c.method, c.subject, res.Code, c.want)
			}
		}
		if tokenWalletSnapshot(t, r, ctx) != before {
			t.Fatal("rejected workload changed financial facts")
		}
	})
	var first TokenAccountWalletProvisioningReceipt
	t.Run("actual-token-client-lost-post-ack-reads-original-reference", func(t *testing.T) {
		var drop sync.Once
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			if q.Method == "POST" {
				lost := false
				drop.Do(func() { lost = true })
				if lost {
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, q)
					if rec.Code != 201 {
						t.Errorf("supplier before lost ACK status%d body%s", rec.Code, rec.Body.String())
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
			}
			handler.ServeHTTP(w, q)
		}))
		defer server.Close()
		client, err := token.NewHTTPWalletResolver(server.URL, signed("token"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = client.ProvisionTokenAccountWallet(ctx, ref, "PERSONAL", owner, digest); err == nil {
			t.Fatal("controlled reply loss did not reach real client")
		}
		got, err := client.TokenAccountWalletProvisioningReceipt(ctx, ref)
		if err != nil || got.CommandReference != ref || got.OwnerType != "PERSONAL" || got.OwnerID != owner || got.RequestDigest != digest || got.Status != "SUCCEEDED" || got.WalletID == nil || got.AttemptCount != 1 || got.CompletedAt == nil {
			t.Fatalf("real client receipt mismatch: %+v err%v", got, err)
		}
		first, err = r.TokenAccountWalletProvisioningReceipt(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		before := tokenWalletSnapshot(t, r, ctx)
		again, err := client.ProvisionTokenAccountWallet(ctx, ref, "PERSONAL", owner, digest)
		if err != nil || !reflect.DeepEqual(got, again) {
			t.Fatal("original key did not replay exact receipt", err)
		}
		if _, err = client.TokenAccountWalletProvisioningReceipt(ctx, uuid.New()); !errors.Is(err, token.ErrNotFound) {
			t.Fatal("GET404 not preserved", err)
		}
		if tokenWalletSnapshot(t, r, ctx) != before {
			t.Fatal("replay/query changed facts")
		}
	})
	t.Run("same-reference-changed-owner-or-digest-zero-effect", func(t *testing.T) {
		before := tokenWalletSnapshot(t, r, ctx)
		other := uuid.New()
		for _, c := range []struct {
			kind   string
			owner  uuid.UUID
			digest string
		}{{"PERSONAL", other, tokenAccountWalletRequestDigest("PERSONAL", other)}, {"ORGANIZATION", owner, tokenAccountWalletRequestDigest("ORGANIZATION", owner)}, {"PERSONAL", owner, strings.Repeat("a", 64)}} {
			if _, created, err := r.ProvisionTokenAccountWallet(ctx, ref, c.kind, c.owner, c.digest); !errors.Is(err, ErrConflict) || created {
				t.Fatal("changed identity accepted", created, err)
			}
		}
		if tokenWalletSnapshot(t, r, ctx) != before {
			t.Fatal("identity conflict changed financial state")
		}
	})
	t.Run("history-replay-does-not-reactivate-closed-wallet", func(t *testing.T) {
		if _, err := r.pool.Exec(ctx, `UPDATE wallets SET status='CLOSED',version=version+1 WHERE id=$1`, *first.WalletID); err != nil {
			t.Fatal(err)
		}
		before := tokenWalletSnapshot(t, r, ctx)
		got, created, err := r.ProvisionTokenAccountWallet(ctx, ref, "PERSONAL", owner, digest)
		if err != nil || created || !reflect.DeepEqual(first, got) {
			t.Fatal("historical result no longer replays", err)
		}
		if _, created, err = r.ProvisionTokenAccountWallet(ctx, uuid.New(), "PERSONAL", owner, digest); !errors.Is(err, ErrWalletUnavailable) || created {
			t.Fatal("new command accepted closed wallet", created, err)
		}
		if tokenWalletSnapshot(t, r, ctx) != before {
			t.Fatal("closed-wallet retry changed facts")
		}
	})
}

func TestTokenWalletReceiptConservationAndLifecyclePostgres(t *testing.T) {
	f06WalletActors(t)
	r, ctx := retainedFinanceRepository(t)
	t.Run("concurrent-owner-and-command-have-one-wallet-and-liability", func(t *testing.T) {
		owner, ref := uuid.New(), uuid.New()
		digest := tokenAccountWalletRequestDigest("PERSONAL", owner)
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		receipts := make(chan TokenAccountWalletProvisioningReceipt, 16)
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				v, _, err := r.ProvisionTokenAccountWallet(ctx, ref, "PERSONAL", owner, digest)
				if err != nil {
					errs <- err
				} else {
					receipts <- v
				}
			}()
		}
		wg.Wait()
		close(errs)
		close(receipts)
		for err := range errs {
			t.Error(err)
		}
		var first *TokenAccountWalletProvisioningReceipt
		for v := range receipts {
			if first == nil {
				x := v
				first = &x
			} else if !reflect.DeepEqual(*first, v) {
				t.Fatal("same reference produced different receipts")
			}
		}
		if first == nil {
			t.Fatal("no receipt")
		}
		for i := 0; i < 3; i++ {
			v, created, err := r.ProvisionTokenAccountWallet(ctx, uuid.New(), "PERSONAL", owner, digest)
			if err != nil || !created || *v.WalletID != *first.WalletID {
				t.Fatal("per-owner convergence failed", err)
			}
		}
		var wallets, accounts, receiptsN, journals, postings int
		var posted, reserved, receivable int64
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM wallets WHERE owner_id=$1 AND owner_type='PERSONAL'),(SELECT count(*) FROM ledger_accounts WHERE wallet_id=$2),(SELECT count(*) FROM token_account_wallet_provisioning_receipts WHERE owner_id=$1),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM ledger_postings),posted_microcredits,reserved_microcredits,receivable_microcredits FROM wallets WHERE id=$2`, owner, *first.WalletID).Scan(&wallets, &accounts, &receiptsN, &journals, &postings, &posted, &reserved, &receivable); err != nil || wallets != 1 || accounts != 1 || receiptsN != 4 || journals != 0 || postings != 0 || posted != 0 || reserved != 0 || receivable != 0 {
			t.Fatalf("conservation %d/%d/%d funds %d/%d/%d journals %d/%d err%v", wallets, accounts, receiptsN, posted, reserved, receivable, journals, postings, err)
		}
	})
	t.Run("unknown-organization-and-terminal-owner-write-nothing", func(t *testing.T) {
		for _, state := range []string{"UNKNOWN", "DISABLED", "DELETED"} {
			owner := uuid.New()
			if state != "UNKNOWN" {
				if _, err := r.pool.Exec(ctx, `INSERT INTO organization_lifecycle_states(organization_id,status,account_version,updated_at) VALUES($1,$2,1,now())`, owner, state); err != nil {
					t.Fatal(err)
				}
			}
			before := tokenWalletSnapshot(t, r, ctx)
			if _, created, err := r.ProvisionTokenAccountWallet(ctx, uuid.New(), "ORGANIZATION", owner, tokenAccountWalletRequestDigest("ORGANIZATION", owner)); err == nil || created {
				t.Fatal("unavailable organization accepted", state, created, err)
			}
			if tokenWalletSnapshot(t, r, ctx) != before {
				t.Fatal("unavailable organization left effects", state)
			}
		}
		for _, state := range []string{"DELETION_PENDING", "DELETED"} {
			owner, org := uuid.New(), uuid.New()
			if _, err := r.pool.Exec(ctx, `INSERT INTO organization_unit_lifecycle_states(unit_id,organization_id,status,structure_version,updated_at) VALUES($1,$2,$3,1,now())`, owner, org, state); err != nil {
				t.Fatal(err)
			}
			before := tokenWalletSnapshot(t, r, ctx)
			if _, created, err := r.ProvisionTokenAccountWallet(ctx, uuid.New(), "ORG_UNIT", owner, tokenAccountWalletRequestDigest("ORG_UNIT", owner)); err == nil || created {
				t.Fatal("terminal unit accepted", state, created, err)
			}
			if tokenWalletSnapshot(t, r, ctx) != before {
				t.Fatal("terminal unit left effects", state)
			}
		}
	})
	t.Run("receipt-insert-failure-rolls-back-new-wallet-and-liability", func(t *testing.T) {
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION receipt_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled receipt persistence failure'; END $$; CREATE TRIGGER receipt_failure BEFORE INSERT ON token_account_wallet_provisioning_receipts FOR EACH ROW EXECUTE FUNCTION receipt_failure()`); err != nil {
			t.Fatal(err)
		}
		before := tokenWalletSnapshot(t, r, ctx)
		owner := uuid.New()
		if _, created, err := r.ProvisionTokenAccountWallet(ctx, uuid.New(), "PERSONAL", owner, tokenAccountWalletRequestDigest("PERSONAL", owner)); err == nil || created {
			t.Fatal("injected receipt failure committed", created, err)
		}
		if tokenWalletSnapshot(t, r, ctx) != before {
			t.Fatal("receipt failure leaked wallet or liability")
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER receipt_failure ON token_account_wallet_provisioning_receipts; DROP FUNCTION receipt_failure()`); err != nil {
			t.Fatal(err)
		}
	})
}
