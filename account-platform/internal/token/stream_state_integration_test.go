package token

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
}

func (repository *observedStreamRepository) GetUsageStreamState(ctx context.Context, id uuid.UUID) (UsageStreamState, error) {
	state, err := repository.PostgresRepository.GetUsageStreamState(ctx, id)
	if err == nil && state.DeliveryStatus == "USER_STOPPED" && repository.cancelAcknowledged.Load() != 0 {
		repository.stopObserved.CompareAndSwap(0, time.Now().UnixNano())
	}
	return state, err
}

type streamScenarioResult struct {
	chunks            int
	elapsed           time.Duration
	cancelObservation time.Duration
}

// This exercises the public HTTP inference, cancellation and replay routes with
// public Token migrations and token_gateway. Authorization, tariff, money and
// provider boundaries are controlled. No live provider or financial E2E is claimed.
func runStreamStateScenario(t *testing.T, repository *PostgresRepository, ctx context.Context, mode string, chunks int, interval time.Duration) streamScenarioResult {
	t.Helper()
	fixture := newCredentialWindowFixture(t, repository, ctx)
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
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		_, _ = io.Copy(io.Discard, request.Body)
		w.Header().Set("Content-Type", "text/event-stream")
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
	finished := make(chan struct{})
	handler := httpserver.HandlerWithApplication(config.Config{ServiceName: "token-gateway", Environment: "test", RateLimitRPS: 100, RateLimitBurst: 200}, httpserver.WithStreamingWriteDeadline(fixture.service.Handler(successfulBillingVerify)), nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
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
	invoke := func() *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer controlled-stream-user")
		request.Header.Set("Idempotency-Key", fixture.key)
		request.Header.Set("Content-Type", "application/json")
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
	usage := <-fixture.r.accepted
	if mode == "cancel" {
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
	if err != nil || current.Status != "COMMITTED" || fixture.money.reserves.Load() != 1 || fixture.money.commits.Load() != 1 || fixture.money.releases.Load() != 0 || providerCalls.Load() != 1 {
		t.Fatal("stream changed final money or dispatch count", current.Status, err, fixture.money.reserves.Load(), fixture.money.commits.Load(), fixture.money.releases.Load(), providerCalls.Load())
	}
	snapshot, err := repository.LoadReplay(ctx, usage.ID, 0)
	if err != nil || snapshot.Result == nil || len(snapshot.Events) != chunks+2 {
		t.Fatal("persisted replay lost upstream frames", len(snapshot.Events), err)
	}
	result := streamScenarioResult{chunks: chunks, elapsed: time.Since(started)}
	if mode == "cancel" {
		if current.DeliveryStatus != "USER_STOPPED" || strings.Contains(delivered, `"part":1`) || observed.stopObserved.Load() == 0 {
			t.Fatal("stop was not observed at the next state checkpoint", current.DeliveryStatus, delivered)
		}
		result.cancelObservation = time.Duration(observed.stopObserved.Load() - observed.cancelAcknowledged.Load())
	} else {
		replay := invoke()
		_, replayErr := io.Copy(io.Discard, replay.Body)
		replay.Body.Close()
		if replayErr != nil || replay.StatusCode != http.StatusOK || providerCalls.Load() != 1 || fixture.money.commits.Load() != 1 {
			t.Fatal("replay dispatched or settled again", replay.StatusCode, replayErr)
		}
		if mode == "disconnect" && current.DeliveryStatus != "DISCONNECTED" {
			t.Fatal("disconnect was not persisted", current.DeliveryStatus)
		}
	}
	return result
}

func TestStreamStatePollingPublicPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	for _, mode := range []string{"complete", "cancel", "disconnect"} {
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
