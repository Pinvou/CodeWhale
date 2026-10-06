package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type authorizationBoundaryRepository struct {
	Repository
	mu             sync.Mutex
	usage          UsageRequest
	beforeIncrease func()
	increaseError  error
	reviewReason   string
}

func (r *authorizationBoundaryRepository) GetUsage(context.Context, uuid.UUID) (UsageRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usage, nil
}
func (r *authorizationBoundaryRepository) IncreaseMaximum(_ context.Context, _ uuid.UUID, additional int64) error {
	if r.beforeIncrease != nil {
		r.beforeIncrease()
	}
	if r.increaseError != nil {
		return r.increaseError
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usage.MaximumMicrocredits += additional
	return nil
}
func (r *authorizationBoundaryRepository) MarkReview(_ context.Context, _ uuid.UUID, reason string, _ map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reviewReason = reason
	return nil
}

func awaitAuthorizationWaiter(t *testing.T, s *Service, id uuid.UUID) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		s.usageAuthorization.mu.Lock()
		entry := s.usageAuthorization.entries[id]
		waiting := entry != nil && entry.refs == 2
		s.usageAuthorization.mu.Unlock()
		if waiting {
			return
		}
		select {
		case <-deadline:
			t.Fatal("authorization did not wait at usage gate")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestUsageAuthorizationSerializesRenewalAndAdditionalHTTP(t *testing.T) {
	for _, boundary := range []string{"additional-http", "additional-local-record", "renewal-http"} {
		t.Run(boundary, func(t *testing.T) {
			usageID, holdID, walletID := uuid.New(), uuid.New(), uuid.New()
			repo := &authorizationBoundaryRepository{usage: UsageRequest{ID: usageID, HoldID: &holdID, WalletID: walletID, MaximumMicrocredits: 100}}
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			if boundary == "additional-local-record" {
				repo.beforeIncrease = func() { close(entered); <-release }
			}
			var calls atomic.Int64
			var peerMu sync.Mutex
			peerMaximum := int64(100)
			sequence := []int64{}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					UsageID    uuid.UUID `json:"usage_request_id"`
					HoldID     uuid.UUID `json:"hold_id"`
					Additional int64     `json:"additional_microcredits"`
					Lease      int64     `json:"lease_seconds"`
				}
				if r.URL.Path != "/internal/v1/holds/extend" || json.NewDecoder(r.Body).Decode(&body) != nil || body.UsageID != usageID || body.HoldID != holdID || body.Lease != 900 {
					t.Error("wrong extension facts")
					w.WriteHeader(400)
					return
				}
				ordinal := calls.Add(1)
				if ordinal == 1 && boundary != "additional-local-record" {
					close(entered)
					<-release
				}
				peerMu.Lock()
				peerMaximum += body.Additional
				maximum := peerMaximum
				sequence = append(sequence, body.Additional)
				peerMu.Unlock()
				_ = json.NewEncoder(w).Encode(BillingHold{ID: holdID, UsageRequestID: usageID, WalletID: walletID, Status: "ACTIVE", MaxMicrocredits: &maximum, ExpiresAt: time.Now().Add(15 * time.Minute)})
			}))
			defer func() { unblock(); peer.Close() }()
			client, err := NewHTTPBillingClient(peer.URL, "controlled-service-token")
			if err != nil {
				t.Fatal(err)
			}
			s := &Service{repository: repo, billing: client}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			first, second := make(chan error, 1), make(chan error, 1)
			if boundary == "renewal-http" {
				go func() { first <- s.renewAuthorization(ctx, usageID, holdID) }()
			} else {
				go func() { first <- s.extendAuthorization(ctx, usageID, holdID, 100, 50) }()
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("first operation never reached boundary")
			}
			if boundary == "renewal-http" {
				go func() { second <- s.extendAuthorization(ctx, usageID, holdID, 100, 50) }()
			} else {
				go func() { second <- s.renewAuthorization(ctx, usageID, holdID) }()
			}
			awaitAuthorizationWaiter(t, s, usageID)
			if calls.Load() != 1 {
				t.Fatal("HTTP renewal and addition overlapped")
			}
			unblock()
			for _, result := range []<-chan error{first, second} {
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("authorization did not finish")
				}
			}
			stored, _ := repo.GetUsage(ctx, usageID)
			peerMu.Lock()
			defer peerMu.Unlock()
			if stored.MaximumMicrocredits != 150 || peerMaximum != 150 || calls.Load() != 2 {
				t.Fatal("authorization maximum or HTTP count changed")
			}
			wantFirst := int64(50)
			if boundary == "renewal-http" {
				wantFirst = 0
			}
			if len(sequence) != 2 || sequence[0] != wantFirst || sequence[1] != 50-wantFirst {
				t.Fatal("wrong authorization ordering")
			}
			s.usageAuthorization.mu.Lock()
			defer s.usageAuthorization.mu.Unlock()
			if len(s.usageAuthorization.entries) != 0 {
				t.Fatal("usage gate leaked after completion")
			}
		})
	}
}

func TestUsageAuthorizationCancellationAndIndependentUsage(t *testing.T) {
	var gates usageAuthorizationGates
	first, other := uuid.New(), uuid.New()
	unlock, err := gates.acquire(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	secondUnlock, err := gates.acquire(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	secondUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := gates.acquire(ctx, first); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled wait returned %v", err)
	}
	gates.mu.Lock()
	remaining := len(gates.entries)
	refs := gates.entries[first].refs
	gates.mu.Unlock()
	if remaining != 1 || refs != 1 {
		t.Fatal("cancelled waiter retained its gate reference")
	}
	unlock()
	gates.mu.Lock()
	defer gates.mu.Unlock()
	if len(gates.entries) != 0 {
		t.Fatal("gate retained completed usage")
	}
}

func TestUsageAuthorizationLocalFailureRemainsReviewAndStrictRenewal(t *testing.T) {
	usageID, holdID, walletID := uuid.New(), uuid.New(), uuid.New()
	repo := &authorizationBoundaryRepository{usage: UsageRequest{ID: usageID, HoldID: &holdID, WalletID: walletID, MaximumMicrocredits: 100}, increaseError: errors.New("controlled local write failure")}
	var peerMu sync.Mutex
	peerMaximum := int64(100)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Additional int64 `json:"additional_microcredits"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		peerMu.Lock()
		peerMaximum += body.Additional
		maximum := peerMaximum
		peerMu.Unlock()
		_ = json.NewEncoder(w).Encode(BillingHold{ID: holdID, UsageRequestID: usageID, WalletID: walletID, Status: "ACTIVE", MaxMicrocredits: &maximum, ExpiresAt: time.Now().Add(time.Minute)})
	}))
	defer peer.Close()
	client, err := NewHTTPBillingClient(peer.URL, "controlled-token")
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{repository: repo, billing: client}
	price := versionedTestPrice(PriceVersion{ID: uuid.New(), InputPerMillion: 1_000_000})
	maximum, ok := s.authorizeAdditional(context.Background(), usageID, holdID, price, Usage{InputTokens: 150}, 100)
	if ok || maximum != 100 {
		t.Fatal("local synchronization failure confirmed authorization")
	}
	repo.mu.Lock()
	reason, stored := repo.reviewReason, repo.usage.MaximumMicrocredits
	repo.mu.Unlock()
	if reason != "local_preauthorization_record_pending" || stored != 100 {
		t.Fatal("local write failure lost REVIEW protection")
	}
	if err := s.renewAuthorization(context.Background(), usageID, holdID); err == nil {
		t.Fatal("renewal accepted mismatched remote maximum")
	}
	s.usageAuthorization.mu.Lock()
	defer s.usageAuthorization.mu.Unlock()
	if len(s.usageAuthorization.entries) != 0 {
		t.Fatal("failed authorization leaked gate")
	}
}
