package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"strings"
)

func validateClosureReplayACK(input []migrations.Migration) error {
	var original, guard bool
	for _, m := range input {
		if m.Version == 28 {
			original = true
		}
		if m.Version == 900005 {
			if guard || m.Name != "account_closure_replay_ack" || m.Checksum != "2d0a1bb1f24fd37fa10b69f9ca4da3835f8d2619666afba0a692088361937adb" || closureReplaySQLChecksum(m.UpSQL) != "2d0a1bb1f24fd37fa10b69f9ca4da3835f8d2619666afba0a692088361937adb" || closureReplaySQLChecksum(m.DownSQL) != "f8039071242eb9f1b13227677e6fe7b424fc654a3b698295bf31a457fe40f639" {
				return fmt.Errorf("account closure acknowledgement migration identity mismatch")
			}
			guard = true
		}
	}
	if original != guard {
		return fmt.Errorf("account closure cleanup and permanent acknowledgement must be installed together")
	}
	return nil
}

func closureReplaySQLChecksum(s string) string {
	digest := sha256.Sum256([]byte(strings.ReplaceAll(s, "\r\n", "\n")))
	return hex.EncodeToString(digest[:])
}
