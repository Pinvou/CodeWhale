package account

import "github.com/google/uuid"

type approvalReceivableWriteOffSummary struct {
	ExpectedReceivableVersion    string    `json:"expected_receivable_version"`
	ReceivableID                 uuid.UUID `json:"receivable_id"`
	SourceType                   string    `json:"source_type"`
	SourceReference              string    `json:"source_reference"`
	OriginalMicrocredits         string    `json:"original_microcredits"`
	SettledMicrocredits          string    `json:"settled_microcredits"`
	OutstandingMicrocredits      string    `json:"outstanding_microcredits"`
	WriteOffMicrocredits         string    `json:"write_off_microcredits"`
	WalletReceivableMicrocredits string    `json:"wallet_receivable_microcredits"`
	FactsDigest                  string    `json:"facts_digest"`
	ReasonCode                   string    `json:"reason_code"`
}
