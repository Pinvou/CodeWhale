package account

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type compensationErrorIdentity struct {
	IdentityProvisioner
	disables int
	after    func()
}

func (directory *compensationErrorIdentity) DisableUserAndConfirm(context.Context, string) error {
	directory.disables++
	if directory.after != nil {
		directory.after()
	}
	return nil
}

func TestRegistrationCompensationDatabaseErrorIdentityPostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	for _, stage := range []string{"operation-read", "absence-proof-read", "completion-write"} {
		t.Run(stage, func(t *testing.T) {
			t.Setenv("APP_ENV", "test")
			r := f.repository(t)
			ctx := context.Background()
			principal, _, _ := userCodeActor(t, r, "compensation-operator")
			now := time.Now().UTC().Truncate(time.Microsecond)
			clock := now.Add(-25 * time.Hour)
			r.now = func() time.Time { return clock }
			id := inspectionRegistrationReady(t, r, "compensationcandidate", "+8613800138010")
			if err := r.MarkRegistrationIdentityCreated(ctx, id, "controlled-compensation-identity"); err != nil {
				t.Fatal(err)
			}
			clock = now
			if err := r.ScheduleRegistrationRetry(ctx, id, "ACCOUNT_COMMIT_FAILED"); err != nil {
				t.Fatal(err)
			}
			cfg := f.config.Copy()
			cfg.Database = r.pool.Config().ConnConfig.Database
			cfg.User, cfg.Password = "account_migrator", ""
			ddl, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ddl.Close(context.Background()) })
			if _, err = ddl.Exec(ctx, "SET ROLE account_service"); err != nil {
				t.Fatal(err)
			}
			exec := func(query string) {
				t.Helper()
				if _, err := ddl.Exec(ctx, query); err != nil {
					t.Fatal("owned fixture DDL failed", err)
				}
			}
			snapshot := func() string {
				t.Helper()
				var row string
				if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(ro)::text FROM registration_operations ro WHERE id=$1`, id).Scan(&row); err != nil {
					t.Fatal(err)
				}
				return row
			}
			directory := &compensationErrorIdentity{}
			var prepared string
			var restore func()
			directory.after = func() {
				prepared = snapshot() // Actual Prepare has committed; only Complete must roll back.
				switch stage {
				case "operation-read":
					exec("ALTER TABLE registration_operations RENAME TO owned_compensation_operations")
					restore = func() { exec("ALTER TABLE owned_compensation_operations RENAME TO registration_operations") }
				case "absence-proof-read":
					exec("ALTER TABLE wallet_provisioning_jobs RENAME TO owned_compensation_wallet_jobs")
					restore = func() { exec("ALTER TABLE owned_compensation_wallet_jobs RENAME TO wallet_provisioning_jobs") }
				case "completion-write":
					exec(`CREATE FUNCTION owned_compensation_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='COMPENSATED' THEN RAISE EXCEPTION 'controlled completion store failure' USING ERRCODE='40001'; END IF; RETURN NEW; END $$;
CREATE TRIGGER owned_compensation_failure BEFORE UPDATE ON registration_operations FOR EACH ROW EXECUTE FUNCTION owned_compensation_failure()`)
					restore = func() {
						exec("DROP TRIGGER owned_compensation_failure ON registration_operations; DROP FUNCTION owned_compensation_failure()")
					}
				}
			}
			t.Cleanup(func() {
				if restore != nil {
					restore()
				}
			})
			auth := newMobileAuthForTest(t, r, &smsProviderFake{}, directory)
			h := NewService(r, nil).WithMobileAuth(auth, "controlled-compensation-service").Handler(func(_ context.Context, credential string) (identity.Principal, error) {
				if credential != "operator" {
					return identity.Principal{}, errors.New("untrusted controlled identity")
				}
				return principal, nil
			})
			path := "/admin/v1/registration-operations/" + id.String() + "/actions/compensate"
			response := inspectionHTTP(h, "operator", http.MethodPost, path)
			if restore == nil {
				t.Fatal("test did not reach post-identity-disabling completion")
			}
			undo := restore
			restore = nil
			undo()
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "INTERNAL_ERROR") {
				t.Fatalf("database failure was folded into a business rejection: status=%d", response.Code)
			}
			for _, detail := range []string{"owned_compensation", "controlled completion store failure", "42P01", "40001"} {
				if strings.Contains(response.Body.String(), detail) {
					t.Fatal("database detail leaked")
				}
			}
			if snapshot() != prepared || directory.disables != 1 {
				t.Fatal("failed completion changed the prepared operation or repeated identity effect")
			}
			directory.after = nil
			response = inspectionHTTP(h, "operator", http.MethodPost, path)
			op, err := r.RegistrationOperation(ctx, id)
			if response.Code != http.StatusNoContent || err != nil || op.Status != "COMPENSATED" || directory.disables != 2 {
				t.Fatal("restored original operation did not recover", response.Code, err)
			}
		})
	}
	t.Run("missing-completion-remains-business-rejection", func(t *testing.T) {
		r := f.repository(t)
		principal, _, _ := userCodeActor(t, r, "compensation-missing")
		var before, after int
		if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := r.CompleteRegistrationCompensation(context.Background(), principal, uuid.New()); !errors.Is(err, ErrRegistrationCompensationForbidden) {
			t.Fatal("missing completion changed its existing business rejection", err)
		}
		if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs`).Scan(&after); err != nil || after != before {
			t.Fatal("missing completion wrote a successful audit", err)
		}
	})
	t.Run("noncompensable-registration-remains-rejected", func(t *testing.T) {
		r := f.repository(t)
		principal, _, _ := userCodeActor(t, r, "compensation-proof")
		id := inspectionRegistrationReady(t, r, "recentcompensation", "+8613800138011")
		if err := r.MarkRegistrationIdentityCreated(context.Background(), id, "controlled-too-recent-identity"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.PrepareRegistrationCompensation(context.Background(), principal, id); !errors.Is(err, ErrRegistrationCompensationForbidden) {
			t.Fatal("ineligible registration bypassed existing compensation proof gate", err)
		}
		t.Setenv("APP_ENV", "test")
		directory := &compensationErrorIdentity{}
		auth := newMobileAuthForTest(t, r, &smsProviderFake{}, directory)
		h := NewService(r, nil).WithMobileAuth(auth, "controlled-compensation-service").Handler(func(_ context.Context, credential string) (identity.Principal, error) {
			if credential != "operator" {
				return identity.Principal{}, errors.New("untrusted controlled identity")
			}
			return principal, nil
		})
		response := inspectionHTTP(h, "operator", http.MethodPost, "/admin/v1/registration-operations/"+id.String()+"/actions/compensate")
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "REGISTRATION_COMPENSATION_FORBIDDEN") || directory.disables != 0 {
			t.Fatal("business proof rejection lost its HTTP identity or reached the external effect", response.Code)
		}
	})
}
