package token

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"testing"
)

type cleanupOwnerDirectory struct{ calls int }

func (d *cleanupOwnerDirectory) Scan(context.Context, identity.Principal, string, *uuid.UUID, func([]AdminOwnerCandidate) error) error {
	d.calls++
	return nil
}

func TestOwnerCandidateOrganizationFilterFailsBeforeAuthority(t *testing.T) {
	for _, query := range []string{"organization_id=" + uuid.NewString(), "organization_id=malformed", "organization_id=unsupported"} {
		d := &cleanupOwnerDirectory{}
		s := &Service{ownerCandidates: d}
		req := httptest.NewRequest(http.MethodGet, "/token/admin/v1/owner-candidates?"+query, nil)
		w := httptest.NewRecorder()
		s.adminOwnerCandidates(w, req)
		if w.Code != 400 || d.calls != 0 {
			t.Fatalf("unsupported organization filter status=%d calls=%d", w.Code, d.calls)
		}
	}
	for _, query := range []string{"", "organization_id=", "organization_id=%20"} {
		f, err := parseAdminQuery(httptest.NewRequest(http.MethodGet, "/?"+query, nil).URL.Query(), []string{"organization_id"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = (&Service{}).ownerCandidateSnapshotSource(httptest.NewRequest(http.MethodGet, "/", nil), f)
		if errors.Is(err, ErrAdminQueryInvalidFilter) {
			t.Fatalf("empty compatibility rejected: %q", query)
		}
	}
}
