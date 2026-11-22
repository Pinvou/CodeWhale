package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Gate only genuine writer BEGIN queries. Both public methods must already
// have missed the runtime preflight before either writer starts. The second
// begins after the first HTTP ACK, guaranteeing LEGACY/checkpoint-nil reads
// the committed APPROVED row instead of relying on a probabilistic race.
type decisionLegacyBeginGate struct {
	begins         atomic.Int32
	forbiddenReads atomic.Int32
	secondBegin    chan struct{}
	firstFinished  chan struct{}
}

func (g *decisionLegacyBeginGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.ToLower(strings.TrimSpace(data.SQL))
	if strings.Contains(sql, "from approval_state_events e join approval_decisions") {
		g.forbiddenReads.Add(1)
	}
	if strings.HasPrefix(sql, "begin") {
		switch g.begins.Add(1) {
		case 1:
			select {
			case <-g.secondBegin:
			case <-ctx.Done():
			}
		case 2:
			close(g.secondBegin)
			select {
			case <-g.firstFinished:
			case <-ctx.Done():
			}
		}
	}
	return ctx
}
func (*decisionLegacyBeginGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestApprovalDecisionLegacyConcurrentReceiptPostgres(t *testing.T) {
	t.Run("LEGACY-public-preflight-miss-to-writer-CAS-mismatch-confirms-after-rollback", func(t *testing.T) {
		r, f, observer := approvalListPoolFixture(t)
		ctx := f.ctx
		r.approvalPresenterReady = approvalPolicyPresenterReady
		r.WithBillingGovernanceActivation(queryLeaseBillingActivation{})
		control, err := r.ApprovalActionControl(ctx)
		if err != nil || control.Mode != AdminApprovalModeLegacy || control.ProofRequired {
			t.Fatal("fixture did not retain genuine LEGACY mode", control, err)
		}
		var writerCanRead bool
		if err = observer.QueryRow(ctx, `SELECT has_table_privilege('account_approval_writer','approval_decisions','SELECT') OR has_table_privilege('account_approval_writer','approval_state_events','SELECT')`).Scan(&writerCanRead); err != nil || writerCanRead {
			t.Fatal("writer SELECT isolation changed", writerCanRead, err)
		}
		owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "legacy-decision-owner")
		actor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "legacy-decision-approver")
		draft := ApprovalDraft{CommandType: "WALLET_ADJUSTMENT", IdempotencyKey: uuid.NewString(), TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: uuid.NewString(), NormalizedCommand: json.RawMessage(`{}`), Reason: "controlled legacy original decision", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`)}
		// Use the same installed command producer as the permanent primary receipt root.
		input := decisionLegacyWalletInput(t)
		draft.TargetResourceType = input.TargetResourceType
		draft.TargetResourceID = input.TargetResourceID
		draft.NormalizedCommand = input.NormalizedCommand
		draft.CriticalResourceVersions = input.CriticalResourceVersions
		draft.FinancialSnapshot = input.FinancialSnapshot
		item := wallClockSubmitDraft(t, r, ctx, entitlementApprovalPrincipal(owner), draft)
		gate := &decisionLegacyBeginGate{secondBegin: make(chan struct{}), firstFinished: make(chan struct{})}
		config := r.approvalWriterPool.Config().Copy()
		config.ConnConfig.Tracer = gate
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		original := r.approvalWriterPool
		r.approvalWriterPool = pool
		original.Close()
		handler := NewService(r, nil).Handler(func(context.Context, string) (identity.Principal, error) {
			return entitlementApprovalPrincipal(actor), nil
		})
		const reason = " Controlled legacy decision reason "
		raw, _ := json.Marshal(map[string]any{"expected_version": item.Version, "decision": "APPROVED", "reason": reason, "command_protocol": 1, "observed_versions": draft.CriticalResourceVersions})
		post := func() *httptest.ResponseRecorder {
			requestContext, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			req := httptest.NewRequest("POST", fmt.Sprintf("/admin/v1/approval-requests/%s/actions/decide", item.ID), strings.NewReader(string(raw)))
			req = req.WithContext(requestContext)
			req.Header.Set("Authorization", "Bearer controlled")
			req.Header.Set("X-Pinvou-Approval-Action-Control-Version", fmt.Sprint(control.Version))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			return w
		}
		done := make(chan *httptest.ResponseRecorder, 2)
		for range 2 {
			go func() { done <- post() }()
		}
		first := <-done
		close(gate.firstFinished)
		second := <-done
		if first.Code != 200 || second.Code != 200 {
			t.Fatalf("LEGACY original recovery got %d/%d want200/200; writer begins=%d forbidden receipt SELECT=%d; first=%s second=%s", first.Code, second.Code, gate.begins.Load(), gate.forbiddenReads.Load(), first.Body.String(), second.Body.String())
		}
		if gate.begins.Load() != 2 || gate.forbiddenReads.Load() != 0 {
			t.Fatal("LEGACY cut point or writer read boundary not exercised", gate.begins.Load(), gate.forbiddenReads.Load())
		}
		var decisions, events, conflicts, authorizations int
		err = observer.QueryRow(ctx, `SELECT (SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),(SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1 AND reason_code='INDEPENDENT_DECISION'),(SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1 AND reason_code='DECISION_CONFLICT'),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1')`, item.ID).Scan(&decisions, &events, &conflicts, &authorizations)
		if err != nil || decisions != 1 || events != 1 || conflicts != 0 || authorizations != 1 {
			t.Fatal("competing original duplicated effects", decisions, events, conflicts, authorizations, err)
		}
		digest := sha256.Sum256([]byte(reason))
		readctx := context.WithValue(ctx, approvalV2ControlVersionKey{}, control.Version)
		proof, err := r.ApprovalDecisionResult(readctx, entitlementApprovalPrincipal(actor), item.ID, item.Version, control.Version, "APPROVED", hex.EncodeToString(digest[:]))
		if err != nil || proof.ExpectedVersion != item.Version || proof.ResultVersion != item.Version+1 || proof.OriginActionControlVersion != control.Version {
			t.Fatal("original runtime receipt missing", proof, err)
		}
	})
}

func decisionLegacyWalletInput(t *testing.T) approvalencoding.LegacyProofInput {
	t.Helper()
	raw, err := os.ReadFile("testdata/approval_recovery_query_command_inputs.json")
	if err != nil {
		t.Fatal(err)
	}
	var inputs map[string]approvalencoding.LegacyProofInput
	if json.Unmarshal(raw, &inputs) != nil {
		t.Fatal("missing installed Billing input")
	}
	return inputs["WALLET_ADJUSTMENT"]
}
