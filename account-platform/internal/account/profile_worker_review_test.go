package account

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type reviewRecoveryStore struct {
	profileStoreFake
	profileError, resetError error
	resetClaims              int
}

func (s *reviewRecoveryStore) ClaimProfileChange(context.Context) (*ProfileChange, error) {
	return nil, s.profileError
}
func (s *reviewRecoveryStore) ClaimPasswordReset(context.Context) (*PasswordResetOperation, error) {
	s.resetClaims++
	return nil, s.resetError
}

func TestProfileWorkerReportsIndependentClaimFailuresSafely(t *testing.T) {
	a, b := errors.New("private-profile-storage-detail"), errors.New("private-password-storage-detail")
	store := &reviewRecoveryStore{profileError: a, resetError: b}
	worker := NewProfileChangeWorker(&ProfileChangeService{store: store}, 0)
	err := worker.RunOnce(context.Background())
	if !errors.Is(err, a) || !errors.Is(err, b) || store.resetClaims != 1 {
		t.Fatal("claim failures lost or independent reset skipped")
	}
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	recordProfileRecoveryFailure(context.Background(), "PROFILE_RECOVERY_FAILED", err)
	if !strings.Contains(output.String(), "PROFILE_RECOVERY_FAILED") || strings.Contains(output.String(), "private-") {
		t.Fatal("failure diagnostic missing or leaked private detail")
	}
	store.profileError = nil
	store.resetError = nil
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal("empty successful pass failed")
	}
}
