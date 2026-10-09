package account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClosureClaimACKOrder(t *testing.T) {
	if os.Getenv("F08_ACCOUNT_URL") == "" || os.Getenv("F08_BILLING_URL") == "" || os.Getenv("F08_TOKEN_URL") == "" {
		t.Skip("explicit public closure Account/Billing/Token runtime fixture is required")
	}
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	r, err := NewPostgresRepository(ctx, os.Getenv("F08_ACCOUNT_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	bp, err := pgxpool.New(ctx, os.Getenv("F08_BILLING_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer bp.Close()
	br := billing.NewPostgresRepositoryWithPool(bp)
	secret, err := token.NewLocalAESProvider("closure-fixture", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := token.NewPostgresRepository(ctx, os.Getenv("F08_TOKEN_URL"), secret)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	fixtureToken := strings.Repeat("controlled-closure-authority-", 2)
	var lostACK atomic.Bool
	var calls atomic.Int32
	lostACK.Store(true)
	auth := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			if q.Header.Get("Authorization") != "Bearer "+fixtureToken {
				w.WriteHeader(401)
				return
			}
			h.ServeHTTP(w, q)
		})
	}
	billServer := httptest.NewServer(auth(billing.PrivateClosureHTTPHandler(br)))
	defer billServer.Close()
	tokenHandler := token.PrivateClosureHTTPHandler(tr)
	tokenServer := httptest.NewServer(auth(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == http.MethodDelete {
			calls.Add(1)
			var ordered bool
			if e := r.pool.QueryRow(ctx, `SELECT identity_deleted_at IS NOT NULL AND grants_isolated_at IS NOT NULL AND token_replays_cleared_at IS NULL FROM account_closure_requests WHERE user_id=$1`, strings.Split(q.URL.Path, "/")[4]).Scan(&ordered); e != nil || !ordered {
				w.WriteHeader(409)
				return
			}
			if lostACK.Swap(false) {
				rr := httptest.NewRecorder()
				tokenHandler.ServeHTTP(rr, q)
				if rr.Code != 204 {
					w.WriteHeader(rr.Code)
					return
				}
				w.WriteHeader(503)
				return
			}
		}
		tokenHandler.ServeHTTP(w, q)
	})))
	defer tokenServer.Close()
	idServer := httptest.NewServer(auth(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method != http.MethodDelete {
			w.WriteHeader(405)
			return
		}
		w.WriteHeader(204)
	})))
	defer idServer.Close()
	ids, err := NewHTTPIdentityProvisioner(idServer.URL, fixtureToken)
	if err != nil {
		t.Fatal(err)
	}
	billClient, err := NewHTTPAccountClosureClient(billServer.URL, fixtureToken, "BILLING")
	if err != nil {
		t.Fatal(err)
	}
	tokenClient, err := NewHTTPAccountClosureClient(tokenServer.URL, fixtureToken, "TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	worker := NewAccountClosureWorker(r, time.Second, billClient, tokenClient).WithCleanup(ids, billClient, tokenClient)
	seed := func(t *testing.T) (uuid.UUID, identity.Principal) {
		user := uuid.New()
		now := time.Now().UTC()
		subject := user.String()
		if _, e := r.pool.Exec(ctx, `INSERT INTO users(id,status,user_code,username,mobile_e164,created_at,updated_at) VALUES($1,'ACTIVE',generate_unique_user_code(),$2,$3,$4,$4)`, user, "closure_"+strings.ReplaceAll(subject, "-", "")[0:14], fmt.Sprintf("+86138%08d", time.Now().UnixNano()%100000000), now); e != nil {
			t.Fatal(e)
		}
		if _, e := r.pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('closure-controlled',$1,$2,$3,$3)`, subject, user, now); e != nil {
			t.Fatal(e)
		}
		if _, _, e := br.EnsureWallet(ctx, "PERSONAL", user); e != nil {
			t.Fatal(e)
		}
		return user, identity.Principal{Issuer: "closure-controlled", Subject: subject, AuthenticatedAt: now}
	}
	t.Run("unknown-Token-ACK-keeps-account-live-then-idempotent-retry", func(t *testing.T) {
		user, p := seed(t)
		item, e := r.CreateAccountClosure(ctx, p, uuid.NewString(), accountClosureConfirmation)
		if e != nil {
			t.Fatal(e)
		}
		worker.runOnce(ctx)
		var stage, status string
		var ack *time.Time
		if e = r.pool.QueryRow(ctx, `SELECT q.stage,u.status,q.token_replays_cleared_at FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, item.ID).Scan(&stage, &status, &ack); e != nil || stage != "IDENTITY_CLEANUP" || status != "CLOSING" || ack != nil {
			t.Fatalf("unknown ACK stage=%s user=%s ack=%v error=%v", stage, status, ack, e)
		}
		var receipts int
		if e = bp.QueryRow(ctx, `SELECT count(*) FROM account_closure_grant_isolations WHERE closure_request_id=$1 AND member_user_id=$2`, item.ID, user).Scan(&receipts); e != nil || receipts != 1 {
			t.Fatal("real grant isolation receipt", receipts, e)
		}
		if _, e = r.pool.Exec(ctx, `UPDATE account_closure_requests SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, item.ID); e != nil {
			t.Fatal(e)
		}
		worker.runOnce(ctx)
		if e = r.pool.QueryRow(ctx, `SELECT q.stage,u.status,q.token_replays_cleared_at FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, item.ID).Scan(&stage, &status, &ack); e != nil || stage != "ANONYMIZED" || status != "DELETED" || ack == nil {
			t.Fatalf("retry stage=%s user=%s ack=%v error=%v", stage, status, ack, e)
		}
		if calls.Load() != 2 {
			t.Fatalf("Token effect call count=%d", calls.Load())
		}
		if e = bp.QueryRow(ctx, `SELECT count(*) FROM account_closure_grant_isolations WHERE closure_request_id=$1`, item.ID).Scan(&receipts); e != nil || receipts != 1 {
			t.Fatal("replay duplicated isolation", e)
		}
	})
	t.Run("expired-current-claim-cannot-record-external-ACK", func(t *testing.T) {
		_, p := seed(t)
		_, e := r.CreateAccountClosure(ctx, p, uuid.NewString(), accountClosureConfirmation)
		if e != nil {
			t.Fatal(e)
		}
		item, e := r.ClaimAccountClosure(ctx)
		if e != nil || item == nil {
			t.Fatal("real claim", e)
		}
		if e = r.AdvanceAccountClosureToIdentityCleanup(ctx, *item); e != nil {
			t.Fatal(e)
		}
		item, e = r.ClaimAccountClosure(ctx)
		if e != nil || item == nil {
			t.Fatal(e)
		}
		prep, e := r.PrepareAccountClosureCleanup(ctx, item)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = r.pool.Exec(ctx, `UPDATE account_closure_requests SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, item.ID); e != nil {
			t.Fatal(e)
		}
		if e = r.MarkAccountClosureIdentityDeleted(ctx, item, prep.SubjectLifecycleVersion); !errors.Is(e, ErrConflict) {
			t.Fatalf("expired claim recorded ACK=%v", e)
		}
		var ack *time.Time
		if e = r.pool.QueryRow(ctx, `SELECT identity_deleted_at FROM account_closure_requests WHERE id=$1`, item.ID).Scan(&ack); e != nil || ack != nil {
			t.Fatal("stale ACK mutated claim", e)
		}
	})
}
