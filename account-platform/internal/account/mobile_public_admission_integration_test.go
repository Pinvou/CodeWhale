package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublicMobilePersistentAdmissionAndCaptcha(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("fixture connection failed")
	}
	defer connection.Close(context.Background())
	schema := "public_admission_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = connection.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := connection.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("fixture cleanup failed")
		}
	}()
	if _, err = connection.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"000089_public_mobile_admission.up.sql", "000009_slider_captcha.up.sql"} {
		sql, readErr := os.ReadFile("../../migrations/account/" + filename)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err = connection.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		t.Fatal("fixture pool configuration failed")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("fixture pool creation failed")
	}
	defer pool.Close()
	repository := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}
	var group sync.WaitGroup
	var admitted atomic.Int32
	start := make(chan struct{})
	for range 24 {
		group.Go(func() {
			<-start
			err := repository.AdmitPublicMobileRequest(ctx, "CAPTCHA", "13800138000", "peer-one")
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrSMSRateLimited) {
				t.Error("concurrent admission failed unexpectedly")
			}
		})
	}
	close(start)
	group.Wait()
	if admitted.Load() != 10 {
		t.Fatalf("admitted %d, want exactly10", admitted.Load())
	}
	// A replacement process reads the same persistent quota.
	restarted := &PostgresRepository{pool: pool, now: repository.now}
	if err := restarted.AdmitPublicMobileRequest(ctx, "CAPTCHA", "13800138000", "new-peer"); !errors.Is(err, ErrSMSRateLimited) {
		t.Fatal("restart or source rotation reset target quota")
	}
	if _, err = connection.Exec(ctx, `UPDATE mobile_public_admission_buckets SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		operation := "PRECHECK"
		if i%2 == 0 {
			operation = "CAPTCHA"
		}
		if err = repository.AdmitPublicMobileRequest(ctx, operation, fmt.Sprintf("139%08d", i), "shared-peer"); err != nil {
			t.Fatal("shared source limit rejected below boundary")
		}
	}
	for _, operation := range []string{"PRECHECK", "CAPTCHA"} {
		if err = repository.AdmitPublicMobileRequest(ctx, operation, "13700138000", "shared-peer"); !errors.Is(err, ErrSMSRateLimited) {
			t.Fatal("changing public endpoint reset shared source quota")
		}
	}
	if _, err = connection.Exec(ctx, `UPDATE mobile_public_admission_buckets SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err = repository.AdmitPublicMobileRequest(ctx, "CAPTCHA", "13800138000", "peer-one"); err != nil {
		t.Fatal("expired counter did not reopen")
	}
	var count int
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM mobile_public_admission_buckets`).Scan(&count); err != nil || count != 3 {
		t.Fatal("expiry sweep did not remove unreferenced counters")
	}
	// Near the global boundary, unique arbitrary sources/targets can add at
	// most one admission. The remaining requests create no new bucket rows.
	key := sha256.Sum256([]byte("global"))
	if _, err = connection.Exec(ctx, `UPDATE mobile_public_admission_buckets SET attempts=299 WHERE scope='GLOBAL' AND bucket_hash=$1`, key[:]); err != nil {
		t.Fatal(err)
	}
	admitted.Store(0)
	for i := range 40 {
		group.Go(func() {
			err := repository.AdmitPublicMobileRequest(ctx, "PRECHECK", fmt.Sprintf("138%08d", i), fmt.Sprintf("peer-%d", i))
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrSMSRateLimited) {
				t.Error("global admission error")
			}
		})
	}
	group.Wait()
	if admitted.Load() != 1 {
		t.Fatal("concurrent global limit exceeded")
	}
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM mobile_public_admission_buckets`).Scan(&count); err != nil || count > 5 {
		t.Fatal("rejected random keys grew persistent bucket cardinality")
	}
	challenge := SliderCaptcha{ID: uuid.New(), Mobile: "+8613800138000", Purpose: SMSPurposeLogin, TargetX: 140, ExpiresAt: time.Now().Add(time.Minute)}
	if err = repository.CreateSliderCaptcha(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	if err = repository.ConsumeSliderCaptcha(ctx, challenge.ID, challenge.Mobile, challenge.Purpose, 140); err != nil {
		t.Fatal("correct human offset rejected")
	}
	if err = repository.ConsumeSliderCaptcha(ctx, challenge.ID, challenge.Mobile, challenge.Purpose, 140); !errors.Is(err, ErrCaptchaInvalid) {
		t.Fatal("captcha replay accepted")
	}
}
