package billing

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestReconciliationReferenceSQLTrimMatchesGo(t *testing.T) {
	set := map[rune]bool{}
	for _, r := range reconciliationReferenceTrimCharacters {
		if set[r] {
			t.Fatal("duplicate SQL trim character")
		}
		set[r] = true
	}
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if set[r] != unicode.IsSpace(r) {
			t.Fatalf("SQL trim differs from Go Unicode space at U+%04X", r)
		}
		if set[r] && strings.TrimSpace(string(r)+"reference"+string(r)) != "reference" {
			t.Fatalf("Go reference trimming differs at U+%04X", r)
		}
	}
}

func TestReconciliationCanonicalReferenceCollisionRemainsRejected(t *testing.T) {
	rows := []ProviderBillEntry{{EntryType: "REFUND", ProviderReferenceID: "\u3000same\u0085", MerchantOrderNo: "order", AmountFen: 100, Currency: "CNY", Status: "SUCCESS"}}
	facts := []ReconciliationInternalFact{
		{EntryType: "REFUND", ProviderReferenceID: strings.TrimSpace(" same "), MerchantOrderNo: "order", AmountFen: 100, Currency: "CNY", LedgerPresent: true, LedgerValid: true, EventDateVerified: true},
		{EntryType: "REFUND", ProviderReferenceID: strings.TrimSpace("\u3000same\u0085"), MerchantOrderNo: "order", AmountFen: 100, Currency: "CNY", LedgerPresent: true, LedgerValid: true, EventDateVerified: true},
	}
	differences := CompareFourWayReconciliation(rows, facts)
	if len(differences) != 1 || differences[0].CaseType != "DUPLICATE_TRANSACTION" || differences[0].Severity != "P0" {
		t.Fatalf("canonical collision did not fail closed: %+v", differences)
	}
}
