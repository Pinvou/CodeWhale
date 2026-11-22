package main

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Existing memberships were checked before any DDL. Avoid issuing redundant
// GRANTs under a new administrator: PostgreSQL can retain multiple grantor
// edges, which would change a previously exact candidate capability graph.
func grantMissingMembership(ctx context.Context, tx pgx.Tx, parent, child string, inherit bool) error {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m
 JOIN pg_catalog.pg_roles p ON p.oid=m.roleid JOIN pg_catalog.pg_roles c ON c.oid=m.member
 WHERE p.rolname=$1 AND c.rolname=$2)`, parent, child).Scan(&present); err != nil {
		return err
	}
	if present {
		return nil
	}
	options := " WITH INHERIT FALSE,SET TRUE"
	if inherit {
		options = " WITH INHERIT TRUE,SET TRUE"
	}
	_, err := tx.Exec(ctx, "GRANT "+pgx.Identifier{parent}.Sanitize()+" TO "+pgx.Identifier{child}.Sanitize()+options)
	return err
}
