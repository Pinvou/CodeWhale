package billingschema

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

func TestRefundRecoveryActualCLI(t *testing.T) {
	raw, natsBinary, binary := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY"), os.Getenv("PINVOU_TEST_APPROVAL_OPERATOR_BINARY")
	if raw == "" || natsBinary == "" || binary == "" {
		t.Skip("owned PostgreSQL, TLS NATS and compiled operator required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	db := f.database(t, ctx, "account", "account_service")
	all, err := migrations.Load("../../migrations/account")
	if err != nil || accountschema.Up(ctx, db, all) != nil {
		t.Fatal("actual recovery Account schema failed")
	}
	b := existingActualBrokerRefundFixture(t, ctx, natsBinary, true)
	dsn := f.serviceURL(t, db, "account_migrator")
	originalFile, originalSHA := "", ""
	invoke := func(action string, version int64, wantSuccess bool, extra ...string) json.RawMessage {
		t.Helper()
		command := exec.CommandContext(ctx, binary, append([]string{"existing-broker", "-action", action, "-expected-version", strconv.FormatInt(version, 10)}, extra...)...)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "ACCOUNT_PLATFORM_PROFILE=models", "APPROVAL_INITIALIZER_DATABASE_URL=" + dsn,
			"NATS_URL=" + b.url, "NATS_CA_FILE=" + b.cfg.CAFile, "NATS_NKEY_SEED_FILE=" + b.cfg.SeedFile,
			"APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE=" + b.files.Identity.CurrentFile, "APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256=" + b.files.Identity.CurrentSHA256,
			"APPROVAL_RESOURCE_MANIFEST_FILE=" + b.files.ManifestFile, "APPROVAL_RESOURCE_MANIFEST_SHA256=" + b.files.ManifestSHA256,
			"APPROVAL_EXISTING_PLAN_FILE=" + b.files.PlanFile, "APPROVAL_EXISTING_PLAN_SHA256=" + b.files.PlanSHA256,
			"APPROVAL_RESOURCE_RECEIPT_FILE=" + b.files.ReceiptFile, "APPROVAL_RESOURCE_RECEIPT_SHA256=" + b.files.ReceiptSHA256,
			"APPROVAL_REFUND_RECOVERY_ORIGINAL_PLAN_FILE=" + originalFile, "APPROVAL_REFUND_RECOVERY_ORIGINAL_PLAN_SHA256=" + originalSHA}
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		if (err == nil) != wantSuccess {
			t.Fatalf("actual recovery CLI %s success=%v expected=%v; sanitized stderr=%s", action, err == nil, wantSuccess, stderr.String())
		}
		var got struct {
			Resource json.RawMessage `json:"resource_result"`
		}
		if wantSuccess {
			if json.Unmarshal(stdout.Bytes(), &got) != nil {
				t.Fatal("actual recovery CLI returned invalid result")
			}
		} else if stdout.Len() != 0 || stderr.String() != "APPROVAL_EXISTING_BROKER_OPERATION_BLOCKED\n" {
			t.Fatal("actual recovery failure lost stable sanitized output")
		}
		return got.Resource
	}
	invoke("quiesce", 1, true)
	invoke("fence", 2, true)
	_, err = b.js.Publish(ctx, "account.fixture.retention.v1", []byte(`{"fixture":true}`))
	if err != nil {
		t.Fatal(err)
	}
	b.files.PlanFile = b.resources + "/original-cli-refund-plan.json"
	plan := invoke("prepare-refund-upgrade", 3, true, "-operation-id", uuid.NewString())
	b.files.PlanSHA256 = existingPublicHash(plan)
	originalFile, originalSHA = b.files.PlanFile, b.files.PlanSHA256
	stream, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	before, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.DeleteMsg(ctx, before.State.FirstSeq); err != nil {
		t.Fatal(err)
	}
	invoke("upgrade-refund-consumer", 3, false)
	b.files.PlanFile, b.files.PlanSHA256 = b.resources+"/recovery-cli-refund-plan.json", ""
	plan = invoke("prepare-refund-recovery", 3, true, "-operation-id", uuid.NewString())
	b.files.PlanSHA256 = existingPublicHash(plan)
	invoke("upgrade-refund-recovery", 3, false, "-resume")
	invoke("upgrade-refund-recovery", 3, true)
	invoke("upgrade-refund-recovery", 3, false)
	invoke("check-refund-recovery", 3, true)
	retained, err := os.ReadFile(originalFile)
	if err != nil || existingPublicHash(retained) != originalSHA {
		t.Fatal("recovery changed the original public plan")
	}
}

// DeleteMsg is used only by the owned fixture to create a deterministic
// reduced retained snapshot. Production recovery never deletes a message.
func TestRefundExplicitRecoveryKeepsOldAttemptAndSendsOnlyOnce(t *testing.T) {
	raw, binary := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if raw == "" || binary == "" {
		t.Skip("explicit owned PostgreSQL and NATS fixture required")
	}
	for _, name := range []string{"success", "response-lost-after-update", "new-snapshot-drift"} {
		loseResult, drift := name == "response-lost-after-update", name == "new-snapshot-drift"
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			f := newStageFixture(t, ctx, raw)
			all, err := migrations.Load("../../migrations/account")
			if err != nil {
				t.Fatal(err)
			}
			db := f.database(t, ctx, "account", "account_service")
			if err = accountschema.Up(ctx, db, all); err != nil {
				t.Fatal(err)
			}
			dsn := f.serviceURL(t, db, "account_migrator")
			b := existingActualBrokerRefundFixture(t, ctx, binary, true)
			for _, step := range []struct {
				action  account.FreshApprovalStep
				version int64
			}{{account.FreshApprovalQuiesce, 1}, {account.FreshApprovalFence, 2}} {
				if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, step.version, step.action, "", nil); err != nil {
					t.Fatal(err)
				}
			}
			facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
			_, err = b.js.Publish(ctx, "account.fixture.retention.v1", []byte(`{"fixture":true}`))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundUpgrade, "", func(ctx context.Context, _ account.AdminApprovalControl) (json.RawMessage, error) {
				plan, e := events.PrepareRefundUpgradePlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString())
				if e == nil {
					b.files.PlanFile = existingPublicWrite(t, b.resources, "original-refund-plan.json", plan)
					b.files.PlanSHA256 = existingPublicHash(plan)
				}
				return plan, e
			}); err != nil {
				t.Fatal(err)
			}
			originalFiles := b.files
			earlyCalled := false
			if _, err = account.RunRefundRecoveryApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundRecovery, originalFiles.PlanSHA256, "", func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
				earlyCalled = true
				return []byte(`{}`), nil
			}); err == nil || earlyCalled {
				t.Fatal("recovery was reachable without an original attempt")
			}
			stream, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
			if err != nil {
				t.Fatal(err)
			}
			before, err := stream.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = stream.DeleteMsg(ctx, before.State.FirstSeq); err != nil {
				t.Fatal(err)
			}
			if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefund, originalFiles.PlanSHA256, func(ctx context.Context, _ account.AdminApprovalControl) (json.RawMessage, error) {
				return events.UpgradeLegacyRefundConsumer(ctx, b.url, b.cfg, originalFiles, facts, true, false)
			}); err == nil {
				t.Fatal("original attempt ignored retained snapshot drift")
			}
			called := 0
			if _, err = account.RunRefundRecoveryApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundRecovery, strings.Repeat("0", 64), "", func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
				called++
				return []byte(`{}`), nil
			}); err == nil || called != 0 {
				t.Fatal("unrelated predecessor reached recovery callback")
			}
			if _, err = account.RunRefundRecoveryApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundRecovery, originalFiles.PlanSHA256, "", func(ctx context.Context, _ account.AdminApprovalControl) (json.RawMessage, error) {
				plan, e := events.PrepareRefundRecoveryPlan(ctx, b.url, b.cfg, originalFiles, facts, uuid.NewString())
				if e == nil {
					b.files.PlanFile = existingPublicWrite(t, b.resources, "recovery-refund-plan.json", plan)
					b.files.PlanSHA256 = existingPublicHash(plan)
				}
				return plan, e
			}); err != nil {
				t.Fatal("explicit recovery preparation failed", err)
			}
			if drift {
				if _, err = b.js.Publish(ctx, "account.fixture.new-message.v1", []byte(`{"fixture":true}`)); err != nil {
					t.Fatal(err)
				}
			}
			apply := func(ctx context.Context, _ account.AdminApprovalControl) (json.RawMessage, error) {
				called++
				result, e := events.UpgradeLegacyRefundConsumer(ctx, b.url, b.cfg, b.files, facts, true, false)
				if e == nil && loseResult {
					return nil, events.ErrNATSResources
				}
				return result, e
			}
			_, err = account.RunRefundRecoveryApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefundRecovery, originalFiles.PlanSHA256, b.files.PlanSHA256, apply)
			if (err != nil) != (loseResult || drift) || called != 1 {
				t.Fatal("unexpected recovery result", err, called)
			}
			if _, err = account.RunRefundRecoveryApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefundRecovery, originalFiles.PlanSHA256, b.files.PlanSHA256, apply); err == nil || called != 1 {
				t.Fatal("recovery attempted a second send")
			}
			if _, err = account.RunRefundRecoveryApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundRecovery, originalFiles.PlanSHA256, "", apply); err == nil || called != 1 {
				t.Fatal("new plan bypassed recovery attempt")
			}
			if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefund, originalFiles.PlanSHA256, apply); err == nil || called != 1 {
				t.Fatal("original attempt became retryable")
			}
			if _, err = account.RunRefundRecoveryApprovalStep(ctx, dsn, 3, account.ExistingBrokerCheckRefundRecovery, originalFiles.PlanSHA256, b.files.PlanSHA256, func(ctx context.Context, _ account.AdminApprovalControl) (json.RawMessage, error) {
				return events.UpgradeLegacyRefundConsumer(ctx, b.url, b.cfg, b.files, facts, false, false)
			}); (err != nil) != drift {
				t.Fatal("recovery readback did not honor the new exact snapshot", err)
			}
			if _, err = db.Exec(ctx, `DELETE FROM public.audit_logs WHERE action IN ('security.approval_refund_upgrade_attempt','security.approval_refund_recovery_attempt')`); err == nil {
				t.Fatal("immutable attempt evidence was deletable")
			}
		})
	}
}
