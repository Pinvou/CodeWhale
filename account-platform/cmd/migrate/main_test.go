package main

import (
	"testing"
	"time"
)

func TestProductionBackupEvidence(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	t.Setenv("PINVOU_DEPLOYMENT_ENVIRONMENT", "production-new")
	t.Setenv("MIGRATION_BACKUP_EVIDENCE_ID", "snapshot-20260824")
	t.Setenv("MIGRATION_BACKUP_COMPLETED_AT", now.Add(-time.Hour).Format(time.RFC3339))
	t.Setenv("MIGRATION_BACKUP_SHA256", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, _, _, err := productionBackupEvidence(now); err != nil {
		t.Fatalf("productionBackupEvidence returned error: %v", err)
	}

	t.Setenv("MIGRATION_BACKUP_COMPLETED_AT", now.Add(-25*time.Hour).Format(time.RFC3339))
	if _, _, _, err := productionBackupEvidence(now); err == nil {
		t.Fatal("productionBackupEvidence accepted stale evidence")
	}
}

func TestNonProductionDoesNotRequireBackupEvidence(t *testing.T) {
	t.Setenv("PINVOU_DEPLOYMENT_ENVIRONMENT", "staging")
	if id, _, _, err := productionBackupEvidence(time.Now()); err != nil || id != "" {
		t.Fatalf("non-production evidence = %q, %v", id, err)
	}
}

func TestFilepathBase(t *testing.T) {
	if got := filepathBase(`C:\\migrations\\billing`); got != "billing" {
		t.Fatalf("filepathBase = %q", got)
	}
}
