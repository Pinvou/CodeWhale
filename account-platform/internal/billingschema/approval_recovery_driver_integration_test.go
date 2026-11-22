package billingschema

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApprovalRecoveryDedicatedLoginHTTPDriver(t *testing.T) {
	retainedRecoveryLeaseScenario(t, false, false, func(ctx context.Context, f *stageFixture, a, b *pgx.Conn, repo *account.PostgresRepository, candidate account.ApprovalRecoveryCandidate) {
		psql := os.Getenv("PINVOU_TEST_PSQL")
		if psql == "" {
			t.Fatal("PINVOU_TEST_PSQL is required to execute the actual deployment activation script")
		}
		adminURL := f.serviceURL(t, a, f.config.User)
		var activationOutput bytes.Buffer
		activate := func(verified bool) error {
			activationOutput.Reset()
			args := []string{"-X", "--no-password", "--dbname", adminURL, "--file", "../../deploy/postgres/account-approval-recovery-role.sql"}
			if verified {
				args = append(args, "--set", "ACCOUNT_MIGRATIONS_VERIFIED=true")
			}
			command := exec.CommandContext(ctx, psql, args...)
			command.Stdout, command.Stderr = &activationOutput, &activationOutput
			return command.Run()
		}
		if activate(false) == nil {
			t.Fatal("activation script did not fail without explicit verification")
		}
		if _, err := f.admin.Exec(ctx, `ALTER ROLE account_approval_recovery INHERIT`); err != nil {
			t.Fatal("negative role fixture setup failed")
		}
		if activate(true) == nil {
			t.Fatal("activation accepted unsafe role attributes")
		}
		if _, err := f.admin.Exec(ctx, `ALTER ROLE account_approval_recovery NOINHERIT`); err != nil {
			t.Fatal("restore isolated role failed")
		}
		if err := activate(true); err != nil {
			t.Fatalf("actual activation script failed: %s", activationOutput.String())
		}
		defer func() {
			if _, err := f.admin.Exec(context.Background(), `ALTER ROLE account_approval_recovery NOLOGIN`); err != nil {
				t.Error("disable fixture recovery role failed")
			}
		}()
		cfg, err := pgxpool.ParseConfig(f.serviceURL(t, a, "account_approval_recovery"))
		if err != nil {
			t.Fatal("parse isolated actor connection failed")
		}
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal("real dedicated login failed")
		}
		defer pool.Close()
		if err = account.VerifyApprovalRecoveryActor(ctx, pool); err != nil {
			t.Fatal("real low privilege session did not pass runtime verification")
		}
		for _, sql := range []string{`SELECT * FROM approval_requests`, `UPDATE approval_requests SET status='EXECUTED'`, `DELETE FROM approval_recovery_query_leases`, `SET ROLE account_service`, `SET ROLE account_migrator`} {
			if _, err = pool.Exec(ctx, sql); err == nil {
				t.Fatal("dedicated recovery actor gained forbidden authority")
			}
		}
		if _, err = f.admin.Exec(ctx, `ALTER ROLE account_approval_recovery INHERIT`); err != nil {
			t.Fatal(err)
		}
		if account.VerifyApprovalRecoveryActor(ctx, pool) == nil {
			t.Fatal("runtime accepted altered privileged actor")
		}
		if _, err = f.admin.Exec(ctx, `ALTER ROLE account_approval_recovery NOINHERIT`); err != nil {
			t.Fatal(err)
		}
		leaser, err := account.NewRecoveryQueryLeaseStore(pool)
		if err != nil {
			t.Fatal(err)
		}

		certificate, ca := recoveryDriverTLS(t)
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		authority := recoveryDriverServer(t, certificate, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/certs" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "recovery-fixture", Algorithm: "RS256", Use: "sig"}}})
		}))
		t.Setenv("APP_ENV", "production")
		t.Setenv("WORKLOAD_IDENTITY_ISSUER", authority.URL+"/realms/workload")
		t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", authority.URL+"/certs")
		t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "pinvou-account-platform-internal")
		t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
		t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"recovery-fixture","service":"account-approval-recovery","permissions":["internal.approval-recovery.plan","internal.approval-recovery.confirm","internal.approval-recovery.zero-effect","internal.billing.approval-effect.read","internal.billing.approval-result.read"]},{"subject":"wrong-service","service":"account-service","permissions":["internal.approval-recovery.plan"]},{"subject":"wrong-scope","service":"account-approval-recovery","permissions":["internal.authorization.check"]}]`)
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "recovery-fixture"))
		if err != nil {
			t.Fatal(err)
		}
		sign := func(subject string) string {
			body, _ := json.Marshal(map[string]any{"iss": authority.URL + "/realms/workload", "aud": "pinvou-account-platform-internal", "sub": subject, "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()})
			value, err := signer.Sign(body)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := value.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
		accountServer := recoveryDriverServer(t, certificate, account.NewService(repo, nil).Handler(nil))
		billingRepo, err := billing.NewPostgresRepository(ctx, f.serviceURL(t, b, "billing_service"))
		if err != nil {
			t.Fatal("real Billing repository failed")
		}
		defer billingRepo.Close()
		billingServer := recoveryDriverServer(t, certificate, billing.NewService(billingRepo).WithApprovalEffectReader(billingRepo).Handler(""))
		client, err := serviceauth.NewWorkloadHTTPClient(ca)
		if err != nil {
			t.Fatal(err)
		}
		for _, subject := range []string{"wrong-service", "wrong-scope"} {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, accountServer.URL+"/internal/v1/approval-recovery/plan?limit=200", nil)
			req.Header.Set("Authorization", "Bearer "+sign(subject))
			response, err := client.Do(req)
			if err != nil {
				t.Fatal("negative registered auth request failed")
			}
			response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("wrong identity/scope status=%d", response.StatusCode)
			}
		}
		tokenFile := filepath.Join(t.TempDir(), "projected-token")
		if err = os.WriteFile(tokenFile, []byte(sign("recovery-fixture")), 0600); err != nil {
			t.Fatal(err)
		}
		driver, err := account.NewApprovalRecoveryRuntime(accountServer.URL, billingServer.URL, "file:"+tokenFile, leaser, client)
		if err != nil {
			t.Fatal(err)
		}
		pass, err := driver.RunOnce(ctx, 200)
		if err != nil || pass.Candidates != 1 || pass.Pending != 1 || pass.Confirmed != 0 || pass.Invalidated != 0 || pass.Unavailable != 0 {
			t.Fatalf("unknown driver summary=%+v error=%v", pass, err != nil)
		}
		var requestStatus, executionStatus string
		if err = a.QueryRow(ctx, `SELECT r.status,e.status FROM approval_requests r JOIN approval_executions e ON e.approval_request_id=r.id WHERE r.id=$1`, candidate.RequestID).Scan(&requestStatus, &executionStatus); err != nil || requestStatus != "EXECUTING" || executionStatus != "EXECUTING" {
			t.Fatal("unknown proof changed original execution state")
		}
		delivery, err := repo.ApprovalAuthorizationDeliveryState(ctx, candidate.OriginalEventID, candidate.Durable, candidate.Workload)
		if err != nil || delivery.DomainAttemptCount != 1 || delivery.State != "DOMAIN_INFLIGHT" {
			t.Fatal("unknown proof changed financial delivery authority")
		}
		var count int
		if err = b.QueryRow(ctx, `SELECT count(*) FROM manual_adjustments`).Scan(&count); err != nil || count != 0 {
			t.Fatal("recovery executed a financial command")
		}
		var queries int
		var released bool
		if err = a.QueryRow(ctx, `SELECT query_count,lease_until<=clock_timestamp() AND next_query_at=lease_until+interval '2 seconds' AND version=2 FROM approval_recovery_query_leases WHERE approval_request_id=$1`, candidate.RequestID).Scan(&queries, &released); err != nil || queries != 1 || !released {
			t.Fatal("real query lease was not released with cooldown")
		}
		// Keep a deterministic future cooldown while inspecting several HTTP/DB
		// boundaries; machine scheduling must not turn a two-second lease test
		// into an assertion about wall-clock execution speed.
		if _, err = a.Exec(ctx, `UPDATE approval_recovery_query_leases SET next_query_at=clock_timestamp()+interval '1 minute' WHERE approval_request_id=$1`, candidate.RequestID); err != nil {
			t.Fatal(err)
		}
		plan, err := repo.PlanProofApprovalRecovery(ctx, 200)
		if err != nil || len(plan.Candidates) != 0 {
			t.Fatal("Account runtime failed to respect query cooldown")
		}
		pass, err = driver.RunOnce(ctx, 200)
		if err != nil || pass.Candidates != 0 {
			t.Fatal("immediate worker restart reset persistent cooldown")
		}
		fresh := recoveryDriverFreshCandidate(t, ctx, f, a, repo)
		// Only move the mutable scheduler clock in this owned fixture; immutable
		// financial/request/execution windows and facts stay untouched.
		if _, err = a.Exec(ctx, `UPDATE approval_recovery_query_leases SET next_query_at=clock_timestamp() WHERE approval_request_id=$1`, candidate.RequestID); err != nil {
			t.Fatal(err)
		}
		plan, err = repo.PlanProofApprovalRecovery(ctx, 1)
		if err != nil || len(plan.Candidates) != 1 || plan.Candidates[0].RequestID != fresh.RequestID || !plan.MoreCandidates {
			t.Fatal("old unknown candidate starved an uninvestigated execution")
		}
		lease, err := leaser.Claim(ctx, fresh)
		if err != nil {
			t.Fatal("fresh query lease failed")
		}
		plan, err = repo.PlanProofApprovalRecovery(ctx, 200)
		if err != nil || len(plan.Candidates) != 1 || plan.Candidates[0].RequestID != candidate.RequestID {
			t.Fatal("active lease was not excluded by real Account planner")
		}
		if _, err = leaser.Release(ctx, lease); err != nil {
			t.Fatal("release competing query lease failed")
		}
		t.Log("REAL_DEDICATED_LOGIN_TLS_JWT_REGISTERED_ROUTES_UNKNOWN_REMAINS_EXECUTING_ATTEMPT_ONE_LEASE_RELEASED")
	})
}

func recoveryDriverFreshCandidate(t *testing.T, ctx context.Context, f *stageFixture, c *pgx.Conn, repo *account.PostgresRepository) account.ApprovalRecoveryCandidate {
	t.Helper()
	resource := uuid.NewString()
	principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-requester", SessionID: "owned-requester-session", AuthenticatedAt: time.Now()}
	approver := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-approver", SessionID: "owned-approver-session", AuthenticatedAt: time.Now()}
	command, _ := json.Marshal(map[string]any{"adjustment_id": resource, "wallet_id": "0198f110-f538-7f30-a8b1-f3984fd459a1", "amount_microcredits": 100, "requested_by": "11111111-1111-4111-8111-111111111111"})
	draft := account.ApprovalDraft{CommandType: "WALLET_ADJUSTMENT", IdempotencyKey: "owned-fairness-" + resource, NormalizedCommand: command, TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: resource, Reason: "owned independent recovery fairness", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{"delta":100}`), CriticalResourceVersions: json.RawMessage(`{"adjustment_version":1}`), FinancialSnapshot: json.RawMessage(`{"wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","delta_microcredits":100}`)}
	item, err := repo.CreateApprovalDraft(ctx, principal, draft)
	if err != nil {
		t.Fatal("fresh recovery draft failed")
	}
	item, err = repo.SubmitApprovalRequest(ctx, principal, item.ID, item.Version)
	if err != nil {
		t.Fatal("fresh recovery submit failed")
	}
	item, err = repo.DecideApprovalRequest(ctx, approver, item.ID, item.Version, "APPROVED", "owned independent decision", draft.CriticalResourceVersions)
	if err != nil {
		t.Fatal("fresh recovery approval failed")
	}
	relay, err := account.NewApprovalRelayStore(ctx, f.serviceURL(t, c, "account_approval_relay"))
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	claims, err := relay.ClaimFrozen(ctx, "owned-recovery-fairness-relay", 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatal("fresh relay claim failed")
	}
	_, err = relay.ProcessFrozen(ctx, "owned-recovery-fairness-relay", claims[0], 12, time.Now(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	})
	if err != nil {
		t.Fatal("fresh relay observation failed")
	}
	binding, err := repo.ResolveApprovalAuthorizationBinding(ctx, claims[0].EventID, "billing-wallet-adjustment-executor-p1-v1", "billing-command-executor")
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := repo.ObserveApprovalAuthorizationDelivery(ctx, account.ApprovalDeliveryObservation{Binding: binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, delivery.Version)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.BeginApprovalExecutionWithAuthorization(ctx, item.ID, binding.Workload, binding.EventID.String(), draft.CriticalResourceVersions, draft.FinancialSnapshot, binding)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := repo.PlanProofApprovalRecovery(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range plan.Candidates {
		if candidate.RequestID == item.ID {
			return candidate
		}
	}
	t.Fatal("new real execution absent from recovery plan")
	return account.ApprovalRecoveryCandidate{}
}

func recoveryDriverServer(t *testing.T, certificate tls.Certificate, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func recoveryDriverTLS(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "owned recovery fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "ca.crt")
	if err = os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, file
}
