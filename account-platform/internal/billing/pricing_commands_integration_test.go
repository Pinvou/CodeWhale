package billing

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestPostgresMeteringCreateLostReplyAndConcurrentReplay(t *testing.T) {
	ctx := context.Background()
	repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	actor, key := uuid.New(), uuid.NewString()
	command := MeteringUnitDefinition{UnitCode: "CONTROLLED_UNIT", MinimumPrecision: 1, PriceDenominator: 100, RoundingRule: "CEILING"}
	var wg sync.WaitGroup
	results := make(chan MeteringUnitDefinition, 8)
	failures := make(chan error, 8)
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			unit, err := repo.CreateMeteringUnitVersion(ctx, command, actor, key)
			results <- unit
			failures <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first MeteringUnitDefinition
	for unit := range results {
		if first.ID == uuid.Nil {
			first = unit
		}
		if unit.ID != first.ID || unit.Version != 1 {
			t.Fatal("logical create produced multiple versions")
		}
	}
	command.MinimumPrecision = 2
	if _, err := repo.CreateMeteringUnitVersion(ctx, command, actor, key); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed command accepted: %v", err)
	}
	inactive, err := repo.DeactivateMeteringUnit(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := repo.DeactivateMeteringUnit(ctx, first.ID)
	if err != nil || inactive.DeactivatedAt == nil || replay.DeactivatedAt == nil || !inactive.DeactivatedAt.Equal(*replay.DeactivatedAt) {
		t.Fatal("deactivation replay changed lifecycle fact")
	}
	command.MinimumPrecision = 1
	created, err := repo.CreateMeteringUnitVersion(ctx, command, actor, key)
	if err != nil || created.ID != first.ID || created.Status != "ACTIVE" {
		t.Fatalf("lost creation receipt replaced with later state: %v", err)
	}
	var units, receipts int
	err = repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM metering_unit_definitions WHERE unit_code='CONTROLLED_UNIT'),(SELECT count(*) FROM pricing_operation_commands WHERE actor_user_id=$1 AND idempotency_key=$2)`, actor, key).Scan(&units, &receipts)
	if err != nil || units != 1 || receipts != 1 {
		t.Fatalf("duplicate create effects %d/%d %v", units, receipts, err)
	}
}

func TestPostgresPricingDraftLostReplySurvivesSpaceETagChange(t *testing.T) {
	ctx := context.Background()
	repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	actor, key := uuid.New(), uuid.NewString()
	future := time.Now().In(shanghaiLocation).AddDate(0, 0, 2)
	from := time.Date(future.Year(), future.Month(), future.Day(), 0, 0, 0, 0, shanghaiLocation)
	zero := int64(0)
	one := int64(1)
	command := PriceDraftCommand{ScopeType: "GLOBAL", ProtocolCode: "CHAT", FormulaVersionID: chatFormulaVersionID, CreditUnit: "MICROCREDIT", EffectiveFrom: from, ExpectedETag: 1, Units: []PriceUnitConfiguration{
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000001"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &one},
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000002"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &zero},
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000003"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &one}}}
	var wg sync.WaitGroup
	results := make(chan ManagedPriceVersion, 8)
	failures := make(chan error, 8)
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := repo.CreatePriceDraft(ctx, command, actor, key)
			results <- result
			failures <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first ManagedPriceVersion
	for item := range results {
		if first.PriceVersionID == uuid.Nil {
			first = item
		}
		if item.PriceVersionID != first.PriceVersionID || item.Version != 1 {
			t.Fatal("logical draft produced multiple versions")
		}
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE pricing_space_versions SET etag=etag+1 WHERE scope_type='GLOBAL' AND protocol_code='CHAT'`); err != nil {
		t.Fatal(err)
	}
	replay, err := repo.CreatePriceDraft(ctx, command, actor, key)
	if err != nil || replay.PriceVersionID != first.PriceVersionID || replay.SpaceETag != 1 {
		t.Fatalf("lost acknowledgement rejected after ETag change: %v", err)
	}
	command.EffectiveFrom = from.AddDate(0, 0, 1)
	if _, err := repo.CreatePriceDraft(ctx, command, actor, key); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed draft accepted: %v", err)
	}
	command.EffectiveFrom = from
	if _, err := repo.CreatePriceDraft(ctx, command, actor, uuid.NewString()); !errors.Is(err, ErrPriceETagConflict) {
		t.Fatalf("new stale command bypassed ETag: %v", err)
	}
	var drafts, audits, receipts int
	err = repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pricing_versions WHERE id=$1),(SELECT count(*) FROM pricing_audits WHERE price_version_id=$1 AND action='DRAFT_CREATED'),(SELECT count(*) FROM pricing_operation_commands WHERE actor_user_id=$2 AND idempotency_key=$3)`, first.PriceVersionID, actor, key).Scan(&drafts, &audits, &receipts)
	if err != nil || drafts != 1 || audits != 1 || receipts != 1 {
		t.Fatalf("duplicate draft effects %d/%d/%d %v", drafts, audits, receipts, err)
	}
}

func pricingModelDraftFixture(t *testing.T) (*PostgresRepository, uuid.UUID, PriceDraftCommand, ManagedPriceVersion) {
	t.Helper()
	ctx := context.Background()
	repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	actor := uuid.New()
	model := "controlled-" + uuid.NewString()
	future := time.Now().In(shanghaiLocation).AddDate(0, 0, 2)
	from := time.Date(future.Year(), future.Month(), future.Day(), 0, 0, 0, 0, shanghaiLocation)
	zero, one := int64(0), int64(1)
	command := PriceDraftCommand{ScopeType: "MODEL", ScopeID: &model, ProtocolCode: "CHAT", FormulaVersionID: chatFormulaVersionID, CreditUnit: "MICROCREDIT", EffectiveFrom: from, ExpectedETag: 1, Units: []PriceUnitConfiguration{
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000001"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &one},
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000002"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &zero},
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000003"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &one}}}
	draft, err := repo.CreatePriceDraft(ctx, command, actor, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return repo, actor, command, draft
}

func TestPostgresDraftUpdateRejectsStaleFactsAndReplaysConfirmedFacts(t *testing.T) {
	repo, actor, command, draft := pricingModelDraftFixture(t)
	ctx := context.Background()
	command.EffectiveFrom = command.EffectiveFrom.AddDate(0, 0, 1)
	updated, err := repo.UpdatePriceDraft(ctx, draft.PriceVersionID, actor, command)
	if err != nil {
		t.Fatal(err)
	}
	stale := command
	stale.EffectiveFrom = stale.EffectiveFrom.AddDate(0, 0, 1)
	if _, err := repo.UpdatePriceDraft(ctx, draft.PriceVersionID, actor, stale); !errors.Is(err, ErrPriceETagConflict) {
		t.Errorf("old ETag changed a previously updated draft: %v", err)
	}
	replay, err := repo.UpdatePriceDraft(ctx, draft.PriceVersionID, actor, command)
	if err != nil || replay.PriceVersionID != updated.PriceVersionID || !replay.EffectiveFrom.Equal(updated.EffectiveFrom) || replay.SpaceETag != updated.SpaceETag {
		t.Errorf("lost update acknowledgement did not replay original facts: %v", err)
	}
	var stored time.Time
	if err := repo.pool.QueryRow(ctx, `SELECT effective_from FROM pricing_versions WHERE id=$1`, draft.PriceVersionID).Scan(&stored); err != nil || !stored.Equal(updated.EffectiveFrom) {
		t.Errorf("stale write changed stored facts: %v", err)
	}
}

func TestPostgresSimplePricePublicationPreservesManagedDraft(t *testing.T) {
	repo, _, command, draft := pricingModelDraftFixture(t)
	ctx := context.Background()
	simple, err := repo.AdminCreatePriceVersion(ctx, *command.ScopeID, 7, 9, 2)
	if err != nil {
		t.Fatal(err)
	}
	var until *time.Time
	if err := repo.pool.QueryRow(ctx, `SELECT effective_to FROM pricing_versions WHERE id=$1`, draft.PriceVersionID).Scan(&until); err != nil || until != nil {
		t.Errorf("simple publication truncated an unpublished managed draft: %v", err)
	}
	resolved, err := repo.ResolveCurrentPrice(ctx, *command.ScopeID, "CHAT", chatFormulaVersionID, "MICROCREDIT", time.Now().UTC())
	if err != nil || resolved.ID != simple.ID || resolved.InputPerMillion != 7 || resolved.OutputPerMillion != 9 || resolved.CachedInputPerMillion != 2 || len(resolved.Units) != 3 {
		t.Errorf("simple publication lost executable three-unit mapping: %v", err)
	}
}

func TestPostgresPricePublishConcurrentLostReplyUsesOneReceipt(t *testing.T) {
	repo, actor, _, draft := pricingModelDraftFixture(t)
	ctx := context.Background()
	key := uuid.NewString()
	var wg sync.WaitGroup
	results := make(chan ManagedPriceVersion, 8)
	failures := make(chan error, 8)
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := repo.PublishPriceVersion(ctx, draft.PriceVersionID, actor, draft.SpaceETag, key)
			results <- result
			failures <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Error(err)
		}
	}
	for result := range results {
		if result.PriceVersionID != draft.PriceVersionID || result.LifecycleStatus != "PUBLISHED" || result.SpaceETag != draft.SpaceETag+1 || len(result.Units) != 3 {
			t.Error("publish response lost original immutable facts")
		}
	}
	var receipts, audits int
	if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pricing_publish_commands WHERE actor_user_id=$1 AND idempotency_key=$2),(SELECT count(*) FROM pricing_audits WHERE price_version_id=$3 AND action='PUBLISHED')`, actor, key, draft.PriceVersionID).Scan(&receipts, &audits); err != nil || receipts != 1 || audits != 1 {
		t.Errorf("publish effects %d/%d err=%v", receipts, audits, err)
	}
}

func TestPostgresPriceUpdateConcurrentReplayAndCurrentNamespaceRead(t *testing.T) {
	repo, actor, command, draft := pricingModelDraftFixture(t)
	ctx := context.Background()
	command.EffectiveFrom = command.EffectiveFrom.AddDate(0, 0, 1)
	var wg sync.WaitGroup
	results := make(chan ManagedPriceVersion, 8)
	failures := make(chan error, 8)
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := repo.UpdatePriceDraft(ctx, draft.PriceVersionID, actor, command)
			results <- result
			failures <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	for item := range results {
		if item.PriceVersionID != draft.PriceVersionID || item.SpaceETag != 2 || !item.EffectiveFrom.Equal(command.EffectiveFrom) {
			t.Fatal("concurrent logical update returned different facts")
		}
	}
	if _, err := repo.AdminCreatePriceVersion(ctx, *command.ScopeID, 7, 9, 2); err != nil {
		t.Fatal(err)
	}
	replay, err := repo.UpdatePriceDraft(ctx, draft.PriceVersionID, actor, command)
	if err != nil || replay.SpaceETag != 2 || !replay.EffectiveFrom.Equal(command.EffectiveFrom) {
		t.Fatalf("later namespace change replaced confirmed update: %v", err)
	}
	items, err := repo.ListManagedPrices(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.PriceVersionID == draft.PriceVersionID {
			found = true
			if item.SpaceETag != 3 || !item.EffectiveFrom.Equal(command.EffectiveFrom) {
				t.Fatal("draft list did not return current namespace CAS with stored facts")
			}
		}
	}
	if !found {
		t.Fatal("draft disappeared from managed list")
	}
	var receipts, audits int
	if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pricing_operation_commands WHERE actor_user_id=$1 AND operation='PRICE_DRAFT_UPDATE'),(SELECT count(*) FROM pricing_audits WHERE price_version_id=$2 AND action='DRAFT_UPDATED')`, actor, draft.PriceVersionID).Scan(&receipts, &audits); err != nil || receipts != 1 || audits != 1 {
		t.Fatalf("update effects %d/%d err=%v", receipts, audits, err)
	}
	command.ExpectedETag = 3
	command.EffectiveFrom = command.EffectiveFrom.AddDate(0, 0, 1)
	updated, err := repo.UpdatePriceDraft(ctx, draft.PriceVersionID, actor, command)
	if err != nil || updated.SpaceETag != 4 {
		t.Fatalf("current namespace update failed: %v", err)
	}
}

func TestPostgresPriceUpdateCompetingActorsHaveOneFinancialConfigurationWinner(t *testing.T) {
	repo, _, command, draft := pricingModelDraftFixture(t)
	ctx := context.Background()
	type outcome struct {
		item ManagedPriceVersion
		err  error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for i := range 2 {
		command := command
		command.EffectiveFrom = command.EffectiveFrom.AddDate(0, 0, i+1)
		actor := uuid.New()
		go func() {
			<-start
			item, err := repo.UpdatePriceDraft(ctx, draft.PriceVersionID, actor, command)
			results <- outcome{item, err}
		}()
	}
	close(start)
	winners, rejected := 0, 0
	var winner ManagedPriceVersion
	for range 2 {
		result := <-results
		if result.err == nil {
			winners++
			winner = result.item
		} else if errors.Is(result.err, ErrPriceETagConflict) {
			rejected++
		} else {
			t.Fatal(result.err)
		}
	}
	if winners != 1 || rejected != 1 || winner.SpaceETag != 2 {
		t.Fatalf("competing CAS outcomes %d/%d", winners, rejected)
	}
	var stored time.Time
	var receipts, audits int
	if err := repo.pool.QueryRow(ctx, `SELECT effective_from,(SELECT count(*) FROM pricing_operation_commands WHERE operation='PRICE_DRAFT_UPDATE'),(SELECT count(*) FROM pricing_audits WHERE price_version_id=$1 AND action='DRAFT_UPDATED') FROM pricing_versions WHERE id=$1`, draft.PriceVersionID).Scan(&stored, &receipts, &audits); err != nil || !stored.Equal(winner.EffectiveFrom) || receipts != 1 || audits != 1 {
		t.Fatalf("winner lost immutable configuration or produced duplicate effects: %v", err)
	}
}

func TestPostgresSimplePublicationPreservesOtherProtocolAndFuturePublishedWindow(t *testing.T) {
	repo, actor, command, draft := pricingModelDraftFixture(t)
	ctx := context.Background()
	futureChat, err := repo.PublishPriceVersion(ctx, draft.PriceVersionID, actor, 1, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	formulaID := uuid.New()
	ids := make([]uuid.UUID, 0, len(command.Units))
	for _, unit := range command.Units {
		ids = append(ids, unit.UnitDefinitionID)
	}
	snapshot, err := json.Marshal(map[string]any{
		"evaluator_version": "SAFE_PARAMETER_MAP_V1",
		"required_unit_versions": map[string]uuid.UUID{
			"INPUT_TOKEN": ids[0], "CACHED_INPUT_TOKEN": ids[1], "OUTPUT_TOKEN": ids[2],
		},
		"expression": map[string]string{
			"INPUT_TOKEN": "estimated_input_tokens", "CACHED_INPUT_TOKEN": "cached_input_tokens", "OUTPUT_TOKEN": "max_output_tokens",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RegisterFormulaBinding(ctx, formulaID, "CONTROLLED_PROTOCOL", ids, snapshot, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	otherCommand := command
	otherCommand.ProtocolCode = "CONTROLLED_PROTOCOL"
	otherCommand.FormulaVersionID = formulaID
	otherDraft, err := repo.CreatePriceDraft(ctx, otherCommand, actor, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	otherPublished, err := repo.PublishPriceVersion(ctx, otherDraft.PriceVersionID, actor, 1, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	simple, err := repo.AdminCreatePriceVersion(ctx, *command.ScopeID, 7, 9, 2)
	if err != nil || simple.EffectiveTo == nil || !simple.EffectiveTo.Equal(futureChat.EffectiveFrom) {
		t.Fatalf("simple publication lost scheduled successor boundary: %v", err)
	}
	for _, published := range []ManagedPriceVersion{futureChat, otherPublished} {
		var until *time.Time
		var status string
		if err := repo.pool.QueryRow(ctx, `SELECT effective_to,lifecycle_status FROM pricing_versions WHERE id=$1`, published.PriceVersionID).Scan(&until, &status); err != nil || until != nil || status != "PUBLISHED" {
			t.Fatalf("simple publication mutated future/other protocol: %v", err)
		}
		resolved, err := repo.ResolveCurrentPrice(ctx, *command.ScopeID, published.ProtocolCode, published.FormulaVersionID, "MICROCREDIT", published.EffectiveFrom)
		if err != nil || resolved.ID != published.PriceVersionID {
			t.Fatalf("published configuration lost future execution: %v", err)
		}
	}
	var otherETag int64
	if err := repo.pool.QueryRow(ctx, `SELECT etag FROM pricing_space_versions WHERE scope_type='MODEL' AND scope_key=$1 AND protocol_code='CONTROLLED_PROTOCOL' AND formula_version_id=$2`, *command.ScopeID, formulaID).Scan(&otherETag); err != nil || otherETag != 2 {
		t.Fatalf("simple publication changed another namespace: %v", err)
	}
}
