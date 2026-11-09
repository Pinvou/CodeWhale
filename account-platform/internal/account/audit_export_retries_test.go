package account

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestAuditExportRetryClassificationRequiresTemporaryDatabaseFailure(t *testing.T) {
	for _, sample := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"serialization", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"connection-failure", &pgconn.PgError{Code: "08006"}, true},
		{"database-shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"connection-capacity", &pgconn.PgError{Code: "53300"}, true},
		{"source-query-cancelled", wrapAuditExportDatabaseError(&pgconn.PgError{Code: "57014"}), false},
		{"invalid-query", wrapAuditExportDatabaseError(&pgconn.PgError{Code: "42601"}), false},
		{"constraint", wrapAuditExportDatabaseError(&pgconn.PgError{Code: "23514"}), false},
		{"local-eof", io.EOF, false},
		{"database-eof", wrapAuditExportDatabaseError(io.EOF), true},
		{"local-transport", &net.OpError{Op: "read", Err: io.EOF}, false},
		{"database-transport", wrapAuditExportDatabaseError(&net.OpError{Op: "read", Err: io.EOF}), true},
		{"quota", ErrAuditExportQuota, false},
		{"key-integrity", errors.New("controlled key integrity failure"), false},
		{"supervisor-cancelled", wrapAuditExportDatabaseError(context.Canceled), false},
		{"success", wrapAuditExportDatabaseError(nil), false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			if actual := transientAuditExportDatabaseError(sample.err); actual != sample.retry {
				t.Fatal("unexpected retry classification", actual)
			}
		})
	}
	underlying := &pgconn.PgError{Code: "08006", Message: "controlled private driver marker"}
	wrapped := wrapAuditExportDatabaseError(underlying)
	var recovered *pgconn.PgError
	if !errors.As(wrapped, &recovered) || recovered != underlying || wrapped.Error() != "audit export database operation failed" {
		t.Fatal("database origin or redacted error contract changed")
	}
}
