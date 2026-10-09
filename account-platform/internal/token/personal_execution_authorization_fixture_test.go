package token

import (
	"context"
	"encoding/json"
	"net/http"

	"sync"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type fixedBillingContextResolver struct{ resolved AuthorizationContext }

func (r fixedBillingContextResolver) Resolve(context.Context, string) (AuthorizationContext, error) {
	return r.resolved, nil
}

type authorizationDecisionRecorder struct {
	mu             sync.Mutex
	allowed        bool
	userID         uuid.UUID
	permissions    []string
	organizationID *uuid.UUID
}

func (r *authorizationDecisionRecorder) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	var body struct {
		Permission           string     `json:"permission"`
		TargetOrganizationID *uuid.UUID `json:"target_organization_id"`
	}
	if request.URL.Path != "/internal/v1/authorization/check" || json.NewDecoder(request.Body).Decode(&body) != nil {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.permissions = append(r.permissions, body.Permission)
	r.organizationID = body.TargetOrganizationID
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Authorization-Version", "17")
	_ = json.NewEncoder(w).Encode(map[string]any{"allowed": r.allowed, "user_id": r.userID})
}
func successfulBillingVerify(context.Context, string) (identity.Principal, error) {
	return identity.Principal{Issuer: "https://issuer.example", Subject: "subject"}, nil
}
