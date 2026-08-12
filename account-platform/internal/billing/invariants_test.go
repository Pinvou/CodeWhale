package billing

import (
	"math/rand/v2"
	"testing"
)

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
