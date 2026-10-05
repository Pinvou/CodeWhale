package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type walletProvisioningRepositoryFake struct {
	jobs      []WalletProvisioningJob
	completed uuid.UUID
	failed    uuid.UUID
}

func (f *walletProvisioningRepositoryFake) LeaseWalletProvisioningJobs(context.Context, int, time.Duration) ([]WalletProvisioningJob, error) {
	return f.jobs, nil
}
func (f *walletProvisioningRepositoryFake) CompleteWalletProvisioning(_ context.Context, job WalletProvisioningJob) error {
	f.completed = job.UserID
	return nil
}
func (f *walletProvisioningRepositoryFake) FailWalletProvisioning(_ context.Context, job WalletProvisioningJob, _ string) error {
	f.failed = job.UserID
	return nil
}

func TestWalletProvisioningWorkerConvergesWithoutRegistrationRequest(t *testing.T) {
	userID := uuid.New()
	repository := &walletProvisioningRepositoryFake{jobs: []WalletProvisioningJob{{UserID: userID, LeaseUntil: time.Now().Add(time.Minute)}}}
	wallets := &fakeWalletProvisioner{}
	worker := NewWalletProvisioningWorker(repository, wallets, time.Second)
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if wallets.calls != 1 || repository.completed != userID || repository.failed != uuid.Nil {
		t.Fatalf("wallet calls=%d completed=%s failed=%s", wallets.calls, repository.completed, repository.failed)
	}
}

func TestWalletProvisioningWorkerPersistsFailureForRetry(t *testing.T) {
	userID := uuid.New()
	repository := &walletProvisioningRepositoryFake{jobs: []WalletProvisioningJob{{UserID: userID, LeaseUntil: time.Now().Add(time.Minute)}}}
	wallets := &fakeWalletProvisioner{err: errors.New("billing unavailable")}
	if err := NewWalletProvisioningWorker(repository, wallets, time.Second).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.failed != userID || repository.completed != uuid.Nil {
		t.Fatalf("completed=%s failed=%s", repository.completed, repository.failed)
	}
}
