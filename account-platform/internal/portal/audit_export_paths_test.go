package portal

import (
	"net/http"
	"testing"
)

func TestAuditExportBFFPathsAndDownloadProofAreClosed(t *testing.T) {
	const id = "ec4df7c4-2bd1-4cbb-a267-2018abf90f63"
	for _, c := range []struct {
		method, path string
		allow        bool
	}{
		{http.MethodPost, "/admin/v1/audit-exports", true}, {http.MethodGet, "/admin/v1/audit-exports/" + id, true},
		{http.MethodGet, "/admin/v1/audit-exports/" + id + "/download", true}, {http.MethodPost, "/admin/v1/audit-exports/" + id + "/download-tickets", true},
		{http.MethodHead, "/admin/v1/audit-exports/" + id + "/download", false}, {http.MethodHead, "/admin/v1/audit-exports/" + id, true},
		{http.MethodDelete, "/admin/v1/audit-exports/" + id, false}, {http.MethodPost, "/admin/v1/audit-exports/" + id + "/download", false},
		{http.MethodGet, "/admin/v1/audit-exports/not-a-uuid/download", false}, {http.MethodGet, "/admin/v1/audit-exports/" + id + "/download/extra", false},
	} {
		if accountAdminPathAllowed(c.method, c.path) != c.allow {
			t.Fatalf("allowlist mismatch for %s", c.method)
		}
	}
	if !accountProofHeaderAllowed(http.MethodGet, "/admin/v1/audit-exports/"+id+"/download", "X-Pinvou-Download-Token") {
		t.Fatal("download proof stripped")
	}
	if accountProofHeaderAllowed(http.MethodHead, "/admin/v1/audit-exports/"+id+"/download", "X-Pinvou-Download-Token") {
		t.Fatal("HEAD must not forward a single-use download proof")
	}
	for _, path := range []string{"/admin/v1/audit-exports", "/admin/v1/users", "/admin/v1/audit-exports/" + id + "/download-tickets"} {
		if accountProofHeaderAllowed(http.MethodGet, path, "X-Pinvou-Download-Token") {
			t.Fatal("download token leaked outside exact read")
		}
	}
}
