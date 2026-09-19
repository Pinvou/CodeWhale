package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// LedgerRepairResult is the closed domain result of one committed ledger
// repair. Every field is derived from the frozen owner facts, never from the
// request body.
type LedgerRepairResult struct {
	CaseID                 uuid.UUID `json:"case_id"`
	TargetJournalEntryID   uuid.UUID `json:"target_journal_entry_id"`
	TargetPostingID        uuid.UUID `json:"target_posting_id"`
	WrongLedgerAccountID   uuid.UUID `json:"wrong_ledger_account_id"`
	CorrectLedgerAccountID uuid.UUID `json:"correct_ledger_account_id"`
	EntryType              string    `json:"entry_type"`
	AmountMicrocredits     int64     `json:"amount_microcredits"`
	PostingSetDigest       string    `json:"posting_set_digest"`
	JournalEntryID         uuid.UUID `json:"journal_entry_id"`
	Status                 string    `json:"status"`
}

// ledgerRepairCommand is the closed command name shared by the proof
// authority, the activation projection and the consumer mapping.
const ledgerRepairCommand = "LEDGER_REPAIR"

// ledgerRepairProofInput is the closed request-assembly projection shared with
// the authoritative proof recomputation. It is deliberately not
// activation-gated: a request may be assembled while the command is INACTIVE,
// and only proof issuance and execution require the live ACTIVE row.
func (r *PostgresRepository) LedgerRepairProofInput(ctx context.Context, target LedgerRepairCommandInput, reasonCode string) (approvalencoding.LegacyProofInput, error) {
	if strings.TrimSpace(reasonCode) == "" || strings.TrimSpace(reasonCode) != reasonCode || len(reasonCode) > 80 {
		return approvalencoding.LegacyProofInput{}, ErrConflict
	}
	snapshot, err := r.ApprovalCommandActivations(ctx)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	var activation ApprovalCommandActivation
	for _, item := range snapshot.Activations {
		if item.CommandType == ledgerRepairCommand {
			activation = item
			break
		}
	}
	if activation.CommandType == "" {
		return approvalencoding.LegacyProofInput{}, ErrConflict
	}
	plan, err := r.LedgerRepairPlanForCommand(ctx, target, activation.PolicyVersion, int64(activation.SnapshotSchemaVersion), activation.Version)
	if err != nil {
		return approvalencoding.LegacyProofInput{}, err
	}
	critical, financial := ledgerRepairProofProjection(plan)
	return approvalencoding.LegacyProofInput{
		CommandType: ledgerRepairCommand, PolicyVersion: activation.PolicyVersion, SnapshotSchemaVersion: int(activation.SnapshotSchemaVersion),
		TargetResourceType: "LEDGER_POSTING", TargetResourceID: plan.TargetPostingID.String(),
		NormalizedCommand: encodeEntitlementCorrectionProjection(map[string]any{
			"target_journal_entry_id": plan.TargetJournalEntryID, "target_posting_id": plan.TargetPostingID,
			"wrong_ledger_account_id": plan.WrongLedgerAccountID, "correct_ledger_account_id": plan.CorrectLedgerAccountID,
			"amount_microcredits": plan.AmountMicrocredits, "reason_code": reasonCode,
		}),
		CriticalResourceVersions: critical,
		FinancialSnapshot:        financial,
	}, nil
}

// ledgerRepairProofProjection is the closed proof projection of one repair:
// the frozen critical revision and the financial facts Account stores on the
// approval. Both objects are recomputed by the owner domain, never supplied by
// the caller.
func ledgerRepairProofProjection(plan LedgerRepairPlan) ([]byte, []byte) {
	critical, _ := json.Marshal(map[string]any{
		"target_journal_entry_id": plan.TargetJournalEntryID.String(),
		"target_posting_id":       plan.TargetPostingID.String(),
		"posting_set_digest":      plan.PostingSetDigest,
		"entry_type":              plan.EntryType,
		"policy_version":          1,
	})
	financial, _ := json.Marshal(map[string]any{
		"amount_microcredits":       plan.AmountMicrocredits,
		"wrong_ledger_account_id":   plan.WrongLedgerAccountID.String(),
		"correct_ledger_account_id": plan.CorrectLedgerAccountID.String(),
		"wrong_account_type":        plan.WrongLedgerAccountType,
		"correct_account_type":      plan.CorrectLedgerAccountType,
		"wallet_id":                 nil,
	})
	return critical, financial
}

// LedgerRepairCommandInput is the closed command the approval froze. Account
// only routes these identifiers; every amount and account type is recomputed
// from the ledger by the proof.
type LedgerRepairCommandInput struct {
	TargetJournalEntryID   uuid.UUID
	TargetPostingID        uuid.UUID
	WrongLedgerAccountID   uuid.UUID
	CorrectLedgerAccountID uuid.UUID
	AmountMicrocredits     int64
}

// LedgerRepairPlan is the closed, recomputable owner projection of one
// attribution-error repair.
type LedgerRepairPlan struct {
	TargetJournalEntryID     uuid.UUID `json:"target_journal_entry_id"`
	TargetPostingID          uuid.UUID `json:"target_posting_id"`
	EntryType                string    `json:"entry_type"`
	AmountMicrocredits       int64     `json:"amount_microcredits"`
	WrongLedgerAccountID     uuid.UUID `json:"wrong_ledger_account_id"`
	WrongLedgerAccountType   string    `json:"wrong_ledger_account_type"`
	CorrectLedgerAccountID   uuid.UUID `json:"correct_ledger_account_id"`
	CorrectLedgerAccountType string    `json:"correct_ledger_account_type"`
	PostingSetDigest         string    `json:"posting_set_digest"`
	FactsDigest              string    `json:"facts_digest"`
}

// LedgerRepairFrozenFacts is the owner revision an approval froze. FactsDigest
// is an owner-only recomputation input; a consumed authorization leaves it
// empty and the execution transaction recomputes and stores it. Any caller that
// does present a digest must match the recomputed one exactly.
type LedgerRepairFrozenFacts struct {
	TargetJournalEntryID   uuid.UUID
	TargetPostingID        uuid.UUID
	WrongLedgerAccountID   uuid.UUID
	CorrectLedgerAccountID uuid.UUID
	EntryType              string
	AmountMicrocredits     int64
	PostingSetDigest       string
	FactsDigest            string
}

// LedgerRepairExecutionInput carries the approval identity plus the frozen
// owner facts and the execution idempotency key.
type LedgerRepairExecutionInput struct {
	ApprovalRequestID    uuid.UUID
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	ExecutionKey         string
	PolicyVersion        int64
	SchemaVersion        int64
	ActivationVersion    int64
	Command              LedgerRepairCommandInput
	Frozen               LedgerRepairFrozenFacts
}

// ledgerRepairContractLeg freezes one account-type leg of an entry type.
// Sign 0 means "any non-zero delta in either direction" (the ADJUSTMENT pair
// legitimately appears in both directions); Optional legs may be absent when
// the frozen amount is zero.
type ledgerRepairContractLeg struct {
	AccountType string
	Sign        int64
	Optional    bool
}

// ledgerRepairContracts is the frozen account-type contract table of the
// GSA-23 design: the account types and net directions the existing writers of
// each entry type produce. A ledger repair may only move one platform-account
// posting back inside this contract.
var ledgerRepairContracts = map[string][]ledgerRepairContractLeg{
	"RECHARGE":      {{AccountType: "PAYMENT_CLEARING", Sign: -1}, {AccountType: "WALLET_LIABILITY", Sign: 1}},
	"USAGE":         {{AccountType: "USAGE_REVENUE", Sign: 1}, {AccountType: "WALLET_LIABILITY", Sign: -1}, {AccountType: "RECEIVABLE_ASSET", Sign: -1, Optional: true}},
	"ADJUSTMENT":    {{AccountType: "ADJUSTMENT", Sign: 0}, {AccountType: "WALLET_LIABILITY", Sign: 0}},
	"REFUND":        {{AccountType: "REFUND_CLEARING", Sign: 1}, {AccountType: "WALLET_LIABILITY", Sign: -1}, {AccountType: "RECEIVABLE_ASSET", Sign: -1, Optional: true}},
	"EXPIRY":        {{AccountType: "ADJUSTMENT", Sign: 1}, {AccountType: "WALLET_LIABILITY", Sign: -1}},
	"CHARGEBACK":    {{AccountType: "CHARGEBACK_CLEARING", Sign: 1}, {AccountType: "WALLET_LIABILITY", Sign: -1}, {AccountType: "RECEIVABLE_ASSET", Sign: -1, Optional: true}},
	"REPAIR_CREDIT": {{AccountType: "PAYMENT_CLEARING", Sign: -1}, {AccountType: "WALLET_LIABILITY", Sign: 1}, {AccountType: "RECEIVABLE_ASSET", Sign: 1, Optional: true}},
	"FORCED_REFUND": {{AccountType: "REFUND_CLEARING", Sign: 1}, {AccountType: "WALLET_LIABILITY", Sign: -1}, {AccountType: "RECEIVABLE_ASSET", Sign: -1, Optional: true}},
}

type ledgerRepairPosting struct {
	ID              uuid.UUID
	LedgerAccountID uuid.UUID
	AccountType     string
	WalletID        *uuid.UUID
	Delta           int64
	CreditLotID     *uuid.UUID
}

type ledgerRepairTarget struct {
	JournalEntryID    uuid.UUID
	EntryType         string
	BusinessReference string
	CreatedAt         time.Time
	Postings          []ledgerRepairPosting
}

// LedgerRepairPlanForCommand recomputes the closed repair projection from the
// ledger. It creates no fact and refuses every input the frozen contract does
// not admit with ErrApprovalSnapshotMismatch.
func (r *PostgresRepository) LedgerRepairPlanForCommand(ctx context.Context, input LedgerRepairCommandInput, policyVersion, schemaVersion, activationVersion int64) (LedgerRepairPlan, error) {
	if r == nil || r.pool == nil {
		return LedgerRepairPlan{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return LedgerRepairPlan{}, err
	}
	defer tx.Rollback(ctx)
	plan, err := ledgerRepairPlanFrom(ctx, tx, input, policyVersion, schemaVersion, activationVersion)
	if err != nil {
		return LedgerRepairPlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LedgerRepairPlan{}, err
	}
	return plan, nil
}

type ledgerRepairReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func ledgerRepairPlanFrom(ctx context.Context, source ledgerRepairReader, input LedgerRepairCommandInput, policyVersion, schemaVersion, activationVersion int64) (LedgerRepairPlan, error) {
	if input.TargetJournalEntryID == uuid.Nil || input.TargetPostingID == uuid.Nil ||
		input.WrongLedgerAccountID == uuid.Nil || input.CorrectLedgerAccountID == uuid.Nil ||
		input.WrongLedgerAccountID == input.CorrectLedgerAccountID || input.AmountMicrocredits < 1 {
		return LedgerRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	target, err := ledgerRepairReadTarget(ctx, source, input.TargetJournalEntryID)
	if err != nil {
		return LedgerRepairPlan{}, err
	}
	contract, ok := ledgerRepairContracts[target.EntryType]
	if !ok {
		return LedgerRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var targetPosting *ledgerRepairPosting
	for index := range target.Postings {
		if target.Postings[index].ID == input.TargetPostingID {
			targetPosting = &target.Postings[index]
			break
		}
	}
	if targetPosting == nil || targetPosting.Delta == 0 || absInt64(targetPosting.Delta) != input.AmountMicrocredits {
		return LedgerRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	if targetPosting.LedgerAccountID != input.WrongLedgerAccountID || targetPosting.WalletID != nil || targetPosting.CreditLotID != nil {
		return LedgerRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	var alreadyRepaired bool
	if err := source.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ledger_repair_cases WHERE target_posting_id=$1)`, targetPosting.ID).Scan(&alreadyRepaired); err != nil {
		return LedgerRepairPlan{}, err
	}
	if alreadyRepaired {
		return LedgerRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	correct, err := ledgerRepairPlatformAccount(ctx, source, input.CorrectLedgerAccountID)
	if err != nil {
		return LedgerRepairPlan{}, err
	}
	if correct.WalletID != nil {
		return LedgerRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	currentSignature := map[string]int64{}
	for _, posting := range target.Postings {
		currentSignature[posting.AccountType] += posting.Delta
	}
	if ledgerRepairSignatureMatches(contract, currentSignature) {
		return LedgerRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	repaired := map[string]int64{}
	for accountType, delta := range currentSignature {
		repaired[accountType] = delta
	}
	repaired[targetPosting.AccountType] -= targetPosting.Delta
	if repaired[targetPosting.AccountType] == 0 {
		delete(repaired, targetPosting.AccountType)
	}
	repaired[correct.AccountType] += targetPosting.Delta
	if !ledgerRepairSignatureMatches(contract, repaired) {
		return LedgerRepairPlan{}, ErrApprovalSnapshotMismatch
	}
	postingSetDigest := ledgerRepairPostingSetDigest(target)
	factsDigest := ledgerRepairFactsDigest(target, *targetPosting, correct, input, policyVersion, schemaVersion, activationVersion, postingSetDigest)
	return LedgerRepairPlan{
		TargetJournalEntryID:     target.JournalEntryID,
		TargetPostingID:          targetPosting.ID,
		EntryType:                target.EntryType,
		AmountMicrocredits:       absInt64(targetPosting.Delta),
		WrongLedgerAccountID:     targetPosting.LedgerAccountID,
		WrongLedgerAccountType:   targetPosting.AccountType,
		CorrectLedgerAccountID:   correct.ID,
		CorrectLedgerAccountType: correct.AccountType,
		PostingSetDigest:         postingSetDigest,
		FactsDigest:              factsDigest,
	}, nil
}

func ledgerRepairReadTarget(ctx context.Context, source ledgerRepairReader, journalEntryID uuid.UUID) (ledgerRepairTarget, error) {
	target := ledgerRepairTarget{JournalEntryID: journalEntryID}
	if err := source.QueryRow(ctx, `SELECT entry_type,business_reference,created_at FROM journal_entries WHERE id=$1`, journalEntryID).
		Scan(&target.EntryType, &target.BusinessReference, &target.CreatedAt); err != nil {
		return ledgerRepairTarget{}, ledgerRepairLookupError(err)
	}
	rows, err := source.Query(ctx, `SELECT posting.id,posting.ledger_account_id,account.account_type,account.wallet_id,posting.delta_microcredits,posting.credit_lot_id
FROM ledger_postings posting JOIN ledger_accounts account ON account.id=posting.ledger_account_id
WHERE posting.journal_entry_id=$1 ORDER BY posting.id`, journalEntryID)
	if err != nil {
		return ledgerRepairTarget{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var posting ledgerRepairPosting
		if err := rows.Scan(&posting.ID, &posting.LedgerAccountID, &posting.AccountType, &posting.WalletID, &posting.Delta, &posting.CreditLotID); err != nil {
			return ledgerRepairTarget{}, err
		}
		target.Postings = append(target.Postings, posting)
	}
	if err := rows.Err(); err != nil {
		return ledgerRepairTarget{}, err
	}
	return target, nil
}

type ledgerRepairAccount struct {
	ID          uuid.UUID
	Code        string
	AccountType string
	WalletID    *uuid.UUID
}

func ledgerRepairPlatformAccount(ctx context.Context, source ledgerRepairReader, accountID uuid.UUID) (ledgerRepairAccount, error) {
	var account ledgerRepairAccount
	err := source.QueryRow(ctx, `SELECT id,code,account_type,wallet_id FROM ledger_accounts WHERE id=$1`, accountID).
		Scan(&account.ID, &account.Code, &account.AccountType, &account.WalletID)
	if err != nil {
		return ledgerRepairAccount{}, ledgerRepairLookupError(err)
	}
	return account, nil
}

func ledgerRepairLookupError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrApprovalSnapshotMismatch
	}
	return err
}

// ledgerRepairSignatureMatches evaluates the frozen contract against an
// account-type signature: every present account type must belong to the
// contract, every mandatory leg must be present with the frozen direction, and
// an account type may only appear once.
func ledgerRepairSignatureMatches(contract []ledgerRepairContractLeg, signature map[string]int64) bool {
	allowed := map[string]ledgerRepairContractLeg{}
	for _, leg := range contract {
		allowed[leg.AccountType] = leg
	}
	for accountType, delta := range signature {
		if delta == 0 {
			continue
		}
		leg, ok := allowed[accountType]
		if !ok {
			return false
		}
		if leg.Sign != 0 && ((leg.Sign > 0 && delta < 0) || (leg.Sign < 0 && delta > 0)) {
			return false
		}
	}
	for _, leg := range contract {
		delta, present := signature[leg.AccountType]
		if !present {
			if leg.Optional {
				continue
			}
			return false
		}
		if delta == 0 {
			return false
		}
	}
	return true
}

func ledgerRepairPostingSetDigest(target ledgerRepairTarget) string {
	postings := make([]map[string]any, 0, len(target.Postings))
	sorted := append([]ledgerRepairPosting(nil), target.Postings...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID.String() < sorted[j].ID.String() })
	for _, posting := range sorted {
		postings = append(postings, map[string]any{
			"id": posting.ID.String(), "ledger_account_id": posting.LedgerAccountID.String(),
			"delta_microcredits": posting.Delta, "credit_lot_id": ledgerRepairOptionalID(posting.CreditLotID),
		})
	}
	encoded, err := json.Marshal(map[string]any{
		"entry_type": target.EntryType, "business_reference": target.BusinessReference,
		"created_at": target.CreatedAt.UTC().Format(time.RFC3339Nano), "postings": postings,
	})
	if err != nil {
		return ""
	}
	return digestHex(encoded)
}

func ledgerRepairFactsDigest(target ledgerRepairTarget, posting ledgerRepairPosting, correct ledgerRepairAccount,
	input LedgerRepairCommandInput, policyVersion, schemaVersion, activationVersion int64, postingSetDigest string) string {
	encoded, err := json.Marshal(map[string]any{
		"command_type":                 "LEDGER_REPAIR",
		"target_journal_entry_id":      target.JournalEntryID.String(),
		"target_entry_type":            target.EntryType,
		"target_business_reference":    target.BusinessReference,
		"target_posting_id":            posting.ID.String(),
		"target_posting_delta":         posting.Delta,
		"target_posting_account_type":  posting.AccountType,
		"target_posting_credit_lot_id": ledgerRepairOptionalID(posting.CreditLotID),
		"correct_account_id":           correct.ID.String(),
		"correct_account_code":         correct.Code,
		"correct_account_type":         correct.AccountType,
		"posting_set_digest":           postingSetDigest,
		"amount_microcredits":          absInt64(posting.Delta),
		"policy_version":               policyVersion,
		"snapshot_schema_version":      schemaVersion,
		"activation_version":           activationVersion,
	})
	if err != nil || input.CorrectLedgerAccountID != correct.ID {
		return ""
	}
	return digestHex(encoded)
}

func ledgerRepairOptionalID(value *uuid.UUID) any {
	if value == nil {
		return nil
	}
	return value.String()
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// ApplyLedgerRepair commits exactly one attribution correction: one REVERSAL
// entry with two opposite platform-account postings plus the immutable case
// row. It is idempotent per approval and per target posting, and it never
// touches wallet balances, credit lots or entitlement facts.
func (r *PostgresRepository) ApplyLedgerRepair(ctx context.Context, input LedgerRepairExecutionInput) (LedgerRepairResult, error) {
	executionKey := strings.TrimSpace(input.ExecutionKey)
	if r == nil || r.pool == nil || input.ApprovalRequestID == uuid.Nil || input.AuthorizationEventID == uuid.Nil ||
		input.ApprovalEpoch < 1 || executionKey == "" || executionKey != input.ExecutionKey ||
		input.PolicyVersion < 1 || input.SchemaVersion < 1 || input.ActivationVersion < 1 {
		return LedgerRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return LedgerRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	if stored, err := readLedgerRepairCase(ctx, tx, input.ApprovalRequestID); err == nil {
		if !stored.matchesFrozen(input, executionKey) {
			return LedgerRepairResult{}, ErrConflict
		}
		return stored.LedgerRepairResult, tx.Commit(ctx)
	} else if !errors.Is(err, ErrNotFound) {
		return LedgerRepairResult{}, err
	}
	plan, err := ledgerRepairPlanFrom(ctx, tx, input.Command, input.PolicyVersion, input.SchemaVersion, input.ActivationVersion)
	if err != nil {
		return LedgerRepairResult{}, err
	}
	if mismatch := ledgerRepairFrozenMismatch(plan, input.Frozen); mismatch != "" {
		return LedgerRepairResult{}, fmt.Errorf("%w: %s", ErrApprovalSnapshotMismatch, mismatch)
	}
	caseID := uuid.Must(uuid.NewV7())
	journalID := uuid.Must(uuid.NewV7())
	reference := "ledger-repair:" + plan.TargetPostingID.String()
	now := r.now()
	if err := insertJournal(ctx, tx, journalID, "REVERSAL", reference, "ledger repair: "+plan.EntryType, now); err != nil {
		return LedgerRepairResult{}, err
	}
	amount := plan.AmountMicrocredits
	if err := insertPosting(ctx, tx, journalID, plan.WrongLedgerAccountID, -amount, nil, now); err != nil {
		return LedgerRepairResult{}, err
	}
	if err := insertPosting(ctx, tx, journalID, plan.CorrectLedgerAccountID, amount, nil, now); err != nil {
		return LedgerRepairResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_repair_cases(id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,
target_journal_entry_id,target_posting_id,wrong_ledger_account_id,correct_ledger_account_id,entry_type,amount_microcredits,
posting_set_digest,facts_digest,journal_entry_id,state,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'EXECUTED',$15)`,
		caseID, input.ApprovalRequestID, input.AuthorizationEventID, input.ApprovalEpoch, ledgerRepairExecutionKeyDigest(executionKey),
		plan.TargetJournalEntryID, plan.TargetPostingID, plan.WrongLedgerAccountID, plan.CorrectLedgerAccountID,
		plan.EntryType, amount, plan.PostingSetDigest, plan.FactsDigest, journalID, now); err != nil {
		if ledgerRepairUniqueViolation(err) {
			// A different approval already corrected this posting: never create a
			// second domain effect, only report the conflict.
			if stored, readErr := readLedgerRepairCaseByPosting(ctx, tx, plan.TargetPostingID); readErr == nil && stored.matchesFrozen(input, executionKey) {
				return stored.LedgerRepairResult, tx.Commit(ctx)
			}
			return LedgerRepairResult{}, ErrConflict
		}
		return LedgerRepairResult{}, err
	}
	// The target journal is immutable, so the post-effect proof is that its
	// posting set digest is byte-identical to the frozen revision.
	confirmed, err := ledgerRepairReadTarget(ctx, tx, plan.TargetJournalEntryID)
	if err != nil {
		return LedgerRepairResult{}, err
	}
	if ledgerRepairPostingSetDigest(confirmed) != plan.PostingSetDigest {
		return LedgerRepairResult{}, ErrApprovalSnapshotMismatch
	}
	result := LedgerRepairResult{
		CaseID: caseID, TargetJournalEntryID: plan.TargetJournalEntryID, TargetPostingID: plan.TargetPostingID,
		WrongLedgerAccountID: plan.WrongLedgerAccountID, CorrectLedgerAccountID: plan.CorrectLedgerAccountID,
		EntryType: plan.EntryType, AmountMicrocredits: amount, PostingSetDigest: plan.PostingSetDigest,
		JournalEntryID: journalID, Status: "SUCCEEDED",
	}
	if err := tx.Commit(ctx); err != nil {
		return LedgerRepairResult{}, err
	}
	stored, err := r.LedgerRepairForRequest(ctx, input.ApprovalRequestID)
	if err != nil || stored != result {
		if err == nil {
			err = ErrConflict
		}
		return LedgerRepairResult{}, err
	}
	return stored, nil
}

// LedgerRepairForRequest reads the committed repair of one approval request
// without creating any new fact (result-first).
func (r *PostgresRepository) LedgerRepairForRequest(ctx context.Context, approvalRequestID uuid.UUID) (LedgerRepairResult, error) {
	if r == nil || r.pool == nil || approvalRequestID == uuid.Nil {
		return LedgerRepairResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return LedgerRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	stored, err := readLedgerRepairCase(ctx, tx, approvalRequestID)
	if err != nil {
		return LedgerRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LedgerRepairResult{}, err
	}
	return stored.LedgerRepairResult, nil
}

type ledgerRepairCaseRecord struct {
	LedgerRepairResult
	ApprovalRequestID    uuid.UUID
	AuthorizationEventID uuid.UUID
	ApprovalEpoch        int64
	ExecutionKeyDigest   string
}

func (record ledgerRepairCaseRecord) matchesFrozen(input LedgerRepairExecutionInput, executionKey string) bool {
	return record.ApprovalRequestID == input.ApprovalRequestID && record.AuthorizationEventID == input.AuthorizationEventID &&
		record.ApprovalEpoch == input.ApprovalEpoch && record.ExecutionKeyDigest == ledgerRepairExecutionKeyDigest(executionKey) &&
		record.TargetJournalEntryID == input.Frozen.TargetJournalEntryID && record.TargetPostingID == input.Frozen.TargetPostingID &&
		record.WrongLedgerAccountID == input.Frozen.WrongLedgerAccountID && record.CorrectLedgerAccountID == input.Frozen.CorrectLedgerAccountID &&
		record.EntryType == input.Frozen.EntryType && record.AmountMicrocredits == input.Frozen.AmountMicrocredits &&
		record.PostingSetDigest == input.Frozen.PostingSetDigest
}

func readLedgerRepairCase(ctx context.Context, source ledgerRepairReader, approvalRequestID uuid.UUID) (ledgerRepairCaseRecord, error) {
	var record ledgerRepairCaseRecord
	err := source.QueryRow(ctx, `SELECT id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,
target_journal_entry_id,target_posting_id,wrong_ledger_account_id,correct_ledger_account_id,entry_type,amount_microcredits,
posting_set_digest,journal_entry_id FROM ledger_repair_cases WHERE approval_request_id=$1`, approvalRequestID).Scan(
		&record.CaseID, &record.ApprovalRequestID, &record.AuthorizationEventID, &record.ApprovalEpoch, &record.ExecutionKeyDigest,
		&record.TargetJournalEntryID, &record.TargetPostingID, &record.WrongLedgerAccountID, &record.CorrectLedgerAccountID,
		&record.EntryType, &record.AmountMicrocredits, &record.PostingSetDigest, &record.JournalEntryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledgerRepairCaseRecord{}, ErrNotFound
	}
	if err != nil {
		return ledgerRepairCaseRecord{}, err
	}
	record.Status = "SUCCEEDED"
	return record, nil
}

func readLedgerRepairCaseByPosting(ctx context.Context, source ledgerRepairReader, targetPostingID uuid.UUID) (ledgerRepairCaseRecord, error) {
	var record ledgerRepairCaseRecord
	err := source.QueryRow(ctx, `SELECT id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,
target_journal_entry_id,target_posting_id,wrong_ledger_account_id,correct_ledger_account_id,entry_type,amount_microcredits,
posting_set_digest,journal_entry_id FROM ledger_repair_cases WHERE target_posting_id=$1`, targetPostingID).Scan(
		&record.CaseID, &record.ApprovalRequestID, &record.AuthorizationEventID, &record.ApprovalEpoch, &record.ExecutionKeyDigest,
		&record.TargetJournalEntryID, &record.TargetPostingID, &record.WrongLedgerAccountID, &record.CorrectLedgerAccountID,
		&record.EntryType, &record.AmountMicrocredits, &record.PostingSetDigest, &record.JournalEntryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledgerRepairCaseRecord{}, ErrNotFound
	}
	if err != nil {
		return ledgerRepairCaseRecord{}, err
	}
	record.Status = "SUCCEEDED"
	return record, nil
}

func ledgerRepairUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

func ledgerRepairFrozenMismatch(plan LedgerRepairPlan, frozen LedgerRepairFrozenFacts) string {
	switch {
	case plan.TargetJournalEntryID != frozen.TargetJournalEntryID:
		return "target_journal_entry_id"
	case plan.TargetPostingID != frozen.TargetPostingID:
		return "target_posting_id"
	case plan.WrongLedgerAccountID != frozen.WrongLedgerAccountID:
		return "wrong_ledger_account_id"
	case plan.CorrectLedgerAccountID != frozen.CorrectLedgerAccountID:
		return "correct_ledger_account_id"
	case plan.EntryType != frozen.EntryType:
		return "entry_type"
	case plan.AmountMicrocredits != frozen.AmountMicrocredits:
		return "amount_microcredits"
	case plan.PostingSetDigest != frozen.PostingSetDigest:
		return "posting_set_digest"
	// The consumed authorization freezes only the wire-visible owner
	// projection; the owner-only facts digest is recomputed inside the
	// execution transaction, so an empty value is not a caller claim.
	case frozen.FactsDigest == "":
		return ""
	case plan.FactsDigest != frozen.FactsDigest:
		return "facts_digest"
	}
	return ""
}

func ledgerRepairExecutionKeyDigest(key string) string {
	encoded, err := json.Marshal(map[string]any{"command_type": "LEDGER_REPAIR", "execution_key": strings.TrimSpace(key)})
	if err != nil {
		return ""
	}
	return digestHex(encoded)
}
