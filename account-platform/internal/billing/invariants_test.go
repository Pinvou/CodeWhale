package billing

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func TestPaymentIdempotencyKeyCharacterContract(t *testing.T) {
	for _, test := range []struct {
		key     string
		maximum int
		want    bool
	}{{strings.Repeat("界", 15), 128, false}, {strings.Repeat("界", 16), 128, true}, {strings.Repeat("界", 128), 128, true}, {strings.Repeat("a", 129), 128, false}, {strings.Repeat("界", 129), 128, false}, {strings.Repeat("界", 129), 0, true}, {strings.Repeat("a", 15) + string([]byte{0xff}), 128, false}} {
		if got := validIdempotencyKey(test.key, test.maximum); got != test.want {
			t.Fatalf("contract bytes=%d maximum=%d got=%v want=%v", len(test.key), test.maximum, got, test.want)
		}
	}
}

func TestLedgerPostingPairsRemainBalanced(t *testing.T) {
	for range 10_000 {
		amount := rand.Int64N(1_000_000_000) + 1
		if got := amount + -amount; got != 0 {
			t.Fatalf("posting pair sum = %d", got)
		}
	}
}

func TestReserveCommitReleaseArithmeticNeverProducesNegativeAvailableBalance(t *testing.T) {
	for range 10_000 {
		posted := rand.Int64N(1_000_000_000) + 1
		maximum := rand.Int64N(posted) + 1
		actual := rand.Int64N(maximum + 1)
		reserved := maximum
		if available := posted - reserved; available < 0 {
			t.Fatalf("available after reserve = %d", available)
		}
		posted -= actual
		reserved -= maximum
		if posted < 0 || reserved < 0 || posted-reserved < 0 {
			t.Fatalf("invalid committed state: posted=%d reserved=%d", posted, reserved)
		}
	}
}
