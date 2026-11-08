package billingschema

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type creditBoundaryAdmission func(context.Context, paymentcredit.Request) (paymentcredit.Decision, error)

func (a creditBoundaryAdmission) Admit(ctx context.Context, r paymentcredit.Request) (paymentcredit.Decision, error) {
	return a(ctx, r)
}

type creditBoundaryResolver struct {
	repository             *billing.PostgresRepository
	resolveCalls, ackCalls int
	dropAckReplyOnce       bool
	corruptResolveReply    bool
}

func (a *creditBoundaryResolver) Resolve(ctx context.Context, r paymentcredit.Request) (paymentcredit.Decision, error) {
	a.resolveCalls++
	decision, err := a.repository.ResolvePaymentCredit(ctx, r)
	if err == nil && a.corruptResolveReply {
		decision.ID = uuid.New()
	}
	return decision, err
}

func (a *creditBoundaryResolver) Acknowledge(ctx context.Context, r paymentcredit.Request, state string) (paymentcredit.Decision, error) {
	a.ackCalls++
	decision, err := a.repository.AcknowledgePaymentCredit(ctx, r, state)
	if err == nil && a.dropAckReplyOnce {
		a.dropAckReplyOnce = false
		return paymentcredit.Decision{}, context.DeadlineExceeded
	}
	return decision, err
}

type creditBoundaryProvider struct {
	billing.PaymentProvider
	result billing.ProviderOrder
}

func (p *creditBoundaryProvider) Capabilities() billing.PaymentCapabilities {
	return billing.PaymentCapabilities{ProviderID: "credit-boundary", Version: "v1", Channels: []string{"ALIPAY"}, OrderQuerySupported: true, MinimumQueryIntervalSeconds: 1, TestOnly: true}
}
func (p *creditBoundaryProvider) Get(context.Context, string) (billing.ProviderOrder, error) {
	return p.result, nil
}

// Real published migrations, runtime roles and public repository/coordinator
// entries; only the provider response and admission transport timing are
// controlled. This is not workload-HTTP, approval-consumer or supplier E2E.
func TestPaymentCreditClosureBoundaryPostgres(t *testing.T) {
	url := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if url == "" {
		t.Skip("explicit owned schema database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newStageFixture(t, ctx, url)
	f.installClosureTokenActors(t, ctx)
	tc := f.database(t, ctx, "token", "token_schema_owner")
	if _, err := tc.Exec(ctx, `CREATE EXTENSION pgcrypto; ALTER SCHEMA public OWNER TO token_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO token_gateway`); err != nil {
		t.Fatal(err)
	}
	cfg := tc.Config().Copy()
	cfg.User = "token_migrator"
	cfg.Password = ""
	tm, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.connections = append(f.connections, tm)
	if _, err = tm.Exec(ctx, `SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	ms, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err = tokenschema.Up(ctx, tm, ms); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, tm)
	secret, err := token.NewLocalAESProvider("isolated-credit-boundary", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := token.NewPostgresRepository(ctx, f.serviceURL(t, tc, "token_gateway"), secret)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	ac := f.database(t, ctx, "account", "account_service")
	bc := f.database(t, ctx, "billing", "billing_schema_owner")
	am, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, ac, am); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, ac)
	bm, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err = Up(ctx, bc, bm); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, bc)
	ar, err := account.NewPostgresRepository(ctx, f.serviceURL(t, ac, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	br, err := billing.NewPostgresRepository(ctx, f.serviceURL(t, bc, "billing_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	admission := creditBoundaryAdmission(ar.AdmitPaymentCredit)
	resolver := &creditBoundaryResolver{repository: br}
	type scenario struct {
		user, order, wallet uuid.UUID
		principal           identity.Principal
		coordinator         *billing.PaymentCoordinator
		provider            *creditBoundaryProvider
	}
	seed := func(t *testing.T) scenario {
		t.Helper()
		s := scenario{user: uuid.New(), order: uuid.New()}
		now := time.Now().UTC()
		s.principal = identity.Principal{Issuer: "credit-boundary", Subject: s.user.String(), AuthenticatedAt: now}
		if _, err := ac.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,status,user_code,username,created_at,updated_at) SELECT $1,'ACTIVE',generate_unique_user_code(),$2,$3,$3 FROM capability`, s.user, "cb_"+strings.ReplaceAll(s.user.String(), "-", "")[:16], now); err != nil {
			t.Fatal(err)
		}
		if _, err := ac.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, s.principal.Issuer, s.principal.Subject, s.user, now); err != nil {
			t.Fatal(err)
		}
		wallet, _, err := br.EnsureWallet(ctx, "PERSONAL", s.user)
		if err != nil {
			t.Fatal(err)
		}
		s.wallet = wallet.ID
		merchant := "cb-" + s.order.String()
		expiry := now.Add(-time.Hour)
		created := expiry.Add(-time.Hour)
		paid := expiry.Add(-time.Minute)
		transaction := uuid.NewString()
		product, _ := json.Marshal(billing.Product{ProductID: "CNY_1", PlanName: "Historical credit", AmountFen: 100, CreditMicrocredits: 1000000, BasePoints: 1, ValidityDays: 30, Currency: "CNY"})
		if _, err := bc.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'CNY_1',$5,'ALIPAY',100,'CNY',1000000,'EXPIRED','PENDING',$6,$6,$6::timestamptz+interval '72 hours',$6::timestamptz+interval '8 days',$7,$7,'credit-boundary','v1')`, s.order, merchant, s.user, s.wallet, product, expiry, created); err != nil {
			t.Fatal(err)
		}
		provider := &creditBoundaryProvider{result: billing.ProviderOrder{MerchantOrderNo: merchant, Status: "SUCCESS", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", ChannelTransactionID: &transaction, PaidAt: &paid, ExternalCustomerKey: s.user.String()}}
		s.provider = provider
		s.coordinator, err = billing.NewPaymentCoordinatorWithProvider(br, provider, "test")
		if err != nil {
			t.Fatal(err)
		}
		s.coordinator.WithPaymentCreditAdmission(admission)
		return s
	}
	claimReady := func(t *testing.T, s scenario) *account.AccountClosureRequest {
		t.Helper()
		ba, err := br.AccountClosureAssessment(ctx, s.user)
		if err != nil || !ba.Ready {
			t.Fatalf("original Billing assessment: %+v %v", ba, err)
		}
		ta, err := tr.AccountClosureAssessment(ctx, s.user)
		if err != nil || !ta.Ready {
			t.Fatalf("original Token assessment: %+v %v", ta, err)
		}
		request, err := ar.CreateAccountClosure(ctx, s.principal, uuid.NewString(), "CLOSE_MY_ACCOUNT")
		if err != nil {
			t.Fatal(err)
		}
		// Each scenario owns its subject. Park only scheduler metadata afterward
		// so a later scenario cannot claim this unfinished controlled closure.
		// No financial fact, tested stage or policy gate is changed by the fixture.
		t.Cleanup(func() {
			if _, err := ac.Exec(ctx, `UPDATE account_closure_requests SET next_attempt_at=clock_timestamp()+interval '1 day',lease_until=NULL WHERE id=$1`, request.ID); err != nil {
				t.Error("park owned scenario scheduling", err)
			}
		})
		claim, err := ar.ClaimAccountClosure(ctx)
		if err != nil || claim == nil || claim.ID != request.ID {
			t.Fatal("original initial claim", err)
		}
		if err = ar.EnterAccountClosureQuiescing(ctx, *claim); err != nil {
			t.Fatal(err)
		}
		claim, err = ar.ClaimAccountClosure(ctx)
		if err != nil || claim == nil || claim.ID != request.ID {
			t.Fatal("original final claim", err)
		}
		ba, err = br.AccountClosureAssessment(ctx, s.user)
		if err != nil || !ba.Ready {
			t.Fatalf("original final Billing assessment: %+v %v", ba, err)
		}
		ta, err = tr.AccountClosureAssessment(ctx, s.user)
		if err != nil || !ta.Ready {
			t.Fatalf("original final Token assessment: %+v %v", ta, err)
		}
		return claim
	}
	assertEffects := func(t *testing.T, s scenario, wanted int64) {
		t.Helper()
		var balance, walletPosting int64
		var packages, journals int
		if err := bc.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM entitlement_packages WHERE payment_order_id=$1),(SELECT count(*) FROM journal_entries journal JOIN payment_transactions transaction ON journal.business_reference IN ('payment-transaction:'||transaction.id::text,'repair-payment-transaction:'||transaction.id::text) WHERE transaction.payment_order_id=$1),(SELECT COALESCE(sum(posting.delta_microcredits),0) FROM ledger_postings posting JOIN ledger_accounts account ON account.id=posting.ledger_account_id WHERE account.wallet_id=$2) FROM wallets WHERE id=$2`, s.order, s.wallet).Scan(&balance, &packages, &journals, &walletPosting); err != nil {
			t.Fatal(err)
		}
		count := 0
		if wanted > 0 {
			count = 1
		}
		if balance != wanted || walletPosting != wanted || packages != count || journals != count {
			t.Fatalf("incorrect wallet/entitlement/ledger effects: balance=%d posting=%d packages=%d journals=%d", balance, walletPosting, packages, journals)
		}
	}
	t.Run("cleanup-wins-retains-evidence-without-credit", func(t *testing.T) {
		s := seed(t)
		claim := claimReady(t, s)
		if err := ar.AdvanceAccountClosureToIdentityCleanup(ctx, *claim); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			result, err := s.coordinator.SyncOrder(ctx, s.order)
			if err != nil || result.Status != "MANUAL_REVIEW" || result.FulfillmentStatus != "MANUAL_REVIEW" {
				t.Fatal("original provider convergence must preserve manual review", result.Status, result.FulfillmentStatus, err)
			}
			assertEffects(t, s, 0)
		}
		var evidence, transactions, manual int
		if err := bc.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1),(SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED' AND fulfillment_status='REVIEW_REQUIRED'),(SELECT count(*) FROM payment_anomaly_cases WHERE payment_order_id=$1 AND status='OPEN')`, s.order).Scan(&evidence, &transactions, &manual); err != nil || evidence != 1 || transactions != 1 || manual != 1 {
			t.Fatal("durable original payment/manual evidence", evidence, transactions, manual, err)
		}
		if _, err := s.coordinator.SyncReviewedOrder(ctx, s.order); !errors.Is(err, billing.ErrConflict) {
			t.Fatal("review cannot override closed identity", err)
		}
		assertEffects(t, s, 0)
	})
	t.Run("inflight-credit-and-resolved-credit-invalidate-stale-ready", func(t *testing.T) {
		s := seed(t)
		claim := claimReady(t, s)
		admitted := make(chan paymentcredit.Request, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseCredit := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseCredit()
		s.coordinator.WithPaymentCreditAdmission(creditBoundaryAdmission(func(c context.Context, r paymentcredit.Request) (paymentcredit.Decision, error) {
			d, err := ar.AdmitPaymentCredit(c, r)
			if err != nil {
				return d, err
			}
			admitted <- r
			select {
			case <-release:
				return d, nil
			case <-c.Done():
				return d, c.Err()
			}
		}))
		done := make(chan error, 1)
		go func() { _, err := s.coordinator.SyncOrder(ctx, s.order); done <- err }()
		var input paymentcredit.Request
		select {
		case input = <-admitted:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := ar.AdvanceAccountClosureToIdentityCleanup(ctx, *claim); !errors.Is(err, account.ErrConflict) {
			releaseCredit()
			t.Fatal("stale pre-assessment claim advanced while credit pending", err)
		}
		// Advance only the owned scheduler clock, then really claim after
		// admission but before the financial commit and final assessment.
		if _, err := ac.Exec(ctx, `UPDATE account_closure_requests SET lease_until=clock_timestamp()-interval '1 second',next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, claim.ID); err != nil {
			t.Fatal(err)
		}
		fresh, err := ar.ClaimAccountClosure(ctx)
		if err != nil || fresh == nil || fresh.ID != claim.ID || fresh.UserVersion <= claim.UserVersion {
			t.Fatal("actual post-admission claim", err)
		}
		assessment, err := br.AccountClosureAssessment(ctx, s.user)
		if err != nil || !assessment.Ready {
			t.Fatal("actual assessment while credit uncommitted", assessment.Ready, err)
		}
		tokenAssessment, err := tr.AccountClosureAssessment(ctx, s.user)
		if err != nil || !tokenAssessment.Ready {
			t.Fatal("actual Token assessment", err)
		}
		reconciled := make(chan error, 1)
		go func() { reconciled <- ar.ReconcilePaymentCredits(ctx, resolver) }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var blocked bool
			if err := bc.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND query LIKE 'SELECT payer_user_id FROM payment_orders WHERE id=$1 FOR UPDATE%' AND wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0)`, bc.Config().Database).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			select {
			case err := <-reconciled:
				t.Fatal("resolver did not wait for original order writer", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("resolver never reached the actual original order lock wait")
			}
			time.Sleep(5 * time.Millisecond)
		}
		var pendingError *pgconn.PgError
		if err := ar.AdvanceAccountClosureToIdentityCleanup(ctx, *fresh); !errors.As(err, &pendingError) || pendingError.Code != "55000" {
			t.Fatal("pending grant did not fence a fresh post-admission claim", err)
		}
		_, err = ac.Exec(ctx, `UPDATE account_closure_requests SET stage='IDENTITY_CLEANUP' WHERE id=$1`, claim.ID)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "55000" {
			releaseCredit()
			t.Fatal("database guard failed older runtime transition", err)
		}
		releaseCredit()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		assertEffects(t, s, 1000000)
		select {
		case err := <-reconciled:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		var state string
		if err := ac.QueryRow(ctx, `SELECT state FROM account_payment_credit_admissions WHERE id=$1`, input.ID).Scan(&state); err != nil || state != "APPLIED" {
			t.Fatal("durable credit outcome", state, err)
		}
		if err := ar.AdvanceAccountClosureToIdentityCleanup(ctx, *fresh); !errors.Is(err, account.ErrConflict) {
			t.Fatal("resolved credit let stale financial assessment advance", err)
		}
		if _, err := s.coordinator.SyncOrder(ctx, s.order); err != nil {
			t.Fatal(err)
		}
		assertEffects(t, s, 1000000)
	})
	t.Run("admission-reply-loss-recovers-abort-and-manual-evidence", func(t *testing.T) {
		s := seed(t)
		var input paymentcredit.Request
		s.coordinator.WithPaymentCreditAdmission(creditBoundaryAdmission(func(c context.Context, r paymentcredit.Request) (paymentcredit.Decision, error) {
			input = r
			_, err := ar.AdmitPaymentCredit(c, r)
			if err != nil {
				return paymentcredit.Decision{}, err
			}
			return paymentcredit.Decision{}, context.DeadlineExceeded
		}))
		result, err := s.coordinator.SyncOrder(ctx, s.order)
		if err != nil || result.Status != "MANUAL_REVIEW" {
			t.Fatal("uncertain admission must retain manual payment", result.Status, err)
		}
		assertEffects(t, s, 0)
		if err := ar.ReconcilePaymentCredits(ctx, resolver); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			d, err := br.ResolvePaymentCredit(ctx, input)
			if err != nil || d.State != "ABORTED" || !d.Matches(input) {
				t.Fatal("stable abort tombstone", d.State, err)
			}
		}
		d, err := ar.AdmitPaymentCredit(ctx, input)
		if err != nil || d.State != "ABORTED" {
			t.Fatal("old admission cannot become new credit", d.State, err)
		}
		s.coordinator.WithPaymentCreditAdmission(admission)
		if _, err = s.coordinator.SyncReviewedOrder(ctx, s.order); err != nil {
			t.Fatal("original controlled repair after confirmed abort", err)
		}
		assertEffects(t, s, 1000000)
		if err := ar.ReconcilePaymentCredits(ctx, resolver); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rollback-after-grant-retains-abort-and-next-payment-recovers", func(t *testing.T) {
		s := seed(t)
		callContext, cancelCall := context.WithCancel(ctx)
		defer cancelCall()
		var input paymentcredit.Request
		s.coordinator.WithPaymentCreditAdmission(creditBoundaryAdmission(func(c context.Context, r paymentcredit.Request) (paymentcredit.Decision, error) {
			input = r
			d, err := ar.AdmitPaymentCredit(c, r)
			if err == nil {
				cancelCall()
			}
			return d, err
		}))
		if _, err := s.coordinator.SyncOrder(callContext, s.order); err == nil || !input.Valid() {
			t.Fatal("controlled cancellation did not reach admitted transaction rollback", err)
		}
		assertEffects(t, s, 0)
		if err := ar.ReconcilePaymentCredits(ctx, resolver); err != nil {
			t.Fatal(err)
		}
		old, err := br.ResolvePaymentCredit(ctx, input)
		if err != nil || old.State != "ABORTED" {
			t.Fatal("rolled-back attempt must have a stable abort tombstone", old.State, err)
		}
		s.coordinator.WithPaymentCreditAdmission(admission)
		if _, err = s.coordinator.SyncOrder(ctx, s.order); err != nil {
			t.Fatal("next original payment could not recover", err)
		}
		assertEffects(t, s, 1000000)
		old, err = br.ResolvePaymentCredit(ctx, input)
		if err != nil || old.State != "ABORTED" {
			t.Fatal("later success changed the original aborted attempt", old.State, err)
		}
		if err := ar.ReconcilePaymentCredits(ctx, resolver); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("lost-ack-reply-resumes-metadata-without-repeating-money", func(t *testing.T) {
		s := seed(t)
		if _, err := s.coordinator.SyncOrder(ctx, s.order); err != nil {
			t.Fatal(err)
		}
		assertEffects(t, s, 1000000)
		control := &creditBoundaryResolver{repository: br, dropAckReplyOnce: true}
		if err := ar.ReconcilePaymentCredits(ctx, control); err != nil {
			t.Fatal(err)
		}
		var state string
		var unacknowledged bool
		if err := ac.QueryRow(ctx, `SELECT state,billing_acknowledged_at IS NULL FROM account_payment_credit_admissions WHERE user_id=$1`, s.user).Scan(&state, &unacknowledged); err != nil || state != "APPLIED" || !unacknowledged || control.resolveCalls != 1 || control.ackCalls != 1 {
			t.Fatal("lost ack reply changed durable Account result", state, unacknowledged, err)
		}
		var billingAcknowledged bool
		if err := bc.QueryRow(ctx, `SELECT account_acknowledged_at IS NOT NULL FROM payment_credit_attempts WHERE payer_user_id=$1`, s.user).Scan(&billingAcknowledged); err != nil || !billingAcknowledged {
			t.Fatal("controlled ack was not committed before reply loss", err)
		}
		// Advance only recovery scheduling to its next eligible tick.
		if _, err := ac.Exec(ctx, `UPDATE account_payment_credit_admissions SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, s.user); err != nil {
			t.Fatal(err)
		}
		if err := ar.ReconcilePaymentCredits(ctx, control); err != nil {
			t.Fatal(err)
		}
		if control.resolveCalls != 1 || control.ackCalls != 2 {
			t.Fatal("metadata retry re-ran financial resolution or money", control.resolveCalls, control.ackCalls)
		}
		if err := ac.QueryRow(ctx, `SELECT billing_acknowledged_at IS NULL FROM account_payment_credit_admissions WHERE user_id=$1`, s.user).Scan(&unacknowledged); err != nil || unacknowledged {
			t.Fatal("lost ack reply did not converge", err)
		}
		assertEffects(t, s, 1000000)
	})
	t.Run("wrong-resolution-identity-retains-pending-then-valid-tick-recovers", func(t *testing.T) {
		s := seed(t)
		if _, err := s.coordinator.SyncOrder(ctx, s.order); err != nil {
			t.Fatal(err)
		}
		var beforeVersion int64
		if err := ac.QueryRow(ctx, `SELECT version FROM users WHERE id=$1`, s.user).Scan(&beforeVersion); err != nil {
			t.Fatal(err)
		}
		control := &creditBoundaryResolver{repository: br, corruptResolveReply: true}
		if err := ar.ReconcilePaymentCredits(ctx, control); err != nil {
			t.Fatal(err)
		}
		var state string
		var version int64
		if err := ac.QueryRow(ctx, `SELECT admission.state,users.version FROM account_payment_credit_admissions admission JOIN users ON users.id=admission.user_id WHERE admission.user_id=$1`, s.user).Scan(&state, &version); err != nil || state != "PENDING" || version != beforeVersion || control.ackCalls != 0 {
			t.Fatal("wrong outcome identity released the subject fence", state, version, err)
		}
		control.corruptResolveReply = false
		if _, err := ac.Exec(ctx, `UPDATE account_payment_credit_admissions SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, s.user); err != nil {
			t.Fatal(err)
		}
		if err := ar.ReconcilePaymentCredits(ctx, control); err != nil {
			t.Fatal(err)
		}
		if err := ac.QueryRow(ctx, `SELECT state FROM account_payment_credit_admissions WHERE user_id=$1`, s.user).Scan(&state); err != nil || state != "APPLIED" || control.ackCalls != 1 {
			t.Fatal("valid subsequent outcome did not converge", state, err)
		}
		assertEffects(t, s, 1000000)
	})
	t.Run("unconfigured-authority-retains-manual-payment-without-bypass", func(t *testing.T) {
		s := seed(t)
		unconfigured, err := billing.NewPaymentCoordinatorWithProvider(br, s.provider, "test")
		if err != nil {
			t.Fatal(err)
		}
		result, err := unconfigured.SyncOrder(ctx, s.order)
		if err != nil || result.Status != "MANUAL_REVIEW" {
			t.Fatal("nil authority silently admitted money", result.Status, err)
		}
		assertEffects(t, s, 0)
		if _, err := s.coordinator.SyncReviewedOrder(ctx, s.order); err != nil {
			t.Fatal("valid subsequent admitted repair could not continue", err)
		}
		assertEffects(t, s, 1000000)
		if err := ar.ReconcilePaymentCredits(ctx, resolver); err != nil {
			t.Fatal(err)
		}
	})
}
