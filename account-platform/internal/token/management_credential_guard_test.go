package token

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type credentialAuthorityGuardFunc func(context.Context, pgx.Tx, CredentialIsolationAuthoritySnapshot) error

func (guard credentialAuthorityGuardFunc) LockAndCheckCredentialIsolationAuthority(ctx context.Context, tx pgx.Tx, snapshot CredentialIsolationAuthoritySnapshot) error {
	return guard(ctx, tx, snapshot)
}
