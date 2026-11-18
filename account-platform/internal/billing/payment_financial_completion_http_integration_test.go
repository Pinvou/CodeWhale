package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func paymentCompletionLostACK(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, item := completionNoEffectFixture(t, r, ctx, false)
	first, second := uuid.New(), uuid.New()
	reason := "controlled independently verified financial completion"
	proposal, err := c.PaymentFinancialCompletion(ctx, item.ID, first, uuid.NewString(), reason, PaymentFinancialCompletionInput{ProofKind: "NO_FINANCIAL_EFFECT"}, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed := &atomic.Bool{}
	allowed.Store(true)
	handler := directRepairHandler(t, r, c, allowed)
	var drop atomic.Bool
	drop.Store(true)
	originalACK := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if drop.Swap(false) {
			capture := httptest.NewRecorder()
			handler.ServeHTTP(capture, request)
			if capture.Code != 200 {
				t.Error("first completion did not commit", capture.Code, capture.Body.String())
			}
			originalACK <- bytes.Clone(capture.Body.Bytes())
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		handler.ServeHTTP(w, request)
	}))
	defer server.Close()
	before := completionMoneySnapshot(t, r, ctx)
	key := uuid.NewString()
	body, _ := json.Marshal(map[string]any{"reason": reason})
	path := server.URL + "/admin/v1/payment-anomalies/" + item.ID.String() + "/financial-completion-proposals/" + proposal.Proposal.ID.String() + "/confirm"
	request, _ := http.NewRequest("POST", path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+second.String())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("dropped committed ACK appeared successful")
	}
	committedACK := <-originalACK
	assessment, err := r.AccountClosureAssessment(ctx, order.RequesterID)
	if err != nil || !assessment.Ready {
		t.Fatal("controlled commit did not happen before disconnect", assessment, err)
	}
	c.provider.(*retainedFinancialProvider).err = errors.New("provider unavailable after committed result")
	code, result, raw := completionHTTP(t, server.URL, item.ID, second, key, proposal.Proposal.ID, map[string]any{"reason": reason})
	if code != 200 || result.Case.FinancialActionStatus != "NONE" || !bytes.Equal(raw, committedACK) || completionMoneySnapshot(t, r, ctx) != before {
		t.Fatal("lost ACK did not replay exact committed result", code, string(raw))
	}
	if code, _, _ := completionHTTP(t, server.URL, item.ID, second, key, proposal.Proposal.ID, map[string]any{"reason": reason + " changed"}); code != 409 {
		t.Fatal("same key changed financial decision", code)
	}
	// The wire body remains exact financial strings after its durable JSONB
	// round-trip; no provider call is required to reconstruct the old receipt.
	if !strings.Contains(string(raw), `"amount_fen":"100"`) {
		t.Fatal("completion amount wire contract lost")
	}
	var commands int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_anomaly_commands WHERE payment_anomaly_case_id=$1 AND action=$2`, item.ID, paymentCompletionConfirm).Scan(&commands); err != nil || commands != 1 {
		t.Fatal("lost ACK duplicated completion", commands, err)
	}
}
