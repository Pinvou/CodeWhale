package token

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

type fixtureContexts struct{ value AuthorizationContext }

func (f fixtureContexts) Resolve(context.Context, string) (AuthorizationContext, error) {
	return f.value, nil
}

type fixtureBilling struct {
	BillingClient
	mu                sync.Mutex
	releaseErr        error
	commits, releases int
	actual            int64
	maximum           int64
	price             *PriceVersion
	holdID            uuid.UUID
}

func (f *fixtureBilling) Price(context.Context, string) (PriceVersion, error) {
	if f.price != nil {
		return *f.price, nil
	}
	return PriceVersion{ID: uuid.New(), InputPerMillion: 1_000_000, OutputPerMillion: 1_000_000, CachedInputPerMillion: 1_000_000}, nil
}
func (f *fixtureBilling) Reserve(_ context.Context, usageID, wallet uuid.UUID, maximum int64, price uuid.UUID) (BillingHold, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdID = uuid.New()
	f.maximum = maximum
	return BillingHold{ID: f.holdID, UsageRequestID: usageID, Status: "ACTIVE"}, nil
}
func (f *fixtureBilling) Commit(_ context.Context, _, _, _ uuid.UUID, actual int64, _ Usage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits++
	if actual > f.maximum {
		return errors.New("actual exceeds reservation")
	}
	f.actual = actual
	return nil
}
func (f *fixtureBilling) Release(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	return f.releaseErr
}

func (f *fixtureBilling) commitCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.commits }
func (f *fixtureBilling) releaseCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.releases }
func (f *fixtureBilling) charge() int64     { f.mu.Lock(); defer f.mu.Unlock(); return f.actual }

func proxyFixture(t *testing.T, baseURL string) (*Service, *PostgresRepository, *fixtureBilling) {
	t.Helper()
	key, err := NewLocalAESProvider("test-only", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	repo := &PostgresRepository{pool: testdb.Open(t, "token"), secrets: key, now: func() time.Time { return time.Now().UTC() }}
	user, wallet := uuid.New(), uuid.New()
	account, err := repo.CreateAccount(context.Background(), CreateAccountRequest{OwnerType: "USER", OwnerID: user, WalletID: wallet, Name: "fixture", ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100, CreatedBy: user})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RotateCredential(context.Background(), account.ID, user, "test-provider", baseURL, []byte("test-only-provider-key"), []string{"model-test"}); err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(baseURL)
	policy, err := NewProviderURLPolicy("development", []string{parsed.Hostname()})
	if err != nil {
		t.Fatal(err)
	}
	billing := &fixtureBilling{}
	service := NewService(repo, fixtureContexts{AuthorizationContext{UserID: user, SessionID: "fixture-session", ContextType: "PERSONAL", ContextVersion: 1, AuthorizationVersion: 1}}, nil, nil, policy, billing, NewHTTPUpstreamClient(), 100)
	return service, repo, billing
}

func modelRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Idempotency-Key", uuid.NewString())
	return r
}

func TestPostgresProxyDoneSettlesWithoutTransportEOF(t *testing.T) {
	for _, closeAtDone := range []bool{false, true} {
		name := "upstream-keeps-connection-open"
		if closeAtDone {
			name = "client-closes-at-complete-done-event"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			upstreamClosed := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":50}}}\n\ndata: [DONE]\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(upstreamClosed)
			}))
			defer upstream.Close()
			s, tokenRepo, _ := proxyFixture(t, upstream.URL)
			// Both domains use original migrations in separate isolated PG schemas;
			// authorization remains the same personal-context fixture as other tests.
			billingPool := testdb.Open(t, "billing")
			config := billingPool.Config()
			dsn, err := url.Parse(config.ConnString())
			if err != nil {
				t.Fatal(err)
			}
			query := dsn.Query()
			query.Set("search_path", config.ConnConfig.RuntimeParams["search_path"])
			dsn.RawQuery = query.Encode()
			billingRepo, err := billingdomain.NewPostgresRepository(ctx, dsn.String())
			if err != nil {
				t.Fatal(err)
			}
			defer billingRepo.Close()
			resolved, err := s.contexts.Resolve(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			wallet, _, err := billingRepo.EnsureWallet(ctx, "PERSONAL", resolved.UserID)
			if err != nil {
				t.Fatal(err)
			}
			const initial = int64(1_000_000)
			if _, err = billingRepo.Credit(ctx, wallet.ID, "stream-fixture-funding", initial, true); err != nil {
				t.Fatal(err)
			}
			price := PriceVersion{ID: uuid.New(), Model: "model-test", InputPerMillion: 1_000_000, CachedInputPerMillion: 2_000_000}
			if _, err = billingPool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, price.ID, price.Model, price.InputPerMillion, price.OutputPerMillion, price.CachedInputPerMillion, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err = tokenRepo.pool.Exec(ctx, `UPDATE token_accounts SET wallet_id=$1`, wallet.ID); err != nil {
				t.Fatal(err)
			}
			billingServer := httptest.NewServer(billingdomain.NewService(billingRepo).Handler(strings.Repeat("s", 32)))
			defer billingServer.Close()
			s.billing, err = NewHTTPBillingClient(billingServer.URL, strings.Repeat("s", 32))
			if err != nil {
				t.Fatal(err)
			}
			gateway := httptest.NewServer(http.HandlerFunc(s.chatCompletions))
			defer gateway.Close()
			requestCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, gateway.URL, strings.NewReader(`{"model":"model-test","stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			key := uuid.NewString()
			request.Header.Set("Idempotency-Key", key)
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("chat status %d", response.StatusCode)
			}
			if closeAtDone {
				scanner := bufio.NewScanner(response.Body)
				doneSeen, eventComplete := false, false
				for scanner.Scan() {
					if scanner.Text() == "data: [DONE]" {
						doneSeen = true
					} else if doneSeen && scanner.Text() == "" {
						eventComplete = true
						break
					}
				}
				if !eventComplete || scanner.Err() != nil {
					t.Fatalf("incomplete DONE event: %v", scanner.Err())
				}
				response.Body.Close()
				cancel()
			} else {
				payload, err := io.ReadAll(response.Body)
				if err != nil || !bytes.Contains(payload, []byte("data: [DONE]\n\n")) {
					t.Fatalf("stream waited for transport EOF or lost DONE: %s %v", payload, err)
				}
			}
			select {
			case <-upstreamClosed:
			case <-time.After(2 * time.Second):
				t.Fatal("DONE did not close upstream")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				usage, err := getUsageByIdempotency(ctx, tokenRepo.pool, key, false)
				if err != nil {
					t.Fatal(err)
				}
				if usage.Status == "COMMITTED" {
					if usage.HoldID == nil || usage.ActualMicrocredits == nil || *usage.ActualMicrocredits != 150 {
						t.Fatalf("Token settlement %+v", usage)
					}
					var status string
					var actual int64
					if err = billingPool.QueryRow(ctx, `SELECT status,actual_microcredits FROM usage_holds WHERE id=$1`, *usage.HoldID).Scan(&status, &actual); err != nil || status != "COMMITTED" || actual != 150 {
						t.Fatalf("Billing settlement %s %d %v", status, actual, err)
					}
					break
				}
				if usage.Status == "REVIEW" || time.Now().After(deadline) {
					t.Fatalf("complete DONE failed to settle: %+v", usage)
				}
				time.Sleep(5 * time.Millisecond)
			}
			after, err := billingRepo.GetWallet(ctx, wallet.ID)
			if err != nil || after.ReservedMicrocredits != 0 || after.PostedMicrocredits != initial-150 {
				t.Fatalf("wallet settlement %+v %v", after, err)
			}
			var unbalanced int
			if err = billingPool.QueryRow(ctx, `SELECT count(*) FROM (SELECT journal_entry_id FROM ledger_postings GROUP BY journal_entry_id HAVING sum(delta_microcredits)<>0) x`).Scan(&unbalanced); err != nil || unbalanced != 0 {
				t.Fatalf("unbalanced journals %d %v", unbalanced, err)
			}
		})
	}
}

func usageStatus(t *testing.T, repo *PostgresRepository) string {
	t.Helper()
	var status string
	if err := repo.pool.QueryRow(context.Background(), `SELECT status FROM usage_requests`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestPostgresProxyIncompleteUsageRemainsReview(t *testing.T) {
	for _, usage := range []string{`{}`, `{"prompt_tokens":2}`, `{"completion_tokens":3}`, `{"prompt_tokens":null,"completion_tokens":0}`, `{"prompt_tokens":-1,"completion_tokens":0}`} {
		t.Run(usage, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"usage":`+usage+`}`) }))
			defer upstream.Close()
			s, repo, bill := proxyFixture(t, upstream.URL)
			s.chatCompletions(httptest.NewRecorder(), modelRequest(`{"model":"model-test"}`))
			if usageStatus(t, repo) != "REVIEW" || bill.commitCount() != 0 || bill.releaseCount() != 0 {
				t.Fatalf("malformed usage settled: commit=%d release=%d", bill.commitCount(), bill.releaseCount())
			}
		})
	}
}

func TestPostgresProxyExplicitZeroUsageSettlesAndBoundsOutput(t *testing.T) {
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"usage":{"prompt_tokens":0,"completion_tokens":0}}`)
	}))
	defer upstream.Close()
	s, repo, bill := proxyFixture(t, upstream.URL)
	s.chatCompletions(httptest.NewRecorder(), modelRequest(`{"model":"model-test","metadata":{"counter":9007199254740993}}`))
	if usageStatus(t, repo) != "COMMITTED" || bill.commitCount() != 1 || bill.charge() != 0 {
		t.Fatal("explicit zero usage was not settled")
	}
	if !bytes.Contains(upstreamBody, []byte(`"max_tokens":4096`)) || !bytes.Contains(upstreamBody, []byte(`9007199254740993`)) {
		t.Fatalf("upstream bounds/precision: %s", upstreamBody)
	}
}

func TestPostgresProxyReleaseFailureRemainsReview(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer upstream.Close()
	s, repo, bill := proxyFixture(t, upstream.URL)
	bill.releaseErr = errors.New("Billing unavailable")
	s.chatCompletions(httptest.NewRecorder(), modelRequest(`{"model":"model-test"}`))
	if usageStatus(t, repo) != "REVIEW" || bill.releaseCount() != 1 || bill.commitCount() != 0 {
		t.Fatal("failed release lost its pending reservation")
	}
	var reason string
	if err := repo.pool.QueryRow(context.Background(), `SELECT error_class FROM usage_requests`).Scan(&reason); err != nil || reason != "release_pending" {
		t.Fatalf("reason %s %v", reason, err)
	}
}

func TestProviderRequestsDoNotFollowRedirect(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			targetCalls := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetCalls++
				io.WriteString(w, `{"data":[{"id":"model-test"}]}`)
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+r.URL.Path, status) }))
			defer source.Close()
			parsed, _ := url.Parse(source.URL)
			policy, _ := NewProviderURLPolicy("development", []string{parsed.Hostname()})
			if err := NewHTTPModelValidator(policy).Validate(context.Background(), source.URL, []byte("test-only"), []string{"model-test"}); err == nil {
				t.Fatal("validator accepted redirect")
			}
			response, err := NewHTTPUpstreamClient().Do(context.Background(), source.URL, []byte("test-only"), []byte(`{"model":"model-test"}`), uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != status || targetCalls != 0 {
				t.Fatalf("provider redirect followed: %d calls=%d", response.StatusCode, targetCalls)
			}
		})
	}
}

func TestPostgresProxyRealServerLongStreamClearsWriteDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(80 * time.Millisecond)
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	s, repo, bill := proxyFixture(t, upstream.URL)
	server := httptest.NewUnstartedServer(http.HandlerFunc(s.chatCompletions))
	server.Config.WriteTimeout = 20 * time.Millisecond
	server.Start()
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"model-test","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", uuid.NewString())
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Contains(body, []byte("[DONE]")) {
		t.Fatalf("stream lost after deadline: %s %v", body, err)
	}
	// The last flush can precede settlement, so observe its durable completion.
	deadline := time.Now().Add(time.Second)
	for usageStatus(t, repo) != "COMMITTED" {
		if time.Now().After(deadline) {
			t.Fatal("stream did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	if bill.charge() != 5 {
		t.Fatalf("usage charge %d", bill.charge())
	}
}

func TestPostgresProxyClientDisconnectCancelsUpstreamAndKeepsHold(t *testing.T) {
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	s, repo, bill := proxyFixture(t, upstream.URL)
	server := httptest.NewServer(http.HandlerFunc(s.chatCompletions))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{"model":"model-test","stream":true}`))
	req.Header.Set("Idempotency-Key", uuid.NewString())
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1)
	if _, err := response.Body.Read(chunk); err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel provider")
	}
	deadline := time.Now().Add(time.Second)
	for usageStatus(t, repo) != "REVIEW" {
		if time.Now().After(deadline) {
			t.Fatal("unknown stream was not reviewed")
		}
		time.Sleep(time.Millisecond)
	}
	if bill.commitCount() != 0 || bill.releaseCount() != 0 {
		t.Fatal("unknown stream released or settled funds")
	}
}

func TestAccountDirectoryUnitOwnership(t *testing.T) {
	organization, unit, foreign := uuid.New(), uuid.New(), uuid.New()
	for _, item := range []struct {
		name, status string
		org          uuid.UUID
		want         bool
	}{{"active", "ACTIVE", organization, true}, {"foreign", "ACTIVE", foreign, false}, {"disabled", "DISABLED", organization, false}} {
		t.Run(item.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/organizations/"+organization.String()+"/units" || r.Header.Get("Authorization") != "Bearer caller" {
					t.Error("directory authority request mismatch")
				}
				json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"unit_id": unit, "organization_id": item.org, "status": item.status}}})
			}))
			defer server.Close()
			resolver, err := NewHTTPContextResolver(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			active, err := resolver.UnitActive(context.Background(), "Bearer caller", organization, unit)
			if err != nil || active != item.want {
				t.Fatalf("active %v err %v", active, err)
			}
		})
	}
}

type walletSpy struct{ calls int }

type scopedOrganizationWallet struct{ organization, wallet uuid.UUID }

func (f scopedOrganizationWallet) Ensure(_ context.Context, ownerType string, ownerID uuid.UUID) (uuid.UUID, error) {
	if ownerType != "ORGANIZATION" || ownerID != f.organization {
		return uuid.Nil, ErrForbidden
	}
	return f.wallet, nil
}

type scopedOrganizationBilling struct {
	*fixtureBilling
	OrganizationBillingClient
	organization, member uuid.UUID
}

func (f scopedOrganizationBilling) ReserveMemberQuota(ctx context.Context, usage, wallet, org, member uuid.UUID, version, maximum int64, price uuid.UUID) (BillingHold, error) {
	if org != f.organization || member != f.member || version != 1 {
		return BillingHold{}, ErrForbidden
	}
	return f.fixtureBilling.Reserve(ctx, usage, wallet, maximum, price)
}

func (w *walletSpy) Ensure(context.Context, string, uuid.UUID) (uuid.UUID, error) {
	w.calls++
	return uuid.New(), nil
}

func TestForeignUnitCannotProvisionWallet(t *testing.T) {
	organization, foreignUnit, user := uuid.New(), uuid.New(), uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me/authorization-context" {
			json.NewEncoder(w).Encode(map[string]any{"user_id": user, "session_id": "session", "context_type": "ORGANIZATION", "member_version": 1, "context_version": 1, "authorization_version": 1, "organization_id": organization, "roles": []string{"OWNER"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer server.Close()
	resolver, err := NewHTTPContextResolver(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	wallet := &walletSpy{}
	s := NewService(nil, resolver, wallet, nil, ProviderURLPolicy{}, nil, nil, 100)
	body, _ := json.Marshal(map[string]any{"owner_type": "ORG_UNIT", "owner_id": foreignUnit, "owner_organization_id": organization, "owner_unit_id": foreignUnit, "name": "foreign", "share_scope": "ORG_ALL"})
	r := httptest.NewRequest(http.MethodPost, "/v1/token-accounts", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer caller")
	w := httptest.NewRecorder()
	s.createTokenAccount(w, r)
	if w.Code != http.StatusForbidden || wallet.calls != 0 {
		t.Fatalf("foreign unit provisioned: status=%d calls=%d", w.Code, wallet.calls)
	}
}

func TestPostgresTokenConcurrentUsageAndDisabledCredential(t *testing.T) {
	s, repo, _ := proxyFixture(t, "http://provider.example")
	resolved, _ := s.contexts.Resolve(context.Background(), "")
	candidates, err := repo.ListCandidates(context.Background(), "model-test")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("route %+v %v", candidates, err)
	}
	c := candidates[0]
	key := uuid.NewString()
	start := make(chan struct{})
	failures := make(chan error, 8)
	created := make(chan bool, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, new, err := repo.BeginUsage(context.Background(), UsageRequest{ID: uuid.New(), IdempotencyKey: key, UserID: resolved.UserID, SessionID: resolved.SessionID, TokenAccountID: c.TokenAccountID, CredentialID: c.CredentialID, WalletID: c.WalletID, Model: "model-test", MaximumMicrocredits: 100}, map[string]any{"request_sha256": "same"})
			failures <- err
			created <- new
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	close(created)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for new := range created {
		if new {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("created %d usage requests", count)
	}
	if _, err := repo.SetAccountStatus(context.Background(), c.TokenAccountID, "DISABLED", resolved.UserID, "pause"); err != nil {
		t.Fatal(err)
	}
	if _, secret, err := repo.CredentialSecret(context.Background(), c.TokenAccountID, c.CredentialID); !errors.Is(err, ErrNotFound) || len(secret) != 0 {
		t.Fatalf("disabled account exposed credential: %v", err)
	}
}

func TestPostgresBlankCredentialAndMalformedBodiesReturnBadRequest(t *testing.T) {
	s, repo, _ := proxyFixture(t, "http://provider.example")
	candidates, err := repo.ListCandidates(context.Background(), "model-test")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/credentials", strings.NewReader(`{"secret":" "}`))
	r.SetPathValue("token_account_id", candidates[0].TokenAccountID.String())
	w := httptest.NewRecorder()
	s.rotateCredential(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("blank credential status %d", w.Code)
	}
	for name, handler := range map[string]http.HandlerFunc{"create": s.createTokenAccount, "recharge": s.createRechargeOrder, "sharing": s.updateTokenSharing, "status": s.setTokenAccountStatus, "routes": s.upsertTokenRoute} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{`))
			r.SetPathValue("token_account_id", candidates[0].TokenAccountID.String())
			w := httptest.NewRecorder()
			handler(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("malformed body status %d", w.Code)
			}
		})
	}
}

type failingReleaseRecord struct{ Repository }

func (f failingReleaseRecord) MarkReleased(context.Context, uuid.UUID, string) error {
	return errors.New("local write failed")
}

func TestPostgresConfirmedBillingReleaseLocalFailureRemainsReview(t *testing.T) {
	s, repo, bill := proxyFixture(t, "http://provider.example")
	resolved, _ := s.contexts.Resolve(context.Background(), "")
	c, _ := repo.ListCandidates(context.Background(), "model-test")
	id := uuid.New()
	_, _, err := repo.BeginUsage(context.Background(), UsageRequest{ID: id, IdempotencyKey: uuid.NewString(), UserID: resolved.UserID, SessionID: resolved.SessionID, TokenAccountID: c[0].TokenAccountID, CredentialID: c[0].CredentialID, WalletID: c[0].WalletID, Model: "model-test", MaximumMicrocredits: 100}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkReserved(context.Background(), id, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	s.repository = failingReleaseRecord{repo}
	s.releaseUnused(context.Background(), id, uuid.New(), "known unused")
	if usageStatus(t, repo) != "REVIEW" || bill.releaseCount() != 1 {
		t.Fatal("local release write failure was lost")
	}
	var reason string
	var confirmed bool
	if err := repo.pool.QueryRow(context.Background(), `SELECT error_class,(usage_snapshot->>'billing_released')::boolean FROM usage_requests WHERE id=$1`, id).Scan(&reason, &confirmed); err != nil || reason != "local_release_record_pending" || !confirmed {
		t.Fatalf("release evidence %s %v %v", reason, confirmed, err)
	}
}

func TestPostgresProxyIncompleteStreamCannotSettle(t *testing.T) {
	for name, suffix := range map[string]string{"eof-without-done": "", "incomplete-done-event": "data: [DONE]\n", "error-after-usage": "data: {\"error\":{\"message\":\"failed\"}}\n\ndata: [DONE]\n\n"} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\n"+suffix)
			}))
			defer upstream.Close()
			s, repo, bill := proxyFixture(t, upstream.URL)
			s.chatCompletions(httptest.NewRecorder(), modelRequest(`{"model":"model-test","stream":true}`))
			if usageStatus(t, repo) != "REVIEW" || bill.commitCount() != 0 || bill.releaseCount() != 0 {
				t.Fatal("incomplete stream settled")
			}
		})
	}
}

func TestPostgresProxyTruncatedJSONCannotSettle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := `{"usage":{"prompt_tokens":2,"completion_tokens":3}}`
		io.WriteString(w, prefix+strings.Repeat(" ", (32<<20)-len(prefix))+`{"error":"trailing response"}`)
	}))
	defer upstream.Close()
	s, repo, bill := proxyFixture(t, upstream.URL)
	s.chatCompletions(httptest.NewRecorder(), modelRequest(`{"model":"model-test"}`))
	if usageStatus(t, repo) != "REVIEW" || bill.commitCount() != 0 || bill.releaseCount() != 0 {
		t.Fatal("truncated response settled")
	}
}

func TestPostgresInactiveUnitDoesNotBlockValidOrganizationRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
	}))
	defer upstream.Close()
	s, repo, _ := proxyFixture(t, upstream.URL)
	ctx := context.Background()
	organization, unit, user := uuid.New(), uuid.New(), uuid.New()
	accountServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me/authorization-context" {
			json.NewEncoder(w).Encode(map[string]any{"user_id": user, "session_id": "org-session", "context_type": "ORGANIZATION", "member_version": 1, "context_version": 1, "authorization_version": 1, "organization_id": organization, "roles": []string{"MEMBER"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer accountServer.Close()
	s.contexts, _ = NewHTTPContextResolver(accountServer.URL)
	orgAccount, err := repo.CreateAccount(ctx, CreateAccountRequest{OwnerType: "ORG", OwnerID: organization, OwnerOrganization: &organization, WalletID: uuid.New(), Name: "working organization", ShareScope: "ORG_ALL", DefaultPriority: 100, CreatedBy: user})
	if err != nil {
		t.Fatal(err)
	}
	unitAccount, err := repo.CreateAccount(ctx, CreateAccountRequest{OwnerType: "ORG_UNIT", OwnerID: unit, OwnerOrganization: &organization, OwnerUnit: &unit, WalletID: uuid.New(), Name: "disabled unit", ShareScope: "ORG_ALL", DefaultPriority: 200, CreatedBy: user})
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []TokenAccount{orgAccount, unitAccount} {
		if _, err := repo.RotateCredential(ctx, account.ID, user, "test", upstream.URL, []byte("test-only-key"), []string{"model-test"}); err != nil {
			t.Fatal(err)
		}
	}
	// This routing test controls the member quota/wallet authority seams. Real
	// financial quota success and absence are covered by the dual-PG proxy test.
	s.wallets = scopedOrganizationWallet{organization, orgAccount.WalletID}
	s.billing = scopedOrganizationBilling{fixtureBilling: s.billing.(*fixtureBilling), organization: organization, member: user}
	listRequest := httptest.NewRequest(http.MethodGet, "/v1/token-accounts/eligible?model=model-test", nil)
	listRequest.Header.Set("Authorization", "Bearer caller")
	list := httptest.NewRecorder()
	s.listEligible(list, listRequest)
	var body struct {
		Items []struct {
			ID uuid.UUID `json:"token_account_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil || len(body.Items) != 1 || body.Items[0].ID != orgAccount.ID {
		t.Fatalf("eligible routes %s %v", list.Body.String(), err)
	}
	r := modelRequest(`{"model":"model-test"}`)
	r.Header.Set("Authorization", "Bearer caller")
	response := httptest.NewRecorder()
	s.chatCompletions(response, r)
	if response.Code != http.StatusOK || usageStatus(t, repo) != "COMMITTED" {
		t.Fatalf("fallback status %d body %s", response.Code, response.Body.String())
	}
	var used uuid.UUID
	if err := repo.pool.QueryRow(ctx, `SELECT token_account_id FROM usage_requests`).Scan(&used); err != nil || used != orgAccount.ID {
		t.Fatalf("selected disabled unit %v %v", used, err)
	}
	explicit := modelRequest(`{"model":"model-test"}`)
	explicit.Header.Set("Authorization", "Bearer caller")
	explicit.Header.Set("X-Pinvou-Token-Account-ID", unitAccount.ID.String())
	denied := httptest.NewRecorder()
	s.chatCompletions(denied, explicit)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("explicit disabled route %d", denied.Code)
	}
}

func TestPostgresProxyReservationBoundsValidCachedUsage(t *testing.T) {
	for name, price := range map[string]PriceVersion{"split-ceil": {ID: uuid.New(), InputPerMillion: 1, CachedInputPerMillion: 1}, "cached-higher": {ID: uuid.New(), InputPerMillion: 1_000_000, CachedInputPerMillion: 2_000_000}} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"usage":{"prompt_tokens":100,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":50}}}`)
			}))
			defer upstream.Close()
			s, repo, bill := proxyFixture(t, upstream.URL)
			bill.price = &price
			s.chatCompletions(httptest.NewRecorder(), modelRequest(`{"model":"model-test"}`))
			if usageStatus(t, repo) != "COMMITTED" || bill.commitCount() != 1 {
				t.Fatal("valid cached usage could not settle within reservation")
			}
		})
	}
}

func TestPostgresReviewConvergesAfterConfirmedBillingTerminal(t *testing.T) {
	for _, terminal := range []string{"COMMITTED", "RELEASED"} {
		t.Run(terminal, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
			defer upstream.Close()
			s, repo, bill := proxyFixture(t, upstream.URL)
			s.chatCompletions(httptest.NewRecorder(), modelRequest(`{"model":"model-test"}`))
			if usageStatus(t, repo) != "REVIEW" {
				t.Fatal("initial unknown result not reviewed")
			}
			var id, holdID uuid.UUID
			var maximum int64
			if err := repo.pool.QueryRow(context.Background(), `SELECT id,hold_id,maximum_microcredits FROM usage_requests`).Scan(&id, &holdID, &maximum); err != nil {
				t.Fatal(err)
			}
			if terminal == "COMMITTED" {
				s.settle(context.Background(), id, holdID, maximum, PriceVersion{InputPerMillion: 1_000_000, OutputPerMillion: 1_000_000}, Usage{InputTokens: 2, OutputTokens: 3})
				if bill.commitCount() != 1 {
					t.Fatal("Billing not confirmed committed")
				}
				if err := repo.MarkReleased(context.Background(), id, "conflicting terminal"); !errors.Is(err, ErrForbidden) {
					t.Fatalf("COMMITTED reversed: %v", err)
				}
			} else {
				s.releaseUnused(context.Background(), id, holdID, "confirmed unused")
				if bill.releaseCount() != 1 {
					t.Fatal("Billing not confirmed released")
				}
				if err := repo.MarkCommitted(context.Background(), id, 5, map[string]any{}); !errors.Is(err, ErrForbidden) {
					t.Fatalf("RELEASED charged: %v", err)
				}
			}
			if usageStatus(t, repo) != terminal {
				t.Fatalf("confirmed Billing %s left Token in review", terminal)
			}
		})
	}
}

func TestRechargeInvalidIdempotencyKeyReturnsClientErrorOverHTTP(t *testing.T) {
	wallet := &walletSpy{}
	s := NewService(nil, fixtureContexts{AuthorizationContext{UserID: uuid.New(), SessionID: "session", ContextType: "PERSONAL", ContextVersion: 1, AuthorizationVersion: 1}}, wallet, nil, ProviderURLPolicy{}, nil, nil, 100)
	server := httptest.NewServer(http.HandlerFunc(s.createRechargeOrder))
	defer server.Close()
	for name, key := range map[string]string{"too-few-characters": strings.Repeat("界", 15), "too-many-characters": strings.Repeat("a", 129), "invalid-utf8": strings.Repeat("a", 15) + string([]byte{0xff})} {
		t.Run(name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"product_id":"CNY_1","channel":"ALIPAY"}`))
			request.Header.Set("Idempotency-Key", key)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != http.StatusBadRequest || body.Code != "IDEMPOTENCY_KEY_REQUIRED" {
				t.Fatalf("client input classified as upstream failure: %d %s %v", response.StatusCode, body.Code, err)
			}
		})
	}
	if wallet.calls != 0 {
		t.Fatal("invalid key provisioned a wallet")
	}
}
