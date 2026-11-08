package billing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This counts actual PostgreSQL SELECT operations, not a mock of either
// repository reader. Setup/writes and the unchanged single-price reference
// reads finish before the measured operations.
type managedPriceReadTrace struct {
	mu       sync.Mutex
	selects  int
	afterIDs chan struct{}
	resume   chan struct{}
	once     sync.Once
}

type managedPriceIDsTraceKey struct{}

func (tr *managedPriceReadTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(data.SQL)), "SELECT") {
		tr.mu.Lock()
		tr.selects++
		tr.mu.Unlock()
	}
	tr.mu.Lock()
	barrierEnabled := tr.afterIDs != nil
	tr.mu.Unlock()
	if barrierEnabled && strings.HasPrefix(data.SQL, "SELECT id,created_at FROM pricing_versions") {
		return context.WithValue(ctx, managedPriceIDsTraceKey{}, true)
	}
	return ctx
}
func (tr *managedPriceReadTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if marked, _ := ctx.Value(managedPriceIDsTraceKey{}).(bool); marked && data.Err == nil {
		tr.mu.Lock()
		afterIDs, resume := tr.afterIDs, tr.resume
		tr.mu.Unlock()
		tr.once.Do(func() {
			close(afterIDs)
			select {
			case <-resume:
			case <-ctx.Done():
			}
		})
	}
}
func (tr *managedPriceReadTrace) barrier(afterIDs, resume chan struct{}) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.afterIDs, tr.resume = afterIDs, resume
}
func (tr *managedPriceReadTrace) reset() { tr.mu.Lock(); tr.selects = 0; tr.mu.Unlock() }
func (tr *managedPriceReadTrace) count() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.selects
}

func testManagedPriceBatchReadContract(t *testing.T) {
	repo, ctx := retainedFinanceRepository(t)
	fixed := time.Date(2026, 10, 29, 12, 0, 0, 0, shanghaiLocation)
	from := time.Date(2026, 10, 30, 0, 0, 0, 0, shanghaiLocation)
	repo.now = func() time.Time { return fixed }
	actor := uuid.New()
	command := PriceDraftCommand{ScopeType: "GLOBAL", ProtocolCode: "CHAT", FormulaVersionID: chatFormulaVersionID, CreditUnit: "MICROCREDIT", EffectiveFrom: from, ExpectedETag: 1}
	var ids []uuid.UUID
	for index := range 201 {
		command.Units = nil
		for unit := 1; unit <= 3; unit++ {
			value := int64(index * unit) // Includes explicit zero; not nil/inheritance.
			command.Units = append(command.Units, PriceUnitConfiguration{UnitDefinitionID: uuid.MustParse(fmt.Sprintf("00000000-0000-7000-8100-%012d", unit)), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &value})
		}
		item, err := repo.CreatePriceDraft(ctx, command, actor, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.PriceVersionID)
	}
	model := "controlled-batch-model-" + uuid.NewString()
	command.ScopeType, command.ScopeID = "MODEL", &model
	command.Units[0].ConfigType, command.Units[0].FixedPricePerDenominator = "INHERIT_GLOBAL", nil
	modelDraft, err := repo.CreatePriceDraft(ctx, command, actor, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, modelDraft.PriceVersionID)
	canonical := map[uuid.UUID]ManagedPriceVersion{}
	for _, id := range ids {
		item, err := repo.ReadManagedPrice(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		canonical[id] = item
	}
	originalPool := repo.pool
	trace := &managedPriceReadTrace{}
	config := repo.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // Before the original shared helper's UUID DB drop.
	repo.pool = pool
	check := func(t *testing.T, items []ManagedPriceVersion, want int) {
		t.Helper()
		if len(items) != want {
			t.Fatalf("got %d price rows, want %d", len(items), want)
		}
		for _, item := range items {
			if !reflect.DeepEqual(item, canonical[item.PriceVersionID]) {
				t.Fatalf("batch changed the unchanged single-read contract for %s", item.PriceVersionID)
			}
		}
	}
	space := ManagedPriceSpaceQuery{ScopeType: "GLOBAL", ProtocolCode: "CHAT", FormulaVersionID: chatFormulaVersionID, CreditUnit: "MICROCREDIT"}
	for _, size := range []int{1, 100, 200} {
		t.Run(fmt.Sprintf("list-%d", size), func(t *testing.T) {
			trace.reset()
			page, err := repo.ListManagedPrices(ctx, size, "")
			if err != nil {
				t.Fatal(err)
			}
			selects := trace.count()
			t.Logf("controlled rows=%d entry=list SELECTs=%d budget=3", size, selects)
			if selects > 3 {
				t.Errorf("managed price list SELECTs grow with page size: %d > 3", selects)
			}
			check(t, page.Items, size)
			if !page.HasMore || page.NextCursor == "" {
				t.Fatal("batch lost the original limit+1 continuation")
			}
		})
		t.Run(fmt.Sprintf("namespace-%d", size), func(t *testing.T) {
			trace.reset()
			etag, items, more, err := repo.ReadManagedPriceSpace(ctx, space, size)
			if err != nil {
				t.Fatal(err)
			}
			selects := trace.count()
			t.Logf("controlled rows=%d entry=namespace SELECTs=%d budget=4", size, selects)
			if selects > 4 {
				t.Errorf("namespace price list SELECTs grow with page size: %d > 4", selects)
			}
			check(t, items, size)
			if etag != 1 || !more {
				t.Fatal("batch changed the namespace ETag or continuation")
			}
		})
	}
	t.Run("empty-page", func(t *testing.T) {
		trace.reset()
		page, err := repo.ListManagedPrices(ctx, 200, encodeManagedPriceCursor(fixed.Add(-time.Hour), uuid.New(), 200))
		if err != nil || page.Items == nil || len(page.Items) != 0 || page.HasMore || page.NextCursor != "" || trace.count() != 1 {
			t.Fatal("empty-page contract changed", page, err, trace.count())
		}
	})
	t.Run("absent-namespace", func(t *testing.T) {
		trace.reset()
		absent := "controlled-empty-namespace-" + uuid.NewString()
		query := space
		query.ScopeType, query.ScopeID = "MODEL", &absent
		etag, items, more, err := repo.ReadManagedPriceSpace(ctx, query, 200)
		if err != nil || etag != 1 || items == nil || len(items) != 0 || more || trace.count() != 1 {
			t.Fatal("uncreated namespace contract changed", etag, items, more, err, trace.count())
		}
	})
	t.Run("snapshot-and-next-read", func(t *testing.T) {
		prior, err := repo.ListManagedPrices(ctx, 200, "")
		if err != nil {
			t.Fatal(err)
		}
		target := prior.Items[0]
		afterIDs, resume := make(chan struct{}), make(chan struct{})
		trace.barrier(afterIDs, resume)
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		type outcome struct {
			page ManagedPricePage
			err  error
		}
		done := make(chan outcome, 1)
		joined := false
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(resume) }) }
		defer func() {
			release()
			cancel()
			if !joined {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("owned snapshot reader did not retire")
				}
			}
		}()
		go func() { page, err := repo.ListManagedPrices(readCtx, 200, ""); done <- outcome{page, err} }()
		select {
		case <-afterIDs:
		case <-readCtx.Done():
			t.Fatal("reader did not establish its first SELECT snapshot")
		}
		writer := NewPostgresRepositoryWithPool(originalPool)
		writer.now = repo.now
		change := PriceDraftCommand{ScopeType: target.ScopeType, ScopeID: target.ScopeID, ProtocolCode: target.ProtocolCode, FormulaVersionID: target.FormulaVersionID, CreditUnit: target.CreditUnit, EffectiveFrom: target.EffectiveFrom, ExpectedETag: target.CurrentSpaceETag}
		for _, unit := range target.Units {
			value := int64(42)
			unit.ConfigType, unit.FixedPricePerDenominator = "FIXED_PRICE", &value
			change.Units = append(change.Units, unit)
		}
		if _, err := writer.UpdatePriceDraft(ctx, target.PriceVersionID, actor, change); err != nil {
			t.Fatal(err)
		}
		release()
		var result outcome
		select {
		case result = <-done:
			joined = true
		case <-readCtx.Done():
			t.Fatal("snapshot reader did not complete after writer")
		}
		trace.barrier(nil, nil) // Reader completion is the happens-before boundary.
		if result.err != nil || !reflect.DeepEqual(result.page, prior) {
			t.Fatal("page mixed post-writer units or namespace ETag into its old snapshot", result.err)
		}
		next, err := repo.ListManagedPrices(ctx, 200, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range next.Items {
			reference, err := repo.ReadManagedPrice(ctx, item.PriceVersionID)
			if err != nil || !reflect.DeepEqual(item, reference) {
				t.Fatal("next batch disagreed with original canonical single read", err)
			}
			if item.PriceVersionID == target.PriceVersionID && (item.CurrentSpaceETag != target.CurrentSpaceETag+1 || item.SpaceETag != target.SpaceETag+1 || *item.Units[0].FixedPricePerDenominator != 42) {
				t.Fatal("next read failed to observe committed writer facts")
			}
		}
	})
	t.Run("database-faults-and-recovery", func(t *testing.T) {
		for _, table := range []string{"pricing_versions", "pricing_version_unit_configs", "pricing_space_versions"} {
			t.Run(table, func(t *testing.T) {
				if _, err := retainedFinanceOwnerExec(t, repo, ctx, "REVOKE SELECT ON "+table+" FROM billing_service"); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := retainedFinanceOwnerExec(t, repo, ctx, "GRANT SELECT ON "+table+" TO billing_service"); err != nil {
						t.Error(err)
					}
				}()
				_, listErr := repo.ListManagedPrices(ctx, 20, "")
				_, _, _, spaceErr := repo.ReadManagedPriceSpace(ctx, space, 20)
				for _, err := range []error{listErr, spaceErr} {
					var databaseError *pgconn.PgError
					if !errors.As(err, &databaseError) || databaseError.Code != "42501" || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("database SELECT fault became success or business absence: %v", err)
					}
				}
				if _, err := retainedFinanceOwnerExec(t, repo, ctx, "GRANT SELECT ON "+table+" TO billing_service"); err != nil {
					t.Fatal(err)
				}
				if page, err := repo.ListManagedPrices(ctx, 20, ""); err != nil || len(page.Items) != 20 {
					t.Fatal("reader failed after restored database permission", err)
				}
				if _, items, _, err := repo.ReadManagedPriceSpace(ctx, space, 20); err != nil || len(items) != 20 {
					t.Fatal("namespace reader failed after restored permission", err)
				}
			})
		}
	})
	t.Run("cancelled-and-next-read", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := repo.ListManagedPrices(cancelled, 20, ""); !errors.Is(err, context.Canceled) {
			t.Fatalf("list cancellation identity lost: %v", err)
		}
		if _, _, _, err := repo.ReadManagedPriceSpace(cancelled, space, 20); !errors.Is(err, context.Canceled) {
			t.Fatalf("namespace cancellation identity lost: %v", err)
		}
		if page, err := repo.ListManagedPrices(ctx, 20, ""); err != nil || len(page.Items) != 20 {
			t.Fatal("cancelled read poisoned later page", err)
		}
	})
	// Explicit abnormal historical inputs, NOT normal CreatePriceDraft output.
	// Normal producer validation requires all three unit definitions.
	t.Run("abnormal-read-shapes", func(t *testing.T) {
		for index, name := range []string{"empty-units", "missing-namespace"} {
			t.Run(name, func(t *testing.T) {
				id, scope := uuid.New(), model
				if name == "missing-namespace" {
					scope = "controlled-missing-space-" + uuid.NewString()
				}
				if _, err := repo.pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag) VALUES($1,$2,0,0,0,$3,$4,'MODEL',$2,'CHAT',$5,'MICROCREDIT','DRAFT',999,1)`, id, scope, from, fixed.Add(time.Duration(index+1)*time.Hour), chatFormulaVersionID); err != nil {
					t.Fatal(err)
				}
				if name == "missing-namespace" {
					if _, err := repo.ListManagedPrices(ctx, 1, ""); !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("missing namespace was defaulted or price silently omitted: %v", err)
					}
				} else {
					page, err := repo.ListManagedPrices(ctx, 1, "")
					reference, refErr := repo.ReadManagedPrice(ctx, id)
					if err != nil || refErr != nil || len(page.Items) != 1 || page.Items[0].Units != nil || !reflect.DeepEqual(page.Items[0], reference) {
						t.Fatal("abnormal nil-unit shape was rewritten", err, refErr)
					}
				}
			})
		}
	})
}
