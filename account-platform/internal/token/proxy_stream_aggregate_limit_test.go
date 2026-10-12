package token

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type aggregateLimitRepository struct {
	streamRepositoryFake
	reviewSnapshot map[string]any
}

func (r *aggregateLimitRepository) MarkReview(_ context.Context, _ uuid.UUID, reason string, facts map[string]any) error {
	r.reviewReason, r.reviewSnapshot = reason, facts
	return nil
}

// The sink counts real proxy output without retaining a second oversized stream.
type aggregateLimitWriter struct {
	header http.Header
	bytes  int64
	done   bool
}

func (w *aggregateLimitWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (*aggregateLimitWriter) WriteHeader(int) {}
func (*aggregateLimitWriter) Flush()          {}
func (w *aggregateLimitWriter) Write(body []byte) (int, error) {
	w.bytes += int64(len(body))
	w.done = w.done || bytes.Contains(body, []byte("data: [DONE]"))
	return len(body), nil
}

func aggregateLimitSupplier(content string, chunks int) io.Reader {
	chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"" + content + "\"}}]}\n\n"
	readers := make([]io.Reader, 0, chunks+1)
	for index := 0; index < chunks; index++ {
		readers = append(readers, strings.NewReader(chunk))
	}
	readers = append(readers, strings.NewReader("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n"))
	return io.MultiReader(readers...)
}

func TestF05AggregateOverflowPreservesEventStreamWithoutFinancialSuccess(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		content string
		chunks  int
	}{
		{"plain", strings.Repeat("x", 1<<20), 34},
		{"html-escaped", strings.Repeat("<", 1<<20), 6},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repository := &aggregateLimitRepository{}
			billing := &streamBillingFake{}
			service := &Service{repository: repository, executionBilling: billing, executionUpstream: NewHTTPUpstreamClient()}
			writer := &aggregateLimitWriter{}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			service.proxyStream(writer, request, uuid.New(), uuid.New(), PriceVersion{OutputPerMillion: 1_000_000}, aggregateLimitSupplier(scenario.content, scenario.chunks), make(chan struct{}), 1)
			if repository.resultSaved || repository.committed || repository.released || billing.extendCalls != 0 || billing.commitCalls != 0 || billing.releaseCalls != 0 {
				t.Fatalf("oversized aggregate escaped to success or money calls: saved=%v committed=%v released=%v extend=%d commit=%d release=%d", repository.resultSaved, repository.committed, repository.released, billing.extendCalls, billing.commitCalls, billing.releaseCalls)
			}
			if repository.reviewReason == "" || repository.reviewSnapshot["usage"] != (Usage{OutputTokens: 10}) {
				t.Fatalf("real final usage was not retained for review: reason=%q snapshot=%#v", repository.reviewReason, repository.reviewSnapshot)
			}
			if len(repository.events) != scenario.chunks+1 || writer.done || writer.bytes <= int64(scenario.chunks*(1<<20)) {
				t.Fatalf("ordinary chunk forwarding or unknown terminal was changed: events=%d done=%v bytes=%d", len(repository.events), writer.done, writer.bytes)
			}
		})
	}
}

func aggregateLimitExactSupplier(t *testing.T, size int) (io.Reader, []byte) {
	t.Helper()
	const chunks = 17 // Each source line stays below the unchanged 2MiB scanner bound.
	final := json.RawMessage(`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`)
	events := make([]json.RawMessage, chunks+1)
	for index := 0; index < chunks; index++ {
		events[index] = json.RawMessage(`{"choices":[],"padding":""}`)
	}
	events[chunks] = final
	empty, err := json.Marshal(map[string]any{"events": events, "usage": Usage{}, "last_sequence": chunks + 2})
	if err != nil {
		t.Fatal(err)
	}
	padding := size - len(empty)
	if padding < 0 {
		t.Fatal("fixture size cannot fit the receipt")
	}
	readers := make([]io.Reader, 0, chunks+2)
	for index := 0; index < chunks; index++ {
		length := padding / chunks
		if index < padding%chunks {
			length++
		}
		events[index] = json.RawMessage(`{"choices":[],"padding":"` + strings.Repeat("x", length) + `"}`)
		if len(events[index])+len("data: ") >= 2<<20 {
			t.Fatal("fixture escaped the existing single-line bound")
		}
		readers = append(readers, strings.NewReader("data: "+string(events[index])+"\n\n"))
	}
	readers = append(readers, strings.NewReader("data: "+string(final)+"\n\ndata: [DONE]\n\n"))
	expected, err := json.Marshal(map[string]any{"events": events, "usage": Usage{}, "last_sequence": chunks + 2})
	if err != nil || len(expected) != size {
		t.Fatalf("independent original Marshal fixture: bytes=%d expected=%d err=%v", len(expected), size, err)
	}
	return io.MultiReader(readers...), expected
}

func TestF05AggregateCompleteReceiptBoundary(t *testing.T) {
	for _, size := range []int{streamAggregateLimit, streamAggregateLimit + 1} {
		t.Run(string(rune('A'+size-streamAggregateLimit)), func(t *testing.T) {
			stream, expected := aggregateLimitExactSupplier(t, size)
			repository := &aggregateLimitRepository{}
			billing := &streamBillingFake{}
			service := &Service{repository: repository, executionBilling: billing, executionUpstream: NewHTTPUpstreamClient()}
			writer := &aggregateLimitWriter{}
			service.proxyStream(writer, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), uuid.New(), uuid.New(), PriceVersion{}, stream, make(chan struct{}), 1)
			if size == streamAggregateLimit {
				if !repository.resultSaved || !bytes.Equal(repository.savedResult.Payload, expected) || billing.commitCalls != 1 || !writer.done || repository.reviewReason != "" {
					t.Fatalf("exact 32MiB original receipt was not accepted: saved=%v size=%d commit=%d done=%v review=%q", repository.resultSaved, len(repository.savedResult.Payload), billing.commitCalls, writer.done, repository.reviewReason)
				}
			} else if repository.resultSaved || billing.extendCalls != 0 || billing.commitCalls != 0 || billing.releaseCalls != 0 || writer.done || repository.reviewReason == "" {
				t.Fatalf("wrapper/usage bytes escaped the limit: saved=%v billing=%#v done=%v review=%q", repository.resultSaved, billing, writer.done, repository.reviewReason)
			}
		})
	}
}

func TestF05AggregateEncodingClearsRetentionAndKeepsOriginalJSON(t *testing.T) {
	usage := Usage{InputTokens: 9007199254740993, OutputTokens: 1, CachedInputTokens: 7}
	fields := []json.RawMessage{json.RawMessage(` { "text": "<>&", "value": 9007199254740993 } `), json.RawMessage(`{"done":true}`)}
	var aggregate streamReplayAggregate
	for _, field := range fields {
		aggregate.append(field)
	}
	expected, err := json.Marshal(map[string]any{"events": fields, "usage": usage, "last_sequence": int64(3)})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := aggregate.result(usage, 3)
	if err != nil || !bytes.Equal(actual, expected) || !bytes.Contains(actual, []byte(`\u003c\u003e\u0026`)) || !bytes.Contains(actual, []byte("9007199254740993")) {
		t.Fatalf("original JSON/HTML/int64 encoding changed: result=%s err=%v", actual, err)
	}
	aggregate.append([]byte(`"` + strings.Repeat("<", streamAggregateLimit/6+1) + `"`))
	if !aggregate.exceeded || aggregate.data != nil || aggregate.events != 0 {
		t.Fatalf("overflow retained aggregate memory: exceeded=%v bytes=%d events=%d", aggregate.exceeded, len(aggregate.data), aggregate.events)
	}
	aggregate.append([]byte(`{"later":true}`))
	if aggregate.data != nil || aggregate.fits(usage, 3) {
		t.Fatal("overflow accumulation restarted")
	}
}

func TestF05AggregateByteBudgetCannotOverflow(t *testing.T) {
	maximumInt := int(^uint(0) >> 1)
	for _, value := range []struct {
		retained, additional int
		want                 bool
	}{
		{streamAggregateLimit, 0, true}, {streamAggregateLimit - 1, 1, true},
		{streamAggregateLimit, 1, false}, {0, maximumInt, false},
		{maximumInt, 1, false}, {maximumInt, maximumInt, false},
		{-1, 0, false}, {0, -1, false},
	} {
		if got := streamAggregateBytesFit(value.retained, value.additional); got != value.want {
			t.Fatalf("byte budget (%d,%d)=%v want=%v", value.retained, value.additional, got, value.want)
		}
	}
}
