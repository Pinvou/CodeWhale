package account

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestSMSConfigurationPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository, connection, _, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL, 0)
	actor := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "sms-configuration")
	now := time.Now().UTC()
	repository.now = func() time.Time { return now }
	principal := identity.Principal{
		Issuer: actor.Issuer, Subject: actor.Subject, SessionID: "sms-configuration-session", AuthenticatedAt: now.Add(-time.Minute),
	}
	configuration := testSMSConfiguration(0, true, "http-generated")

	created, replayed, err := repository.UpdateSMSConfiguration(ctx, principal, configuration, 0, "enable the managed SMS route", "sms-config-request-0001")
	if err != nil || replayed || created.Revision != 1 {
		t.Fatalf("create revision=%d replayed=%t err=%v", created.Revision, replayed, err)
	}
	replayedConfiguration, replayed, err := repository.UpdateSMSConfiguration(ctx, principal, configuration, 0, "enable the managed SMS route", "sms-config-request-0001")
	if err != nil || !replayed || replayedConfiguration.Revision != created.Revision {
		t.Fatalf("replay revision=%d replayed=%t err=%v", replayedConfiguration.Revision, replayed, err)
	}
	if _, _, err = repository.UpdateSMSConfiguration(ctx, principal, configuration, 0, "enable the managed SMS route", "sms-config-request-0002"); !errors.Is(err, ErrSMSConfigurationConflict) {
		t.Fatalf("CAS error=%v, want %v", err, ErrSMSConfigurationConflict)
	}
	changed := configuration
	changed.Enabled = false
	if _, _, err = repository.UpdateSMSConfiguration(ctx, principal, changed, 0, "disable the managed SMS route", "sms-config-request-0001"); !errors.Is(err, ErrSMSConfigurationKeyReused) {
		t.Fatalf("idempotency reuse error=%v, want %v", err, ErrSMSConfigurationKeyReused)
	}
	stale := principal
	stale.AuthenticatedAt = now.Add(-time.Hour)
	if _, _, err = repository.UpdateSMSConfiguration(ctx, stale, changed, 1, "disable the managed SMS route", "sms-config-request-0003"); !errors.Is(err, ErrRecentAuthenticationRequired) {
		t.Fatalf("recent authentication error=%v, want %v", err, ErrRecentAuthenticationRequired)
	}

	var versions, auditEvents int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM sms_configuration_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_user_id=$1 AND action='account.sms_configuration.update'`, actor.ID).Scan(&auditEvents); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || auditEvents != 1 {
		t.Fatalf("versions=%d audit_events=%d, want one atomic configuration event", versions, auditEvents)
	}

	for name, statement := range map[string]string{
		"update":   `UPDATE sms_configuration_versions SET enabled=FALSE WHERE revision=1`,
		"delete":   `DELETE FROM sms_configuration_versions WHERE revision=1`,
		"truncate": `TRUNCATE sms_configuration_state, sms_configuration_versions`,
	} {
		t.Run("immutable_"+name, func(t *testing.T) {
			if _, mutationErr := connection.Exec(ctx, statement); postgresSQLState(mutationErr) != "55000" {
				t.Fatalf("SQLSTATE=%s err=%v", postgresSQLState(mutationErr), mutationErr)
			}
		})
	}
}
