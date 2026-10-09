package account

import (
	"encoding/csv"
	"strings"
	"testing"
)

func f17CSVPrefixCases() []struct{ name, value, want string } {
	return []struct{ name, value, want string }{
		{"equals", "=1+1", "'=1+1"},
		{"plus", "+SUM(1)", "'+SUM(1)"},
		{"minus", "-2", "'-2"},
		{"at", "@SUM(1)", "'@SUM(1)"},
		{"tab", "\t=1+1", "'\t=1+1"},
		{"carriage-return", "\r=1+1", "'\r=1+1"},
		{"line-feed", "\n=1+1", "'\n=1+1"},
		{"wide-equals", "＝1+1", "'＝1+1"},
		{"wide-plus", "＋1", "'＋1"},
		{"wide-minus", "－2", "'－2"},
		{"wide-at", "＠SUM(1)", "'＠SUM(1)"},
		{"empty", "", ""},
		{"ordinary-English", "Operator Name", "Operator Name"},
		{"ordinary-Chinese", "审计操作人", "审计操作人"},
		{"ordinary-Japanese", "監査担当者", "監査担当者"},
		{"ordinary-emoji", "🙂 Name", "🙂 Name"},
		{"internal-symbol", "Name =1", "Name =1"},
		{"existing-quote", "'=1", "'=1"},
		{"internal-control", "Name\ncontinued", "Name\ncontinued"},
	}
}

func TestF17AuditCSVPrefixProtection(t *testing.T) {
	for _, tc := range f17CSVPrefixCases() {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeAuditCSVCell(tc.value); got != tc.want {
				t.Fatalf("expected original quote-prefix policy: got %q, want %q", got, tc.want)
			}
		})
	}
}

// The production writer calls this helper for every record cell, then uses
// encoding/csv. This test checks that combination and source bytes, not an
// Excel/LibreOffice import or save/reopen guarantee.
func TestF17AuditCSVPrefixWriterRoundTrip(t *testing.T) {
	var output strings.Builder
	writer := csv.NewWriter(&output)
	expected := make([][]string, 0)
	for _, tc := range f17CSVPrefixCases() {
		row := []string{tc.name, safeAuditCSVCell(tc.value), safeAuditCSVCell("ordinary, \"quoted\" text")}
		if err := writer.Write(row); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, []string{tc.name, tc.want, "ordinary, \"quoted\" text"})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(output.String())).ReadAll()
	if err != nil || len(rows) != len(expected) {
		t.Fatal("CSV round trip", err)
	}
	for i, row := range rows {
		for j, cell := range row {
			if cell != expected[i][j] {
				t.Fatalf("cell %d/%d changed: got %q, want %q", i, j, cell, expected[i][j])
			}
		}
	}
}
