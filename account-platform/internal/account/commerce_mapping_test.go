package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/google/uuid"
)

type commerceSyncStoreStub struct {
	job       *CommerceCustomerSyncJob
	provider  string
	version   string
	completed bool
	retried   bool
	reason    string
}

func (s *commerceSyncStoreStub) ClaimCommerceCustomerSyncJob(_ context.Context, provider, version string, _ time.Duration) (*CommerceCustomerSyncJob, error) {
	s.provider, s.version = provider, version
	return s.job, nil
}

func (s *commerceSyncStoreStub) CompleteCommerceCustomerSyncJob(_ context.Context, job CommerceCustomerSyncJob, result commerce.CustomerProvisionResult) error {
	s.completed = job.ID == s.job.ID && result.ExternalReferenceRedacted == "***1234"
	return nil
}

func (s *commerceSyncStoreStub) RetryCommerceCustomerSyncJob(_ context.Context, job CommerceCustomerSyncJob, reason string, _ time.Time) error {
	s.retried, s.reason = job.ID == s.job.ID, reason
	return nil
}

type commerceAdapterStub struct {
	request commerce.CustomerProvisionRequest
	err     error
}

func (a *commerceAdapterStub) EnsureCustomer(_ context.Context, request commerce.CustomerProvisionRequest) (commerce.CustomerProvisionResult, error) {
	a.request = request
	return commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***1234"}, a.err
}

func TestCommerceCustomerSyncWorkerCompletesSupplierNeutralJob(t *testing.T) {
	job := &CommerceCustomerSyncJob{ID: uuid.New(), SubjectType: "USER", SubjectID: uuid.New(), ContactMobile: "+8613800001234", LeaseUntil: time.Now().Add(time.Minute)}
	store, adapter := &commerceSyncStoreStub{job: job}, &commerceAdapterStub{}
	worker := NewCommerceCustomerSyncWorker(store, adapter, "commerce-a", "adapter-v2", time.Second)

	worker.runOne(context.Background())

	if !store.completed || store.retried || store.provider != "commerce-a" || store.version != "adapter-v2" {
		t.Fatalf("unexpected worker result: %#v", store)
	}
	if adapter.request.SubjectType != "USER" || adapter.request.SubjectID != job.SubjectID.String() || adapter.request.ContactMobile != job.ContactMobile {
		t.Fatalf("unexpected adapter request: %#v", adapter.request)
	}
}

func TestCommerceCustomerSyncWorkerUsesStableRedactedFailure(t *testing.T) {
	job := &CommerceCustomerSyncJob{ID: uuid.New(), SubjectType: "USER", SubjectID: uuid.New(), ContactMobile: "+8613800001234", Attempts: 2, LeaseUntil: time.Now().Add(time.Minute)}
	store := &commerceSyncStoreStub{job: job}
	worker := NewCommerceCustomerSyncWorker(store, &commerceAdapterStub{err: errors.New("provider leaked message")}, "commerce-a", "adapter-v2", time.Second)

	worker.runOne(context.Background())

	if !store.retried || store.completed || store.reason != "COMMERCE_PROVIDER_UNAVAILABLE" {
		t.Fatalf("unexpected retry result: %#v", store)
	}
}

func TestConfigureCommerceProviderRejectsMissingIdentifiers(t *testing.T) {
	repository := &PostgresRepository{}
	if err := repository.ConfigureCommerceProvider("", "v1"); err == nil {
		t.Fatal("expected empty provider identifier to be rejected")
	}
	if err := repository.ConfigureCommerceProvider("commerce-a", "v1"); err != nil {
		t.Fatalf("configure commerce provider: %v", err)
	}
}

func TestSafeExternalReferenceRejectsUnredactedProviderValue(t *testing.T) {
	if value := safeExternalReference("13800001234"); value != "" {
		t.Fatalf("expected raw external reference to be discarded, got %q", value)
	}
	if value := safeExternalReference("***1234"); value != "***1234" {
		t.Fatalf("expected redacted external reference, got %q", value)
	}
}
