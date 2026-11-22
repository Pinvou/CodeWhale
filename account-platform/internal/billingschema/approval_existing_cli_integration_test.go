package billingschema

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"
)

// Run the actual executable, including its environment readers, file writer,
// control comparisons and recovery flags. Broker/SQL setup identities exist
// only inside the explicitly owned PostgreSQL and TLS NATS fixture.
func TestExistingBrokerCLIActualPGTLSNATS(t *testing.T) {
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
		t.Fatal("actual Account schema failed")
	}
	b := existingActualBroker(t, ctx, natsBinary)
	dsn := f.serviceURL(t, db, "account_migrator")
	type result struct {
		Control  account.AdminApprovalControl `json:"control"`
		Resource json.RawMessage              `json:"resource_result"`
	}
	invoke := func(action string, version int64, wantSuccess bool, extra ...string) result {
		t.Helper()
		command := exec.CommandContext(ctx, binary, append([]string{"existing-broker", "-action", action, "-expected-version", strconv.FormatInt(version, 10)}, extra...)...)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "ACCOUNT_PLATFORM_PROFILE=models", "APPROVAL_INITIALIZER_DATABASE_URL=" + dsn,
			"NATS_URL=" + b.url, "NATS_CA_FILE=" + b.cfg.CAFile, "NATS_NKEY_SEED_FILE=" + b.cfg.SeedFile,
			"APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE=" + b.files.Identity.CurrentFile, "APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256=" + b.files.Identity.CurrentSHA256,
			"APPROVAL_RESOURCE_MANIFEST_FILE=" + b.files.ManifestFile, "APPROVAL_RESOURCE_MANIFEST_SHA256=" + b.files.ManifestSHA256,
			"APPROVAL_EXISTING_PLAN_FILE=" + b.files.PlanFile, "APPROVAL_EXISTING_PLAN_SHA256=" + b.files.PlanSHA256,
			"APPROVAL_RESOURCE_RECEIPT_FILE=" + b.files.ReceiptFile, "APPROVAL_RESOURCE_RECEIPT_SHA256=" + b.files.ReceiptSHA256}
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		if (err == nil) != wantSuccess {
			t.Fatalf("actual CLI %s success=%v expected=%v; sanitized stderr=%s", action, err == nil, wantSuccess, stderr.String())
		}
		var got result
		if wantSuccess {
			if json.Unmarshal(stdout.Bytes(), &got) != nil || got.Control.Version <= 0 {
				t.Fatalf("actual CLI %s returned no authoritative result", action)
			}
		} else if stdout.Len() != 0 || stderr.String() != "APPROVAL_EXISTING_BROKER_OPERATION_BLOCKED\n" {
			t.Fatal("CLI failure leaked output or lost stable error")
		}
		return got
	}
	// Controlled audit failure commits no SQL but leaves real broker/file facts.
	// Recovery must use their original identities, never recapture a new plan.
	adminConfig := f.config.Copy()
	adminConfig.Database = db.Config().Database
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal("owned fault setup connection failed")
	}
	defer admin.Close(context.Background())
	failAudit := func(step string) {
		t.Helper()
		_, err := admin.Exec(ctx, `CREATE FUNCTION public.fixture_reject_operator_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='APPROVAL_EXISTING_BROKER_INITIALIZATION' AND NEW.change_summary->>'step'=TG_ARGV[0] THEN RAISE EXCEPTION 'controlled operator audit failure'; END IF; RETURN NEW; END $$`)
		if err != nil {
			t.Fatal("owned audit cutpoint function failed")
		}
		// step is a fixed test action, never external SQL input.
		_, err = admin.Exec(ctx, `CREATE TRIGGER fixture_reject_operator_audit BEFORE INSERT ON public.audit_logs FOR EACH ROW EXECUTE FUNCTION public.fixture_reject_operator_audit('`+step+`')`)
		if err != nil {
			t.Fatal("owned audit cutpoint trigger failed")
		}
	}
	restoreAudit := func() {
		t.Helper()
		if _, err := admin.Exec(ctx, `DROP TRIGGER fixture_reject_operator_audit ON public.audit_logs; DROP FUNCTION public.fixture_reject_operator_audit()`); err != nil {
			t.Fatal("owned cutpoint restoration failed")
		}
	}
	readPlan := func() []byte {
		t.Helper()
		body, e := os.ReadFile(b.files.PlanFile)
		if e != nil {
			t.Fatal("actual immutable plan unavailable")
		}
		b.files.PlanSHA256 = existingPublicHash(body)
		return body
	}
	invoke("status", 1, true)
	// An already supplied plan digest cannot accidentally break unrelated CAS steps.
	b.files.PlanSHA256 = "irrelevant-to-control-step"
	invoke("quiesce", 1, true)
	invoke("fence", 2, true)
	b.files.PlanSHA256 = ""
	b.files.PlanFile = filepath.Join(b.resources, "token-plan.json")
	failAudit("prepare-token-upgrade")
	invoke("prepare-token-upgrade", 3, false, "-operation-id", uuid.NewString())
	tokenPlan := readPlan()
	invoke("inspect-token-upgrade", 3, false) // no committed prepare audit
	restoreAudit()
	invoke("prepare-token-upgrade", 3, true, "-resume")
	if after := readPlan(); !bytes.Equal(tokenPlan, after) {
		t.Fatal("prepare recovery rewrote plan or Q")
	}
	inspection := invoke("inspect-token-upgrade", 3, true)
	var tokenState struct{ Upgraded, Remaining int }
	if json.Unmarshal(inspection.Resource, &tokenState) != nil || tokenState.Upgraded != 0 || tokenState.Remaining != 3 {
		t.Fatal("legacy state cannot be queried")
	}
	stream, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal("owned stream unavailable")
	}
	// A first update took effect but the prior operation did not finish. This
	// is authoritative partial-state readback, not a replacement consumer.
	one, err := stream.Consumer(ctx, "token-model-lifecycle-executor-v1")
	if err != nil {
		t.Fatal("owned durable missing")
	}
	info, err := one.Info(ctx)
	if err != nil {
		t.Fatal("owned original INFO failed")
	}
	configuration := info.Config
	configuration.AckWait = 30 * time.Second
	configuration.MaxAckPending = 64
	configuration.BackOff = nil
	if _, err := stream.UpdateConsumer(ctx, configuration); err != nil {
		t.Fatal("owned partial update setup failed")
	}
	invoke("upgrade-token-consumers", 3, false)
	inspection = invoke("inspect-token-upgrade", 3, true)
	if json.Unmarshal(inspection.Resource, &tokenState) != nil || tokenState.Upgraded != 1 || tokenState.Remaining != 2 {
		t.Fatal("partial update is not queryable")
	}
	invoke("upgrade-token-consumers", 3, true, "-resume")
	invoke("upgrade-token-consumers", 3, false) // even completed effects require explicit recovery
	invoke("check-token-upgrade", 3, true)
	// Billing uses a separate plan after the exact Token transition.
	b.files.PlanFile, b.files.PlanSHA256 = filepath.Join(b.resources, "existing-plan.json"), ""
	invoke("prepare-existing-consumers", 3, true, "-operation-id", uuid.NewString())
	readPlan()
	member := jetstream.ConsumerConfig{Name: "billing-member-lifecycle-v1", Durable: "billing-member-lifecycle-v1", FilterSubject: "account.membership.changed.v1", AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxDeliver: -1, BackOff: []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}}
	if _, err := stream.CreateConsumer(ctx, member); err != nil {
		t.Fatal("owned partial creation setup failed")
	}
	b.files.ReceiptFile = filepath.Join(b.resources, "receipt.json")
	invoke("initialize-consumers", 3, false) // partial writes require explicit recovery
	inspection = invoke("inspect-consumers", 3, true)
	var resourceState struct{ Resources []struct{ Present bool } }
	if json.Unmarshal(inspection.Resource, &resourceState) != nil || len(resourceState.Resources) != 12 {
		t.Fatal("partial creation is not queryable")
	}
	present := 0
	for _, item := range resourceState.Resources {
		if item.Present {
			present++
		}
	}
	if present != 1 {
		t.Fatal("readback or failed unresumed operation changed resources")
	}
	failAudit("initialize-consumers")
	invoke("initialize-consumers", 3, false, "-resume")
	receipt, err := os.ReadFile(b.files.ReceiptFile)
	if err != nil {
		t.Fatal("committed broker work lacks its original file evidence")
	}
	b.files.ReceiptSHA256 = existingPublicHash(receipt)
	if _, err := b.files.Load(time.Now().UTC()); err != nil {
		t.Fatal("actual Billing reader rejected real cutover receipt")
	}
	beforeRecovery := existingAllConsumerInfo(t, ctx, stream)
	restoreAudit()
	invoke("initialize-consumers", 3, true, "-resume")
	b.files.ReceiptSHA256 = ""
	b.files.ReceiptFile = filepath.Join(b.resources, "unresumed-receipt.json")
	invoke("initialize-consumers", 3, false) // all-present is not a fresh invocation
	b.files.ReceiptFile = filepath.Join(b.resources, "receipt.json")
	b.files.ReceiptSHA256 = existingPublicHash(receipt)
	if !bytes.Equal(beforeRecovery, existingAllConsumerInfo(t, ctx, stream)) {
		t.Fatal("receipt/audit recovery mutated known resources")
	}
	var beforeAudit int
	if db.QueryRow(ctx, `SELECT count(*) FROM public.audit_logs`).Scan(&beforeAudit) != nil {
		t.Fatal("audit read failed")
	}
	invoke("verify-consumers", 3, true)
	var afterAudit int
	if db.QueryRow(ctx, `SELECT count(*) FROM public.audit_logs`).Scan(&afterAudit) != nil || afterAudit != beforeAudit {
		t.Fatal("verification wrote an audit record")
	}
	got := invoke("activate", 3, true)
	if got.Control.Mode != account.AdminApprovalModeProofRequired || got.Control.Version != 4 {
		t.Fatal("CLI activation did not use captured control facts")
	}
	invoke("activate", 3, false)
	got = invoke("resume", 4, true)
	if got.Control.AuthorizationRelayState != account.AdminApprovalRelayRunning || got.Control.Version != 5 {
		t.Fatal("CLI resume did not use captured epoch transition")
	}
	invoke("resume", 4, false)
	final, err := stream.Info(ctx)
	if err != nil || final.State.Msgs != 1 || final.State.LastSeq != 1 || final.State.Consumers != 17 {
		t.Fatal("CLI path changed original messages or resource inventory")
	}
}

// Keep only stable identity/config/position facts; timestamps such as last
// activity and response times are not preserved resource state.
func existingAllConsumerInfo(t *testing.T, ctx context.Context, stream jetstream.Stream) []byte {
	t.Helper()
	resources := map[string]any{}
	names := stream.ConsumerNames(ctx)
	for name := range names.Name() {
		c, err := stream.Consumer(ctx, name)
		if err != nil {
			t.Fatal("owned consumer INFO failed")
		}
		i, err := c.Info(ctx)
		if err != nil {
			t.Fatal("owned consumer INFO failed")
		}
		resources[name] = struct {
			Created                                                    time.Time
			Config                                                     jetstream.ConsumerConfig
			DeliveredConsumer, DeliveredStream, AckConsumer, AckStream uint64
		}{i.Created, i.Config, i.Delivered.Consumer, i.Delivered.Stream, i.AckFloor.Consumer, i.AckFloor.Stream}
	}
	if names.Err() != nil {
		t.Fatal("owned inventory failed")
	}
	body, err := json.Marshal(resources)
	if err != nil {
		t.Fatal("owned metadata encode failed")
	}
	return body
}
