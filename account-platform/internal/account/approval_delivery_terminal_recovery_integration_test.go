package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	tokendomain "github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type terminalRecoveryFixture struct {
	binding ApprovalAuthorizationBinding
	wire    []byte
	state   ApprovalDeliveryState
}

// Public Account approval writers produce every binding. External Token
// precheck/proof and broker transports are controlled; runtime SQL uses the
// genuine account_service login and the original owned-database fixture.
func TestApprovalDeliveryTerminalRecoveryPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if raw == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	repo, operator, relay, _ := newEntitlementApprovalFixture(t, ctx, raw)
	t.Run("public-migration-rollback-and-upgrade", func(t *testing.T) {
		installed, err := migrations.Load("../../migrations/account")
		if err != nil {
			t.Fatal(err)
		}
		if err = accountschema.DownOne(ctx, operator, installed); err != nil {
			t.Fatal("public down", err)
		}
		var absent bool
		if err = operator.QueryRow(ctx, `SELECT to_regprocedure('public.recover_approval_authorization_terminals(integer)') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("recovery migration rollback incomplete", err)
		}
		if err = accountschema.Up(ctx, operator, installed); err != nil {
			t.Fatal("public upgrade", err)
		}
		if _, err = accountschema.VerifyApplied(ctx, operator, installed); err != nil {
			t.Fatal("public verification", err)
		}
	})
	requester := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "terminal-requester")
	approver := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "terminal-approver")
	repo.WithBillingGovernanceActivation(queryLeaseBillingActivation{})
	sequence := uint64(0)
	produce := func(t *testing.T, stage string) terminalRecoveryFixture {
		t.Helper()
		now := time.Now().UTC().Truncate(time.Second)
		draft := tokenRecentAuthDraft("TOKEN_CREDENTIAL_REVOKE", now)
		billing := strings.HasPrefix(stage, "BILLING_")
		if billing {
			data, err := os.ReadFile("testdata/approval_recovery_query_command_inputs.json")
			if err != nil {
				t.Fatal(err)
			}
			var inputs map[string]approvalencoding.LegacyProofInput
			if err := json.Unmarshal(data, &inputs); err != nil {
				t.Fatal(err)
			}
			input := inputs["WALLET_ADJUSTMENT"]
			draft = ApprovalDraft{CommandType: input.CommandType, IdempotencyKey: uuid.NewString(), TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID, NormalizedCommand: input.NormalizedCommand, CriticalResourceVersions: input.CriticalResourceVersions, FinancialSnapshot: input.FinancialSnapshot, Reason: "controlled terminal metadata approval", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`)}
			stage = strings.TrimPrefix(stage, "BILLING_")
		}
		repo.WithTokenGovernanceActivation(&tokenRecentAuthActivation{true}).WithApprovalSnapshotSource(&tokenRecentAuthSnapshot{draft: draft}).WithApprovalProofSource(&tokenRecentAuthProof{})
		owner, reviewer := entitlementApprovalPrincipal(requester), entitlementApprovalPrincipal(approver)
		created, err := repo.CreateApprovalDraft(ctx, owner, draft)
		if err != nil {
			t.Fatal("real create", err)
		}
		submitted, err := repo.SubmitApprovalRequest(ctx, owner, created.ID, created.Version)
		if err != nil {
			t.Fatal("real submit", err)
		}
		if _, err = repo.DecideApprovalRequest(ctx, reviewer, created.ID, submitted.Version, "APPROVED", "independent terminal recovery review", draft.CriticalResourceVersions); err != nil {
			t.Fatal("real approval", err)
		}
		if _, err = operator.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
			t.Fatal(err)
		}
		var item terminalRecoveryFixture
		var id uuid.UUID
		err = operator.QueryRow(ctx, `SELECT o.event_id,s.delivery_bytes FROM public.outbox_events o JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id WHERE o.aggregate_id=$1 AND o.event_type='account.approval-command.authorized.v1'`, created.ID).Scan(&id, &item.wire)
		if err != nil {
			t.Fatal("actual frozen bytes", err)
		}
		if _, err = operator.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
			t.Fatal(err)
		}
		claims, err := relay.ClaimFrozen(ctx, "terminal-fixture-relay", 1, 30*time.Second)
		if err != nil || len(claims) != 1 || claims[0].EventID != id {
			t.Fatal("actual frozen relay claim", err)
		}
		disposition, err := relay.ProcessFrozen(ctx, "terminal-fixture-relay", claims[0], 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
			if message.EventID != id || string(message.Body) != string(item.wire) {
				t.Fatal("relay changed stored bytes")
			}
			return events.FrozenPublishResult{Disposition: events.FrozenPublished}
		})
		if err != nil || disposition != events.FrozenPublished {
			t.Fatal("actual relay publication", err)
		}
		durable, workload := "token-credential-revoke-executor-v1", "token-command-executor"
		if billing {
			durable, workload = "billing-wallet-adjustment-executor-p1-v1", "billing-command-executor"
		}
		item.binding, err = repo.ResolveApprovalAuthorizationBinding(ctx, id, durable, workload)
		if err != nil {
			t.Fatal("real binding", err)
		}
		sequence++
		item.state, err = repo.ObserveApprovalAuthorizationDelivery(ctx, ApprovalDeliveryObservation{Binding: item.binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: approvalconsumer.Sequence(sequence)})
		if err != nil || item.state.State != "READY" {
			t.Fatal("real observe", err)
		}
		if stage == "READY" {
			return item
		}
		if billing {
			item.state, err = repo.GrantApprovalAuthorizationDomainAttempt(ctx, id, durable, workload, item.state.Version)
			if err != nil {
				t.Fatal("real Billing grant", err)
			}
			if stage == "DOMAIN" {
				return item
			}
		}
		job := id.String()
		if _, err = repo.BeginApprovalExecutionWithAuthorization(ctx, created.ID, item.binding.Workload, job, draft.CriticalResourceVersions, draft.FinancialSnapshot, item.binding); err != nil {
			t.Fatal("real begin", err)
		}
		if stage == "UNKNOWN" {
			return item
		}
		result, code := "EXECUTED", ""
		if stage == "FAILED" {
			result, code = "FAILED", "UPSTREAM_UNAVAILABLE"
		}
		if err = repo.CompleteApprovalExecution(ctx, created.ID, item.binding.Workload, job, result, json.RawMessage(`{"controlled_external_result":true}`), json.RawMessage(`{}`), code); err != nil {
			t.Fatal("real complete", err)
		}
		item.state, err = repo.RecordApprovalAuthorizationResult(ctx, id, item.binding.Durable, item.binding.Workload, item.state.Version)
		if err != nil || item.state.State != "RESULT_CONFIRMED" {
			t.Fatal("real result", err)
		}
		return item
	}
	read := func(t *testing.T, item terminalRecoveryFixture) ApprovalDeliveryState {
		t.Helper()
		state, err := repo.ApprovalAuthorizationDeliveryState(ctx, item.binding.EventID, item.binding.Durable, item.binding.Workload)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	facts := func(t *testing.T) string {
		t.Helper()
		if _, err := operator.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
			t.Fatal(err)
		}
		var frozen, snapshot string
		err := operator.QueryRow(ctx, `SELECT jsonb_build_object(
'deliveries',(SELECT jsonb_agg(to_jsonb(d)-'state'-'version'-'updated_at' ORDER BY original_event_id,durable) FROM account_approval_authorization.approval_delivery_states d),
'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM public.outbox_events o))::TEXT`).Scan(&frozen)
		if _, resetErr := operator.Exec(ctx, `RESET ROLE; SET ROLE account_service`); resetErr != nil {
			t.Fatal(resetErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = repo.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'requests',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM public.approval_requests r),
'executions',(SELECT jsonb_agg(to_jsonb(e) ORDER BY approval_request_id,command_type) FROM public.approval_executions e))::TEXT`).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return frozen + snapshot
	}
	ready, unknown, inflight := produce(t, "READY"), produce(t, "UNKNOWN"), produce(t, "BILLING_DOMAIN")

	t.Run("ack-success-terminal-write-failure-worker-and-redelivery", func(t *testing.T) {
		item := produce(t, "EXECUTED")
		credential := configureTerminalRecoveryTokenIdentity(t)
		handler := NewService(repo, nil).WithServiceToken("unused-static-token").Handler(nil)
		terminalCalls, forbiddenCalls := 0, 0
		terminalStatus := make(chan int, 2)
		failTerminal := true
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/terminal") {
				terminalCalls++
				if failTerminal {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				terminalStatus <- response.Code
				for key, values := range response.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(response.Code)
				_, _ = w.Write(response.Body.Bytes())
				return
			}
			if strings.Contains(r.URL.Path, "/executions") || strings.HasSuffix(r.URL.Path, "/execute") || strings.HasSuffix(r.URL.Path, "/grant") {
				forbiddenCalls++
			}
			handler.ServeHTTP(w, r)
		}))
		defer server.Close()
		consumer, err := tokendomain.NewTokenAuthorizationCommandConsumer(item.binding.CommandType, item.binding.Durable, server.URL, server.URL, credential, &tokenRecentAuthActivation{true})
		if err != nil {
			t.Fatal(err)
		}
		delivery := &terminalRecoveryDelivery{raw: item.wire, id: item.binding.EventID.String(), sequence: sequence}
		consumer.Handle(ctx, delivery)
		if delivery.ack != 1 || delivery.nak != 0 || terminalCalls != 1 || read(t, item).State != "RESULT_CONFIRMED" || forbiddenCalls != 0 {
			t.Fatalf("real Account ACK window not reached: ack=%d nak=%d terminal=%d forbidden=%d", delivery.ack, delivery.nak, terminalCalls, forbiddenCalls)
		}
		// Run the production worker without any further broker delivery.
		workerCtx, stop := context.WithCancel(ctx)
		probe := &terminalRecoveryWorkerProbe{repository: repo, after: stop}
		before := facts(t)
		NewApprovalDeliveryTerminalRecoveryWorker(probe, time.Hour).Run(workerCtx)
		if probe.calls != 1 || read(t, item).State != "TERMINAL_CONFIRMED" {
			t.Fatal("independent startup pass did not recover ACKed metadata")
		}
		if facts(t) != before {
			t.Fatal("sweep changed frozen identity, execution, approval or outbox facts")
		}
		failTerminal = false
		replay := &terminalRecoveryDelivery{raw: item.wire, id: item.binding.EventID.String(), sequence: sequence}
		consumer.Handle(ctx, replay)
		if replay.ack != 1 || replay.nak != 0 || forbiddenCalls != 0 || terminalCalls != 1 {
			t.Fatal("terminal replay reentered domain or terminal write")
		}
		// Freeze the consumer's RESULT version, then let the sweep win
		// synchronously inside the controlled ACK boundary before HTTP CAS.
		raceItem := produce(t, "EXECUTED")
		raceDelivery := &terminalRecoveryDelivery{raw: raceItem.wire, id: raceItem.binding.EventID.String(), sequence: sequence}
		raceDelivery.onACK = func() {
			if n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 100); err != nil || n != 1 {
				t.Fatal("ACK barrier sweep", n, err)
			}
		}
		consumer.Handle(ctx, raceDelivery)
		if raceDelivery.ack != 1 || raceDelivery.nak != 0 || forbiddenCalls != 0 || terminalCalls != 2 || read(t, raceItem).Version != raceItem.state.Version+1 || <-terminalStatus != http.StatusOK {
			t.Fatal("stale RESULT consumer did not converge after sweep won")
		}
	})
	t.Run("bounded-batches-replay-and-negative-states", func(t *testing.T) {
		items := []terminalRecoveryFixture{produce(t, "EXECUTED"), produce(t, "EXECUTED"), produce(t, "FAILED"), produce(t, "BILLING_EXECUTED")}
		before := facts(t)
		for _, want := range []int{2, 2, 0} {
			n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 2)
			if err != nil || n != want {
				t.Fatalf("batch got %d want %d: %v", n, want, err)
			}
		}
		for _, item := range items {
			current := read(t, item)
			if current.State != "TERMINAL_CONFIRMED" || current.Version != item.state.Version+1 || current.DomainAttemptCount != item.state.DomainAttemptCount {
				t.Fatal("sweep changed more than terminal metadata")
			}
			replayed, err := repo.RecordApprovalAuthorizationTerminal(ctx, item.binding.EventID, item.binding.Durable, item.binding.Workload, item.state.Version)
			if err != nil || replayed.Version != current.Version {
				t.Fatal("stale consumer CAS did not read back confirmed terminal", err)
			}
			if _, err = repo.RecordApprovalAuthorizationTerminal(ctx, item.binding.EventID, item.binding.Durable, item.binding.Workload, current.Version+1); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
				t.Fatal("future version accepted", err)
			}
		}
		failed := read(t, items[2])
		if failed.ExecutionStatus == nil || *failed.ExecutionStatus != "FAILED" || failed.ExecutionErrorClass == nil || *failed.ExecutionErrorClass != "UPSTREAM_UNAVAILABLE" {
			t.Fatal("terminal metadata rewrote failed domain result")
		}
		for _, item := range []terminalRecoveryFixture{ready, unknown, inflight} {
			if current := read(t, item); current.State != item.state.State || current.Version != item.state.Version {
				t.Fatal("unconfirmed delivery changed")
			}
		}
		if facts(t) != before {
			t.Fatal("bounded recovery changed frozen or domain facts")
		}
	})
	t.Run("locked-row-and-concurrent-workers-then-next-pass", func(t *testing.T) {
		items := []terminalRecoveryFixture{produce(t, "EXECUTED"), produce(t, "EXECUTED"), produce(t, "EXECUTED")}
		tx, err := operator.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `SET LOCAL ROLE account_approval_authorization_owner;`); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `SELECT 1 FROM account_approval_authorization.approval_delivery_states WHERE original_event_id=$1 AND durable=$2 FOR UPDATE`, items[0].binding.EventID, items[0].binding.Durable); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		counts, failures := make(chan int, 2), make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 1)
				counts <- n
				failures <- err
			}()
		}
		wg.Wait()
		if a, b := <-counts, <-counts; a+b != 2 {
			t.Fatal("locked row blocked other eligible rows", a, b)
		}
		for i := 0; i < 2; i++ {
			if err = <-failures; err != nil {
				t.Fatal(err)
			}
		}
		if read(t, items[0]).State != "RESULT_CONFIRMED" {
			t.Fatal("held row was changed")
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 100); err != nil || n != 1 {
			t.Fatal("released row did not recover next pass", n, err)
		}
	})
	t.Run("permission-fault-zero-effect-and-restoration", func(t *testing.T) {
		item := produce(t, "EXECUTED")
		ownerSQL := func(sql string) {
			t.Helper()
			if _, err := operator.Exec(ctx, `SET ROLE account_approval_authorization_owner;`+sql+`; RESET ROLE; SET ROLE account_service`); err != nil {
				t.Fatal(err)
			}
		}
		ownerSQL(`REVOKE EXECUTE ON FUNCTION public.recover_approval_authorization_terminals(INTEGER) FROM account_service`)
		if n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 100); n != 0 || !errors.Is(err, ErrForbidden) || read(t, item).State != "RESULT_CONFIRMED" {
			t.Fatal("permission fault concealed", n, err)
		}
		ownerSQL(`GRANT EXECUTE ON FUNCTION public.recover_approval_authorization_terminals(INTEGER) TO account_service`)
		if n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 100); err != nil || n != 1 {
			t.Fatal("restored permission did not recover", n, err)
		}
		for _, limit := range []int{0, -1, 101} {
			if _, err := repo.RecoverApprovalDeliveryTerminals(ctx, limit); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
				t.Fatal("invalid runtime limit accepted", limit, err)
			}
		}
		for _, limit := range []any{nil, 0, -1, 101} {
			var n int
			err := repo.pool.QueryRow(ctx, `SELECT public.recover_approval_authorization_terminals($1)`, limit).Scan(&n)
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "22023" {
				t.Fatal("SQL limit guard bypassed", limit, err)
			}
		}
		var forbidden bool
		if _, err := operator.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
			t.Fatal(err)
		}
		if err := operator.QueryRow(ctx, `SELECT NOT has_table_privilege('account_service','account_approval_authorization.approval_delivery_states','SELECT,INSERT,UPDATE,DELETE,TRUNCATE') AND NOT has_schema_privilege('account_approval_authorization_owner','public','CREATE')`).Scan(&forbidden); err != nil || !forbidden {
			t.Fatal("runtime table privileges expanded", err)
		}
		if _, err := operator.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
			t.Fatal(err)
		}
		var ignored int
		if err := operator.QueryRow(ctx, `SELECT public.recover_approval_authorization_terminals(100)`).Scan(&ignored); err == nil {
			t.Fatal("migrator SET ROLE bypassed session identity")
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		if _, err := repo.RecoverApprovalDeliveryTerminals(canceled, 100); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation concealed", err)
		}
		if n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 100); err != nil || n != 0 {
			t.Fatal("cancel poisoned later call", n, err)
		}
		ownerSQL(`REVOKE EXECUTE ON FUNCTION public.record_approval_authorization_terminal(UUID,TEXT,TEXT,BIGINT) FROM account_service`)
		if _, err := repo.RecordApprovalAuthorizationTerminal(ctx, item.binding.EventID, item.binding.Durable, item.binding.Workload, item.state.Version); !errors.Is(err, ErrForbidden) {
			t.Fatal("terminal permission error swallowed by readback", err)
		}
		ownerSQL(`GRANT EXECUTE ON FUNCTION public.record_approval_authorization_terminal(UUID,TEXT,TEXT,BIGINT) TO account_service`)
		if _, err := repo.RecordApprovalAuthorizationTerminal(canceled, item.binding.EventID, item.binding.Durable, item.binding.Workload, item.state.Version); !errors.Is(err, context.Canceled) {
			t.Fatal("terminal cancellation swallowed", err)
		}
		if _, err := repo.RecordApprovalAuthorizationTerminal(ctx, item.binding.EventID, item.binding.Durable, "billing-command-executor", item.state.Version); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
			t.Fatal("wrong workload accepted by readback", err)
		}
	})
	t.Run("batch-sql-failure-is-atomic-and-next-call-recovers", func(t *testing.T) {
		items := []terminalRecoveryFixture{produce(t, "EXECUTED"), produce(t, "EXECUTED")}
		fault := `SET ROLE account_approval_authorization_owner;
CREATE FUNCTION account_approval_authorization.controlled_terminal_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled storage failure' USING ERRCODE='XX000'; END $$;
CREATE TRIGGER controlled_terminal_fault BEFORE UPDATE ON account_approval_authorization.approval_delivery_states FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.controlled_terminal_fault();
RESET ROLE; SET ROLE account_service;`
		if _, err := operator.Exec(ctx, fault); err != nil {
			t.Fatal(err)
		}
		if n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 100); n != 0 || !errors.Is(err, ErrApprovalConsumerBindingUnavailable) {
			t.Fatal("SQL failure concealed", n, err)
		}
		for _, item := range items {
			if state := read(t, item); state.State != "RESULT_CONFIRMED" || state.Version != item.state.Version {
				t.Fatal("failed batch partially committed")
			}
		}
		if _, err := operator.Exec(ctx, `SET ROLE account_approval_authorization_owner; DROP TRIGGER controlled_terminal_fault ON account_approval_authorization.approval_delivery_states; DROP FUNCTION account_approval_authorization.controlled_terminal_fault(); RESET ROLE; SET ROLE account_service`); err != nil {
			t.Fatal(err)
		}
		if n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 100); err != nil || n != 2 {
			t.Fatal("SQL recovery did not resume", n, err)
		}
	})
	t.Run("populated-rollback-retains-frozen-and-terminal-history", func(t *testing.T) {
		allStates := func() string {
			t.Helper()
			if _, err := operator.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := operator.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(d) ORDER BY original_event_id,durable)::TEXT FROM account_approval_authorization.approval_delivery_states d`).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if _, err := operator.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
				t.Fatal(err)
			}
			return state
		}
		before, frozen := allStates(), facts(t)
		installed, err := migrations.Load("../../migrations/account")
		if err != nil {
			t.Fatal(err)
		}
		if err = accountschema.DownOne(ctx, operator, installed); err != nil {
			t.Fatal(err)
		}
		if allStates() != before || facts(t) != frozen {
			t.Fatal("rollback rewrote history")
		}
		if err = accountschema.Up(ctx, operator, installed); err != nil {
			t.Fatal(err)
		}
		if _, err = accountschema.VerifyApplied(ctx, operator, installed); err != nil {
			t.Fatal(err)
		}
		if allStates() != before || facts(t) != frozen {
			t.Fatal("upgrade rewrote history")
		}
		if n, err := repo.RecoverApprovalDeliveryTerminals(ctx, 100); err != nil || n != 0 {
			t.Fatal("upgraded worker changed negative states", n, err)
		}
	})
}

type terminalRecoveryDelivery struct {
	raw      []byte
	id       string
	sequence uint64
	ack, nak int
	onACK    func()
}

func (d *terminalRecoveryDelivery) DeliveryBytes() []byte { return d.raw }
func (d *terminalRecoveryDelivery) MessageID() string     { return d.id }
func (d *terminalRecoveryDelivery) Metadata() events.AuthorizationDeliveryMetadata {
	return events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: d.sequence, NumDelivered: 1}
}
func (d *terminalRecoveryDelivery) DoubleAck(context.Context) error {
	d.ack++
	if d.onACK != nil {
		d.onACK()
	}
	return nil
}
func (d *terminalRecoveryDelivery) NakWithDelay(time.Duration) error { d.nak++; return nil }
func (d *terminalRecoveryDelivery) PublishDeadLetter(context.Context) (events.AuthorizationDLQPubAck, error) {
	return events.AuthorizationDLQPubAck{}, errors.New("no dead-letter capability")
}

type terminalRecoveryWorkerProbe struct {
	repository *PostgresRepository
	after      context.CancelFunc
	calls      int
}

func (p *terminalRecoveryWorkerProbe) RecoverApprovalDeliveryTerminals(ctx context.Context, limit int) (int, error) {
	p.calls++
	n, err := p.repository.RecoverApprovalDeliveryTerminals(ctx, limit)
	p.after()
	return n, err
}
