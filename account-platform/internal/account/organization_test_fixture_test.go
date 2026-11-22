package account

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Controlled repository fixtures exercise domain preflight ordering only.
// They do not represent successful database or wallet provisioning.
type f06LifecycleRepository struct{ organizationState *AdminOrganizationState }

func (*f06LifecycleRepository) Bootstrap(context.Context, identity.Principal) (Me, bool, error) {
	return Me{}, false, ErrForbidden
}
func (*f06LifecycleRepository) GetMe(context.Context, identity.Principal) (Me, error) {
	return Me{}, ErrForbidden
}
func (*f06LifecycleRepository) ResolveAuthorizationContext(context.Context, identity.Principal) (AuthorizationContext, error) {
	return AuthorizationContext{}, ErrForbidden
}
func (*f06LifecycleRepository) Ping(context.Context) error { return ErrForbidden }
func (*f06LifecycleRepository) Close()                     {}
func (f *f06LifecycleRepository) AdminOrganizationState(context.Context, uuid.UUID) (AdminOrganizationState, error) {
	return *f.organizationState, nil
}
func (*f06LifecycleRepository) AdminOrganizationDeletionBlockers(context.Context, uuid.UUID) ([]OrganizationLifecycleBlocker, error) {
	return nil, nil
}

type f06LifecycleWallet struct{}

func (*f06LifecycleWallet) EnsurePersonalWallet(context.Context, uuid.UUID) error {
	return ErrForbidden
}
