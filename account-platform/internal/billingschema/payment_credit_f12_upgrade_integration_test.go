package billingschema

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Models observed upper boundaries using explicit public carriers; the live
// read-only version/count observation did not identify every deployed checksum.
func TestPaymentCreditF12DeployedCarriersUpgradePostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned Account/Billing instance required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	ac := f.database(t, ctx, "account", "account_service")
	bc := f.database(t, ctx, "billing", "billing_schema_owner")
	var billingOID uint32
	if err := f.admin.QueryRow(ctx, `SELECT oid FROM pg_database WHERE datname=$1`, bc.Config().Database).Scan(&billingOID); err != nil || billingOID == 0 {
		t.Fatal("record owned F12 Billing database identity")
	}
	am, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	bm, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var priorAccount, priorBilling []migrations.Migration
	for _, m := range am {
		if m.Version <= 900014 && m.Version != 95 {
			priorAccount = append(priorAccount, m)
		}
	}
	for _, m := range bm {
		if m.Version <= 900038 {
			priorBilling = append(priorBilling, m)
		}
	}
	if err := Up(ctx, bc, priorBilling); err != nil {
		t.Fatal("public Billing38 baseline", err)
	}
	migrations.Unlock(ctx, bc)
	if _, err := migrations.VerifyApplied(ctx, bc, priorBilling); err != nil {
		t.Fatal(err)
	}
	if err := accountschema.Up(ctx, ac, priorAccount); err != nil {
		t.Fatal("public Account14 baseline", err)
	}
	migrations.Unlock(ctx, ac)
	if _, err := migrations.VerifyApplied(ctx, ac, priorAccount); err != nil {
		t.Fatal(err)
	}
	if stageValue(t, ctx, ac, `SELECT max(version)::text||':'||(to_regclass('public.account_payment_credit_admissions') IS NULL)::text FROM schema_migrations`) != "900014:true" || stageValue(t, ctx, bc, `SELECT max(version)::text||':'||(to_regclass('public.payment_credit_attempts') IS NULL)::text FROM schema_migrations`) != "900038:true" {
		t.Fatal("explicit pre-credit carrier boundary differs")
	}
	root := t.TempDir()
	record := filepath.Join(root, "synthetic-f12-receipt.json")
	child := func(phase string) {
		t.Helper()
		binary := os.Getenv("F12_GO_BINARY")
		if binary == "" {
			var err error
			binary, err = exec.LookPath("go")
			if err != nil {
				t.Fatal(err)
			}
		}
		command := exec.CommandContext(ctx, binary, "test", "-race", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-timeout=180s", "-json", "-run", "^TestF12PaymentCreditUpgradeChildPostgres$", ".")
		command.Dir = "../billing"
		command.Env = append(os.Environ(), "PINVOU_F12_UPGRADE_PHASE="+phase, "PINVOU_F12_UPGRADE_RECORD="+record, "PINVOU_F12_UPGRADE_ACCOUNT_URL="+f.serviceURL(t, ac, "account_service"), "PINVOU_F12_UPGRADE_BILLING_URL="+f.serviceURL(t, bc, "billing_service"))
		var stderr bytes.Buffer
		command.Stderr = &stderr
		out, err := command.Output()
		if err := os.WriteFile(filepath.Join(root, phase+".jsonl"), out, 0600); err != nil {
			t.Fatal("retain child evidence", err)
		}
		if writeErr := os.WriteFile(filepath.Join(root, phase+".stderr"), stderr.Bytes(), 0600); writeErr != nil {
			t.Fatal("retain child stderr", writeErr)
		}
		if err != nil {
			exitCode, category := -1, "start_or_io"
			if failed, ok := err.(*exec.ExitError); ok {
				exitCode, category = failed.ExitCode(), "exit"
			}
			if ctx.Err() == context.Canceled {
				category = "canceled"
			} else if ctx.Err() == context.DeadlineExceeded {
				category = "deadline"
			}
			summary := struct {
				Schema    int                       `json:"schema"`
				Phase     string                    `json:"phase"`
				Root      string                    `json:"root"`
				Package   string                    `json:"package"`
				Class     string                    `json:"class"`
				ExitCode  int                       `json:"exit_code"`
				Locations []f12ChildFailureLocation `json:"locations"`
			}{1, phase, "TestF12PaymentCreditUpgradeChildPostgres", "github.com/Pinvou/pinvou3/account-platform/internal/billing", category, exitCode, f12ChildFailureLocations(out)}
			if encoded, encodeErr := json.Marshal(summary); encodeErr == nil {
				t.Log("PINVOU_F12_UPGRADE_PHASE_FAILURE " + string(encoded))
			}
			t.Fatalf("F12 %s child failed: class=%s exit_code=%d", phase, category, exitCode)
		}
		const pkg = "github.com/Pinvou/pinvou3/account-platform/internal/billing"
		runs, passes, skips, packagePass := 0, 0, 0, false
		for _, line := range splitF12JSONLines(out) {
			var event struct{ Action, Package, Test string }
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatal("invalid F12 child JSON")
			}
			if event.Action == "skip" {
				skips++
			}
			if event.Action == "build-fail" || event.Action == "fail" || event.Action == "skip" {
				t.Fatal("F12 child failure/skip cannot be accepted")
			}
			if event.Test != "" && event.Package != pkg {
				t.Fatal("foreign child package")
			}
			if event.Test == "TestF12PaymentCreditUpgradeChildPostgres" {
				if event.Action == "run" {
					runs++
				}
				if event.Action == "pass" {
					passes++
				}
			}
			if event.Package == pkg && event.Test == "" && event.Action == "pass" {
				packagePass = true
			}
		}
		if runs != 1 || passes != 1 || !packagePass || skips != 0 {
			t.Fatal("F12 child must complete its exact real root/package")
		}
		// Raw child output is checked internally and removed with t.TempDir.
		// Only this closed, success-only protocol survives in the parent log.
		summary := struct {
			Schema      int    `json:"schema"`
			Phase       string `json:"phase"`
			Root        string `json:"root"`
			Package     string `json:"package"`
			Runs        int    `json:"runs"`
			Passes      int    `json:"passes"`
			PackagePass bool   `json:"package_pass"`
			Skips       int    `json:"skips"`
		}{1, phase, "TestF12PaymentCreditUpgradeChildPostgres", pkg, runs, passes, packagePass, skips}
		encoded, err := json.Marshal(summary)
		if err != nil {
			t.Fatal("encode fixed F12 phase result")
		}
		t.Log("PINVOU_F12_UPGRADE_PHASE_RESULT " + string(encoded))
	}
	activateF12ParentFixture(t, ctx, f, bc, billingOID)
	child("historical")
	// Do not cross the Account migration until Billing succeeds and its entire
	// public inventory is verified. The original contexts/roles remain in force.
	if err := Up(ctx, bc, bm); err != nil {
		t.Fatal("complete Billing upgrade", err)
	}
	migrations.Unlock(ctx, bc)
	if _, err := migrations.VerifyApplied(ctx, bc, bm); err != nil {
		t.Fatal(err)
	}
	if err := accountschema.Up(ctx, ac, am); err != nil {
		t.Fatal("complete Account upgrade", err)
	}
	migrations.Unlock(ctx, ac)
	if _, err := migrations.VerifyApplied(ctx, ac, am); err != nil {
		t.Fatal(err)
	}
	child("current")
}

func splitF12JSONLines(raw []byte) [][]byte {
	var rows [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			rows = append(rows, line)
		}
	}
	return rows
}

// Parent owns this exact database through its existing opaque lease. Keep the
// retained_finance-only helper untouched; never admit a new database by prefix.
func activateF12ParentFixture(t *testing.T, ctx context.Context, f *stageFixture, bc *pgx.Conn, expectedOID uint32) {
	t.Helper()
	name := bc.Config().Database
	if f.databaseLeases[name] == nil || f.dbs[name] != "billing_schema_owner" || expectedOID == 0 {
		t.Fatal("unregistered F12 activation database")
	}
	tx, err := bc.Begin(ctx)
	if err != nil {
		t.Fatal("begin owned F12 activation")
	}
	defer tx.Rollback(ctx)
	var database, actor, current, owner string
	var oid uint32
	var super bool
	if err := tx.QueryRow(ctx, `SELECT current_database(),session_user,current_user,oid,pg_get_userbyid(datdba),(SELECT rolsuper FROM pg_roles WHERE rolname=session_user) FROM pg_database WHERE datname=current_database()`).Scan(&database, &actor, &current, &oid, &owner, &super); err != nil || database != name || oid != expectedOID || actor != "billing_migrator" || current != "billing_service" || owner != "billing_schema_owner" || super {
		t.Fatal("owned F12 activation identity changed")
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal("owned F12 activation role unavailable")
	}
	var state string
	var policy, schema, version int64
	err = tx.QueryRow(ctx, `UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1 WHERE command_type='PAYMENT_FINANCIAL_REPAIR' AND policy_version=1 AND snapshot_schema_version=1 AND state<>'ACTIVE' RETURNING state,policy_version,snapshot_schema_version,version`).Scan(&state, &policy, &schema, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT state,policy_version,snapshot_schema_version,version FROM billing_approval_command_activations WHERE command_type='PAYMENT_FINANCIAL_REPAIR'`).Scan(&state, &policy, &schema, &version)
	}
	if err != nil || state != "ACTIVE" || policy != 1 || schema != 1 || version < 1 {
		t.Fatal("owned F12 activation contract differs")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("owned F12 activation did not commit")
	}
}

type f12ChildFailureLocation struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	SQLState string `json:"sqlstate,omitempty"`
}

var f12FailureSource = regexp.MustCompile(`^[ \t]*(payment_credit_f12_upgrade_child_test\.go|payment_lookup_credit_history_fixture_test\.go|payment_repair_upgrade_integration_test\.go|payment_financial_repair_fixture_test\.go):([1-9][0-9]{0,5}):(?:[ \t]|$)`)
var f12FailureSQLState = regexp.MustCompile(`\(SQLSTATE ([0-9A-Z]{5})\)`)

func f12ChildFailureLocations(raw []byte) []f12ChildFailureLocation {
	locations := []f12ChildFailureLocation{}
	seen := map[f12ChildFailureLocation]bool{}
	for _, line := range splitF12JSONLines(raw) {
		if len(line) > 16384 {
			continue
		}
		var event struct{ Action, Package, Test, Output string }
		if json.Unmarshal(line, &event) != nil || event.Action != "output" || event.Package != "github.com/Pinvou/pinvou3/account-platform/internal/billing" || event.Test != "TestF12PaymentCreditUpgradeChildPostgres" || len(event.Output) > 4096 {
			continue
		}
		for _, output := range strings.Split(event.Output, "\n") {
			matched := f12FailureSource.FindStringSubmatch(output)
			if matched == nil {
				continue
			}
			number, err := strconv.Atoi(matched[2])
			if err != nil || number < 1 || number > 999999 {
				continue
			}
			location := f12ChildFailureLocation{File: matched[1], Line: number}
			if state := f12FailureSQLState.FindStringSubmatch(output); state != nil {
				location.SQLState = state[1]
			}
			if !seen[location] {
				seen[location] = true
				locations = append(locations, location)
				if len(locations) == 4 {
					return locations
				}
			}
		}
	}
	return locations
}

func TestF12ChildFailureDiagnosticBoundary(t *testing.T) {
	const secret = "sensitive-sentinel-postgres://private/credential"
	event := func(pkg, test, action, output string) []byte {
		raw, err := json.Marshal(map[string]string{"Package": pkg, "Test": test, "Action": action, "Output": output})
		if err != nil {
			t.Fatal("encode synthetic event")
		}
		return append(raw, '\n')
	}
	const pkg = "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	const root = "TestF12PaymentCreditUpgradeChildPostgres"
	valid := event(pkg, root, "output", "    payment_credit_f12_upgrade_child_test.go:66: "+secret+" (SQLSTATE 42501)\n")
	got := f12ChildFailureLocations(append(append([]byte{}, valid...), valid...))
	if len(got) != 1 || got[0].File != "payment_credit_f12_upgrade_child_test.go" || got[0].Line != 66 || got[0].SQLState != "42501" {
		t.Fatal("valid source location/dedup lost")
	}
	for _, raw := range [][]byte{
		[]byte(secret), []byte(`{"Action":`),
		event("foreign", root, "output", "payment_credit_f12_upgrade_child_test.go:66: "+secret),
		event(pkg, "TestForeign", "output", "payment_credit_f12_upgrade_child_test.go:66: "+secret),
		event(pkg, root, "run", "payment_credit_f12_upgrade_child_test.go:66: "+secret),
		event(pkg, root, "output", "other.go:66: "+secret),
		event(pkg, root, "output", "/private/payment_credit_f12_upgrade_child_test.go:66: "+secret),
		event(pkg, root, "output", "payment_credit_f12_upgrade_child_test.go:0: "+secret),
		event(pkg, root, "output", "payment_credit_f12_upgrade_child_test.go:-1: "+secret),
		event(pkg, root, "output", "payment_credit_f12_upgrade_child_test.go:1000000: "+secret),
		event(pkg, root, "output", "payment_credit_f12_upgrade_child_test.go:66: "+strings.Repeat(secret, 1024)),
	} {
		if len(f12ChildFailureLocations(raw)) != 0 {
			t.Fatal("untrusted failure input became a location")
		}
	}
	for _, state := range []string{"42p01", "4250", "425001", secret} {
		locations := f12ChildFailureLocations(event(pkg, root, "output", "payment_credit_f12_upgrade_child_test.go:66: "+secret+" (SQLSTATE "+state+")"))
		if len(locations) != 1 || locations[0].SQLState != "" {
			t.Fatal("invalid SQLSTATE escaped")
		}
	}
	var many []byte
	for i := 1; i <= 6; i++ {
		many = append(many, event(pkg, root, "output", "payment_credit_f12_upgrade_child_test.go:"+strconv.Itoa(i)+": "+secret)...)
	}
	capped := f12ChildFailureLocations(many)
	encoded, err := json.Marshal(capped)
	if err != nil || len(capped) != 4 || bytes.Contains(encoded, []byte(secret)) || bytes.Contains(encoded, []byte("postgres://")) {
		t.Fatal("failure location cap/redaction failed")
	}
}
