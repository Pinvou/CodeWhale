package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"

	"github.com/google/uuid"
)

type unitLifecycleBodyRepository struct {
	Repository
	calls int
}

func (r *unitLifecycleBodyRepository) ApplyOrganizationUnitLifecycle(_ context.Context, org, unit uuid.UUID, _ string, _ int64) (OrganizationUnitDeletionAssessment, error) {
	r.calls++
	return OrganizationUnitDeletionAssessment{OrganizationID: org, UnitID: unit, Ready: true, Blockers: []OrganizationUnitBlocker{}}, nil
}
func TestOrganizationUnitLifecycleRejectsInvalidBodyBeforeRepository(t *testing.T) {
	for _, row := range []struct {
		name, body string
		want       int
	}{
		{"malformed", "{", 400}, {"unknown", `{"status":"DELETED","structure_version":1,"private":"never echo"}`, 400},
		{"trailing", `{"status":"DELETED","structure_version":1} garbage`, 400},
		{"second-json", `{"status":"DELETED","structure_version":1} {}`, 400},
		{"oversized", `{"status":"DELETED","structure_version":1}` + strings.Repeat(" ", 1<<20), 400},
		{"valid", `{"status":"DELETED","structure_version":1}`, 200},
	} {
		t.Run(row.name, func(t *testing.T) {
			repo := &unitLifecycleBodyRepository{}
			service := &Service{repository: repo}
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(row.body))
			req.SetPathValue("organization_id", uuid.NewString())
			req.SetPathValue("unit_id", uuid.NewString())
			rec := httptest.NewRecorder()
			service.organizationUnitDeletionBlockers(rec, req)
			if rec.Code != row.want {
				t.Fatalf("status=%d want=%d", rec.Code, row.want)
			}
			if row.want != 200 {
				var problem struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(rec.Body.Bytes(), &problem) != nil || problem.Code != "INVALID_REQUEST" || repo.calls != 0 || strings.Contains(rec.Body.String(), "private") {
					t.Fatal("invalid input leaked or reached repository")
				}
			} else if repo.calls != 1 {
				t.Fatal("valid request did not reach repository")
			}
		})
	}
}

func TestPostgresOrganizationUnitLifecyclePreservesBlockersAndTerminalState(t *testing.T) {
	ctx := context.Background()
	r := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	org, unit := uuid.New(), uuid.New()
	if _, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 1); !errors.Is(err, ErrConflict) {
		t.Fatal("uninitialized node deleted", err)
	}
	if _, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.SetOrganizationUnitGovernanceBlocker(ctx, org, unit, "OWNED", "fixture", "one", true, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	result, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETION_PENDING", 1)
	if err != nil || result.Ready || len(result.Blockers) != 1 {
		t.Fatal("pending lifecycle lost blocker", err)
	}
	result, err = r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 2)
	if err != nil || result.Ready {
		t.Fatal("blocked node deleted", err)
	}
	if _, err = r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "ACTIVE", 2); !errors.Is(err, ErrConflict) {
		t.Fatal("pending node resurrected", err)
	}
	if err = r.SetOrganizationUnitGovernanceBlocker(ctx, org, unit, "OWNED", "fixture", "one", false, nil, time.Now()); err != nil {
		t.Fatal("blocker clear rejected", err)
	}
	result, err = r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 2)
	if err != nil || !result.Ready {
		t.Fatal("clear node could not delete", err)
	}
	if _, err = r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 2); err != nil {
		t.Fatal("terminal replay rejected", err)
	}
	if _, err = r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "ACTIVE", 3); !errors.Is(err, ErrConflict) {
		t.Fatal("deleted node resurrected", err)
	}
	if err = r.SetOrganizationUnitGovernanceBlocker(ctx, org, unit, "OWNED", "fixture", "late", true, nil, time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatal("deleted node accepted new blocker", err)
	}
	if err = r.SetOrganizationUnitGovernanceBlocker(ctx, org, unit, "OWNED", "fixture", "late", false, nil, time.Now()); err != nil {
		t.Fatal("terminal blocker cleanup rejected", err)
	}
}
