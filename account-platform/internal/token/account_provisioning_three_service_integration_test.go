package token

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/Pinvou/pinvou3/account-platform/internal/account"
	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenmigrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type threeServiceActor struct {
	id      uuid.UUID
	issuer  string
	subject string
	token   string
}

type threeServiceActivation struct {
	mu      sync.RWMutex
	current TokenCommandGovernanceActivation
}

func (a *threeServiceActivation) CurrentTokenCommandGovernanceActivation(context.Context) (TokenCommandGovernanceActivation, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.current, nil
}

func (a *threeServiceActivation) LockAndCheckTokenRouteActivation(_ context.Context, _ pgx.Tx, selected TokenCommandGovernanceActivation) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.current != selected || selected.State != "ACTIVE" {
		return ErrTokenRouteActivationChanged
	}
	return nil
}

func (a *threeServiceActivation) set(state string, epoch int64) {
	a.mu.Lock()
	a.current = TokenCommandGovernanceActivation{State: state, Epoch: epoch}
	a.mu.Unlock()
}

// responseLossHandler lets the real downstream handler commit before the
// connection is closed. The caller observes an ambiguous transport result and
// must recover through the production GET endpoint.
type responseLossHandler struct {
	next  http.Handler
	mu    sync.Mutex
	drop  map[string]int
	seen  map[string]int
	after map[string][]func()
}

type oneShotJSONReader struct {
	body   []byte
	offset int
}

func (r *oneShotJSONReader) Read(target []byte) (int, error) {
	if r.offset >= len(r.body) {
		return 0, io.EOF
	}
	n := copy(target, r.body[r.offset:])
	r.offset += n
	return n, nil
}

func newResponseLossHandler(next http.Handler) *responseLossHandler {
	return &responseLossHandler{next: next, drop: map[string]int{}, seen: map[string]int{}, after: map[string][]func(){}}
}

func (h *responseLossHandler) afterNext(key string, action func()) {
	h.mu.Lock()
	h.after[key] = append(h.after[key], action)
	h.mu.Unlock()
}

func (h *responseLossHandler) arm(key string) {
	h.mu.Lock()
	h.drop[key]++
	h.mu.Unlock()
}

func (h *responseLossHandler) count(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen[key]
}

func (h *responseLossHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	recorder := httptest.NewRecorder()
	h.next.ServeHTTP(recorder, r)
	h.mu.Lock()
	h.seen[key]++
	drop := h.drop[key] > 0
	if drop {
		h.drop[key]--
	}
	var after func()
	if actions := h.after[key]; len(actions) > 0 {
		after = actions[0]
		h.after[key] = actions[1:]
	}
	h.mu.Unlock()
	if after != nil {
		after()
	}
	if drop {
		if hijacker, ok := w.(http.Hijacker); ok {
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
				return
			}
		}
		return
	}
	for name, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

// TestTokenAccountProvisioningThreeServicePostgresIntegration is the no-SKIP
// APT-123-R2 release gate. It composes the real Account, Token and Billing
// repositories, production HTTP handlers and production clients. Faults are
// injected only after a real handler has committed, never by replacing the
// domain service with a fake.
func TestTokenAccountProvisioningThreeServicePostgresIntegration(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", "")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", "")
	tokenURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	accountURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	billingURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if tokenURL == "" || accountURL == "" || billingURL == "" {
		t.Skip("three-service PostgreSQL URLs are not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	accountRepository, accountConnection := newThreeServiceAccountRepository(t, ctx, accountURL)
	billingRepository, billingConnection := newThreeServiceBillingRepository(t, ctx, billingURL)
	tokenRepository := newThreeServiceTokenRepository(t, ctx, tokenURL)
	if err := tokenRepository.ConfigureAdminPagination(AdminPaginationConfig{Enabled: true, CursorHMACKey: bytes.Repeat([]byte{23}, 32)}); err != nil {
		t.Fatal(err)
	}
	if err := tokenRepository.SetAdminPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	activation := &threeServiceActivation{current: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 31}}
	tokenRepository.WithTokenRouteActivationTransactionGuard(activation)

	creator := seedThreeServiceActor(t, ctx, accountConnection, "creator")
	operator := seedThreeServiceActor(t, ctx, accountConnection, "operator")
	owner := seedThreeServiceOwner(t, ctx, accountConnection, "owner")
	const serviceToken = "apt-123-three-service-workload"
	accountProxy := newResponseLossHandler(accountdomain.NewService(accountRepository, nil).WithServiceToken(serviceToken).Handler(nil))
	accountServer := httptest.NewServer(accountProxy)
	t.Cleanup(accountServer.Close)
	billingProxy := newResponseLossHandler(billingdomain.NewService(billingRepository).Handler(serviceToken))
	billingServer := httptest.NewServer(billingProxy)
	t.Cleanup(billingServer.Close)
	authorizer, err := authorization.NewClient(accountServer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewHTTPOwnerAuthorityClient(accountServer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	wallets, err := NewHTTPWalletResolver(billingServer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	actors := map[string]threeServiceActor{creator.token: creator, operator.token: operator}
	verify := func(_ context.Context, raw string) (identity.Principal, error) {
		lookup := raw
		if raw == "stale-human-token" {
			lookup = operator.token
		}
		actor, ok := actors[lookup]
		if !ok {
			return identity.Principal{}, errors.New("unknown integration bearer")
		}
		authenticatedAt := time.Now().UTC().Add(-time.Minute)
		if raw == "stale-human-token" {
			authenticatedAt = time.Now().UTC().Add(-20 * time.Minute)
		}
		return identity.Principal{Issuer: actor.issuer, Subject: actor.subject, SessionID: "three-service-" + actor.token, AuthenticatedAt: authenticatedAt}, nil
	}
	service := NewService(tokenRepository, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithAuthorizer(authorizer).
		WithTokenAccountProvisioning(wallets, authority).
		WithTokenAccountCreateCapability(true).
		WithTokenCommandGovernanceActivation(activation)
	tokenProxy := newResponseLossHandler(service.Handler("", verify))
	tokenServer := httptest.NewServer(tokenProxy)
	t.Cleanup(tokenServer.Close)

	// PRE is closed before any Account or Billing request and writes no Token
	// command/reservation fact. This is the production state until APT-138.
	preActivation := &threeServiceActivation{current: TokenCommandGovernanceActivation{State: "PRE_ACTIVATION", Epoch: 30}}
	preService := NewService(tokenRepository, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithAuthorizer(authorizer).
		WithTokenAccountProvisioning(wallets, authority).
		WithTokenCommandGovernanceActivation(preActivation)
	beforeAuthority := accountProxy.count(http.MethodPost + " /internal/v1/token-account-authority/snapshot")
	preResponse, _, preErr := threeServiceJSON(ctx, preService.Handler("", verify), http.MethodPost, "/admin/v1/commands:precheck", creator.token, "", TokenCommandPrecheckRequest{CommandType: "TOKEN_ACCOUNT_CREATE", TargetResourceType: "TOKEN_ACCOUNT", TargetResourceID: owner.id.String(), Command: threeServiceAccountCommand(t, owner.id, "PRE blocked")})
	if preErr != nil || preResponse != http.StatusServiceUnavailable || accountProxy.count(http.MethodPost+" /internal/v1/token-account-authority/snapshot") != beforeAuthority {
		t.Fatalf("PRE gate status=%d authority delta=%d err=%v", preResponse, accountProxy.count(http.MethodPost+" /internal/v1/token-account-authority/snapshot")-beforeAuthority, preErr)
	}

	// An active unit from another organization must not pass T1 or create any
	// Token/Billing facts. Parent organization state is authoritative as well.
	unitOrg, wrongOrg, unitID := uuid.New(), uuid.New(), uuid.New()
	if _, err := accountConnection.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'unit org','ACTIVE',$3,now(),now()),($2,'wrong org','ACTIVE',$3,now(),now()); INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($5,$1,$3,'ACTIVE',now(),now()),($6,$2,$3,'ACTIVE',now(),now()); INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at) VALUES(gen_random_uuid(),$5,'OWNER',$3,now(),now()),(gen_random_uuid(),$6,'OWNER',$3,now(),now()); INSERT INTO organization_units(id,organization_id,parent_id,name,status,created_at,updated_at) VALUES($4,$1,NULL,'unit','ACTIVE',now(),now())`, unitOrg, wrongOrg, creator.id, unitID, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	unitCommand := governedAccountCreateCommand{OwnerType: "ORG_UNIT", OwnerID: unitID, OwnerOrganization: &wrongOrg, OwnerUnit: &unitID, Name: "mismatched organization", ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100}
	unitRaw, _ := json.Marshal(unitCommand)
	unitStatus, unitBody, unitErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands:precheck", creator.token, "", TokenCommandPrecheckRequest{CommandType: "TOKEN_ACCOUNT_CREATE", TargetResourceType: "TOKEN_ACCOUNT", TargetResourceID: unitID.String(), Command: unitRaw})
	if unitErr != nil || unitStatus == http.StatusOK {
		t.Fatalf("mismatched unit precheck=%d/%s err=%v", unitStatus, unitBody, unitErr)
	}
	unitCommand.OwnerOrganization = &unitOrg
	unitRaw, _ = json.Marshal(unitCommand)
	unitStatus, unitBody, unitErr = threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands:precheck", creator.token, "", TokenCommandPrecheckRequest{CommandType: "TOKEN_ACCOUNT_CREATE", TargetResourceType: "TOKEN_ACCOUNT", TargetResourceID: unitID.String(), Command: unitRaw})
	if unitErr != nil || unitStatus != http.StatusOK {
		t.Fatalf("valid unit precheck=%d/%s err=%v", unitStatus, unitBody, unitErr)
	}
	if _, err := accountConnection.Exec(ctx, `UPDATE organizations SET status='DISABLED',version=version+1 WHERE id=$1`, unitOrg); err != nil {
		t.Fatal(err)
	}
	unitStatus, unitBody, unitErr = threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands:precheck", creator.token, "", TokenCommandPrecheckRequest{CommandType: "TOKEN_ACCOUNT_CREATE", TargetResourceType: "TOKEN_ACCOUNT", TargetResourceID: unitID.String(), Command: unitRaw})
	if unitErr != nil || unitStatus == http.StatusOK {
		t.Fatalf("inactive organization unit precheck=%d/%s err=%v", unitStatus, unitBody, unitErr)
	}
	var unitFacts, unitWallets int
	if err := tokenRepository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_accounts WHERE owner_id=$1)+(SELECT count(*) FROM token_account_provisioning_reservations WHERE owner_id=$1)`, unitID).Scan(&unitFacts); err != nil || unitFacts != 0 {
		t.Fatalf("unit Token facts=%d err=%v", unitFacts, err)
	}
	if err := billingConnection.QueryRow(ctx, `SELECT count(*) FROM token_account_wallet_provisioning_receipts WHERE owner_id=$1`, unitID).Scan(&unitWallets); err != nil || unitWallets != 0 {
		t.Fatalf("unit Billing facts=%d err=%v", unitWallets, err)
	}

	// T1 response loss: the Token handler commits the immutable reservation,
	// then the network disappears. The actor-scoped key lookup returns the same
	// reference and no second command POST is needed.
	lostKey := "apt-123-three-service-t1-loss"
	lostEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, owner.id, "T1 response loss")
	tokenProxy.arm(http.MethodPost + " /admin/v1/commands")
	_, _, lostErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, lostKey, lostEnvelope)
	if lostErr == nil {
		t.Fatal("Token response loss was not observed")
	}
	lost := threeServiceLookupCommand(t, ctx, tokenServer.URL, creator, lostKey)
	if lost.Status != "PENDING" || lost.CommandReference == uuid.Nil {
		t.Fatalf("T1 lookup=%+v", lost)
	}

	// T2 response loss: Billing commits wallet+receipt and closes the socket.
	// The production worker performs exactly one POST then recovers by GET.
	billingPost := http.MethodPost + " /internal/v1/token-account-wallet-provisioning-receipts"
	billingGet := http.MethodGet + " /internal/v1/token-account-wallet-provisioning-receipts/" + lost.CommandReference.String()
	billingProxy.arm(billingPost)
	worker, err := NewTokenAccountProvisioningWorker(tokenRepository, wallets, authority, "three-service-worker-a", TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 31})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed || billingProxy.count(billingPost) != 1 || billingProxy.count(billingGet) != 1 {
		t.Fatalf("T2 recovery processed=%v post/get=%d/%d err=%v", processed, billingProxy.count(billingPost), billingProxy.count(billingGet), err)
	}
	completed := threeServiceCommandResult(t, ctx, tokenServer.URL, creator, lost.CommandReference)
	if completed.Status != "SUCCEEDED" {
		t.Fatalf("completed three-service command=%+v", completed)
	}
	assertThreeServiceFacts(t, ctx, tokenRepository, billingConnection, lost.CommandReference, owner.id, 1, "SUCCEEDED")

	// An owner lifecycle/version change between the reviewed precheck and T1 is
	// rejected before reservation, wallet, audit or outbox facts are created.
	driftOwner := seedThreeServiceOwner(t, ctx, accountConnection, "drift-owner")
	driftEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, driftOwner.id, "Owner version drift")
	var receiptsBeforeDrift int
	if err = billingConnection.QueryRow(ctx, `SELECT count(*) FROM token_account_wallet_provisioning_receipts`).Scan(&receiptsBeforeDrift); err != nil {
		t.Fatal(err)
	}
	if _, err = accountConnection.Exec(ctx, `UPDATE users SET version=version+1,updated_at=transaction_timestamp() WHERE id=$1`, driftOwner.id); err != nil {
		t.Fatal(err)
	}
	driftStatus, driftBody, driftErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, "apt-123-owner-version-drift", driftEnvelope)
	if driftErr != nil || driftStatus != http.StatusConflict || !bytes.Contains(driftBody, []byte("COMMAND_PRECHECK_CHANGED")) {
		t.Fatalf("owner drift status=%d body=%s err=%v", driftStatus, driftBody, driftErr)
	}
	assertNoThreeServiceCommand(t, ctx, tokenRepository, billingConnection, creator.id, "apt-123-owner-version-drift", receiptsBeforeDrift)

	revokedActor := seedThreeServiceActor(t, ctx, accountConnection, "permission-revoked")
	revokedOwner := seedThreeServiceOwner(t, ctx, accountConnection, "permission-owner")
	actors[revokedActor.token] = revokedActor
	revokedEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, revokedActor, revokedOwner.id, "Permission revoked before T1")
	if _, err = accountConnection.Exec(ctx, `DELETE FROM role_assignments WHERE user_id=$1 AND role_code='PLATFORM_MODEL_ADMIN'`, revokedActor.id); err != nil {
		t.Fatal(err)
	}
	revokedStatus, revokedBody, revokedErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", revokedActor.token, "apt-123-permission-revoked", revokedEnvelope)
	if revokedErr != nil || revokedStatus != http.StatusForbidden || !bytes.Contains(revokedBody, []byte("PERMISSION_DENIED")) {
		t.Fatalf("permission drift status=%d body=%s err=%v", revokedStatus, revokedBody, revokedErr)
	}
	assertNoThreeServiceCommand(t, ctx, tokenRepository, billingConnection, revokedActor.id, "apt-123-permission-revoked", receiptsBeforeDrift)

	// The owner-cap policy is also frozen by version and digest. An otherwise
	// equivalent cap revision between precheck and T1 is still a conflict and
	// cannot leave a command, reservation or Billing receipt.
	policyOwner := seedThreeServiceOwner(t, ctx, accountConnection, "policy-owner")
	policyEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, policyOwner.id, "Policy version drift")
	installThreeServicePolicy(t, ctx, tokenRepository, 2, 1000, "three-service policy drift")
	policyStatus, policyBody, policyErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, "apt-123-policy-version-drift", policyEnvelope)
	if policyErr != nil || policyStatus != http.StatusConflict || !bytes.Contains(policyBody, []byte("COMMAND_PRECHECK_CHANGED")) {
		t.Fatalf("policy drift status=%d body=%s err=%v", policyStatus, policyBody, policyErr)
	}
	assertNoThreeServiceCommand(t, ctx, tokenRepository, billingConnection, creator.id, "apt-123-policy-version-drift", receiptsBeforeDrift)

	// A committed Billing receipt followed by a lost Account T3 response stays
	// PENDING. The next fenced lease uses the existing receipt and a fresh real
	// authority response to converge without a second domain effect.
	t3Owner := seedThreeServiceOwner(t, ctx, accountConnection, "t3-loss-owner")
	t3Envelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, t3Owner.id, "T3 response loss")
	t3Status, t3Body, t3Err := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, "apt-123-t3-response-loss", t3Envelope)
	t3Command := decodeThreeServiceCommand(t, t3Status, t3Body, t3Err)
	accountProxy.arm(http.MethodPost + " /internal/v1/token-account-authority/snapshot")
	if processed, err = worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("T3 response loss processed=%v err=%v", processed, err)
	}
	var pendingStatus string
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT status FROM token_command_results WHERE command_reference=$1`, t3Command.CommandReference).Scan(&pendingStatus); err != nil || pendingStatus != "PENDING" {
		t.Fatalf("T3 response loss status=%s err=%v", pendingStatus, err)
	}
	if _, err = tokenRepository.pool.Exec(ctx, `UPDATE token_account_provisioning_reservations SET next_attempt_at=transaction_timestamp() WHERE command_reference=$1`, t3Command.CommandReference); err != nil {
		t.Fatal(err)
	}
	if processed, err = worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("T3 GET recovery processed=%v err=%v", processed, err)
	}
	if result := threeServiceCommandResult(t, ctx, tokenServer.URL, creator, t3Command.CommandReference); result.Status != "SUCCEEDED" {
		t.Fatalf("T3 recovered result=%+v", result)
	}

	// A definitive creator lifecycle change after T1 is read from the real
	// Account authority endpoint at T3 and releases the reservation without
	// exposing or activating a Token Account. The owner wallet receipt remains
	// recoverable owner-level infrastructure by contract.
	failedCreator := seedThreeServiceActor(t, ctx, accountConnection, "disabled-after-t1")
	failedOwner := seedThreeServiceOwner(t, ctx, accountConnection, "disabled-owner")
	actors[failedCreator.token] = failedCreator
	failedKey := "apt-123-creator-disabled"
	failedEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, failedCreator, failedOwner.id, "Creator disabled after T1")
	status, body, callErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", failedCreator.token, failedKey, failedEnvelope)
	failed := decodeThreeServiceCommand(t, status, body, callErr)
	if _, err = accountConnection.Exec(ctx, `UPDATE users SET status='DISABLED',version=version+1,updated_at=transaction_timestamp() WHERE id=$1`, failedCreator.id); err != nil {
		t.Fatal(err)
	}
	if processed, err = worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("definitive T3 processed=%v err=%v", processed, err)
	}
	failed, err = tokenRepository.TokenCommandResult(ctx, failed.CommandReference, failedCreator.id)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "FAILED" || failed.StableErrorCode == nil || *failed.StableErrorCode != "ACCOUNT_CREATOR_UNAVAILABLE" {
		t.Fatalf("definitive T3 result=%+v", failed)
	}
	assertThreeServiceTerminalFailure(t, ctx, tokenRepository, billingConnection, failed.CommandReference, failedOwner.id)

	// SAFE can begin after Account has produced ALLOW but before Token finalizes.
	// The real Billing receipt survives; no account or terminal result may commit.
	inflightOwner := seedThreeServiceOwner(t, ctx, accountConnection, "inflight-safe-owner")
	inflightEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, inflightOwner.id, "SAFE during T3")
	inflightStatus, inflightBody, inflightErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, "apt-123-inflight-safe", inflightEnvelope)
	inflight := decodeThreeServiceCommand(t, inflightStatus, inflightBody, inflightErr)
	accountProxy.afterNext(http.MethodPost+" /internal/v1/token-account-authority/snapshot", func() { activation.set("SAFE_ROLLBACK", 32) })
	if processed, err = worker.ProcessOne(ctx); !processed || !errors.Is(err, ErrTokenRouteActivationChanged) {
		t.Fatalf("inflight SAFE processed=%v err=%v", processed, err)
	}
	assertThreeServicePendingWithoutAccount(t, ctx, tokenRepository, inflight.CommandReference, inflightOwner.id)
	activation.set("ACTIVE", 33)
	worker, err = NewTokenAccountProvisioningWorker(tokenRepository, wallets, authority, "three-service-worker-after-safe", TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 33})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tokenRepository.pool.Exec(ctx, `UPDATE token_account_provisioning_reservations SET lease_until=transaction_timestamp()-interval '1 second',next_attempt_at=transaction_timestamp() WHERE command_reference=$1`, inflight.CommandReference); err != nil {
		t.Fatal(err)
	}
	if processed, err = worker.ProcessOne(ctx); !processed || err != nil {
		t.Fatalf("inflight SAFE recovery processed=%v err=%v", processed, err)
	}
	assertThreeServiceFacts(t, ctx, tokenRepository, billingConnection, inflight.CommandReference, inflightOwner.id, 1, "SUCCEEDED")

	// A lease that expires after a genuine authority response cannot finalize,
	// even when no replacement worker has claimed it yet. This exercises the
	// expiry check independently of the random fencing token/version checks.
	expiredOwner := seedThreeServiceOwner(t, ctx, accountConnection, "expired-lease-owner")
	expiredEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, expiredOwner.id, "Lease expires during T3")
	expiredStatus, expiredBody, expiredErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, "apt-123-expired-lease", expiredEnvelope)
	expired := decodeThreeServiceCommand(t, expiredStatus, expiredBody, expiredErr)
	var expireHookErr error
	accountProxy.afterNext(http.MethodPost+" /internal/v1/token-account-authority/snapshot", func() {
		_, expireHookErr = tokenRepository.pool.Exec(ctx, `UPDATE token_account_provisioning_reservations SET lease_until=transaction_timestamp()-interval '1 second' WHERE command_reference=$1`, expired.CommandReference)
	})
	if processed, err = worker.ProcessOne(ctx); !processed || !errors.Is(err, ErrCatalogVersionConflict) || expireHookErr != nil {
		t.Fatalf("expired lease processed=%v err=%v hook=%v", processed, err, expireHookErr)
	}
	assertThreeServicePendingWithoutAccount(t, ctx, tokenRepository, expired.CommandReference, expiredOwner.id)
	if processed, err = worker.ProcessOne(ctx); !processed || err != nil {
		t.Fatalf("expired lease recovery processed=%v err=%v", processed, err)
	}
	assertThreeServiceFacts(t, ctx, tokenRepository, billingConnection, expired.CommandReference, expiredOwner.id, 1, "SUCCEEDED")

	// SAFE_ROLLBACK prevents new leases. Returning to the selected ACTIVE epoch
	// permits two worker instances to race; SKIP LOCKED/fencing and Billing's
	// existing-first receipt still produce one account and one terminal result.
	safeOwner := seedThreeServiceOwner(t, ctx, accountConnection, "safe-owner")
	safeKey := "apt-123-safe-lease"
	safeEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, safeOwner.id, "SAFE lease boundary")
	safeStatus, safeBody, safeCallErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, safeKey, safeEnvelope)
	safeCommand := decodeThreeServiceCommand(t, safeStatus, safeBody, safeCallErr)
	activation.set("SAFE_ROLLBACK", 34)
	if processed, err = worker.ProcessOne(ctx); !errors.Is(err, ErrTokenRouteActivationChanged) || processed {
		t.Fatalf("SAFE worker processed=%v err=%v", processed, err)
	}
	var leased int
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT count(*) FROM token_account_provisioning_reservations WHERE command_reference=$1 AND lease_owner IS NOT NULL`, safeCommand.CommandReference).Scan(&leased); err != nil || leased != 0 {
		t.Fatalf("SAFE lease count=%d err=%v", leased, err)
	}
	activation.set("ACTIVE", 35)
	workers := make([]*TokenAccountProvisioningWorker, 2)
	for index := range workers {
		workers[index], err = NewTokenAccountProvisioningWorker(tokenRepository, wallets, authority, "three-service-takeover-"+string(rune('a'+index)), TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 35})
		if err != nil {
			t.Fatal(err)
		}
	}
	var workerGroup sync.WaitGroup
	workerGroup.Add(2)
	for _, current := range workers {
		go func(item *TokenAccountProvisioningWorker) {
			defer workerGroup.Done()
			_, _ = item.ProcessOne(ctx)
		}(current)
	}
	workerGroup.Wait()
	worker = workers[0]
	if result := threeServiceCommandResult(t, ctx, tokenServer.URL, creator, safeCommand.CommandReference); result.Status != "SUCCEEDED" {
		t.Fatalf("lease takeover result=%+v", result)
	}

	// MANUAL_REVIEW is recovered by a different currently authorized operator.
	// OPERATOR_AUTHORIZE and T3_OPERATOR both traverse the real Account handler;
	// the original creator checkpoint remains immutable.
	manualOwner := seedThreeServiceOwner(t, ctx, accountConnection, "manual-owner")
	manualEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, manualOwner.id, "Manual recovery")
	manualStatus, manualBody, manualErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, "apt-123-manual-review", manualEnvelope)
	manual := decodeThreeServiceCommand(t, manualStatus, manualBody, manualErr)
	if _, err = tokenRepository.pool.Exec(ctx, `UPDATE token_account_provisioning_reservations SET attempt_count=8,next_attempt_at=transaction_timestamp(),lease_owner=NULL,lease_until=NULL,lease_fencing_token=NULL WHERE command_reference=$1`, manual.CommandReference); err != nil {
		t.Fatal(err)
	}
	if processed, err = worker.ProcessOne(ctx); err != nil || processed {
		t.Fatalf("manual transition processed=%v err=%v", processed, err)
	}
	// A later operator can discover persisted work after reload without having
	// observed the original POST. Two real commands exercise server-side paging
	// and actor-bound cursors, including the closed public projection.
	queueEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, manualOwner.id, "Second manual recovery item")
	queueStatus, queueBody, queueErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, "apt-123-queue-second", queueEnvelope)
	queued := decodeThreeServiceCommand(t, queueStatus, queueBody, queueErr)
	if _, err = tokenRepository.pool.Exec(ctx, `UPDATE token_account_provisioning_reservations SET attempt_count=8,next_attempt_at=transaction_timestamp() WHERE command_reference=$1`, queued.CommandReference); err != nil {
		t.Fatal(err)
	}
	if processed, err = worker.ProcessOne(ctx); err != nil || processed {
		t.Fatalf("queue second manual transition processed=%v err=%v", processed, err)
	}
	queuePath := "/admin/v1/commands/recovery-queue?limit=1"
	queueStatus, queueBody, queueErr = threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodGet, queuePath, operator.token, "", nil)
	var queuePage struct {
		Items      []AccountRecoveryQueueItem `json:"items"`
		NextCursor string                     `json:"next_cursor"`
		HasMore    bool                       `json:"has_more"`
	}
	if queueErr != nil || queueStatus != http.StatusOK || json.Unmarshal(queueBody, &queuePage) != nil || len(queuePage.Items) != 1 || !queuePage.HasMore || queuePage.NextCursor == "" {
		t.Fatalf("recovery queue page=%d/%s err=%v", queueStatus, queueBody, queueErr)
	}
	for _, forbidden := range []string{"actor_user_id", "owner_id", "lease_owner", "wallet_id", "identity_digest", "request_digest"} {
		if bytes.Contains(queueBody, []byte(forbidden)) {
			t.Fatalf("recovery queue leaked %s", forbidden)
		}
	}
	secondPath := queuePath + "&cursor=" + url.QueryEscape(queuePage.NextCursor)
	crossStatus, _, crossErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodGet, secondPath, creator.token, "", nil)
	if crossErr != nil || crossStatus != http.StatusBadRequest {
		t.Fatalf("cross-operator cursor status=%d err=%v", crossStatus, crossErr)
	}
	secondStatus, secondBody, secondErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodGet, secondPath, operator.token, "", nil)
	var secondPage struct {
		Items   []AccountRecoveryQueueItem `json:"items"`
		HasMore bool                       `json:"has_more"`
	}
	if secondErr != nil || secondStatus != http.StatusOK || json.Unmarshal(secondBody, &secondPage) != nil || len(secondPage.Items) != 1 || secondPage.HasMore || secondPage.Items[0].CommandReference == queuePage.Items[0].CommandReference {
		t.Fatalf("recovery queue next=%d/%s err=%v", secondStatus, secondBody, secondErr)
	}
	accountCallsBeforeStale := accountProxy.count(http.MethodPost + " /internal/v1/token-account-authority/snapshot")
	billingCallsBeforeStale := billingProxy.count(http.MethodGet + " /internal/v1/token-account-wallet-provisioning-receipts/" + manual.CommandReference.String())
	staleStatus, staleBody, staleErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands/"+manual.CommandReference.String()+"/recovery-actions", "stale-human-token", "apt-123-stale-cancel-0001", map[string]any{"action": "CANCEL", "expected_recovery_epoch": 1, "reason": "cancel after a stale authentication attempt"})
	if staleErr != nil || staleStatus != http.StatusForbidden || !bytes.Contains(staleBody, []byte("RECENT_AUTH_REQUIRED")) || accountProxy.count(http.MethodPost+" /internal/v1/token-account-authority/snapshot") != accountCallsBeforeStale || billingProxy.count(http.MethodGet+" /internal/v1/token-account-wallet-provisioning-receipts/"+manual.CommandReference.String()) != billingCallsBeforeStale {
		t.Fatalf("stale CANCEL status=%d body=%s account/billing delta=%d/%d err=%v", staleStatus, staleBody, accountProxy.count(http.MethodPost+" /internal/v1/token-account-authority/snapshot")-accountCallsBeforeStale, billingProxy.count(http.MethodGet+" /internal/v1/token-account-wallet-provisioning-receipts/"+manual.CommandReference.String())-billingCallsBeforeStale, staleErr)
	}
	var staleActionFacts int
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT count(*) FROM token_account_provisioning_recovery_actions WHERE command_reference=$1`, manual.CommandReference).Scan(&staleActionFacts); err != nil || staleActionFacts != 0 {
		t.Fatalf("stale CANCEL action facts=%d err=%v", staleActionFacts, err)
	}
	changedOperator := seedThreeServiceActor(t, ctx, accountConnection, "operator-disabled-between-phases")
	actors[changedOperator.token] = changedOperator
	var disableOperatorErr error
	accountProxy.afterNext(http.MethodPost+" /internal/v1/token-account-authority/snapshot", func() {
		_, disableOperatorErr = accountConnection.Exec(ctx, `UPDATE users SET status='DISABLED',version=version+1,updated_at=transaction_timestamp() WHERE id=$1`, changedOperator.id)
	})
	changedStatus, changedBody, changedErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands/"+manual.CommandReference.String()+"/recovery-actions", changedOperator.token, "apt-123-changed-operator", map[string]any{"action": "REQUEUE", "expected_recovery_epoch": 1, "reason": "operator lifecycle changed between the two authority checks"})
	if changedErr != nil || disableOperatorErr != nil || changedStatus != http.StatusForbidden || !bytes.Contains(changedBody, []byte("PERMISSION_DENIED")) {
		t.Fatalf("changed operator status=%d body=%s err=%v hook=%v", changedStatus, changedBody, changedErr, disableOperatorErr)
	}
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT count(*) FROM token_account_provisioning_recovery_actions WHERE command_reference=$1`, manual.CommandReference).Scan(&staleActionFacts); err != nil || staleActionFacts != 0 {
		t.Fatalf("changed operator action facts=%d err=%v", staleActionFacts, err)
	}
	actionPath := "/admin/v1/commands/" + manual.CommandReference.String() + "/recovery-actions"
	tokenProxy.arm(http.MethodPost + " " + actionPath)
	_, _, actionErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, actionPath, operator.token, "apt-123-recovery-action-0001", map[string]any{"action": "REQUEUE", "expected_recovery_epoch": 1, "reason": "resume after an operator reviewed the authoritative evidence"})
	if actionErr == nil {
		t.Fatal("manual recovery response loss was not observed")
	}
	actionStatus, actionBody, actionErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodGet, actionPath+"/by-idempotency-key", operator.token, "apt-123-recovery-action-0001", nil)
	if actionErr != nil || actionStatus != http.StatusOK || tokenProxy.count(http.MethodPost+" "+actionPath) != 3 {
		// Earlier POSTs were rejected for stale auth and operator lifecycle drift.
		t.Fatalf("manual key recovery status=%d body=%s post=%d err=%v", actionStatus, actionBody, tokenProxy.count(http.MethodPost+" "+actionPath), actionErr)
	}
	var action TokenAccountRecoveryActionResult
	if json.Unmarshal(actionBody, &action) != nil || action.Status != "SUCCEEDED" || action.Action != "REQUEUE" || action.CommandReference != manual.CommandReference {
		t.Fatalf("manual action=%s", actionBody)
	}
	lookupStatus, lookupBody, lookupErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodGet, "/admin/v1/commands/"+manual.CommandReference.String()+"/recovery-actions/"+action.ActionID.String(), operator.token, "", nil)
	if lookupErr != nil || lookupStatus != http.StatusOK || !bytes.Equal(bytes.TrimSpace(lookupBody), bytes.TrimSpace(actionBody)) {
		t.Fatalf("manual action GET=%d/%s err=%v", lookupStatus, lookupBody, lookupErr)
	}
	var creatorCheckpoint, operatorCheckpoint uuid.UUID
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT reservation.actor_user_id,recovery.operator_actor_id FROM token_account_provisioning_reservations reservation JOIN token_account_provisioning_recovery_actions recovery USING(command_reference) WHERE reservation.command_reference=$1`, manual.CommandReference).Scan(&creatorCheckpoint, &operatorCheckpoint); err != nil || creatorCheckpoint != creator.id || operatorCheckpoint != operator.id {
		t.Fatalf("creator/operator checkpoint=%s/%s err=%v", creatorCheckpoint, operatorCheckpoint, err)
	}
	var recoveryFacts, recoveryAudits, recoveryEvents int
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_account_provisioning_recovery_actions WHERE command_reference=$1),(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_ACCOUNT_PROVISIONING_RECOVERY' AND summary->>'command_reference'=$1::text),(SELECT count(*) FROM outbox_events WHERE event_type='token.account-provisioning.recovery.v1' AND aggregate_id=$1)`, manual.CommandReference).Scan(&recoveryFacts, &recoveryAudits, &recoveryEvents); err != nil || recoveryFacts != 1 || recoveryAudits != 1 || recoveryEvents != 1 {
		t.Fatalf("manual recovery exactly-once facts/actions/audit/outbox=%d/%d/%d err=%v", recoveryFacts, recoveryAudits, recoveryEvents, err)
	}
	var completeOperatorEvidence bool
	var auditReason string
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT summary->>'reason' FROM token_audit_logs WHERE action='TOKEN_ACCOUNT_PROVISIONING_RECOVERY' AND summary->>'audit_reference'=$1`, action.AuditReference.String()).Scan(&auditReason); err != nil || auditReason != "resume after an operator reviewed the authoritative evidence" {
		t.Fatalf("recovery audit reason=%q err=%v", auditReason, err)
	}
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT expected_operator_platform_authorization_version<=operator_platform_authorization_version AND expected_operator_actor_version<=operator_actor_version AND length(expected_operator_actor_identity_digest)=64 AND expected_operator_owner_version<=operator_owner_version AND length(expected_operator_owner_identity_digest)=64 FROM token_account_provisioning_recovery_actions WHERE action_id=$1`, action.ActionID).Scan(&completeOperatorEvidence); err != nil || !completeOperatorEvidence {
		t.Fatalf("missing expected/observed operator evidence=%v err=%v", completeOperatorEvidence, err)
	}
	// Same key in another operator's namespace must not collide with the first
	// operator's action. Both audit facts retain their distinct human identity.
	if _, err = tokenRepository.pool.Exec(ctx, `UPDATE token_account_provisioning_reservations SET attempt_count=8,next_attempt_at=now() WHERE command_reference=$1`, manual.CommandReference); err != nil {
		t.Fatal(err)
	}
	if processed, err = worker.ProcessOne(ctx); err != nil || processed {
		t.Fatalf("second manual transition=%v err=%v", processed, err)
	}
	crossActionStatus, crossActionBody, crossActionErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, actionPath, creator.token, "apt-123-recovery-action-0001", map[string]any{"action": "REQUEUE", "expected_recovery_epoch": 2, "reason": "another operator reviewed the subsequent manual recovery"})
	if crossActionErr != nil || crossActionStatus != http.StatusAccepted {
		t.Fatalf("cross-operator same key=%d/%s err=%v", crossActionStatus, crossActionBody, crossActionErr)
	}
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT count(DISTINCT operator_actor_id) FROM token_account_provisioning_recovery_actions WHERE command_reference=$1`, manual.CommandReference).Scan(&recoveryFacts); err != nil || recoveryFacts != 2 {
		t.Fatalf("cross operator facts=%d err=%v", recoveryFacts, err)
	}

	// A genuine SQL failure is not flattened into a stale-version conflict.
	// T3 remains atomic and a later lease can converge after the fault is fixed.
	if processed, err = worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("finish manual requeue=%v err=%v", processed, err)
	}
	sqlOwner := seedThreeServiceOwner(t, ctx, accountConnection, "sql-failure-owner")
	sqlEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, sqlOwner.id, "SQL fault recovery")
	sqlStatus, sqlBody, sqlErr := threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, "apt-123-sql-failure", sqlEnvelope)
	sqlCommand := decodeThreeServiceCommand(t, sqlStatus, sqlBody, sqlErr)
	if _, err = tokenRepository.pool.Exec(ctx, `CREATE FUNCTION integration_reject_finalization() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='SUCCEEDED' AND NEW.command_reference='`+sqlCommand.CommandReference.String()+`'::uuid THEN RAISE EXCEPTION 'integration finalization SQL failure' USING ERRCODE='P0001'; END IF; RETURN NEW; END $$; CREATE TRIGGER integration_reject_finalization BEFORE UPDATE ON token_command_results FOR EACH ROW EXECUTE FUNCTION integration_reject_finalization()`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	processed, err = worker.ProcessOne(ctx)
	var sqlFailure *pgconn.PgError
	if !processed || !errors.As(err, &sqlFailure) || sqlFailure.Code != "P0001" {
		t.Fatalf("finalization SQL error was misclassified: processed=%v err=%v", processed, err)
	}
	var sqlAccounts int
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT count(*) FROM token_accounts WHERE owner_id=$1`, sqlOwner.id).Scan(&sqlAccounts); err != nil || sqlAccounts != 0 {
		t.Fatalf("SQL failure partial account=%d err=%v", sqlAccounts, err)
	}
	if result := threeServiceCommandResult(t, ctx, tokenServer.URL, creator, sqlCommand.CommandReference); result.Status != "PENDING" {
		t.Fatalf("SQL failure command=%+v", result)
	}
	if _, err = tokenRepository.pool.Exec(ctx, `DROP TRIGGER integration_reject_finalization ON token_command_results; DROP FUNCTION integration_reject_finalization(); UPDATE token_account_provisioning_reservations SET lease_until=now()-interval '1 second',next_attempt_at=now() WHERE command_reference=$1`, pgx.QueryExecModeSimpleProtocol, sqlCommand.CommandReference); err != nil {
		t.Fatal(err)
	}
	if processed, err = worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("SQL fault recovery=%v err=%v", processed, err)
	}
	if result := threeServiceCommandResult(t, ctx, tokenServer.URL, creator, sqlCommand.CommandReference); result.Status != "SUCCEEDED" {
		t.Fatalf("SQL recovery command=%+v", result)
	}

	// The system/default-route creator and the governed HTTP creator share the
	// exact owner lock, active policy and reservation-inclusive count. Starting
	// both at cap-1 can create only one PLATFORM account capacity fact.
	raceOwner := seedThreeServiceOwner(t, ctx, accountConnection, "default-route-race")
	installThreeServicePolicy(t, ctx, tokenRepository, 3, 1, "default-route race")
	raceEnvelope := threeServicePrecheckedEnvelope(t, ctx, tokenServer.URL, creator, raceOwner.id, "Generic versus default route")
	providerPolicy, err := NewProviderURLPolicy("test", []string{"provider.example"})
	if err != nil {
		t.Fatal(err)
	}
	secretProvider, err := NewLocalAESProvider("three-service-key", bytes.Repeat([]byte{31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tokenRepository.secrets = secretProvider
	provisioner, err := NewDefaultRouteProvisioner(tokenRepository, providerPolicy, DefaultRouteConfig{Provider: "three-service", AdapterVersion: "v1", BaseURL: "https://provider.example", Secret: []byte("integration-secret"), Models: []string{"three-service-model"}, Priority: 100})
	if err != nil {
		t.Fatal(err)
	}
	provisioner.WithTokenAccountCreateCapability(true)
	model := seedThreeServiceCatalogModel(t, ctx, tokenRepository, creator.id)
	walletID := uuid.New()
	start := make(chan struct{})
	var genericStatus int
	var genericBody []byte
	var genericErr, defaultErr error
	var raceGroup sync.WaitGroup
	raceGroup.Add(2)
	go func() {
		defer raceGroup.Done()
		<-start
		genericStatus, genericBody, genericErr = threeServiceHTTPJSON(ctx, tokenServer.URL, http.MethodPost, "/admin/v1/commands", creator.token, "apt-123-default-route-race", raceEnvelope)
	}()
	go func() {
		defer raceGroup.Done()
		<-start
		defaultErr = provisioner.provisionResources(ctx, AuthorizationContext{UserID: raceOwner.id, SessionID: "default-route-race", ContextType: "PERSONAL", MembershipVersion: 1, ContextVersion: 1, AuthorizationVersion: 1}, walletID, model)
	}()
	close(start)
	raceGroup.Wait()
	genericWon := genericErr == nil && (genericStatus == http.StatusCreated || genericStatus == http.StatusAccepted || genericStatus == http.StatusOK)
	defaultWon := defaultErr == nil
	if genericWon == defaultWon {
		t.Fatalf("cap race generic=%d/%s err=%v default err=%v", genericStatus, genericBody, genericErr, defaultErr)
	}
	if !genericWon && (genericStatus != http.StatusConflict || !(bytes.Contains(genericBody, []byte("TOKEN_ACCOUNT_CAP_REACHED")) || bytes.Contains(genericBody, []byte("COMMAND_PRECHECK_CHANGED")))) {
		t.Fatalf("generic cap loser=%d/%s err=%v", genericStatus, genericBody, genericErr)
	}
	if !defaultWon && !errors.Is(defaultErr, ErrTokenAccountCapReached) {
		t.Fatalf("default-route cap loser err=%v", defaultErr)
	}
	var accountCapacity int
	if err = tokenRepository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_accounts WHERE owner_type='USER' AND owner_id=$1 AND management_mode='PLATFORM' AND status IN ('ACTIVE','DISABLED','FROZEN'))+(SELECT count(*) FROM token_account_provisioning_reservations WHERE owner_type='USER' AND owner_id=$1 AND status IN ('PENDING','MANUAL_REVIEW'))`, raceOwner.id).Scan(&accountCapacity); err != nil || accountCapacity != 1 {
		t.Fatalf("default/generic cap capacity=%d err=%v", accountCapacity, err)
	}

	// Public responses, audit and outbox facts never contain the raw identity
	// subject or workload credential.
	secretNeedles := []string{creator.subject, operator.subject, serviceToken}
	for _, needle := range secretNeedles {
		if bytes.Contains(actionBody, []byte(needle)) || bytes.Contains(lookupBody, []byte(needle)) {
			t.Fatalf("sensitive value leaked in HTTP projection: %q", needle)
		}
		var leaked bool
		if err = tokenRepository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM token_audit_logs WHERE summary::text LIKE '%'||$1||'%') OR EXISTS(SELECT 1 FROM outbox_events WHERE payload::text LIKE '%'||$1||'%')`, needle).Scan(&leaked); err != nil || leaked {
			t.Fatalf("sensitive value leaked in Token facts=%v err=%v", leaked, err)
		}
	}
}

func newThreeServiceAccountRepository(t *testing.T, ctx context.Context, databaseURL string) (*accountdomain.PostgresRepository, *pgx.Conn) {
	t.Helper()
	connection, isolatedURL := newThreeServiceDatabase(t, ctx, databaseURL, "account_saga_")
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	repository, err := accountdomain.NewPostgresRepository(ctx, isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repository.Close)
	return repository, connection
}

func newThreeServiceBillingRepository(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgx.Conn) {
	t.Helper()
	connection, isolatedURL := newThreeServiceDatabase(t, ctx, databaseURL, "billing_saga_")
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	repository, err := billingdomain.NewPostgresRepository(ctx, isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repository.Close)
	return repository, connection
}

func newThreeServiceTokenRepository(t *testing.T, ctx context.Context, databaseURL string) *PostgresRepository {
	t.Helper()
	connection, isolatedURL := newThreeServiceDatabase(t, ctx, databaseURL, "token_saga_")
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err = tokenmigrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	secrets, err := NewLocalAESProvider("three-service-fixture", bytes.Repeat([]byte{37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewPostgresRepository(ctx, isolatedURL, secrets)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repository.Close)
	installThreeServicePolicy(t, ctx, repository, 1, 1000, "three-service initial policy")
	return repository
}

func newThreeServiceDatabase(t *testing.T, ctx context.Context, databaseURL, prefix string) (*pgx.Conn, string) {
	t.Helper()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture seeding contains parameterized multi-statement transactions.
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	bootstrap, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bootstrap.Close(context.Background()) })
	// These are the same external deployment roles required by the unchanged
	// production migrations. This harness only provisions missing fixture roles;
	// the separate migration-role gate exercises non-superuser restrictions.
	if _, err = bootstrap.Exec(ctx, `DO $$ BEGIN
IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_gateway') THEN CREATE ROLE token_gateway NOLOGIN; END IF;
IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_cap_policy_deployer') THEN CREATE ROLE token_cap_policy_deployer NOLOGIN; END IF;
IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_schema_owner') THEN CREATE ROLE token_schema_owner NOLOGIN; END IF;
IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_migrator') THEN CREATE ROLE token_migrator LOGIN NOINHERIT; END IF;
IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='token_policy_operator') THEN CREATE ROLE token_policy_operator LOGIN NOINHERIT; END IF;
IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_service') THEN CREATE ROLE billing_service NOLOGIN; END IF;
IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_wallet_receipt_deployer') THEN CREATE ROLE billing_wallet_receipt_deployer NOLOGIN; END IF;
IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_schema_owner') THEN CREATE ROLE billing_schema_owner NOLOGIN; END IF;
IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_migrator') THEN CREATE ROLE billing_migrator LOGIN NOINHERIT; END IF;
END $$`); err != nil {
		t.Fatal(err)
	}
	database := prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{database}.Sanitize()
	if _, err = bootstrap.Exec(ctx, `CREATE DATABASE `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := bootstrap.Exec(cleanup, `DROP DATABASE `+identifier+` WITH (FORCE)`); err != nil {
			t.Errorf("drop disposable three-service database: %v", err)
		}
	})
	// Several three-service gates apply the real Account migration chain to this
	// fresh database, and Account migration 000050 asserts that the migrator
	// holds CREATE on the current database. Every Account-suite fixture grants
	// the same privilege on its own disposable database.
	if _, err = bootstrap.Exec(ctx, `GRANT CREATE ON DATABASE `+identifier+` TO account_migrator`); err != nil {
		t.Fatal(err)
	}
	isolatedURL := threeServiceDatabaseURL(t, databaseURL, database)
	isolatedConfig, err := pgx.ParseConfig(isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	isolatedConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	connection, err := pgx.ConnectConfig(ctx, isolatedConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	if _, err = connection.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatal(err)
	}
	// As in deployment, the domain runtime role owns the pre-existing schema
	// and migration ledger. The new migration temporarily assumes its dedicated
	// object-owner role and returns to that runtime role before recording itself.
	role := ""
	if prefix == "billing_saga_" {
		role = "billing_service"
	} else if prefix == "token_saga_" {
		role = "token_gateway"
	}
	if role != "" {
		quotedRole := pgx.Identifier{role}.Sanitize()
		if _, err = bootstrap.Exec(ctx, `ALTER DATABASE `+identifier+` OWNER TO `+quotedRole); err != nil {
			t.Fatal(err)
		}
		if prefix == "token_saga_" {
			if _, err = bootstrap.Exec(ctx, `ALTER DATABASE `+identifier+` OWNER TO token_schema_owner`); err != nil {
				t.Fatal(err)
			}
			if _, err = connection.Exec(ctx, `ALTER SCHEMA public OWNER TO token_schema_owner; GRANT USAGE,CREATE ON SCHEMA public TO token_gateway; SET ROLE token_gateway`); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err = bootstrap.Exec(ctx, `ALTER DATABASE `+identifier+` OWNER TO billing_schema_owner`); err != nil {
				t.Fatal(err)
			}
			if _, err = connection.Exec(ctx, `ALTER SCHEMA public OWNER TO billing_schema_owner; GRANT USAGE,CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`); err != nil {
				t.Fatal(err)
			}
		}
	}
	return connection, isolatedURL
}

func threeServiceDatabaseURL(t *testing.T, rawURL, database string) string {
	t.Helper()
	if _, err := pgx.ParseConfig(rawURL); err != nil {
		t.Fatal(err)
	}
	// ConnConfig.ConnString returns the original text, so build the actual
	// isolated URL consumed by each production repository explicitly.
	if strings.HasPrefix(rawURL, "postgres://") || strings.HasPrefix(rawURL, "postgresql://") {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		query.Del("search_path")
		query.Del("default_query_exec_mode")
		parsed.Path = "/" + database
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return rawURL + " dbname=" + database + " search_path=public default_query_exec_mode=cache_statement"
}

func seedThreeServiceActor(t *testing.T, ctx context.Context, connection *pgx.Conn, suffix string) threeServiceActor {
	t.Helper()
	actor := threeServiceActor{id: uuid.New(), issuer: "https://issuer.three-service.example", subject: "subject-" + suffix + "-private", token: "bearer-" + suffix}
	now := time.Now().UTC().Add(-time.Hour)
	if _, err := connection.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,created_at,updated_at) VALUES($1,'ACTIVE',$2,generate_unique_user_code(),$3,$3); INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($4,$5,$1,$3,$3); INSERT INTO role_assignments(id,user_id,role_code,source,grant_source,starts_at,created_at) VALUES($6,$1,'PLATFORM_MODEL_ADMIN','INTEGRATION_TEST','INTEGRATION_TEST',$3,$3)`, actor.id, suffix, now, actor.issuer, actor.subject, uuid.New()); err != nil {
		t.Fatal(err)
	}
	return actor
}

func seedThreeServiceOwner(t *testing.T, ctx context.Context, connection *pgx.Conn, suffix string) threeServiceActor {
	t.Helper()
	owner := threeServiceActor{id: uuid.New(), issuer: "https://issuer.three-service.example", subject: "owner-" + suffix, token: ""}
	now := time.Now().UTC().Add(-time.Hour)
	if _, err := connection.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,created_at,updated_at) VALUES($1,'ACTIVE',$2,generate_unique_user_code(),$3,$3)`, owner.id, suffix, now); err != nil {
		t.Fatal(err)
	}
	return owner
}

func installThreeServicePolicy(t *testing.T, ctx context.Context, repository *PostgresRepository, version, cap int64, reason string) {
	t.Helper()
	eventID, requestID := uuid.New(), uuid.New()
	keyDigest := sha256.Sum256([]byte(requestID.String()))
	if _, err := repository.pool.Exec(ctx, `INSERT INTO token_account_owner_cap_policy_revisions(policy_version,max_per_owner,config_digest,request_digest,idempotency_key_digest,principal_digest,changed_by,reason,request_id,outbox_event_id) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6,'integration-fixture',$7,$8,$9); INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) SELECT $9,'token.account-owner-cap-policy.changed.v1',policy_id,$1,jsonb_build_object('policy_version',$1,'max_per_owner',$2,'config_digest',$3,'changed_by','integration-fixture','request_id',$8),$8::text,transaction_timestamp() FROM token_account_owner_cap_policy_active WHERE singleton_key; UPDATE token_account_owner_cap_policy_active SET active_policy_version=$1,pointer_version=pointer_version+1,updated_by='integration-fixture',updated_at=transaction_timestamp() WHERE singleton_key; SELECT setval(pg_get_serial_sequence('token_account_owner_cap_policy_revisions','policy_version'),$1,true)`, pgx.QueryExecModeSimpleProtocol, version, cap, tokenAccountOwnerCapConfigDigest(cap, version), strings.Repeat("a", 64), hex.EncodeToString(keyDigest[:]), strings.Repeat("c", 64), reason, requestID, eventID); err != nil {
		t.Fatal(err)
	}
}

func seedThreeServiceCatalogModel(t *testing.T, ctx context.Context, repository *PostgresRepository, actor uuid.UUID) CatalogModel {
	t.Helper()
	var protocolID uuid.UUID
	var protocolVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT id,version FROM protocol_definitions WHERE protocol_code='CHAT' AND status='PUBLISHED' ORDER BY version DESC LIMIT 1`).Scan(&protocolID, &protocolVersion); err != nil {
		t.Fatal(err)
	}
	modelID, capabilityID := uuid.New(), uuid.New()
	now := time.Now().UTC().Add(-time.Minute)
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'three-service default route race',$2,$3,$3) RETURNING version`, uuid.New(), actor, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'three-service','v1','three-service-model',$2,$3,$4); INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($5,$1,1,$6,4096,1024,'["text"]','v1',$3,$4); INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($7,$1,'Three Service Model','three service model','','{}','PUBLISHED',$5,$6,true)`, pgx.QueryExecModeSimpleProtocol, modelID, strings.Repeat("d", 64), actor, now, capabilityID, protocolID, catalogVersion); err != nil {
		t.Fatal(err)
	}
	return CatalogModel{ModelID: modelID, CatalogVersion: catalogVersion, DisplayName: "Three Service Model", LifecycleStatus: "PUBLISHED", ProtocolCode: "CHAT", ProtocolVersion: protocolVersion, ProtocolDefinitionID: protocolID, ModelCapabilityVersionID: capabilityID, CapabilityVersion: 1, ContextLength: 4096, MaximumOutputTokens: 1024, SupportedModalities: []string{"text"}, TokenizerVersion: "v1", SourceAvailable: true, ProviderID: "three-service", AdapterVersion: "v1", UpstreamModelRef: "three-service-model"}
}

func threeServiceAccountCommand(t *testing.T, owner uuid.UUID, name string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(governedAccountCreateCommand{OwnerType: "USER", OwnerID: owner, Name: name, ShareScope: "OWNER_UNIT_ONLY", DefaultPriority: 100})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func threeServicePrecheckedEnvelope(t *testing.T, ctx context.Context, baseURL string, actor threeServiceActor, owner uuid.UUID, name string) TokenCommandEnvelope {
	t.Helper()
	command := threeServiceAccountCommand(t, owner, name)
	status, body, err := threeServiceHTTPJSON(ctx, baseURL, http.MethodPost, "/admin/v1/commands:precheck", actor.token, "", TokenCommandPrecheckRequest{CommandType: "TOKEN_ACCOUNT_CREATE", TargetResourceType: "TOKEN_ACCOUNT", TargetResourceID: owner.String(), Command: command})
	if err != nil || status != http.StatusOK {
		t.Fatalf("precheck status=%d body=%s err=%v", status, body, err)
	}
	var precheck TokenCommandPrecheckResult
	if json.Unmarshal(body, &precheck) != nil || len(precheck.Blockers) != 0 {
		t.Fatalf("precheck projection=%s", body)
	}
	return modelAccountEnvelope(t, "TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", owner.String(), command, "three-service governed Token Account creation", precheck)
}

func threeServiceHTTPJSON(ctx context.Context, baseURL, method, path, bearer, key string, body any) (int, []byte, error) {
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		input = &oneShotJSONReader{body: raw}
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, input)
	if err != nil {
		return 0, nil, err
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, raw, err
}

func threeServiceJSON(ctx context.Context, handler http.Handler, method, path, bearer, key string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, response.Body.Bytes(), nil
}

func decodeThreeServiceCommand(t *testing.T, status int, body []byte, err error) TokenCommandResult {
	t.Helper()
	if err != nil || (status != http.StatusCreated && status != http.StatusOK && status != http.StatusAccepted) {
		t.Fatalf("command status=%d body=%s err=%v", status, body, err)
	}
	var result TokenCommandResult
	if json.Unmarshal(body, &result) != nil || result.CommandReference == uuid.Nil {
		t.Fatalf("command result=%s", body)
	}
	return result
}

func threeServiceLookupCommand(t *testing.T, ctx context.Context, baseURL string, actor threeServiceActor, key string) TokenCommandResult {
	t.Helper()
	status, body, err := threeServiceHTTPJSON(ctx, baseURL, http.MethodGet, "/admin/v1/commands/by-idempotency-key?command_type=TOKEN_ACCOUNT_CREATE", actor.token, key, nil)
	return decodeThreeServiceCommand(t, status, body, err)
}

func threeServiceCommandResult(t *testing.T, ctx context.Context, baseURL string, actor threeServiceActor, reference uuid.UUID) TokenCommandResult {
	t.Helper()
	status, body, err := threeServiceHTTPJSON(ctx, baseURL, http.MethodGet, "/admin/v1/commands/"+reference.String(), actor.token, "", nil)
	return decodeThreeServiceCommand(t, status, body, err)
}

func assertThreeServiceFacts(t *testing.T, ctx context.Context, repository *PostgresRepository, billingConnection *pgx.Conn, reference, owner uuid.UUID, accounts int, status string) {
	t.Helper()
	var accountFacts, commandFacts, reservationFacts, auditFacts, outboxFacts, receiptFacts int
	if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_accounts WHERE owner_type='USER' AND owner_id=$1),(SELECT count(*) FROM token_command_results WHERE command_reference=$2 AND status=$3),(SELECT count(*) FROM token_account_provisioning_reservations WHERE command_reference=$2),(SELECT count(*) FROM token_audit_logs WHERE summary->>'command_reference'=$2::text),(SELECT count(*) FROM outbox_events WHERE payload->>'command_reference'=$2::text)`, owner, reference, status).Scan(&accountFacts, &commandFacts, &reservationFacts, &auditFacts, &outboxFacts); err != nil {
		t.Fatal(err)
	}
	if err := billingConnection.QueryRow(ctx, `SELECT count(*) FROM token_account_wallet_provisioning_receipts WHERE command_reference=$1 AND status='SUCCEEDED'`, reference).Scan(&receiptFacts); err != nil {
		t.Fatal(err)
	}
	if accountFacts != accounts || commandFacts != 1 || reservationFacts != 1 || auditFacts < 2 || outboxFacts < 2 || receiptFacts != 1 {
		t.Fatalf("three-service facts account/command/reservation/audit/outbox/receipt=%d/%d/%d/%d/%d/%d", accountFacts, commandFacts, reservationFacts, auditFacts, outboxFacts, receiptFacts)
	}
}

func assertNoThreeServiceCommand(t *testing.T, ctx context.Context, repository *PostgresRepository, billingConnection *pgx.Conn, actor uuid.UUID, key string, expectedReceiptFacts int) {
	t.Helper()
	digest := sha256.Sum256([]byte(key))
	var commandFacts, reservationFacts, receiptFacts int
	if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_command_results WHERE actor_user_id=$1 AND idempotency_key_digest=$2),(SELECT count(*) FROM token_account_provisioning_reservations reservation JOIN token_command_results result USING(command_reference) WHERE result.actor_user_id=$1 AND result.idempotency_key_digest=$2)`, actor, hex.EncodeToString(digest[:])).Scan(&commandFacts, &reservationFacts); err != nil {
		t.Fatal(err)
	}
	if err := billingConnection.QueryRow(ctx, `SELECT count(*) FROM token_account_wallet_provisioning_receipts`).Scan(&receiptFacts); err != nil {
		t.Fatal(err)
	}
	if commandFacts != 0 || reservationFacts != 0 || receiptFacts != expectedReceiptFacts {
		t.Fatalf("zero-write facts command/reservation/receipt=%d/%d/%d", commandFacts, reservationFacts, receiptFacts)
	}
}

func assertThreeServicePendingWithoutAccount(t *testing.T, ctx context.Context, repository *PostgresRepository, reference, owner uuid.UUID) {
	t.Helper()
	var status string
	var accounts int
	if err := repository.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM token_accounts WHERE owner_type='USER' AND owner_id=$2) FROM token_command_results WHERE command_reference=$1`, reference, owner).Scan(&status, &accounts); err != nil || status != "PENDING" || accounts != 0 {
		t.Fatalf("pending facts status=%s accounts=%d err=%v", status, accounts, err)
	}
}

func assertThreeServiceTerminalFailure(t *testing.T, ctx context.Context, repository *PostgresRepository, billingConnection *pgx.Conn, reference, owner uuid.UUID) {
	t.Helper()
	var failedReservations, accounts, receipts int
	if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_account_provisioning_reservations WHERE command_reference=$1 AND status='FAILED' AND lease_owner IS NULL),(SELECT count(*) FROM token_accounts WHERE owner_type='USER' AND owner_id=$2)`, reference, owner).Scan(&failedReservations, &accounts); err != nil {
		t.Fatal(err)
	}
	if err := billingConnection.QueryRow(ctx, `SELECT count(*) FROM token_account_wallet_provisioning_receipts WHERE command_reference=$1 AND status='SUCCEEDED'`, reference).Scan(&receipts); err != nil || failedReservations != 1 || accounts != 0 || receipts != 1 {
		t.Fatalf("terminal failure reservation/account/receipt=%d/%d/%d err=%v", failedReservations, accounts, receipts, err)
	}
}
