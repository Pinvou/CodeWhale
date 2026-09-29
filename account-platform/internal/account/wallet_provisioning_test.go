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

type registrationWalletProvisionerFake struct {
	fakeWalletProvisioner
	request *RegistrationWalletProvisionRequest
}

func (f *registrationWalletProvisionerFake) ProvisionRegistrationWallet(_ context.Context, request RegistrationWalletProvisionRequest) error {
	f.request = &request
	return f.err
}

func (f *walletProvisioningRepositoryFake) LeaseWalletProvisioningJobs(context.Context, int, time.Duration) ([]WalletProvisioningJob, error) {
	return f.jobs, nil
}
func (f *walletProvisioningRepositoryFake) CompleteWalletProvisioning(_ context.Context, userID uuid.UUID) error {
	f.completed = userID
	return nil
}
func (f *walletProvisioningRepositoryFake) FailWalletProvisioning(_ context.Context, userID uuid.UUID, _ string) error {
	f.failed = userID
	return nil
}

func TestWalletProvisioningWorkerConvergesWithoutRegistrationRequest(t *testing.T) {
	userID := uuid.New()
	repository := &walletProvisioningRepositoryFake{jobs: []WalletProvisioningJob{{UserID: userID}}}
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
	repository := &walletProvisioningRepositoryFake{jobs: []WalletProvisioningJob{{UserID: userID}}}
	wallets := &fakeWalletProvisioner{err: errors.New("billing unavailable")}
	if err := NewWalletProvisioningWorker(repository, wallets, time.Second).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.failed != userID || repository.completed != uuid.Nil {
		t.Fatalf("completed=%s failed=%s", repository.completed, repository.failed)
	}
}

func TestWalletProvisioningWorkerUsesRegistrationBonusPath(t *testing.T) {
	userID, operationID := uuid.New(), uuid.New()
	fingerprint := make([]byte, 32)
	for index := range fingerprint {
		fingerprint[index] = byte(index + 1)
	}
	repository := &walletProvisioningRepositoryFake{jobs: []WalletProvisioningJob{{
		UserID: userID, RegistrationOperationID: &operationID, RegistrationBonusMode: "EVALUATE",
		FingerprintKeyID: "registration-bonus-v1", MobileFingerprint: fingerprint,
	}}}
	wallets := &registrationWalletProvisionerFake{}
	if err := NewWalletProvisioningWorker(repository, wallets, time.Second).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if wallets.calls != 0 || wallets.request == nil || wallets.request.UserID != userID || wallets.request.RegistrationOperationID != operationID || wallets.request.FingerprintKeyID != "registration-bonus-v1" || wallets.request.MobileFingerprint != "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20" {
		t.Fatalf("unexpected registration provisioning request: calls=%d request=%+v", wallets.calls, wallets.request)
	}
	if repository.completed != userID || repository.failed != uuid.Nil {
		t.Fatalf("completed=%s failed=%s", repository.completed, repository.failed)
	}
}

func TestWalletProvisioningWorkerRejectsMalformedRegistrationBonusJob(t *testing.T) {
	userID := uuid.New()
	repository := &walletProvisioningRepositoryFake{jobs: []WalletProvisioningJob{{UserID: userID, RegistrationBonusMode: "EVALUATE"}}}
	wallets := &registrationWalletProvisionerFake{}
	if err := NewWalletProvisioningWorker(repository, wallets, time.Second).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if wallets.calls != 0 || wallets.request != nil || repository.failed != userID || repository.completed != uuid.Nil {
		t.Fatalf("wallet calls=%d request=%+v completed=%s failed=%s", wallets.calls, wallets.request, repository.completed, repository.failed)
	}
}
