package token

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type streamReadTraceKey struct{}
type streamReadObservation struct {
	kind    string
	started time.Time
}

type streamReadTrace struct {
	mu    sync.Mutex
	full  []time.Duration
	state []time.Duration
}

func (trace *streamReadTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	kind := ""
	if strings.TrimSpace(data.SQL) == "SELECT status,delivery_status FROM usage_requests WHERE id=$1" {
		kind = "state"
	} else if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT "+usageSelectColumns+",EXISTS(") {
		kind = "full"
	}
	if kind == "" {
		return ctx
	}
	return context.WithValue(ctx, streamReadTraceKey{}, streamReadObservation{kind: kind, started: time.Now()})
}

func (trace *streamReadTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	observation, ok := ctx.Value(streamReadTraceKey{}).(streamReadObservation)
	if !ok {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if observation.kind == "state" {
		trace.state = append(trace.state, time.Since(observation.started))
	} else {
		trace.full = append(trace.full, time.Since(observation.started))
	}
}

func tracedStreamRepository(t *testing.T) (*PostgresRepository, context.Context, *streamReadTrace) {
	t.Helper()
	repository, ctx := retainedManagementTokenRepository(t)
	trace := &streamReadTrace{}
	configuration := repository.pool.Config()
	configuration.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository.pool = pool
	return repository, ctx, trace
}

type observedStreamRepository struct {
	*credentialWindowRepository
	cancelAcknowledged atomic.Int64
	stopObserved       atomic.Int64
	terminalObserved   atomic.Int64
	lateNumeric        atomic.Int64
}

func (repository *observedStreamRepository) GetUsageStreamState(ctx context.Context, id uuid.UUID) (UsageStreamState, error) {
	state, err := repository.PostgresRepository.GetUsageStreamState(ctx, id)
	if err == nil && (state.Status == "COMMITTED" || state.Status == "RELEASED") {
		repository.terminalObserved.Add(1)
	}
	if err == nil && state.DeliveryStatus == "USER_STOPPED" && repository.cancelAcknowledged.Load() != 0 {
		repository.stopObserved.CompareAndSwap(0, time.Now().UnixNano())
	}
	return state, err
}

func (repository *observedStreamRepository) RecordFinalUsage(ctx context.Context, id uuid.UUID, provider, adapter string, usage Usage) error {
	err := repository.PostgresRepository.RecordFinalUsage(ctx, id, provider, adapter, usage)
	if err == nil {
		repository.lateNumeric.Add(1)
	}
	return err
}

type streamTerminalBilling struct {
	*credentialWindowBilling
	extends atomic.Int64
}

func (billing *streamTerminalBilling) PriceByID(_ context.Context, id uuid.UUID) (PriceVersion, error) {
	if id != billing.price.ID {
		return PriceVersion{}, ErrNotFound
	}
	return billing.price, nil
}

func (billing *streamTerminalBilling) Extend(ctx context.Context, usage, hold uuid.UUID, amount int64, operation uuid.UUID) error {
	billing.extends.Add(1)
	return billing.credentialWindowBilling.Extend(ctx, usage, hold, amount, operation)
}

type streamTerminalEvidence struct {
	provider, adapter, reference string
	rejected                     bool
	calls                        atomic.Int64
}

func (evidence *streamTerminalEvidence) ProviderID() string     { return evidence.provider }
func (evidence *streamTerminalEvidence) AdapterVersion() string { return evidence.adapter }
func (evidence *streamTerminalEvidence) Lookup(ctx context.Context, reference string) (UsageEvidence, error) {
	if reference != evidence.reference || ctx.Value(dispatchLeaseOwnerKey{}) != nil {
		return UsageEvidence{}, ErrForbidden
	}
	evidence.calls.Add(1)
	if evidence.rejected {
		return UsageEvidence{Rejected: true}, nil
	}
	return UsageEvidence{Usage: &Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

type streamTerminalClaimStore struct {
	*PostgresRepository
	claimed []ReconciliationItem
	lease   time.Duration
}

func (store *streamTerminalClaimStore) ClaimReconciliationExcluding(ctx context.Context, limit int, lease time.Duration, excluded []uuid.UUID) ([]ReconciliationItem, error) {
	items, err := store.PostgresRepository.ClaimReconciliationExcluding(ctx, limit, lease, excluded)
	if err == nil {
		store.claimed = append(store.claimed, items...)
		store.lease = lease
	}
	return items, err
}

func finalizePausedStream(t *testing.T, ctx context.Context, repository *PostgresRepository, billing *streamTerminalBilling, clock *atomic.Int64, usageID uuid.UUID, reference, adapter string, rejected bool) (UsageRequest, ReplaySnapshot) {
	t.Helper()
	if ctx.Value(dispatchLeaseOwnerKey{}) != nil {
		t.Fatal("independent recovery inherited the old dispatch capability")
	}
	before, err := repository.GetUsage(ctx, usageID)
	if err != nil || before.Status != "UPSTREAM_ACTIVE" || before.ExecutionStatus != "STREAMING" || before.HoldID == nil || before.PriceVersionID == nil {
		t.Fatal("public stream did not reach its live persisted phase", err)
	}
	var owner string
	var until, acquired time.Time
	var idempotent, consumed bool
	err = repository.pool.QueryRow(ctx, `SELECT worker_lease_owner,worker_lease_until,worker_acquired_at,adapter_supports_idempotency,encrypted_envelope IS NULL FROM usage_dispatch_intents WHERE usage_request_id=$1`, usageID).Scan(&owner, &until, &acquired, &idempotent, &consumed)
	if err != nil || owner == "" || until.Sub(acquired) != dispatchLeaseDuration || idempotent || !consumed {
		t.Fatal("stream did not acquire the original one-shot ten-minute dispatch lease", err)
	}
	var provider, storedAdapter, storedReference string
	err = repository.pool.QueryRow(ctx, `SELECT provider_id,adapter_version,upstream_request_id FROM usage_attempts WHERE usage_request_id=$1`, usageID).Scan(&provider, &storedAdapter, &storedReference)
	if err != nil || !strings.EqualFold(provider, "controlled") || storedAdapter != adapter || storedReference != reference {
		t.Fatal("public dispatch did not capture its actual provider reference", err)
	}
	clock.Store(until.Add(time.Second).UnixNano())
	if err := repository.RecoverUncertainDispatches(ctx); err != nil {
		t.Fatal("expired dispatch recovery failed", err)
	}
	review, err := repository.GetUsage(ctx, usageID)
	if err != nil || review.Status != "REVIEW" || review.ExecutionStatus != "REVIEW" {
		t.Fatal("real expired-owner recovery did not admit review", err)
	}
	store := &streamTerminalClaimStore{PostgresRepository: repository}
	evidence := &streamTerminalEvidence{provider: provider, adapter: adapter, reference: reference, rejected: rejected}
	reconciler := NewUsageReconciler(store, billing, evidence, 15*time.Second)
	reconciler.now = repository.now
	reconciler.runOnce(ctx)
	if len(store.claimed) != 1 || store.claimed[0].ID != usageID || store.claimed[0].Attempts != 1 || store.lease != 45*time.Second || evidence.calls.Load() != 1 {
		t.Fatal("native sequential reconciler did not claim and query the original operation exactly once")
	}
	terminal, err := repository.GetUsage(ctx, usageID)
	wantStatus, wantExecution, wantSettlement := "COMMITTED", "SUCCEEDED", "SETTLED"
	wantCommit, wantRelease := int64(1), int64(0)
	if rejected {
		wantStatus, wantExecution, wantSettlement = "RELEASED", "FAILED", "RELEASED"
		wantCommit, wantRelease = 0, 1
	}
	if err != nil || terminal.Status != wantStatus || terminal.ExecutionStatus != wantExecution || terminal.SettlementStatus != wantSettlement || terminal.HoldID == nil || *terminal.HoldID != *before.HoldID || billing.reserves.Load() != 1 || billing.commits.Load() != wantCommit || billing.releases.Load() != wantRelease || billing.extends.Load() != 0 {
		t.Fatal("real external finalizer did not establish the expected money terminal", err)
	}
	if rejected {
		if terminal.ActualMicrocredits != nil {
			t.Fatal("rejected provider evidence invented a customer charge")
		}
	} else {
		actual, err := Cost(billing.price, Usage{InputTokens: 1, OutputTokens: 1})
		if err != nil || terminal.ActualMicrocredits == nil || *terminal.ActualMicrocredits != actual {
			t.Fatal("external settlement did not retain its original historical price", err)
		}
	}
	replay, err := repository.LoadReplay(ctx, usageID, 0)
	if err != nil || len(replay.Events) != 1 || replay.Events[0].EventType != "CHUNK" || replay.Result != nil {
		t.Fatal("paused stream had unexpected retained body or DONE before takeover", err)
	}
	return terminal, replay
}

type streamScenarioResult struct {
	chunks            int
	elapsed           time.Duration
	cancelObservation time.Duration
}

func assertStreamReplayBody(t *testing.T, response *http.Response, body []byte, events []ReplayEvent, after int64) {
	t.Helper()
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "text/event-stream" || response.StatusCode != http.StatusOK {
		t.Fatal("public replay is not a successful SSE response", response.StatusCode, contentType, err)
	}
	var expected strings.Builder
	previous := int64(0)
	for index, event := range events {
		if event.Sequence <= previous || (index == len(events)-1 && (event.EventType != "DONE" || string(event.Payload) != "[DONE]")) {
			t.Fatal("persisted replay order or final DONE is invalid", event.Sequence, event.EventType)
		}
		previous = event.Sequence
		if event.Sequence > after {
			fmt.Fprintf(&expected, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, strings.ToLower(event.EventType), event.Payload)
		}
	}
	if expected.Len() == 0 || string(body) != expected.String() {
		t.Fatal("public replay lost, reordered or changed persisted SSE frames", after, len(body), expected.Len())
	}
}

func assertStreamReplayResult(t *testing.T, snapshot ReplaySnapshot, chunks int) {
	t.Helper()
	if snapshot.Result == nil || snapshot.Result.Kind != "STREAM_AGGREGATE" || snapshot.Result.HTTPStatus != http.StatusOK || snapshot.Result.ContentType != "application/json" || len(snapshot.Events) != chunks+2 {
		t.Fatal("persisted stream aggregate is incomplete")
	}
	var aggregate struct {
		Events       []json.RawMessage `json:"events"`
		Usage        Usage             `json:"usage"`
		LastSequence int64             `json:"last_sequence"`
	}
	if err := json.Unmarshal(snapshot.Result.Payload, &aggregate); err != nil || len(aggregate.Events) != chunks+1 || aggregate.LastSequence != snapshot.Events[len(snapshot.Events)-1].Sequence || snapshot.Result.LastSequence != aggregate.LastSequence || aggregate.Usage.InputTokens != 1 || aggregate.Usage.OutputTokens != 1 {
		t.Fatal("stored aggregate lost its full events, final usage or sequence", err)
	}
	for index, payload := range aggregate.Events {
		var aggregated, persisted any
		if err := json.Unmarshal(payload, &aggregated); err != nil {
			t.Fatal("aggregate chunk is not JSON", index, err)
		}
		if err := json.Unmarshal(snapshot.Events[index].Payload, &persisted); err != nil || snapshot.Events[index].EventType != "CHUNK" || !reflect.DeepEqual(aggregated, persisted) {
			t.Fatal("aggregate differs from persisted chunk", index)
		}
		if index < chunks {
			var part struct {
				Part *int `json:"part"`
			}
			if err := json.Unmarshal(payload, &part); err != nil || part.Part == nil || *part.Part != index {
				t.Fatal("persisted provider chunk changed", index, err)
			}
		}
	}
}

// This exercises the public HTTP inference, cancellation and replay routes with
// public Token migrations and token_gateway. Authorization, tariff, money and
// provider boundaries are controlled. No live provider or financial E2E is claimed.
func runStreamStateScenario(t *testing.T, repository *PostgresRepository, ctx context.Context, mode string, chunks int, interval time.Duration) streamScenarioResult {
	t.Helper()
	externalTerminal := mode == "external-committed" || mode == "external-released"
	var clock atomic.Int64
	if externalTerminal {
		clock.Store(repository.now().UnixNano())
		repository.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	}
	fixture := newCredentialWindowFixture(t, repository, ctx)
	terminalBilling := &streamTerminalBilling{credentialWindowBilling: fixture.money}
	if externalTerminal {
		fixture.service.executionBilling = terminalBilling
	}
	observed := &observedStreamRepository{credentialWindowRepository: fixture.r}
	fixture.service.repository = observed
	resume := make(chan struct{})
	if mode == "complete" {
		close(resume)
	}
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	})
	var providerCalls atomic.Int64
	providerReference := "stream-" + uuid.NewString()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		_, _ = io.Copy(io.Discard, request.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if externalTerminal {
			w.Header().Set("X-Request-ID", providerReference)
		}
		for index := 0; index < chunks; index++ {
			_, _ = fmt.Fprintf(w, "data: {\"part\":%d}\n\n", index)
			_ = http.NewResponseController(w).Flush()
			if index == 0 {
				select {
				case <-resume:
				case <-request.Context().Done():
					return
				}
			}
			if interval > 0 {
				timer := time.NewTimer(interval)
				select {
				case <-timer.C:
				case <-request.Context().Done():
					timer.Stop()
					return
				}
			}
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	if _, err := repository.pool.Exec(ctx, `UPDATE provider_credentials SET base_url=$2 WHERE id=$1`, fixture.credential, provider.URL); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(fixture.body, &body); err != nil {
		t.Fatal(err)
	}
	body["stream"] = true
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	var replayArrivals atomic.Int64
	replayReady := make(chan struct{})
	finished := make(chan struct{})
	handler := httpserver.HandlerWithApplication(config.Config{ServiceName: "token-gateway", Environment: "test", RateLimitRPS: 100, RateLimitBurst: 200}, httpserver.WithStreamingWriteDeadline(fixture.service.Handler(successfulBillingVerify)), nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Last-Event-ID") != "" {
			// All three real HTTP requests must overlap before any replay route
			// runs. The original client timeout bounds a missing arrival.
			if replayArrivals.Add(1) == 3 {
				close(replayReady)
			}
			select {
			case <-replayReady:
			case <-request.Context().Done():
				return
			}
		}
		if request.URL.Path == "/v1/chat/completions" && requests.Add(1) == 1 {
			defer close(finished)
		}
		handler.ServeHTTP(w, request)
	}))
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
		server.Close()
	})
	client := &http.Client{Timeout: 15 * time.Second}
	newRequest := func(after *int64) *http.Request {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer controlled-stream-user")
		request.Header.Set("Idempotency-Key", fixture.key)
		request.Header.Set("Content-Type", "application/json")
		if after != nil {
			request.Header.Set("Last-Event-ID", strconv.FormatInt(*after, 10))
		}
		return request
	}
	invoke := func() *http.Response {
		t.Helper()
		request := newRequest(nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	started := time.Now()
	response := invoke()
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		failed, _ := io.ReadAll(response.Body)
		t.Fatal("public stream admission", response.StatusCode, string(failed))
	}
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal("first persisted frame", err)
	}
	if externalTerminal {
		// Reading the complete first SSE event proves AppendReplayEvent and
		// delivery finished before the independent worker advances the clock.
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal("first complete persisted SSE event", err)
			}
			first += line
			if line == "\n" {
				break
			}
		}
	}
	usage := <-fixture.r.accepted
	var externalBefore UsageRequest
	var externalReplay ReplaySnapshot
	if externalTerminal {
		externalBefore, externalReplay = finalizePausedStream(t, ctx, repository, terminalBilling, &clock, usage.ID, providerReference, fixture.service.executionUpstream.AdapterVersion(), mode == "external-released")
		close(resume)
	} else if mode == "cancel" {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/usage-requests/"+usage.ID.String()+"/cancel", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer controlled-stream-user")
		request.Header.Set("Idempotency-Key", "stream-cancel-"+uuid.NewString())
		cancelled, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, cancelled.Body)
		cancelled.Body.Close()
		if cancelled.StatusCode != http.StatusOK {
			t.Fatal("public cancellation", cancelled.StatusCode)
		}
		observed.cancelAcknowledged.Store(time.Now().UnixNano())
		close(resume)
	} else if mode == "disconnect" {
		response.Body.Close()
		close(resume)
	}
	var delivered string
	if mode != "disconnect" {
		rest, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		delivered = first + string(rest)
	}
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("detached stream did not finish")
	}
	current, err := repository.GetUsage(ctx, usage.ID)
	if externalTerminal {
		if err != nil || current.Status != externalBefore.Status || current.ExecutionStatus != externalBefore.ExecutionStatus || current.SettlementStatus != externalBefore.SettlementStatus || !reflect.DeepEqual(current.ActualMicrocredits, externalBefore.ActualMicrocredits) || !bytes.Equal(current.UsageSnapshot, externalBefore.UsageSnapshot) {
			t.Fatal("late stream reopened or changed the external financial terminal", err)
		}
		wantCommit, wantRelease := int64(1), int64(0)
		if mode == "external-released" {
			wantCommit, wantRelease = 0, 1
		}
		if providerCalls.Load() != 1 || fixture.money.reserves.Load() != 1 || fixture.money.commits.Load() != wantCommit || fixture.money.releases.Load() != wantRelease || terminalBilling.extends.Load() != 0 || observed.terminalObserved.Load() == 0 || observed.lateNumeric.Load() == 0 {
			t.Fatal("late numeric stream skipped real terminal observation or repeated financial effects")
		}
		var trusted []byte
		var numericObserved bool
		if err := repository.pool.QueryRow(ctx, `SELECT trusted_usage_snapshot,final_usage_observed_at IS NOT NULL FROM usage_requests WHERE id=$1`, usage.ID).Scan(&trusted, &numericObserved); err != nil || !numericObserved {
			t.Fatal("late terminal numeric evidence was not retained", err)
		}
		if numbers, valid := confirmedUsageNumbers(trusted); !valid || numbers != (Usage{InputTokens: 1, OutputTokens: 1}) {
			t.Fatal("late terminal numeric evidence changed")
		}
		after, err := repository.LoadReplay(ctx, usage.ID, 0)
		if err != nil || after.Result != nil || !reflect.DeepEqual(after.Events, externalReplay.Events) || delivered != first {
			t.Fatal("late stream persisted or delivered new body/DONE after external terminal", err)
		}
		// RELEASED intentionally receives contradictory late supplier usage:
		// it is evidence only, never permission to reopen the released hold.
		return streamScenarioResult{chunks: chunks, elapsed: time.Since(started)}
	}
	if err != nil || current.Status != "COMMITTED" || fixture.money.reserves.Load() != 1 || fixture.money.commits.Load() != 1 || fixture.money.releases.Load() != 0 || providerCalls.Load() != 1 {
		t.Fatal("stream changed final money or dispatch count", current.Status, err, fixture.money.reserves.Load(), fixture.money.commits.Load(), fixture.money.releases.Load(), providerCalls.Load())
	}
	snapshot, err := repository.LoadReplay(ctx, usage.ID, 0)
	if err != nil || snapshot.Result == nil || len(snapshot.Events) != chunks+2 {
		t.Fatal("persisted replay lost upstream frames", len(snapshot.Events), err)
	}
	assertStreamReplayResult(t, snapshot, chunks)
	result := streamScenarioResult{chunks: chunks, elapsed: time.Since(started)}
	if mode == "cancel" {
		if current.DeliveryStatus != "USER_STOPPED" || strings.Contains(delivered, `"part":1`) || observed.stopObserved.Load() == 0 {
			t.Fatal("stop was not observed at the next state checkpoint", current.DeliveryStatus, delivered)
		}
		result.cancelObservation = time.Duration(observed.stopObserved.Load() - observed.cancelAcknowledged.Load())
	} else {
		if mode == "complete" {
			assertStreamReplayBody(t, response, []byte(delivered), snapshot.Events, 0)
		}
		replay := invoke()
		replayed, replayErr := io.ReadAll(replay.Body)
		replay.Body.Close()
		if replayErr != nil || replay.StatusCode != http.StatusOK || providerCalls.Load() != 1 || fixture.money.commits.Load() != 1 {
			t.Fatal("replay dispatched or settled again", replay.StatusCode, replayErr)
		}
		assertStreamReplayBody(t, replay, replayed, snapshot.Events, 0)
		t.Run("concurrent-last-event-id-public-replay", func(t *testing.T) {
			type replayRead struct {
				response *http.Response
				body     []byte
				err      error
				after    int64
			}
			cursors := []int64{0, snapshot.Events[chunks/2].Sequence, snapshot.Events[len(snapshot.Events)-2].Sequence}
			start := make(chan struct{})
			reads := make(chan replayRead, len(cursors))
			for _, after := range cursors {
				request := newRequest(&after)
				go func(after int64) {
					<-start
					response, err := client.Do(request)
					var body []byte
					if err == nil {
						body, err = io.ReadAll(response.Body)
						response.Body.Close()
					}
					reads <- replayRead{response: response, body: body, err: err, after: after}
				}(after)
			}
			close(start)
			for range cursors {
				read := <-reads
				if read.err != nil {
					t.Fatal("concurrent public replay failed", read.err)
				}
				assertStreamReplayBody(t, read.response, read.body, snapshot.Events, read.after)
			}
			if replayArrivals.Load() != int64(len(cursors)) || providerCalls.Load() != 1 || fixture.money.reserves.Load() != 1 || fixture.money.commits.Load() != 1 || fixture.money.releases.Load() != 0 {
				t.Fatal("concurrent replay repeated dispatch or financial effects")
			}
			retained, err := repository.LoadReplay(ctx, usage.ID, 0)
			if err != nil {
				t.Fatal("replay state no longer readable", err)
			}
			assertStreamReplayResult(t, retained, chunks)
			if !bytes.Equal(retained.Result.Payload, snapshot.Result.Payload) || len(retained.Events) != len(snapshot.Events) {
				t.Fatal("public replay changed its original persisted result")
			}
			for index, event := range retained.Events {
				original := snapshot.Events[index]
				if event.Sequence != original.Sequence || event.EventType != original.EventType || !bytes.Equal(event.Payload, original.Payload) {
					t.Fatal("public replay changed a persisted event", index)
				}
			}
		})
		if mode == "disconnect" && current.DeliveryStatus != "DISCONNECTED" {
			t.Fatal("disconnect was not persisted", current.DeliveryStatus)
		}
	}
	return result
}

func TestStreamStatePollingPublicPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	for _, mode := range []string{"complete", "cancel", "disconnect", "external-committed", "external-released"} {
		t.Run(mode, func(t *testing.T) {
			repository, ctx, trace := tracedStreamRepository(t)
			result := runStreamStateScenario(t, repository, ctx, mode, 32, 0)
			trace.mu.Lock()
			defer trace.mu.Unlock()
			if len(trace.state) == 0 || len(trace.full) >= result.chunks {
				t.Fatal("stream still fetched full usage rows per frame", len(trace.state), len(trace.full))
			}
			if mode == "complete" && len(trace.state) != 2*result.chunks+4 {
				t.Fatal("connected stream skipped a state checkpoint", len(trace.state))
			}
			if mode == "cancel" && len(trace.state) != result.chunks+5 {
				t.Fatal("stopped stream skipped a terminal checkpoint", len(trace.state))
			}
			t.Logf("mode=%s chunks=%d state_queries=%d get_usage_queries=%d elapsed=%s cancel_observation=%s", mode, result.chunks, len(trace.state), len(trace.full), result.elapsed, result.cancelObservation)
		})
	}
}
