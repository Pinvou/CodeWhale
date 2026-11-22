package token

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Owned temporary database only. Controlled SELECT permission faults exercise
// real repository scans, not mocked errors or a claim of deployed outages.
func TestF06ModelPolicyStorageFailureKeepsInfrastructureReceipt(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	config := r.pool.Config().ConnConfig.Copy()
	config.User = "token_migrator"
	owner, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err = owner.Exec(ctx, `SET ROLE token_schema_owner`); err != nil {
		t.Fatal(err)
	}
	org, actor := uuid.New(), uuid.New()
	for _, table := range []string{"organization_model_policies", "model_catalog_entries"} {
		t.Run(table, func(t *testing.T) {
			var tableOwner string
			if err := owner.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid=$1::regclass`, table).Scan(&tableOwner); err != nil || tableOwner != "token_schema_owner" {
				t.Fatal("isolated table owner guard", tableOwner, err)
			}
			if _, err := owner.Exec(ctx, "REVOKE SELECT ON "+pgx.Identifier{table}.Sanitize()+" FROM token_gateway"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := owner.Exec(ctx, "GRANT SELECT ON "+pgx.Identifier{table}.Sanitize()+" TO token_gateway"); err != nil {
					t.Error("restore owned permission", err)
				}
			}()
			mode, models := "ALLOW_ALL", []uuid.UUID{}
			if table == "model_catalog_entries" {
				mode = "ALLOW_LIST"
				models = []uuid.UUID{uuid.New()}
			}
			_, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 1, 0, mode, models, uuid.NewString(), "controlled storage failure")
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "42501" || errors.Is(err, ErrModelPolicyVersionConflict) || errors.Is(err, ErrModelUnavailable) {
				t.Fatal("storage fault folded into model domain outcome", err)
			}
			out := httptest.NewRecorder()
			(&Service{}).writeError(out, httptest.NewRequest("PUT", "/v1/organizations/"+org.String()+"/model-policy", nil), err)
			if out.Code != 500 || !strings.Contains(out.Body.String(), "INTERNAL_ERROR") || strings.Contains(out.Body.String(), table) {
				t.Fatal("infrastructure response leaked/falsely rejected", out.Code, out.Body.String())
			}
			var commands, audit int
			if err = owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM organization_model_policy_commands WHERE organization_id=$1),(SELECT count(*) FROM organization_model_policy_audit WHERE organization_id=$1)`, org).Scan(&commands, &audit); err != nil || commands != 0 || audit != 0 {
				t.Fatal("failed policy storage created facts", commands, audit, err)
			}
		})
	}
	t.Run("restored-policy-storage", func(t *testing.T) {
		item, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 1, 0, "ALLOW_ALL", nil, uuid.NewString(), "controlled update")
		if err != nil || item.Version != 1 {
			t.Fatal(item, err)
		}
	})
	t.Run("real-stale-version-remains-domain", func(t *testing.T) {
		_, err := r.SaveOrganizationModelPolicy(ctx, org, actor, 1, 0, "ALLOW_ALL", nil, uuid.NewString(), "controlled stale update")
		if !errors.Is(err, ErrModelPolicyVersionConflict) {
			t.Fatal("actual stale policy lost domain conflict", err)
		}
	})
}
