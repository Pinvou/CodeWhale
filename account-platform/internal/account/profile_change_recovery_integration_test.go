package account

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestProfileChangeSupersededStatusMigrationPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	_, connection, _, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL, 80)
	loaded, err := loadAccountIntegrationMigrations(t)
	if err != nil {
		t.Fatal(err)
	}
	through := func(maximum int64) []migrations.Migration {
		items := make([]migrations.Migration, 0, len(loaded))
		for _, migration := range loaded {
			if migration.Version <= maximum {
				items = append(items, migration)
			}
		}
		return items
	}
	constraint := func(name string) (exists, validated bool) {
		t.Helper()
		err := connection.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conrelid='profile_change_requests'::regclass AND conname=$1`, name).Scan(&validated)
		if err == nil {
			return true, validated
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		return false, false
	}
	apply := func(maximum int64) {
		t.Helper()
		if err := migrations.Up(ctx, connection, through(maximum)); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, connection)
	}

	apply(81)
	if exists, validated := constraint("profile_change_requests_status_v2_check"); !exists || validated {
		t.Fatalf("migration 81 constraint exists=%t validated=%t", exists, validated)
	}
	apply(82)
	if exists, validated := constraint("profile_change_requests_status_v2_check"); !exists || !validated {
		t.Fatalf("migration 82 constraint exists=%t validated=%t", exists, validated)
	}
	if err := migrations.DownOne(ctx, connection, through(82)); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	if exists, validated := constraint("profile_change_requests_status_v2_check"); !exists || validated {
		t.Fatalf("migration 82 down constraint exists=%t validated=%t", exists, validated)
	}
	apply(82)
	if exists, validated := constraint("profile_change_requests_status_v2_check"); !exists || !validated {
		t.Fatalf("migration 82 reapply constraint exists=%t validated=%t", exists, validated)
	}
	apply(83)
	if exists, _ := constraint("profile_change_requests_status_v2_check"); exists {
		t.Fatal("migration 83 left the v2 constraint name behind")
	}
	if exists, validated := constraint("profile_change_requests_status_check"); !exists || !validated {
		t.Fatalf("migration 83 active constraint exists=%t validated=%t", exists, validated)
	}
	apply(84)
	var leaseColumns int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='sms_challenges' AND column_name IN ('delivery_lease_id','delivery_lease_until','delivery_started_at','delivery_uncertain_at')`).Scan(&leaseColumns); err != nil || leaseColumns != 4 {
		t.Fatalf("migration 84 lease columns=%d err=%v", leaseColumns, err)
	}
	if err := migrations.DownOne(ctx, connection, through(84)); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='sms_challenges' AND column_name IN ('delivery_lease_id','delivery_lease_until','delivery_started_at','delivery_uncertain_at')`).Scan(&leaseColumns); err != nil || leaseColumns != 0 {
		t.Fatalf("migration 84 down lease columns=%d err=%v", leaseColumns, err)
	}
	apply(84)
	if err := migrations.DownOne(ctx, connection, through(84)); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	if err := migrations.DownOne(ctx, connection, through(83)); postgresSQLState(err) != "55000" {
		t.Fatalf("migration 83 down SQLSTATE=%s err=%v", postgresSQLState(err), err)
	}
	migrations.Unlock(ctx, connection)
}

func TestProfileChangeCreateRecoversOrSafelySupersedesPendingMobileRequestPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository, _, _, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL, 0)
	actor := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PERSONAL_USER", "profile-change-recovery")
	principal := identity.Principal{Issuer: actor.Issuer, Subject: actor.Subject, SessionID: "profile-change-recovery-session"}

	first, err := repository.CreateProfileChange(ctx, principal, "MOBILE", "+8613800138001")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := repository.CreateProfileChange(ctx, principal, "MOBILE", "+8613800138001")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != first.ID || recovered.Status != "PENDING" {
		t.Fatalf("same target created a second request: first=%s recovered=%s status=%s", first.ID, recovered.ID, recovered.Status)
	}

	replacement, err := repository.CreateProfileChange(ctx, principal, "MOBILE", "+8613800138002")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == first.ID {
		t.Fatal("different unverified target must create a distinct request")
	}
	old, err := repository.ProfileChange(ctx, first.ID, &actor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != "SUPERSEDED" || old.LastErrorCode != "REPLACED_BEFORE_VERIFICATION" {
		t.Fatalf("superseded request status=%s reason=%s", old.Status, old.LastErrorCode)
	}
	var supersededAudits int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='account.profile_change.superseded' AND resource_id=$1`, first.ID.String()).Scan(&supersededAudits); err != nil {
		t.Fatal(err)
	}
	if supersededAudits != 1 {
		t.Fatalf("superseded audit count=%d, want 1", supersededAudits)
	}
	otherActor := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PERSONAL_USER", "profile-change-recovery-other")
	otherPrincipal := identity.Principal{Issuer: otherActor.Issuer, Subject: otherActor.Subject, SessionID: "profile-change-recovery-other-session"}
	otherChange, err := repository.CreateProfileChange(ctx, otherPrincipal, "MOBILE", "+8613800138003")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateProfileChange(ctx, principal, "MOBILE", "+8613800138003"); !errors.Is(err, ErrProfileChangeConflict) {
		t.Fatalf("cross-user target reservation error=%v, want profile conflict", err)
	}
	stillActive, err := repository.ProfileChange(ctx, replacement.ID, &actor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillActive.Status != "PENDING" || stillActive.NewValue != replacement.NewValue {
		t.Fatalf("unique conflict did not roll back supersession: %#v", stillActive)
	}

	uncertainCaptchaID := uuid.New()
	now := time.Now().UTC()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) VALUES($1,$2,'PROFILE_MOBILE',100,'CREATED',$3,$4,$4)`, uncertainCaptchaID, otherChange.NewValue, now.Add(5*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	uncertainCandidate := SMSChallenge{ID: uuid.New(), Mobile: otherChange.NewValue, Purpose: SMSPurposeProfileMobile, ExpiresAt: now.Add(5 * time.Minute), Source: "uncertain-source", Device: "uncertain-device", Provider: SMSProviderDescriptor{ID: "integration", AdapterVersion: "v1"}, VerificationData: []byte("verification")}
	uncertainChallenge, err := repository.ReserveProfileSMSChallenge(ctx, otherChange.ID, otherActor.ID, uncertainCaptchaID, 100, uncertainCandidate)
	if err != nil {
		t.Fatal(err)
	}
	uncertainLease, claimed, err := repository.ClaimProfileSMSDelivery(ctx, otherChange.ID, otherActor.ID, uncertainChallenge.ID, false)
	if err != nil || !claimed {
		t.Fatalf("at-most-once claim lease=%s claimed=%t err=%v", uncertainLease, claimed, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE profile_change_requests SET status='CANCELLED_ACCOUNT_CLOSURE' WHERE id=$1`, otherChange.ID); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkProfileSMSDeliverySent(ctx, otherChange.ID, uncertainChallenge.ID, uncertainLease, SMSDelivery{MessageID: "late-after-cancel"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal request accepted late delivery owner: %v", err)
	}
	var lateProviderMessageID *string
	if err := repository.pool.QueryRow(ctx, `SELECT provider_message_id FROM sms_challenges WHERE id=$1`, uncertainChallenge.ID).Scan(&lateProviderMessageID); err != nil || lateProviderMessageID != nil {
		t.Fatalf("late owner restored provider identifier=%v err=%v", lateProviderMessageID, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE profile_change_requests SET status='PENDING' WHERE id=$1`, otherChange.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE sms_challenges SET delivery_lease_until=$2 WHERE id=$1`, uncertainChallenge.ID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := repository.ClaimProfileSMSDelivery(ctx, otherChange.ID, otherActor.ID, uncertainChallenge.ID, false); err != nil || claimed {
		t.Fatalf("at-most-once expired lease was reclaimed: claimed=%t err=%v", claimed, err)
	}
	if err := repository.MarkProfileSMSDeliveryUncertain(ctx, otherChange.ID, uncertainChallenge.ID, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkProfileSMSDeliverySent(ctx, otherChange.ID, uncertainChallenge.ID, uncertainLease, SMSDelivery{MessageID: "stale"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale uncertain owner completion error=%v", err)
	}
	var deliveryStartedAt, deliveryUncertainAt *time.Time
	var uncertainStatus string
	if err := repository.pool.QueryRow(ctx, `SELECT status,delivery_started_at,delivery_uncertain_at FROM sms_challenges WHERE id=$1`, uncertainChallenge.ID).Scan(&uncertainStatus, &deliveryStartedAt, &deliveryUncertainAt); err != nil || uncertainStatus != "CREATED" || deliveryStartedAt == nil || deliveryUncertainAt == nil {
		t.Fatalf("uncertain durable state status=%s started=%v uncertain=%v err=%v", uncertainStatus, deliveryStartedAt, deliveryUncertainAt, err)
	}
	verification, err := repository.SMSChallengeReady(ctx, uncertainChallenge.ID, otherChange.NewValue, SMSPurposeProfileMobile)
	if err != nil || string(verification.Opaque) != "verification" {
		t.Fatalf("uncertain challenge verification=%#v err=%v", verification, err)
	}
	if err := repository.ConsumeSMSChallenge(ctx, uncertainChallenge.ID, otherChange.NewValue, SMSPurposeProfileMobile, true); err != nil {
		t.Fatal(err)
	}

	challenge := SMSChallenge{
		ID:        uuid.New(),
		Mobile:    replacement.NewValue,
		Purpose:   SMSPurposeProfileMobile,
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
		Source:    "profile-change-recovery-source",
		Device:    "profile-change-recovery-device",
		Provider:  SMSProviderDescriptor{ID: "integration", AdapterVersion: "v1"},
	}
	captchaID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) VALUES($1,$2,'PROFILE_MOBILE',100,'CREATED',$3,$4,$4)`, captchaID, replacement.NewValue, time.Now().UTC().Add(5*time.Minute), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	reserved, err := repository.ReserveProfileSMSChallenge(ctx, replacement.ID, actor.ID, captchaID, 100, challenge)
	if err != nil || reserved.ID != challenge.ID || reserved.Status != "CREATED" {
		t.Fatalf("atomic challenge reservation=%#v err=%v", reserved, err)
	}
	var durableBinding uuid.UUID
	if err := repository.pool.QueryRow(ctx, `SELECT sms_challenge_id FROM profile_change_requests WHERE id=$1`, replacement.ID).Scan(&durableBinding); err != nil || durableBinding != challenge.ID {
		t.Fatalf("durable binding=%s err=%v", durableBinding, err)
	}
	reobserved, err := repository.ReserveProfileSMSChallenge(ctx, replacement.ID, actor.ID, uuid.New(), 0, SMSChallenge{ID: uuid.New(), Mobile: replacement.NewValue, Purpose: SMSPurposeProfileMobile, ExpiresAt: time.Now().UTC().Add(5 * time.Minute), Provider: challenge.Provider})
	if err != nil || reobserved.ID != challenge.ID {
		t.Fatalf("created challenge recovery=%#v err=%v", reobserved, err)
	}
	leaseID, claimed, err := repository.ClaimProfileSMSDelivery(ctx, replacement.ID, actor.ID, challenge.ID, false)
	if err != nil || !claimed || leaseID == uuid.Nil {
		t.Fatalf("first delivery claim lease=%s claimed=%t err=%v", leaseID, claimed, err)
	}
	if _, claimed, err := repository.ClaimProfileSMSDelivery(ctx, replacement.ID, actor.ID, challenge.ID, false); err != nil || claimed {
		t.Fatalf("concurrent delivery claim claimed=%t err=%v", claimed, err)
	}
	if err := repository.MarkProfileSMSDeliverySent(ctx, replacement.ID, challenge.ID, uuid.New(), SMSDelivery{MessageID: "stale"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale lease completion error=%v, want conflict", err)
	}
	if err := repository.CommitProfileSMSVerification(ctx, replacement.ID, actor.ID, challenge.ID, false); !errors.Is(err, ErrSMSInvalid) {
		t.Fatalf("first invalid code error=%v", err)
	}
	if err := repository.CommitProfileSMSVerification(ctx, replacement.ID, actor.ID, challenge.ID, false); !errors.Is(err, ErrSMSInvalid) {
		t.Fatalf("second invalid code error=%v", err)
	}
	var invalidAttempts int
	if err := repository.pool.QueryRow(ctx, `SELECT attempts FROM sms_challenges WHERE id=$1`, challenge.ID).Scan(&invalidAttempts); err != nil || invalidAttempts != 2 {
		t.Fatalf("invalid verification attempts=%d err=%v", invalidAttempts, err)
	}
	var deliveryStatus string
	if err := repository.pool.QueryRow(ctx, `SELECT status FROM sms_challenges WHERE id=$1`, challenge.ID).Scan(&deliveryStatus); err != nil || deliveryStatus != "CREATED" {
		t.Fatalf("stale owner changed delivery status=%s err=%v", deliveryStatus, err)
	}
	if err := repository.MarkProfileSMSDeliveryFailed(ctx, replacement.ID, challenge.ID, leaseID); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := repository.ClaimProfileSMSDelivery(ctx, replacement.ID, actor.ID, challenge.ID, false); !errors.Is(err, ErrSMSUnavailable) || claimed {
		t.Fatalf("failed delivery reclaim claimed=%t err=%v", claimed, err)
	}
	boundRecovery, err := repository.CreateProfileChange(ctx, principal, "MOBILE", replacement.NewValue)
	if err != nil {
		t.Fatal(err)
	}
	if boundRecovery.ID != replacement.ID || boundRecovery.SMSChallengeID == nil || *boundRecovery.SMSChallengeID != challenge.ID {
		t.Fatalf("bound same-target request was not recovered: %#v", boundRecovery)
	}
	if err := repository.ScheduleProfileChangeRetry(ctx, first.ID, "MUST_NOT_REVIVE"); err != nil {
		t.Fatal(err)
	}
	stillSuperseded, err := repository.ProfileChange(ctx, first.ID, &actor.ID)
	if err != nil || stillSuperseded.Status != "SUPERSEDED" || stillSuperseded.LastErrorCode != "REPLACED_BEFORE_VERIFICATION" {
		t.Fatalf("retry revived terminal request: %#v err=%v", stillSuperseded, err)
	}
	if _, err := repository.CreateProfileChange(ctx, principal, "MOBILE", "+8613800138004"); !errors.Is(err, ErrProfileChangeConflict) {
		t.Fatalf("bound request replacement error=%v, want profile conflict", err)
	}
	freshCaptchaID := uuid.New()
	now = time.Now().UTC()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) VALUES($1,$2,'PROFILE_MOBILE',100,'CREATED',$3,$4,$4)`, freshCaptchaID, replacement.NewValue, now.Add(5*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	fresh := SMSChallenge{ID: uuid.New(), Mobile: replacement.NewValue, Purpose: SMSPurposeProfileMobile, ExpiresAt: now.Add(5 * time.Minute), Source: "fresh-source", Device: "fresh-device", Provider: challenge.Provider, VerificationData: []byte("fresh-verification")}
	if _, err := repository.ReserveProfileSMSChallenge(ctx, replacement.ID, actor.ID, freshCaptchaID, 100, fresh); err != nil {
		t.Fatal(err)
	}
	freshLease, claimed, err := repository.ClaimProfileSMSDelivery(ctx, replacement.ID, actor.ID, fresh.ID, false)
	if err != nil || !claimed {
		t.Fatalf("fresh delivery claim claimed=%t err=%v", claimed, err)
	}
	if err := repository.MarkProfileSMSDeliverySent(ctx, replacement.ID, fresh.ID, freshLease, SMSDelivery{MessageID: "fresh", VerificationData: fresh.VerificationData}); err != nil {
		t.Fatal(err)
	}
	var verificationWG sync.WaitGroup
	verificationErrors := make(chan error, 2)
	for range 2 {
		verificationWG.Add(1)
		go func() {
			defer verificationWG.Done()
			verificationErrors <- repository.CommitProfileSMSVerification(ctx, replacement.ID, actor.ID, fresh.ID, true)
		}()
	}
	verificationWG.Wait()
	close(verificationErrors)
	for err := range verificationErrors {
		if err != nil {
			t.Fatalf("concurrent atomic verification failed: %v", err)
		}
	}
	if err := repository.CommitProfileSMSVerification(ctx, replacement.ID, actor.ID, fresh.ID, true); err != nil {
		t.Fatalf("duplicate atomic verification was not idempotent: %v", err)
	}
	var profileStatus, challengeStatus string
	if err := repository.pool.QueryRow(ctx, `SELECT request.status,challenge.status FROM profile_change_requests request JOIN sms_challenges challenge ON challenge.id=request.sms_challenge_id WHERE request.id=$1`, replacement.ID).Scan(&profileStatus, &challengeStatus); err != nil || profileStatus != "VERIFIED" || challengeStatus != "CONSUMED" {
		t.Fatalf("atomic verification profile=%s challenge=%s err=%v", profileStatus, challengeStatus, err)
	}
	if _, err := repository.CreateProfileChange(ctx, principal, "MOBILE", replacement.NewValue); !errors.Is(err, ErrProfileChangeConflict) {
		t.Fatalf("verified same-target recovery error=%v, want profile conflict", err)
	}
	if _, err := repository.CreateProfileChange(ctx, principal, "MOBILE", "+8613800138004"); !errors.Is(err, ErrProfileChangeConflict) {
		t.Fatalf("verified request replacement error=%v, want profile conflict", err)
	}
}
