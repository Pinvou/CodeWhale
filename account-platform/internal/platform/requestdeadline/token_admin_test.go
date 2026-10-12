package requestdeadline

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestTokenAdminDeadlineChainsAreFrozen(t *testing.T) {
	for _, item := range []struct {
		class                  Class
		business, bff, ingress time.Duration
	}{{SnapshotBuild, 10 * time.Second, 12 * time.Second, 14 * time.Second}, {SnapshotRead, 3 * time.Second, 5 * time.Second, 7 * time.Second}, {Precheck, 5 * time.Second, 7 * time.Second, 9 * time.Second}, {OverviewRead, 3 * time.Second, 5 * time.Second, 7 * time.Second}} {
		budget, ok := BudgetFor(item.class)
		if !ok || budget.Business != item.business || budget.BFF != item.bff || budget.Ingress != item.ingress {
			t.Fatalf("%s budget = %+v", item.class, budget)
		}
		start := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
		contract, err := Start(start, item.class)
		if err != nil || contract.BusinessDeadline.Sub(start) != item.business || contract.BFFDeadline.Sub(start) != item.bff || contract.IngressDeadline.Sub(start) != item.ingress {
			t.Fatalf("%s contract = %+v, %v", item.class, contract, err)
		}
	}
}

func TestTokenAdminDeadlineClassificationIsMutuallyExclusive(t *testing.T) {
	for _, item := range []struct {
		method, path, cursor string
		want                 Class
		ok                   bool
	}{
		{http.MethodGet, "/admin/v1/models", "", SnapshotBuild, true},
		{http.MethodGet, "/admin/v1/models", "signed", SnapshotRead, true},
		{http.MethodGet, "/admin/v1/models/impact", "signed", Precheck, true},
		{http.MethodGet, "/admin/v1/overviews/model-catalog", "", OverviewRead, true},
		{http.MethodHead, "/admin/v1/overviews/token-resources", "", OverviewRead, true},
		{http.MethodGet, "/admin/v1/overviews/model-operations", "signed", OverviewRead, true},
		{http.MethodGet, "/admin/v1/overviews/unknown", "", SnapshotBuild, true},
		{http.MethodPost, "/admin/v1/overviews/model-catalog", "", "", false},
		{http.MethodPost, "/admin/v1/models", "", "", false},
		{http.MethodGet, "/v1/models", "", "", false},
	} {
		got, ok := Classify(item.method, item.path, item.cursor)
		if got != item.want || ok != item.ok {
			t.Fatalf("Classify(%s,%s,%s)=(%s,%v), want (%s,%v)", item.method, item.path, item.cursor, got, ok, item.want, item.ok)
		}
	}
}

func TestOverviewTimeoutProblemIsExactForTokenAndPortalPaths(t *testing.T) {
	for _, item := range []struct {
		path string
		code string
	}{
		{"/admin/v1/overviews/model-catalog", "MODEL_CATALOG_OVERVIEW_TIMEOUT"},
		{"/api/admin/token/admin/v1/overviews/token-resources", "TOKEN_RESOURCES_OVERVIEW_TIMEOUT"},
		{"/admin/v1/overviews/model-operations", "MODEL_OPERATIONS_OVERVIEW_TIMEOUT"},
	} {
		code, _, ok := OverviewTimeoutProblem(item.path)
		if !ok || code != item.code {
			t.Fatalf("OverviewTimeoutProblem(%q)=(%q,%v), want %q", item.path, code, ok, item.code)
		}
	}
	if _, _, ok := OverviewTimeoutProblem("/admin/v1/overviews/model-catalog/extra"); ok {
		t.Fatal("non-canonical overview path received a stable route error")
	}
}

func TestConsumeBusinessDeadlineNeverResetsOrEnlarges(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	for name, supplied := range map[string]time.Time{
		"earlier":      now.Add(2 * time.Second),
		"forged-later": now.Add(time.Hour),
	} {
		header := http.Header{}
		header.Set(DeadlineClassHeader, string(SnapshotRead))
		header.Set(AbsoluteDeadlineHeader, supplied.Format(time.RFC3339Nano))
		ctx, cancel, err := ConsumeBusinessDeadline(context.Background(), header, now, SnapshotRead)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		deadline, _ := ctx.Deadline()
		cancel()
		want := supplied
		if want.After(now.Add(3 * time.Second)) {
			want = now.Add(3 * time.Second)
		}
		if !deadline.Equal(want) {
			t.Fatalf("%s deadline=%s want=%s", name, deadline, want)
		}
	}
	parent, parentCancel := context.WithDeadline(context.Background(), now.Add(time.Second))
	defer parentCancel()
	ctx, cancel, err := ConsumeBusinessDeadline(parent, http.Header{}, now, SnapshotBuild)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	deadline, _ := ctx.Deadline()
	if !deadline.Equal(now.Add(time.Second)) {
		t.Fatalf("parent deadline was reset: %s", deadline)
	}
}
