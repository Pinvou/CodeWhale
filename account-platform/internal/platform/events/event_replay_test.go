package events

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestReplayRequestHashSupportsRetryButRejectsSemanticChange(t *testing.T) {
	request := ReplayRequest{EventID: uuid.New(), ExpectedDigest: strings.Repeat("a", 64), IdempotencyKey: "stable-replay-key", Reason: "approved recovery after root cause fixed"}
	identity := serviceauth.Identity{ActorType: "SERVICE", Service: "event-replay", JobType: "EVENT_REPLAY", JobRunID: "first-run"}
	first := replayRequestHash(request, identity)
	identity.JobRunID = "retry-run"
	if replayRequestHash(request, identity) != first {
		t.Fatal("job retry changed the semantic command fingerprint")
	}
	request.Reason = "different audited recovery decision"
	if replayRequestHash(request, identity) == first {
		t.Fatal("semantic change reused a replay request fingerprint")
	}
}
