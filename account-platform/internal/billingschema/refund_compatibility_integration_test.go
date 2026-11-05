package billingschema

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

func TestActualHistoricalRefundUpgrade(t *testing.T) {
	raw, binary := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if raw == "" || binary == "" {
		t.Skip("explicit owned PostgreSQL and NATS fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	db := f.database(t, ctx, "account", "account_service")
	if err := accountschema.Up(ctx, db, all); err != nil {
		t.Fatal(err)
	}
	dsn := f.serviceURL(t, db, "account_migrator")
	b := existingActualBrokerRefundFixture(t, ctx, binary, true)
	for _, x := range []struct {
		step    account.FreshApprovalStep
		version int64
	}{{account.FreshApprovalQuiesce, 1}, {account.FreshApprovalFence, 2}} {
		if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, x.version, x.step, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
	broker, err := events.NewNATSBroker(ctx, b.url, "billing-command-executor", b.billingCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	handler := func(context.Context, events.Event) error { return nil }
	if stop, err := broker.ConsumeUntilResolved(ctx, "billing-refund-executor-v1", "billing.refund.requested.v1", handler); err == nil {
		stop()
		t.Fatal("old refund incorrectly starts")
	}
	if _, err := events.PrepareExistingBrokerPlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString(), true); err == nil {
		t.Fatal("old refund accepted as upgraded")
	}
	var plan []byte
	if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundUpgrade, "", func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		var e error
		plan, e = events.PrepareRefundUpgradePlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString())
		if e == nil {
			b.files.PlanFile = existingPublicWrite(t, b.resources, "refund-plan.json", plan)
			b.files.PlanSHA256 = existingPublicHash(plan)
		}
		return plan, e
	}); err != nil {
		t.Fatal("refund preparation failed", err)
	}
	if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefund, b.files.PlanSHA256, func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		return events.UpgradeLegacyRefundConsumer(ctx, b.url, b.cfg, b.files, facts, true, false)
	}); err != nil {
		t.Fatal("refund upgrade failed", err)
	}
	stream, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	refund, err := stream.Consumer(ctx, "billing-refund-executor-v1")
	if err != nil {
		t.Fatal(err)
	}
	info, err := refund.Info(ctx)
	if err != nil || info.Config.MaxDeliver != -1 || info.NumPending != 0 || info.NumAckPending != 0 {
		t.Fatal("refund readback mismatch")
	}
	shared, err := stream.Consumer(ctx, "billing-command-executor-v1")
	if err != nil {
		t.Fatal(err)
	}
	old, err := shared.Info(ctx)
	if err != nil || old.Config.MaxDeliver != 12 {
		t.Fatal("legacy shared consumer changed")
	}
	stop, err := broker.ConsumeUntilResolved(ctx, "billing-refund-executor-v1", "billing.refund.requested.v1", handler)
	if err != nil {
		t.Fatal("upgraded refund cannot start", err)
	}
	stop()
	if _, err = events.PrepareExistingBrokerPlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString(), true); err != nil {
		t.Fatal("token preparation still blocked", err)
	}
	if _, err = events.InspectLegacyRefundConsumer(ctx, b.url, b.cfg, b.files, facts); err != nil {
		t.Fatal("original refund plan readback failed", err)
	}
	prepare := func(token bool, name string) {
		step := account.ExistingBrokerPrepare
		if token {
			step = account.ExistingBrokerPrepareTokenUpgrade
		}
		_, e := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, step, "", func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
			plan, e := events.PrepareExistingBrokerPlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString(), token)
			if e == nil {
				b.files.PlanFile = existingPublicWrite(t, b.resources, name, plan)
				b.files.PlanSHA256 = existingPublicHash(plan)
			}
			return plan, e
		})
		if e != nil {
			t.Fatal("subsequent plan rejected", e)
		}
	}
	prepare(true, "token-plan.json")
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeToken, b.files.PlanSHA256, func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		return events.UpgradeLegacyTokenConsumers(ctx, b.url, b.cfg, b.files, facts, true, false)
	}); err != nil {
		t.Fatal("subsequent Token upgrade", err)
	}
	prepare(false, "billing-plan.json")
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.FreshApprovalResources, b.files.PlanSHA256, func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		receipt, e := events.InitializeExistingBrokerConsumers(ctx, b.url, b.cfg, b.files, facts, true, false)
		if e == nil {
			b.files.ReceiptFile = existingPublicWrite(t, b.resources, "receipt.json", receipt)
			b.files.ReceiptSHA256 = existingPublicHash(receipt)
		}
		return receipt, e
	}); err != nil {
		t.Fatal("subsequent Billing initialization", err)
	}
	if binding, e := b.files.Load(time.Now().UTC()); e != nil || binding == nil {
		t.Fatal("Billing receipt rejected", e)
	}
	for _, x := range []struct {
		step    account.FreshApprovalStep
		version int64
	}{{account.FreshApprovalActivate, 3}, {account.FreshApprovalResume, 4}} {
		if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, x.version, x.step, b.files.PlanSHA256, func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
			got, e := events.CheckExistingBrokerConsumers(ctx, b.url, b.cfg, b.files)
			if e != nil || got != facts {
				return nil, events.ErrNATSResources
			}
			return b.files.VerifiedReceipt(time.Now().UTC())
		}); err != nil {
			t.Fatal("subsequent activation or resume", err)
		}
	}
}

func TestRefundAttemptIsDurableWhenResultUnknown(t *testing.T) {
	raw, binary := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if raw == "" || binary == "" {
		t.Skip("explicit owned PostgreSQL and NATS fixture required")
	}
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
	for _, x := range []struct {
		step    account.FreshApprovalStep
		version int64
	}{{account.FreshApprovalQuiesce, 1}, {account.FreshApprovalFence, 2}} {
		if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, x.version, x.step, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
	called := false
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefund, strings.Repeat("0", 64), func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
		called = true
		return []byte(`{}`), nil
	}); err == nil || called {
		t.Fatal("unapproved plan reached refund callback")
	}
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundUpgrade, "", func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		plan, e := events.PrepareRefundUpgradePlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString())
		if e == nil {
			b.files.PlanFile = existingPublicWrite(t, b.resources, "refund-plan.json", plan)
			b.files.PlanSHA256 = existingPublicHash(plan)
		}
		return plan, e
	}); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	lost := func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
		attempts++
		return nil, events.ErrNATSResources
	}
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefund, b.files.PlanSHA256, lost); err == nil || attempts != 1 {
		t.Fatal("expected unknown after one attempt")
	}
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefund, b.files.PlanSHA256, lost); err == nil || attempts != 1 {
		t.Fatal("ambiguous attempt was retried")
	}
	if _, err = db.Exec(ctx, `DELETE FROM public.audit_logs WHERE action='security.approval_refund_upgrade_attempt'`); err == nil {
		t.Fatal("attempt marker deletion allowed")
	}
	if _, err = db.Exec(ctx, `UPDATE public.audit_logs SET result='FAILED' WHERE action='security.approval_refund_upgrade_attempt'`); err == nil {
		t.Fatal("attempt marker update allowed")
	}

	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundUpgrade, "", lost); err == nil || attempts != 1 {
		t.Fatal("new plan bypassed attempt barrier")
	}
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerInspectRefundUpgrade, b.files.PlanSHA256, func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		return events.InspectLegacyRefundConsumer(ctx, b.url, b.cfg, b.files, facts)
	}); err != nil {
		t.Fatal("read-only recovery blocked", err)
	}
}

func TestRefundPreparationRejectsHistoricalDrift(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit owned NATS fixture required")
	}
	for _, tc := range []struct {
		name, durable string
		mutate        func(*jetstream.ConsumerConfig)
	}{
		{"shared-maxdeliver", "billing-command-executor-v1", func(c *jetstream.ConsumerConfig) { c.MaxDeliver = -1 }},
		{"refund-maxdeliver", "billing-refund-executor-v1", func(c *jetstream.ConsumerConfig) { c.MaxDeliver = 11 }},
		{"token-pending", "token-model-lifecycle-executor-v1", func(c *jetstream.ConsumerConfig) { c.MaxAckPending = 9 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			b := existingActualBrokerRefundFixture(t, ctx, binary, true)
			stream, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
			if err != nil {
				t.Fatal(err)
			}
			c, err := stream.Consumer(ctx, tc.durable)
			if err != nil {
				t.Fatal(err)
			}
			before, err := c.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			changed := before.Config
			tc.mutate(&changed)
			if _, err = stream.UpdateConsumer(ctx, changed); err != nil {
				t.Fatal(err)
			}
			drift, err := c.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
			if _, err = events.PrepareRefundUpgradePlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString()); err == nil {
				t.Fatal("drift accepted")
			}
			after, err := c.Info(ctx)
			if err != nil || after.Created != drift.Created || after.Delivered != drift.Delivered || after.AckFloor != drift.AckFloor || !reflect.DeepEqual(after.Config, drift.Config) {
				t.Fatal("refused preparation mutated consumer")
			}
		})
	}
}

func TestRefundUpgradeNeedsTemporaryOperatorPermission(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit owned NATS fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	b := existingActualBroker(t, ctx, binary)
	stream, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"billing-command-executor-v1", "billing-refund-executor-v1"} {
		c, e := stream.Consumer(ctx, name)
		if e != nil {
			t.Fatal(e)
		}
		info, e := c.Info(ctx)
		if e != nil {
			t.Fatal(e)
		}
		old := info.Config
		old.MaxDeliver = 12
		if _, e = stream.UpdateConsumer(ctx, old); e != nil {
			t.Fatal(e)
		}
	}
	facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
	raw, err := events.PrepareRefundUpgradePlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	b.files.PlanFile = existingPublicWrite(t, b.resources, "refund-denied.json", raw)
	b.files.PlanSHA256 = existingPublicHash(raw)
	c, err := stream.Consumer(ctx, "billing-refund-executor-v1")
	if err != nil {
		t.Fatal(err)
	}
	before, err := c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deadline, cancelUpdate := context.WithTimeout(ctx, 3*time.Second)
	defer cancelUpdate()
	if _, err = events.UpgradeLegacyRefundConsumer(deadline, b.url, b.cfg, b.files, facts, true, false); err == nil {
		t.Fatal("default ACL accepted refund update")
	}
	after, err := c.Info(ctx)
	if err != nil || !reflect.DeepEqual(before.Config, after.Config) || before.Created != after.Created || before.Delivered != after.Delivered || before.AckFloor != after.AckFloor {
		t.Fatal("denied upgrade mutated consumer")
	}
}

func TestConcurrentRefundAttemptsSendAtMostOnce(t *testing.T) {
	raw, binary := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if raw == "" || binary == "" {
		t.Skip("explicit owned PostgreSQL and NATS fixture required")
	}
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
	for _, x := range []struct {
		step    account.FreshApprovalStep
		version int64
	}{{account.FreshApprovalQuiesce, 1}, {account.FreshApprovalFence, 2}} {
		if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, x.version, x.step, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundUpgrade, "", func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		plan, e := events.PrepareRefundUpgradePlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString())
		if e == nil {
			b.files.PlanFile = existingPublicWrite(t, b.resources, "refund-plan.json", plan)
			b.files.PlanSHA256 = existingPublicHash(plan)
		}
		return plan, e
	}); err != nil {
		t.Fatal(err)
	}
	// Hold the first attempt's independent audit insert before commit, then
	// start a second transaction so its snapshot predates that marker.
	if _, err = db.Exec(ctx, `CREATE FUNCTION refund_fixture_hold_attempt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='security.approval_refund_upgrade_attempt' THEN PERFORM pg_advisory_xact_lock(934872615); END IF; RETURN NEW; END $$; CREATE TRIGGER refund_fixture_hold_attempt BEFORE INSERT ON public.audit_logs FOR EACH ROW EXECUTE FUNCTION refund_fixture_hold_attempt()`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `SELECT pg_advisory_lock(934872615)`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(context.Background(), `SELECT pg_advisory_unlock(934872615)`)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("default_transaction_isolation", "repeatable read")
	parsed.RawQuery = query.Encode()
	dsn = parsed.String()
	var ownedDB string
	if err = db.QueryRow(ctx, `SELECT current_database()`).Scan(&ownedDB); err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	result := make(chan error, 2)
	invoke := func() {
		_, e := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefund, b.files.PlanSHA256, func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
			count.Add(1)
			return nil, events.ErrNATSResources
		})
		result <- e
	}
	waiting := func(min int) bool {
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err = f.admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND usename='account_migrator' AND wait_event_type='Lock'`, ownedDB).Scan(&n); err == nil && n >= min {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	go invoke()
	if !waiting(1) {
		t.Fatal("first audit insert did not wait")
	}
	go invoke()
	blocked := waiting(2)
	if _, err = db.Exec(ctx, `SELECT pg_advisory_unlock(934872615)`); err != nil {
		t.Fatal(err)
	}
	e1, e2 := <-result, <-result
	if !blocked || e1 == nil || e2 == nil || count.Load() != 1 {
		t.Fatalf("repeatable-read race: blocked=%v first_succeeded=%v second_succeeded=%v callbacks=%d", blocked, e1 == nil, e2 == nil, count.Load())
	}

}

func TestRefundLostActualUpdateReplyCannotResend(t *testing.T) {
	raw, binary := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if raw == "" || binary == "" {
		t.Skip("explicit owned PostgreSQL and NATS fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
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
	for _, x := range []struct {
		step    account.FreshApprovalStep
		version int64
	}{{account.FreshApprovalQuiesce, 1}, {account.FreshApprovalFence, 2}} {
		if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, x.version, x.step, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerPrepareRefundUpgrade, "", func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		plan, e := events.PrepareRefundUpgradePlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString())
		if e == nil {
			b.files.PlanFile = existingPublicWrite(t, b.resources, "refund-plan.json", plan)
			b.files.PlanSHA256 = existingPublicHash(plan)
		}
		return plan, e
	}); err != nil {
		t.Fatal(err)
	}
	proxy, dropped, requests := existingDropConsumerReply(t, ctx, b, "billing-refund-executor-v1", false)
	_, _ = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefund, b.files.PlanSHA256, func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		short, stop := context.WithTimeout(ctx, 8*time.Second)
		defer stop()
		return events.UpgradeLegacyRefundConsumer(short, proxy, b.cfg, b.files, facts, true, false)
	})
	select {
	case <-dropped:
	case <-ctx.Done():
		t.Fatal("actual response was not dropped")
	}
	if requests.Load() != 1 {
		t.Fatal("unexpected actual update request count")
	}
	repeated := false
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeRefund, b.files.PlanSHA256, func(context.Context, account.AdminApprovalControl) (json.RawMessage, error) {
		repeated = true
		return []byte(`{}`), nil
	}); err == nil || repeated || requests.Load() != 1 {
		t.Fatal("unknown response allowed resend")
	}
	if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerInspectRefundUpgrade, b.files.PlanSHA256, func(ctx context.Context, c account.AdminApprovalControl) (json.RawMessage, error) {
		return events.InspectLegacyRefundConsumer(ctx, b.url, b.cfg, b.files, facts)
	}); err != nil {
		t.Fatal("original plan readback failed", err)
	}
}

func TestRefundCLIUnknownACKReadback(t *testing.T) {
	raw, nb, operator := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY"), os.Getenv("PINVOU_TEST_APPROVAL_OPERATOR_BINARY")
	if raw == "" || nb == "" || operator == "" {
		t.Skip("owned PostgreSQL, TLS NATS and compiled operator required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
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
	b := existingActualBrokerRefundFixture(t, ctx, nb, true)
	for _, x := range []struct {
		step    account.FreshApprovalStep
		version int64
	}{{account.FreshApprovalQuiesce, 1}, {account.FreshApprovalFence, 2}} {
		if _, err = account.RunExistingBrokerApprovalStep(ctx, dsn, x.version, x.step, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	b.files.PlanFile = filepath.Join(b.resources, "cli-refund-plan.json")
	if out, err := existingFaultCommand(ctx, operator, b, dsn, "prepare-refund-upgrade", "-operation-id", uuid.NewString()).CombinedOutput(); err != nil {
		t.Fatalf("CLI prepare failed: %s", out)
	}
	plan, err := os.ReadFile(b.files.PlanFile)
	if err != nil {
		t.Fatal(err)
	}
	b.files.PlanSHA256 = existingPublicHash(plan)
	proxy, dropped, requests := existingDropConsumerReply(t, ctx, b, "billing-refund-executor-v1", false)
	through := b
	through.url = proxy
	opctx, stop := context.WithCancel(ctx)
	command := existingFaultCommand(opctx, operator, through, dsn, "upgrade-refund-consumer")
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-dropped:
	case <-time.After(12 * time.Second):
		stop()
		<-done
		t.Fatal("update reply not intercepted")
	}
	stop()
	<-done
	if requests.Load() != 1 {
		t.Fatal("actual CLI sent unexpected number of updates")
	}
	if err = existingFaultCommand(ctx, operator, through, dsn, "upgrade-refund-consumer").Run(); err == nil || requests.Load() != 1 {
		t.Fatal("CLI resent after unknown result")
	}
	out, err := existingFaultCommand(ctx, operator, b, dsn, "inspect-refund-upgrade").CombinedOutput()
	if err != nil {
		t.Fatalf("CLI inspection failed: %s", out)
	}
	var inspection struct {
		ResourceResult struct {
			Schema     string `json:"schema"`
			PlanSHA256 string `json:"plan_sha256"`
			Upgraded   int    `json:"upgraded"`
			Remaining  int    `json:"remaining"`
		} `json:"resource_result"`
	}
	if json.Unmarshal(out, &inspection) != nil || inspection.ResourceResult.Schema != "approval-refund-consumer-upgrade-inspection-v1" || inspection.ResourceResult.PlanSHA256 != b.files.PlanSHA256 || inspection.ResourceResult.Upgraded != 1 || inspection.ResourceResult.Remaining != 0 {
		t.Fatal("CLI inspection did not bind original plan and classify the applied update")
	}
}

func TestRefundPreparationRejectsPendingOrAckPending(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit owned NATS fixture required")
	}
	for _, take := range []bool{false, true} {
		name := "pending"
		if take {
			name = "ack-pending"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			b := existingActualBrokerRefundFixture(t, ctx, binary, true)
			if _, err := b.js.Publish(ctx, "billing.refund.requested.v1", []byte(`{"fixture":true}`)); err != nil {
				t.Fatal(err)
			}
			stream, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
			if err != nil {
				t.Fatal(err)
			}
			c, err := stream.Consumer(ctx, "billing-refund-executor-v1")
			if err != nil {
				t.Fatal(err)
			}
			if take {
				batch, e := c.Fetch(1, jetstream.FetchMaxWait(time.Second))
				if e != nil {
					t.Fatal(e)
				}
				if _, ok := <-batch.Messages(); !ok {
					t.Fatal("fixture did not deliver message")
				}
			}
			info, err := c.Info(ctx)
			if err != nil || (!take && info.NumPending != 1) || (take && info.NumAckPending != 1) {
				t.Fatal("fixture did not establish expected pending state")
			}
			before := existingAllConsumerInfo(t, ctx, stream)
			streamBefore, err := stream.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
			if _, err = events.PrepareRefundUpgradePlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString()); err == nil {
				t.Fatal("pending refund accepted")
			}
			after := existingAllConsumerInfo(t, ctx, stream)
			streamAfter, err := stream.Info(ctx)
			if err != nil || !bytes.Equal(before, after) || streamBefore.State.Msgs != streamAfter.State.Msgs || streamBefore.State.Bytes != streamAfter.State.Bytes || streamBefore.State.FirstSeq != streamAfter.State.FirstSeq || streamBefore.State.LastSeq != streamAfter.State.LastSeq {
				t.Fatal("rejected plan changed resources or messages")
			}
		})
	}
}

func TestRefundTemporaryPermissionReload(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit owned NATS fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	b := existingActualBrokerRefundFixture(t, ctx, binary, true)
	connect := func(cfg events.NATSConfig, inbox string) *nats.Conn {
		seed, e := os.ReadFile(cfg.SeedFile)
		if e != nil {
			t.Fatal(e)
		}
		pair, e := nkeys.FromSeed(seed)
		clear(seed)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(pair.Wipe)
		public, e := pair.PublicKey()
		if e != nil {
			t.Fatal(e)
		}
		ca, e := os.ReadFile(cfg.CAFile)
		if e != nil {
			t.Fatal(e)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			t.Fatal("invalid fixture CA")
		}
		c, e := nats.Connect(b.url, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}), nats.Nkey(public, pair.Sign), nats.CustomInboxPrefix(inbox), nats.Timeout(time.Second), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(c.Close)
		return c
	}
	op := connect(b.cfg, "_INBOX.event-resource-operator")
	js, err := jetstream.New(op)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	c, err := admin.Consumer(ctx, "billing-refund-executor-v1")
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	desired := info.Config
	desired.MaxDeliver = -1
	if _, err = stream.UpdateConsumer(ctx, desired); err != nil {
		t.Fatal("temporary permission did not allow exact update", err)
	}
	deny := func(s jetstream.Stream, cfg jetstream.ConsumerConfig) {
		t.Helper()
		short, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		if _, e := s.UpdateConsumer(short, cfg); e == nil {
			t.Fatal("unexpected update authority")
		}
	}
	other, err := admin.Consumer(ctx, "billing-command-executor-v1")
	if err != nil {
		t.Fatal(err)
	}
	oi, err := other.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deny(stream, oi.Config)
	altered := desired
	altered.FilterSubject = "billing.fixture.other.v1"
	deny(stream, altered)
	billingConn := connect(b.billingCfg, "_INBOX.billing-command-executor")
	billingJS, err := jetstream.New(billingConn)
	if err != nil {
		t.Fatal(err)
	}
	billingStream, err := billingJS.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	deny(billingStream, desired)
	_, base, err := b.files.Identity.ExistingBrokerAuthorizationArtifacts(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	raw, err := os.ReadFile(b.configPath)
	if err != nil || json.Unmarshal(raw, &config) != nil {
		t.Fatal("fixture config invalid")
	}
	var baseline map[string]any
	if json.Unmarshal(base.Configuration, &baseline) != nil {
		t.Fatal("baseline invalid")
	}
	currentAuth := config["authorization"].(map[string]any)
	currentUsers := currentAuth["users"].([]any)
	baseUsers := baseline["authorization"].(map[string]any)["users"].([]any)
	// Preserve the fixture observer, which is not part of either production projection.
	currentAuth["users"] = append(baseUsers, currentUsers[len(currentUsers)-1])
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(config) != nil || os.WriteFile(b.configPath, output.Bytes(), 0600) != nil {
		t.Fatal("fixture ACL rewrite failed")
	}
	reload := exec.CommandContext(ctx, binary, "--signal", fmt.Sprintf("reload=%d", b.serverPID))
	if err = reload.Run(); err != nil {
		t.Fatal("fixture reload failed", err)
	}
	time.Sleep(250 * time.Millisecond)
	deny(stream, desired)
	fresh := connect(b.cfg, "_INBOX.event-resource-operator")
	freshJS, err := jetstream.New(fresh)
	if err != nil {
		t.Fatal(err)
	}
	freshStream, err := freshJS.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal(err)
	}
	deny(freshStream, desired)
}
