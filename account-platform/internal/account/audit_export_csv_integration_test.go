package account

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func testAuditExportCSVReferenceMasking(t *testing.T, ctx context.Context, exports *AuditExportService, admin identity.Principal, first uuid.UUID) {
	t.Helper()
	repo := exports.repository
	newActor := func() (identity.Principal, uuid.UUID) {
		t.Helper()
		principal := identity.Principal{Issuer: admin.Issuer, Subject: uuid.NewString(), SessionID: uuid.NewString(), AuthenticatedAt: time.Now().UTC()}
		user, _, err := repo.Bootstrap(ctx, principal)
		if err != nil {
			t.Fatal(err)
		}
		if err := NewWalletProvisioningWorker(repo, auditExportBoundaryWallet{}, time.Second).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		return principal, user.UserID
	}
	actor, actorID := newActor()
	_, second := newActor()
	grant := func(role string) {
		t.Helper()
		expires := time.Now().UTC().Add(time.Hour)
		if _, err := repo.AdminGrantRoleBinding(ctx, admin, RoleBindingGrant{UserID: actorID, RoleCode: role, ExpiresAt: &expires, IdempotencyKey: uuid.NewString(), Reason: "audit reference fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	grant("PLATFORM_AUDITOR")
	const action = "test.audit-reference-masking"
	firstRef, secondRef := "controlled-"+uuid.NewString(), "controlled-"+uuid.NewString()
	fixtures := []struct {
		subject   *uuid.UUID
		reference string
	}{{&first, firstRef}, {&first, secondRef}, {&second, firstRef}, {nil, ""}}
	now := time.Now().UTC()
	for index, fixture := range fixtures {
		if _, err := repo.pool.Exec(ctx, `INSERT INTO audit_logs(id,actor_user_id,action,resource_type,resource_id,result,request_id,trace_id,occurred_at) VALUES($1,$2,$3,'test',$4,'SUCCEEDED','csv-fixture','csv-fixture',$5)`, uuid.New(), fixture.subject, action, fixture.reference, now.Add(time.Duration(index)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	request := AuditExportRequest{Reason: "audit reference fixture", IdempotencyKey: uuid.NewString(), IncludePII: true, Filter: AuditExportFilter{Action: action, From: now.Add(-time.Minute), To: now.Add(time.Second)}}
	if _, err := exports.Create(ctx, actor, request); !errors.Is(err, ErrForbidden) {
		t.Fatalf("PII export before its grant: %v", err)
	}
	readCSV := func(pii bool) [][2]string {
		t.Helper()
		request.IncludePII, request.IdempotencyKey = pii, uuid.NewString()
		job, err := exports.Create(ctx, actor, request)
		if err != nil {
			t.Fatal(err)
		}
		claim, ok, err := exports.claim(ctx)
		if err != nil || !ok || claim.ID != job.ID {
			t.Fatalf("claim export: %v", err)
		}
		if err := exports.generate(ctx, claim); err != nil {
			t.Fatal(err)
		}
		ticket, err := exports.IssueDownloadTicket(ctx, actor, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		file, err := exports.Download(ctx, actor, job.ID, ticket.Token)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct{ Watermark, Nonce, Ciphertext string }
		if err := json.Unmarshal(file.Payload, &envelope); err != nil {
			t.Fatal(err)
		}
		decode := func(value string) []byte {
			t.Helper()
			decoded, err := base64.RawURLEncoding.DecodeString(value)
			if err != nil {
				t.Fatal(err)
			}
			return decoded
		}
		plain, err := decryptAEAD(decode(job.DecryptionKey), decode(envelope.Nonce), decode(envelope.Ciphertext), []byte(envelope.Watermark))
		if err != nil {
			t.Fatal(err)
		}
		if !pii {
			for _, identifier := range []string{first.String(), second.String(), firstRef, secondRef} {
				if bytes.Contains(plain, []byte(identifier)) {
					t.Fatal("non-PII CSV contains an original subject or resource")
				}
			}
		}
		reader := csv.NewReader(bytes.NewReader(plain))
		reader.Comment = '#'
		rows, err := reader.ReadAll()
		if err != nil || len(rows) != 1+len(fixtures) {
			t.Fatalf("downloaded CSV has %d records: %v", len(rows), err)
		}
		header := rows[0]
		subject, resource := slices.Index(header, "actor_user_id"), slices.Index(header, "resource_reference")
		if subject < 0 || resource < 0 || slices.Contains(header, "actor_email") != pii || slices.Contains(header, "actor_display_name") != pii {
			t.Fatal("unexpected audit CSV columns", header)
		}
		values := make([][2]string, 0, len(fixtures))
		for _, row := range rows[1:] {
			values = append(values, [2]string{row[subject], row[resource]})
		}
		return values
	}
	redacted := readCSV(false)
	subjectMask := regexp.MustCompile(`^subject_[0-9a-f]{24}$`)
	resourceMask := regexp.MustCompile(`^ref_[0-9a-f]{24}$`)
	for _, row := range redacted[:3] {
		if !subjectMask.MatchString(row[0]) || !resourceMask.MatchString(row[1]) {
			t.Fatal("non-PII CSV contains an unmasked subject or resource")
		}
	}
	if redacted[0][0] != redacted[1][0] || redacted[0][0] == redacted[2][0] ||
		redacted[0][1] != redacted[2][1] || redacted[0][1] == redacted[1][1] || redacted[3] != [2]string{} {
		t.Fatal("non-PII CSV lost stable, distinct or empty references")
	}
	grant("PLATFORM_AUDIT_PII_EXPORT")
	raw := readCSV(true)
	if want := [][2]string{{first.String(), firstRef}, {first.String(), secondRef}, {second.String(), firstRef}, {}}; !slices.Equal(raw, want) {
		t.Fatal("authorized PII CSV did not preserve its original references")
	}
}
