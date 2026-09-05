package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestTokenProtocolFormulaGovernedCommandsPostgresIntegration is a mandatory
// CI PostgreSQL gate. It traverses the production precheck, generic command,
// result lookup and Billing T1 recovery paths; local runs without the isolated
// database remain an explicit SKIP through the shared helper.
func TestTokenProtocolFormulaGovernedCommandsPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	repository.now = func() time.Time { return time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC) }
	actor := uuid.MustParse("018f57a9-9c17-7efc-a450-35c445492101")
	unitID := uuid.MustParse("018f57a9-9c17-7efc-a450-35c445492102")
	var billingMode atomic.Int32
	var billingPutCalls atomic.Int32
	billing := newProtocolFormulaBillingServer(t, unitID, &billingMode, &billingPutCalls)
	var guardCalls atomic.Int32
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error {
		guardCalls.Add(1)
		return nil
	}))
	activation := tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
		return TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 9}, nil
	})
	handler := newProtocolFormulaIntegrationHandler(t, repository, billing.URL, actor, activation, true)

	for _, path := range []string{"/admin/v1/protocols", "/admin/v1/protocols/018f57a9-9c17-7efc-a450-35c445492201/publish", "/admin/v1/protocols/018f57a9-9c17-7efc-a450-35c445492201/formulas", "/admin/v1/formulas/018f57a9-9c17-7efc-a450-35c445492202/publish"} {
		response := performProtocolFormulaCommand(handler, http.MethodPost, path, map[string]any{}, "retired-governed-direct-01", 0)
		if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), "DIRECT_TOKEN_COMMAND_RETIRED") {
			t.Fatalf("direct command was reachable: path=%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}

	protocol := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_PROTOCOL_DRAFT_CREATE", "PROTOCOL", "GOVERNED_V2", map[string]any{"protocol_code": "GOVERNED_V2", "request_schema": map[string]any{"type": "object", "parameters": []any{map[string]any{"name": "prompt"}}}}, "governed-protocol-draft-01")
	protocolResult := decodeGovernedRedacted(t, protocol)
	protocolID := uuid.MustParse(protocolResult["protocol_definition_id"].(string))
	protocolETag := protocolResult["etag"].(string)

	protocol = executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_PROTOCOL_PUBLISH", "PROTOCOL", protocolID.String(), map[string]any{"expected_protocol_version": 1, "draft_etag": protocolETag}, "governed-protocol-publish-01")
	if decodeGovernedRedacted(t, protocol)["status"] != "PUBLISHED" {
		t.Fatalf("protocol was not published: %#v", protocol)
	}

	formula := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_FORMULA_DRAFT_CREATE", "FORMULA", protocolID.String(), map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": unitID}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}}, "governed-formula-draft-01")
	formulaResult := decodeGovernedRedacted(t, formula)
	formulaID := uuid.MustParse(formulaResult["formula_version_id"].(string))
	formulaETag := formulaResult["etag"].(string)

	// The first guard succeeds while reserving the PENDING T1 reference; the
	// second fails after Billing has committed. GET of the durable reference
	// must resume the same Billing receipt and finish Token atomically; the
	// caller never repeats POST.
	baseline := guardCalls.Load()
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error {
		if guardCalls.Add(1) == baseline+2 {
			return ErrTokenRouteActivationChanged
		}
		return nil
	}))
	command := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_FORMULA_PUBLISH", "FORMULA", formulaID.String(), map[string]any{"expected_formula_version": 1, "draft_etag": formulaETag}, "governed-formula-publish-01")
	first := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", command, "governed-formula-publish-01", 0)
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "COMMAND_RESULT_UNKNOWN") {
		t.Fatalf("expected recoverable Token rollback after Billing success: %d %s", first.Code, first.Body.String())
	}
	pendingRef := decodeGovernedUnknownReference(t, first)
	otherActor := uuid.MustParse("018f57a9-9c17-7efc-a450-35c445492199")
	otherHandler := newProtocolFormulaIntegrationHandler(t, repository, billing.URL, otherActor, activation, true)
	putsBeforeUnauthorizedLookup := billingPutCalls.Load()
	unauthorizedLookup := performProtocolFormulaCommand(otherHandler, http.MethodGet, "/admin/v1/commands/"+pendingRef.String(), nil, "unused-result-query", 0)
	if unauthorizedLookup.Code != http.StatusNotFound || billingPutCalls.Load() != putsBeforeUnauthorizedLookup {
		t.Fatalf("cross-actor lookup disclosed or resumed pending command: %d %s", unauthorizedLookup.Code, unauthorizedLookup.Body.String())
	}
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error { return nil }))
	second := performProtocolFormulaCommand(handler, http.MethodGet, "/admin/v1/commands/"+pendingRef.String(), nil, "unused-result-query", 0)
	var published TokenCommandResult
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &published) != nil || published.CommandReference != pendingRef || published.Status != "SUCCEEDED" {
		t.Fatalf("formula T1 did not recover: %d %s", second.Code, second.Body.String())
	}
	lookup := performProtocolFormulaCommand(handler, http.MethodGet, "/admin/v1/commands/"+pendingRef.String(), nil, "unused-result-query", 0)
	if lookup.Code != http.StatusOK || !strings.Contains(lookup.Body.String(), pendingRef.String()) {
		t.Fatalf("result lookup failed: %d %s", lookup.Code, lookup.Body.String())
	}

	mutated := command
	mutated.Command = json.RawMessage(`{"expected_formula_version":1,"draft_etag":"` + strings.Repeat("d", 64) + `"}`)
	var mutatedFacts map[string]any
	if json.Unmarshal(mutated.ExpectedVersions, &mutatedFacts) != nil {
		t.Fatal("invalid frozen expected facts")
	}
	mutatedFacts["request_fingerprint"] = governedRequestFingerprint(mutated, mutatedFacts)
	mutated.ExpectedVersions, _ = json.Marshal(mutatedFacts)
	conflict := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", mutated, "governed-formula-publish-01", 0)
	if conflict.Code != http.StatusConflict && conflict.Code != http.StatusBadRequest {
		t.Fatalf("same key with changed payload must fail closed: %d %s", conflict.Code, conflict.Body.String())
	}

	// Exact concurrent replay is actor scoped and creates one domain/result
	// fact. One request owns the new result while the peer recovers it.
	concurrentCommand := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_PROTOCOL_DRAFT_CREATE", "PROTOCOL", "CONCURRENT_GOVERNED_V2", map[string]any{"protocol_code": "CONCURRENT_GOVERNED_V2", "request_schema": map[string]any{"type": "object"}}, "governed-concurrent-draft-01")
	concurrentResponses := make([]*httptest.ResponseRecorder, 2)
	var concurrentGroup sync.WaitGroup
	for index := range concurrentResponses {
		concurrentGroup.Add(1)
		go func(index int) {
			defer concurrentGroup.Done()
			concurrentResponses[index] = performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", concurrentCommand, "governed-concurrent-draft-01", 0)
		}(index)
	}
	concurrentGroup.Wait()
	statuses := []int{concurrentResponses[0].Code, concurrentResponses[1].Code}
	sort.Ints(statuses)
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusCreated {
		t.Fatalf("concurrent replay did not converge: %v / %s / %s", statuses, concurrentResponses[0].Body.String(), concurrentResponses[1].Body.String())
	}
	var concurrentFirst, concurrentSecond TokenCommandResult
	if json.Unmarshal(concurrentResponses[0].Body.Bytes(), &concurrentFirst) != nil || json.Unmarshal(concurrentResponses[1].Body.Bytes(), &concurrentSecond) != nil || concurrentFirst.CommandReference != concurrentSecond.CommandReference {
		t.Fatalf("concurrent replay returned different command references")
	}
	var concurrentFacts map[string]any
	if json.Unmarshal(concurrentCommand.ExpectedVersions, &concurrentFacts) != nil {
		t.Fatal("invalid concurrent expected facts")
	}
	equivalentCommand := concurrentCommand
	equivalentCommand.Command = json.RawMessage(" { \n \"request_schema\" : { \"type\" : \"object\" }, \"protocol_code\" : \" concurrent_governed_v2 \" } ")
	equivalentCommand.ExpectedVersions = json.RawMessage(`{"request_fingerprint":"` + concurrentFacts["request_fingerprint"].(string) + `","expected_protocol_head_version":0,"protocol_code":" concurrent_governed_v2 "}`)
	equivalentReplay := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", equivalentCommand, "governed-concurrent-draft-01", 0)
	var equivalentResult TokenCommandResult
	if equivalentReplay.Code != http.StatusOK || json.Unmarshal(equivalentReplay.Body.Bytes(), &equivalentResult) != nil || equivalentResult.CommandReference != concurrentFirst.CommandReference {
		t.Fatalf("equivalent canonical JSON did not replay original result: %d %s", equivalentReplay.Code, equivalentReplay.Body.String())
	}

	// A stale precheck and a closed-command violation both fail before command
	// persistence. The stale head is produced by a legitimate competing write.
	staleCommand := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_PROTOCOL_DRAFT_CREATE", "PROTOCOL", "CAS_GOVERNED_V2", map[string]any{"protocol_code": "CAS_GOVERNED_V2", "request_schema": map[string]any{"type": "object"}}, "governed-stale-draft-01")
	winner := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", staleCommand, "governed-cas-winner-01", 0)
	if winner.Code != http.StatusCreated {
		t.Fatalf("CAS winner failed: %d %s", winner.Code, winner.Body.String())
	}
	stale := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", staleCommand, "governed-stale-draft-01", 0)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("stale precheck was not rejected: %d %s", stale.Code, stale.Body.String())
	}
	invalidCommand := staleCommand
	invalidCommand.Command = json.RawMessage(`{"protocol_code":"CAS_GOVERNED_V2","request_schema":{"type":"object"},"secret":"must-not-persist"}`)
	invalid := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", invalidCommand, "governed-invalid-extra-01", 0)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("extra command field was accepted: %d %s", invalid.Code, invalid.Body.String())
	}

	// Billing transport failure occurs after the T1 anchor. It is therefore an
	// unknown result with the original reference, and GET-only recovery converges.
	formula2 := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_FORMULA_DRAFT_CREATE", "FORMULA", protocolID.String(), map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": unitID}, "expression": map[string]string{"INPUT_TOKEN": "request_count"}}, "governed-formula-draft-02")
	formula2Result := decodeGovernedRedacted(t, formula2)
	formula2ID := uuid.MustParse(formula2Result["formula_version_id"].(string))
	formula2Command := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_FORMULA_PUBLISH", "FORMULA", formula2ID.String(), map[string]any{"expected_formula_version": 2, "draft_etag": formula2Result["etag"].(string)}, "governed-formula-publish-02")
	billingMode.Store(2)
	transient := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", formula2Command, "governed-formula-publish-02", 0)
	if transient.Code != http.StatusServiceUnavailable || !strings.Contains(transient.Body.String(), "COMMAND_RESULT_UNKNOWN") {
		t.Fatalf("Billing transient failure was not locked to UNKNOWN: %d %s", transient.Code, transient.Body.String())
	}
	billingMode.Store(0)
	formula2Ref := decodeGovernedUnknownReference(t, transient)
	recovered := performProtocolFormulaCommand(handler, http.MethodGet, "/admin/v1/commands/"+formula2Ref.String(), nil, "unused-result-query", 0)
	if recovered.Code != http.StatusOK || !strings.Contains(recovered.Body.String(), `"status":"SUCCEEDED"`) {
		t.Fatalf("Billing transient recovery failed: %d %s", recovered.Code, recovered.Body.String())
	}

	// A deterministic Billing conflict after T1 becomes an immutable FAILED
	// result. It must never be left as PENDING/UNKNOWN or issue another write
	// when the caller observes the same reference.
	formula3 := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_FORMULA_DRAFT_CREATE", "FORMULA", protocolID.String(), map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": unitID}, "expression": map[string]string{"INPUT_TOKEN": "image_count"}}, "governed-formula-draft-03")
	formula3Result := decodeGovernedRedacted(t, formula3)
	formula3ID := uuid.MustParse(formula3Result["formula_version_id"].(string))
	formula3Command := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_FORMULA_PUBLISH", "FORMULA", formula3ID.String(), map[string]any{"expected_formula_version": 3, "draft_etag": formula3Result["etag"].(string)}, "governed-formula-publish-03")
	conflictingBinding, _ := json.Marshal(map[string]any{"protocol_code": "HANDLER_V2", "required_unit_definition_ids": []uuid.UUID{unitID}, "formula_snapshot": map[string]any{"evaluator_version": "SAFE_PARAMETER_MAP_V1", "expression": map[string]string{"INPUT_TOKEN": "request_count"}}})
	seedRequest, _ := http.NewRequestWithContext(ctx, http.MethodPut, billing.URL+"/internal/v1/pricing/formula-bindings/"+formula3ID.String(), strings.NewReader(string(conflictingBinding)))
	seedRequest.Header.Set("Content-Type", "application/json")
	seedResponse, seedErr := http.DefaultClient.Do(seedRequest)
	if seedErr != nil {
		t.Fatalf("could not pre-seed conflicting immutable Billing receipt: %v", seedErr)
	}
	if seedResponse.StatusCode != http.StatusOK {
		seedResponse.Body.Close()
		t.Fatalf("could not pre-seed conflicting immutable Billing receipt: status=%d", seedResponse.StatusCode)
	}
	seedResponse.Body.Close()
	billingMode.Store(0)
	deterministic := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", formula3Command, "governed-formula-publish-03", 0)
	formula3Ref := decodeGovernedTerminalResult(t, deterministic, "BILLING_BINDING_CONFLICT")
	deterministicLookup := performProtocolFormulaCommand(handler, http.MethodGet, "/admin/v1/commands/"+formula3Ref.String(), nil, "unused-result-query", 0)
	if deterministicLookup.Code != http.StatusOK || !strings.Contains(deterministicLookup.Body.String(), `"status":"FAILED"`) || !strings.Contains(deterministicLookup.Body.String(), "BILLING_BINDING_CONFLICT") {
		t.Fatalf("Billing conflict was not durably finalized: %d %s", deterministicLookup.Code, deterministicLookup.Body.String())
	}
	formulaRejected := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_FORMULA_DRAFT_CREATE", "FORMULA", protocolID.String(), map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": unitID}, "expression": map[string]string{"INPUT_TOKEN": "max_output_tokens"}}, "governed-formula-draft-rejected")
	rejectedResult := decodeGovernedRedacted(t, formulaRejected)
	rejectedID := uuid.MustParse(rejectedResult["formula_version_id"].(string))
	rejectedCommand := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_FORMULA_PUBLISH", "FORMULA", rejectedID.String(), map[string]any{"expected_formula_version": 4, "draft_etag": rejectedResult["etag"].(string)}, "governed-formula-publish-rejected")
	billingMode.Store(1)
	rejected := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", rejectedCommand, "governed-formula-publish-rejected", 0)
	rejectedRef := decodeGovernedTerminalResult(t, rejected, "FORMULA_UNIT_VALIDATION_REJECTED")
	rejectedLookup := performProtocolFormulaCommand(handler, http.MethodGet, "/admin/v1/commands/"+rejectedRef.String(), nil, "unused-result-query", 0)
	if rejectedLookup.Code != http.StatusOK || !strings.Contains(rejectedLookup.Body.String(), `"status":"FAILED"`) || !strings.Contains(rejectedLookup.Body.String(), "FORMULA_UNIT_VALIDATION_REJECTED") {
		t.Fatalf("Billing validation rejection was not durably finalized: %d %s", rejectedLookup.Code, rejectedLookup.Body.String())
	}

	// Billing can commit its immutable receipt and lose the HTTP response. The
	// original GET must use Billing existing-first recovery and complete Token;
	// no second POST is permitted.
	formula4 := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_FORMULA_DRAFT_CREATE", "FORMULA", protocolID.String(), map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": unitID}, "expression": map[string]string{"INPUT_TOKEN": "document_count"}}, "governed-formula-draft-04")
	formula4Result := decodeGovernedRedacted(t, formula4)
	formula4ID := uuid.MustParse(formula4Result["formula_version_id"].(string))
	formula4Command := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_FORMULA_PUBLISH", "FORMULA", formula4ID.String(), map[string]any{"expected_formula_version": 5, "draft_etag": formula4Result["etag"].(string)}, "governed-formula-publish-04")
	billingMode.Store(7)
	lost := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", formula4Command, "governed-formula-publish-04", 0)
	if lost.Code != http.StatusServiceUnavailable || !strings.Contains(lost.Body.String(), "COMMAND_RESULT_UNKNOWN") {
		t.Fatalf("lost Billing response was not locked to original reference: %d %s", lost.Code, lost.Body.String())
	}
	formula4Ref := decodeGovernedUnknownReference(t, lost)
	billingMode.Store(0)
	lostRecovered := performProtocolFormulaCommand(handler, http.MethodGet, "/admin/v1/commands/"+formula4Ref.String(), nil, "unused-result-query", 0)
	if lostRecovered.Code != http.StatusOK || !strings.Contains(lostRecovered.Body.String(), `"status":"SUCCEEDED"`) {
		t.Fatalf("lost Billing response did not recover through GET: %d %s", lostRecovered.Code, lostRecovered.Body.String())
	}

	// A failure before the T1 insert is provably zero-write. The HTTP response
	// carries no reference and the exact same key may be retried safely.
	formula5 := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_FORMULA_DRAFT_CREATE", "FORMULA", protocolID.String(), map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": unitID}, "expression": map[string]string{"INPUT_TOKEN": "cached_input_tokens"}}, "governed-formula-draft-05")
	formula5Result := decodeGovernedRedacted(t, formula5)
	formula5ID := uuid.MustParse(formula5Result["formula_version_id"].(string))
	formula5Command := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_FORMULA_PUBLISH", "FORMULA", formula5ID.String(), map[string]any{"expected_formula_version": 6, "draft_etag": formula5Result["etag"].(string)}, "governed-formula-publish-05")
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error {
		return errors.New("pre-T1 authority temporarily unavailable")
	}))
	preT1 := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", formula5Command, "governed-formula-publish-05", 0)
	if preT1.Code != http.StatusServiceUnavailable || !strings.Contains(preT1.Body.String(), `"code":"COMMAND_RETRYABLE"`) || strings.Contains(preT1.Body.String(), "command_reference") {
		t.Fatalf("pre-T1 failure was not an explicit zero-write retry: %d %s", preT1.Code, preT1.Body.String())
	}
	var preT1Facts int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE target_resource_id=$1 AND command_type='TOKEN_FORMULA_PUBLISH'`, formula5ID.String()).Scan(&preT1Facts); err != nil || preT1Facts != 0 {
		t.Fatalf("pre-T1 failure persisted a command: count=%d err=%v", preT1Facts, err)
	}
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error { return nil }))
	preT1Retry := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", formula5Command, "governed-formula-publish-05", 0)
	if preT1Retry.Code != http.StatusCreated || !strings.Contains(preT1Retry.Body.String(), `"status":"SUCCEEDED"`) {
		t.Fatalf("same-key retry after proven zero-write did not succeed: %d %s", preT1Retry.Code, preT1Retry.Body.String())
	}

	// Once the T1 insert has been attempted, loss of the COMMIT acknowledgement
	// is ambiguous even if PostgreSQL actually committed it. The production
	// handler must return the generated reference, and GET alone resumes it.
	formula6 := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_FORMULA_DRAFT_CREATE", "FORMULA", protocolID.String(), map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": unitID}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}}, "governed-formula-draft-06")
	formula6Result := decodeGovernedRedacted(t, formula6)
	formula6ID := uuid.MustParse(formula6Result["formula_version_id"].(string))
	formula6Command := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_FORMULA_PUBLISH", "FORMULA", formula6ID.String(), map[string]any{"expected_formula_version": 7, "draft_etag": formula6Result["etag"].(string)}, "governed-formula-publish-06")
	repository.protocolFormulaT1Commit = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return errors.New("T1 commit acknowledgement lost")
	}
	commitLost := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", formula6Command, "governed-formula-publish-06", 0)
	repository.protocolFormulaT1Commit = nil
	if commitLost.Code != http.StatusServiceUnavailable || !strings.Contains(commitLost.Body.String(), "COMMAND_RESULT_UNKNOWN") {
		t.Fatalf("ambiguous T1 commit did not return UNKNOWN: %d %s", commitLost.Code, commitLost.Body.String())
	}
	commitLostRef := decodeGovernedUnknownReference(t, commitLost)
	commitRecovered := performProtocolFormulaCommand(handler, http.MethodGet, "/admin/v1/commands/"+commitLostRef.String(), nil, "unused-result-query", 0)
	if commitRecovered.Code != http.StatusOK || !strings.Contains(commitRecovered.Body.String(), `"status":"SUCCEEDED"`) {
		t.Fatalf("ambiguous T1 commit did not recover by GET: %d %s", commitRecovered.Code, commitRecovered.Body.String())
	}

	// Corrupt historical rows can exist from pre-governance migrations. Every
	// generic publish path must re-run the original domain invariants before
	// Billing or any command/audit/outbox persistence.
	unsafeFormula := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_FORMULA_DRAFT_CREATE", "FORMULA", protocolID.String(), map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": unitID}, "expression": map[string]string{"INPUT_TOKEN": "request_count"}}, "governed-formula-draft-unsafe")
	unsafeFormulaResult := decodeGovernedRedacted(t, unsafeFormula)
	unsafeFormulaID := uuid.MustParse(unsafeFormulaResult["formula_version_id"].(string))
	unsafeFormulaCommand := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_FORMULA_PUBLISH", "FORMULA", unsafeFormulaID.String(), map[string]any{"expected_formula_version": 6, "draft_etag": unsafeFormulaResult["etag"].(string)}, "governed-formula-publish-unsafe")
	if _, err := repository.pool.Exec(ctx, `UPDATE protocol_formula_versions SET evaluator_version='UNSAFE_EVAL_V0' WHERE id=$1`, unsafeFormulaID); err != nil {
		t.Fatal(err)
	}
	putsBeforeUnsafe := billingPutCalls.Load()
	unsafeFormulaResponse := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", unsafeFormulaCommand, "governed-formula-publish-unsafe", 0)
	if unsafeFormulaResponse.Code != http.StatusConflict || billingPutCalls.Load() != putsBeforeUnsafe {
		t.Fatalf("unsafe historical formula reached Billing or persistence: %d %s puts=%d/%d", unsafeFormulaResponse.Code, unsafeFormulaResponse.Body.String(), putsBeforeUnsafe, billingPutCalls.Load())
	}
	var unsafeFormulaCommands int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE target_resource_id=$1 AND command_type='TOKEN_FORMULA_PUBLISH'`, unsafeFormulaID.String()).Scan(&unsafeFormulaCommands); err != nil || unsafeFormulaCommands != 0 {
		t.Fatalf("unsafe formula persisted governed facts: count=%d err=%v", unsafeFormulaCommands, err)
	}

	unsafeSchemaDraft := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_PROTOCOL_DRAFT_CREATE", "PROTOCOL", "UNSAFE_SCHEMA_V2", map[string]any{"protocol_code": "UNSAFE_SCHEMA_V2", "request_schema": map[string]any{"type": "object"}}, "governed-protocol-schema-unsafe")
	unsafeSchemaResult := decodeGovernedRedacted(t, unsafeSchemaDraft)
	unsafeSchemaID := uuid.MustParse(unsafeSchemaResult["protocol_definition_id"].(string))
	unsafeSchemaCommand := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_PROTOCOL_PUBLISH", "PROTOCOL", unsafeSchemaID.String(), map[string]any{"expected_protocol_version": 1, "draft_etag": unsafeSchemaResult["etag"].(string)}, "governed-protocol-publish-schema-unsafe")
	if _, err := repository.pool.Exec(ctx, `UPDATE protocol_definitions SET request_schema='[]'::jsonb WHERE id=$1`, unsafeSchemaID); err != nil {
		t.Fatal(err)
	}
	unsafeSchemaResponse := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", unsafeSchemaCommand, "governed-protocol-publish-schema-unsafe", 0)
	if unsafeSchemaResponse.Code != http.StatusConflict {
		t.Fatalf("invalid protocol schema was publishable: %d %s", unsafeSchemaResponse.Code, unsafeSchemaResponse.Body.String())
	}

	unsafeMappingDraft := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_PROTOCOL_DRAFT_CREATE", "PROTOCOL", "UNSAFE_MAPPING_V2", map[string]any{"protocol_code": "UNSAFE_MAPPING_V2", "request_schema": map[string]any{"type": "object"}}, "governed-protocol-mapping-unsafe")
	unsafeMappingResult := decodeGovernedRedacted(t, unsafeMappingDraft)
	unsafeMappingID := uuid.MustParse(unsafeMappingResult["protocol_definition_id"].(string))
	unsafeMappingCommand := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_PROTOCOL_PUBLISH", "PROTOCOL", unsafeMappingID.String(), map[string]any{"expected_protocol_version": 1, "draft_etag": unsafeMappingResult["etag"].(string)}, "governed-protocol-publish-mapping-unsafe")
	if _, err := repository.pool.Exec(ctx, `INSERT INTO protocol_formula_versions(id,protocol_definition_id,version,status,required_unit_versions,expression,evaluator_version,created_at) VALUES($1,$2,1,'DRAFT',$3,$4,'UNSAFE_EVAL_V0',transaction_timestamp())`, uuid.Must(uuid.NewV7()), unsafeMappingID, map[string]uuid.UUID{"INPUT_TOKEN": unitID}, map[string]string{"INPUT_TOKEN": "arbitrary_code"}); err != nil {
		t.Fatal(err)
	}
	unsafeMappingResponse := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", unsafeMappingCommand, "governed-protocol-publish-mapping-unsafe", 0)
	if unsafeMappingResponse.Code != http.StatusConflict {
		t.Fatalf("unsafe protocol formula set was publishable: %d %s", unsafeMappingResponse.Code, unsafeMappingResponse.Body.String())
	}
	var resultCount, auditCount, outboxCount int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE actor_user_id=$1 AND command_type IN ('TOKEN_PROTOCOL_DRAFT_CREATE','TOKEN_PROTOCOL_PUBLISH','TOKEN_FORMULA_DRAFT_CREATE','TOKEN_FORMULA_PUBLISH')`, actor).Scan(&resultCount); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs WHERE actor_user_id=$1 AND action IN ('TOKEN_PROTOCOL_DRAFT_CREATE','TOKEN_PROTOCOL_PUBLISH','TOKEN_FORMULA_DRAFT_CREATE','TOKEN_FORMULA_PUBLISH')`, actor).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1' AND payload->>'command_type' IN ('TOKEN_PROTOCOL_DRAFT_CREATE','TOKEN_PROTOCOL_PUBLISH','TOKEN_FORMULA_DRAFT_CREATE','TOKEN_FORMULA_PUBLISH')`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if resultCount != 21 || auditCount != 21 || outboxCount != 21 {
		t.Fatalf("governed facts must be exactly once: result=%d audit=%d outbox=%d", resultCount, auditCount, outboxCount)
	}
	var leaked int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs WHERE actor_user_id=$1 AND (summary::text ILIKE '%estimated_input_tokens%' OR summary::text ILIKE '%request_schema%' OR summary::text ILIKE '%secret%')`, actor).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("generic audit leaked formula/schema content: %d", leaked)
	}
}

func decodeGovernedUnknownReference(t *testing.T, response *httptest.ResponseRecorder) uuid.UUID {
	t.Helper()
	var value struct {
		Code    string `json:"code"`
		Details struct {
			CommandReference uuid.UUID `json:"command_reference"`
			ResultQuery      string    `json:"result_query"`
		} `json:"details"`
	}
	if json.Unmarshal(response.Body.Bytes(), &value) != nil || value.Code != "COMMAND_RESULT_UNKNOWN" || value.Details.CommandReference == uuid.Nil || value.Details.ResultQuery != "/token/admin/v1/commands/"+value.Details.CommandReference.String() {
		t.Fatalf("invalid UNKNOWN recovery envelope: %d %s", response.Code, response.Body.String())
	}
	return value.Details.CommandReference
}

func decodeGovernedTerminalResult(t *testing.T, response *httptest.ResponseRecorder, stableCode string) uuid.UUID {
	t.Helper()
	var value TokenCommandResult
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &value) != nil || value.CommandReference == uuid.Nil || value.Status != "FAILED" || value.StableErrorCode == nil || *value.StableErrorCode != stableCode || value.CompletedAt == nil {
		t.Fatalf("invalid terminal governed result: %d %s", response.Code, response.Body.String())
	}
	return value.CommandReference
}

func executeProtocolFormulaGovernedHTTP(t *testing.T, handler http.Handler, commandType, resourceType, resourceID string, command map[string]any, key string) TokenCommandResult {
	t.Helper()
	envelope := governedProtocolFormulaHTTPCommand(t, handler, commandType, resourceType, resourceID, command, key)
	response := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", envelope, key, 0)
	var result TokenCommandResult
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Status != "SUCCEEDED" {
		t.Fatalf("%s failed: %d %s", commandType, response.Code, response.Body.String())
	}
	return result
}

func governedProtocolFormulaHTTPCommand(t *testing.T, handler http.Handler, commandType, resourceType, resourceID string, command map[string]any, key string) TokenCommandEnvelope {
	t.Helper()
	precheckResponse := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands:precheck", map[string]any{"command_type": commandType, "target_resource_type": resourceType, "target_resource_id": resourceID, "command": command}, key, 0)
	var precheck TokenCommandPrecheckResult
	if precheckResponse.Code != http.StatusOK || json.Unmarshal(precheckResponse.Body.Bytes(), &precheck) != nil {
		t.Fatalf("%s precheck failed: %d %s", commandType, precheckResponse.Code, precheckResponse.Body.String())
	}
	var facts map[string]any
	if json.Unmarshal(precheck.ExpectedVersions, &facts) != nil {
		t.Fatal("invalid expected versions")
	}
	commandJSON, _ := json.Marshal(command)
	envelope := TokenCommandEnvelope{CommandType: commandType, Reason: "apply reviewed protocol formula governance command", TargetResourceType: resourceType, TargetResourceID: resourceID, ExpectedVersions: precheck.ExpectedVersions, PrecheckFingerprint: precheck.PrecheckFingerprint, Command: commandJSON}
	facts["request_fingerprint"] = governedRequestFingerprint(envelope, facts)
	envelope.ExpectedVersions, _ = json.Marshal(facts)
	return envelope
}

func decodeGovernedRedacted(t *testing.T, result TokenCommandResult) map[string]any {
	t.Helper()
	var value map[string]any
	if json.Unmarshal(result.RedactedResult, &value) != nil {
		t.Fatalf("invalid redacted result: %s", result.RedactedResult)
	}
	return value
}
