package token

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TokenGovernanceSnapshot is a read-only, non-secret activation projection.
// Observation time is metadata, never a state-version substitute.
type TokenGovernanceSnapshot struct {
	State                string    `json:"state"`
	Epoch                int64     `json:"epoch"`
	PolicySetDigest      string    `json:"policy_set_digest"`
	HandlerSetDigest     string    `json:"handler_set_digest"`
	ApprovedRuntimeRoles []string  `json:"approved_runtime_roles"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type TokenGovernanceStore struct {
	pool          *pgxpool.Pool
	handlerDigest string
	policyDigest  string
}

func NewTokenGovernanceStore(pool *pgxpool.Pool, handlerDigest, policyDigest string) (*TokenGovernanceStore, error) {
	if pool == nil || !governanceDigestValid(handlerDigest) || !governanceDigestValid(policyDigest) {
		return nil, errors.New("invalid Token governance store configuration")
	}
	return &TokenGovernanceStore{pool: pool, handlerDigest: handlerDigest, policyDigest: policyDigest}, nil
}

func (r *PostgresRepository) GovernanceStore(handlerDigest, policyDigest string) (*TokenGovernanceStore, error) {
	return NewTokenGovernanceStore(r.pool, handlerDigest, policyDigest)
}

// InstallProductionGovernance wires the same installed dispatch/policy
// manifest into ordinary transactions and isolated credential authority.
func (r *PostgresRepository) InstallProductionGovernance(authorizer *authorization.Client) (*TokenGovernanceStore, error) {
	if r == nil || authorizer == nil {
		return nil, errors.New("governance authority is not configured")
	}
	handlerDigest, err := TokenCommandHandlerDigest()
	if err != nil {
		return nil, err
	}
	policyDigest, err := approvalpolicy.PolicySetDigest()
	if err != nil {
		return nil, err
	}
	store, err := r.GovernanceStore(handlerDigest, policyDigest)
	if err != nil {
		return nil, err
	}
	r.WithTokenRouteActivationTransactionGuard(store)
	r.WithCredentialIsolationAuthorityGuard(NewCredentialIsolationAuthorityGuard(store, authorizer))
	return store, nil
}

func governanceDigestValid(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

const tokenGovernanceSelect = `SELECT state,epoch,coalesce(policy_set_digest,''),coalesce(handler_set_digest,''),approved_runtime_roles,updated_at,session_user FROM public.token_command_governance_activation WHERE singleton=TRUE`

func (s *TokenGovernanceStore) scan(row pgx.Row) (TokenGovernanceSnapshot, error) {
	var snapshot TokenGovernanceSnapshot
	var sessionUser string
	if err := row.Scan(&snapshot.State, &snapshot.Epoch, &snapshot.PolicySetDigest, &snapshot.HandlerSetDigest, &snapshot.ApprovedRuntimeRoles, &snapshot.UpdatedAt, &sessionUser); err != nil {
		return snapshot, ErrTokenRouteActivationChanged
	}
	if snapshot.UpdatedAt.IsZero() {
		return snapshot, ErrTokenRouteActivationChanged
	}
	if snapshot.State == "PRE_ACTIVATION" {
		if snapshot.Epoch != 0 || snapshot.PolicySetDigest != "" || snapshot.HandlerSetDigest != "" || len(snapshot.ApprovedRuntimeRoles) != 0 {
			return snapshot, ErrTokenRouteActivationChanged
		}
		return snapshot, nil
	}
	if (snapshot.State != "ACTIVE" && snapshot.State != "SAFE_ROLLBACK") || snapshot.Epoch < 1 || snapshot.PolicySetDigest != s.policyDigest || snapshot.HandlerSetDigest != s.handlerDigest || !slices.IsSorted(snapshot.ApprovedRuntimeRoles) || !slices.Contains(snapshot.ApprovedRuntimeRoles, sessionUser) {
		return snapshot, ErrTokenRouteActivationChanged
	}
	for index, role := range snapshot.ApprovedRuntimeRoles {
		if strings.TrimSpace(role) != role || role == "" || (index > 0 && role == snapshot.ApprovedRuntimeRoles[index-1]) {
			return snapshot, ErrTokenRouteActivationChanged
		}
	}
	return snapshot, nil
}

func (s *TokenGovernanceStore) Snapshot(ctx context.Context) (TokenGovernanceSnapshot, error) {
	if s == nil || s.pool == nil {
		return TokenGovernanceSnapshot{}, ErrTokenRouteActivationChanged
	}
	return s.scan(s.pool.QueryRow(ctx, tokenGovernanceSelect))
}

func (s *TokenGovernanceStore) CurrentTokenCommandGovernanceActivation(ctx context.Context) (TokenCommandGovernanceActivation, error) {
	snapshot, err := s.Snapshot(ctx)
	return TokenCommandGovernanceActivation{State: snapshot.State, Epoch: snapshot.Epoch}, err
}

// FOR SHARE is held until the caller's commit/rollback. A transition which
// wins first rejects the old selection; an effect which wins first commits
// before transition. No old-epoch effect can commit after a SAFE transition.
func (s *TokenGovernanceStore) LockAndCheckTokenRouteActivation(ctx context.Context, tx pgx.Tx, selected TokenCommandGovernanceActivation) error {
	if s == nil || tx == nil || (selected.State != "PRE_ACTIVATION" && selected.State != "ACTIVE") {
		return ErrTokenRouteActivationChanged
	}
	lockQuery := strings.Replace(tokenGovernanceSelect, "FROM public.token_command_governance_activation", "FROM public.lock_token_command_governance_activation()", 1)
	snapshot, err := s.scan(tx.QueryRow(ctx, lockQuery))
	if err != nil || snapshot.State != selected.State || snapshot.Epoch != selected.Epoch {
		return ErrTokenRouteActivationChanged
	}
	return nil
}
