package main

import (
	"strings"
	"testing"
	"time"
)

func TestBackupEvidenceUsesCanonicalProductionClassification(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, environment := range []string{"production", "production-new", "production-eu", " Production-East "} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("PINVOU_DEPLOYMENT_ENVIRONMENT", environment)
			t.Setenv("MIGRATION_BACKUP_EVIDENCE_ID", "")
			t.Setenv("MIGRATION_BACKUP_COMPLETED_AT", "")
			t.Setenv("MIGRATION_BACKUP_SHA256", "")
			if _, _, _, err := productionBackupEvidence(now); err == nil {
				t.Fatal("production migration bypassed backup evidence")
			}
			t.Setenv("MIGRATION_BACKUP_EVIDENCE_ID", "controlled-backup")
			t.Setenv("MIGRATION_BACKUP_COMPLETED_AT", now.Format(time.RFC3339))
			t.Setenv("MIGRATION_BACKUP_SHA256", strings.Repeat("a", 64))
			id, at, hash, err := productionBackupEvidence(now)
			if err != nil || id != "controlled-backup" || at != now.Format(time.RFC3339) || hash != strings.Repeat("a", 64) {
				t.Fatal("existing valid backup evidence changed", id, at, hash, err)
			}
		})
	}
	t.Run("development unchanged", func(t *testing.T) {
		t.Setenv("PINVOU_DEPLOYMENT_ENVIRONMENT", "development")
		t.Setenv("MIGRATION_BACKUP_EVIDENCE_ID", "")
		if id, at, hash, err := productionBackupEvidence(now); err != nil || id != "" || at != "" || hash != "" {
			t.Fatal("development unexpectedly requires production evidence")
		}
	})
}
