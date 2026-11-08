package billingschema

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Historical ABORTED attempts need no invented money effects. They exercise
// both actual runtime purgers, financial holds, bounds and immutable evidence.
func TestPaymentCreditRetentionPublicPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned schema database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	ac := f.database(t, ctx, "account", "account_service")
	bc := f.database(t, ctx, "billing", "billing_schema_owner")
	am, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	bm, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"account", "billing"} {
		input := am
		if which == "billing" {
			input = bm
		}
		mutated := append([]migrations.Migration(nil), input...)
		for i := range mutated {
			if which == "account" && mutated[i].Version == 95 {
				mutated[i].DownSQL += "\nSELECT 1;"
			}
			if which == "billing" && mutated[i].Version == 900039 {
				mutated[i].UpSQL += "\nSELECT 1;"
			}
		}
		var applyErr error
		var absent bool
		if which == "account" {
			applyErr = accountschema.Up(ctx, ac, mutated)
			err = ac.QueryRow(ctx, `SELECT to_regclass('public.users') IS NULL`).Scan(&absent)
		} else {
			applyErr = Up(ctx, bc, mutated)
			err = bc.QueryRow(ctx, `SELECT to_regclass('public.wallets') IS NULL`).Scan(&absent)
		}
		if applyErr == nil || !strings.Contains(applyErr.Error(), "frozen SQL") || err != nil || !absent {
			t.Fatal("tampered published package did not fail before domain DDL", which, applyErr, err)
		}
	}
	if err = accountschema.Up(ctx, ac, am); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, ac)
	if err = Up(ctx, bc, bm); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, bc)
	ar, err := account.NewPostgresRepository(ctx, f.serviceURL(t, ac, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	br, err := billing.NewPostgresRepository(ctx, f.serviceURL(t, bc, "billing_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	user, order := uuid.New(), uuid.New()
	now := time.Now().UTC()
	if _, err = ac.Exec(ctx, `WITH capability AS(SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,status,user_code,username,created_at,updated_at) SELECT $1,'ACTIVE',generate_unique_user_code(),$2,$3,$3 FROM capability`, user, "ret_"+strings.ReplaceAll(user.String(), "-", "")[:16], now); err != nil {
		t.Fatal(err)
	}
	wallet, _, err := br.EnsureWallet(ctx, "PERSONAL", user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bc.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'CNY_1','{}','ALIPAY',100,'CNY',100,'EXPIRED','PENDING',$5,$5,$5::timestamptz+interval '72 hours',$5::timestamptz+interval '8 days',$6,$6,'retention-fixture','v1')`, order, "ret-"+order.String(), user, wallet.ID, now.Add(-time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	current := paymentcredit.Request{ID: uuid.New(), UserID: user, OrderID: order, TransactionDigest: paymentcredit.Digest([]byte("current")), FactsDigest: paymentcredit.Digest([]byte("current-facts"))}
	if d, err := ar.AdmitPaymentCredit(ctx, current); err != nil || d.State != "ADMITTED" {
		t.Fatal("real current admission", d.State, err)
	}
	if err = ar.ReconcilePaymentCredits(ctx, &creditBoundaryResolver{repository: br}); err != nil {
		t.Fatal(err)
	}
	var accountTerm, billingTerm time.Time
	if err = ac.QueryRow(ctx, `SELECT retained_until FROM account_payment_credit_admissions WHERE id=$1`, current.ID).Scan(&accountTerm); err != nil {
		t.Fatal(err)
	}
	if err = bc.QueryRow(ctx, `SELECT retained_until FROM payment_credit_attempts WHERE id=$1`, current.ID).Scan(&billingTerm); err != nil {
		t.Fatal(err)
	}
	if accountTerm.Before(now.AddDate(3, 0, 0)) || billingTerm.Before(now.AddDate(3, 0, 0)) {
		t.Fatal("current resolution/ack did not retain a full three-year audit term")
	}
	if err = ar.ReconcilePaymentCredits(ctx, &creditBoundaryResolver{repository: br}); err != nil {
		t.Fatal(err)
	}
	var stable time.Time
	if err = ac.QueryRow(ctx, `SELECT retained_until FROM account_payment_credit_admissions WHERE id=$1`, current.ID).Scan(&stable); err != nil || !stable.Equal(accountTerm) {
		t.Fatal("replay arbitrarily extended retention", err)
	}
	seed := func(kind string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		input := paymentcredit.Request{ID: id, UserID: user, OrderID: order, TransactionDigest: paymentcredit.Digest([]byte(id.String())), FactsDigest: paymentcredit.Digest([]byte("historical-aborted"))}
		created := now.AddDate(-4, 0, 0)
		if kind == "early" {
			created = now.AddDate(-1, 0, 0)
		}
		resolvedValue := created.Add(time.Minute)
		ackValue := created.Add(2 * time.Minute)
		var resolved, ack, review *time.Time
		resolved = &resolvedValue
		ack = &ackValue
		accountState, billingState := "ABORTED", "ABORTED"
		hold := kind == "held"
		if hold {
			value := now.Add(24 * time.Hour)
			review = &value
		}
		if kind == "pending" {
			accountState, billingState = "PENDING", "PREPARED"
			resolved, ack = nil, nil
		}
		if kind == "unacknowledged" {
			ack = nil
		}
		anchor := created
		if resolved != nil {
			anchor = *resolved
		}
		if ack != nil {
			anchor = *ack
		}
		until := anchor.AddDate(3, 0, 0)
		accountAck := ack
		if kind == "ack-reply-lost" {
			accountAck = nil
		}
		if _, err := ac.Exec(ctx, `INSERT INTO account_payment_credit_admissions(id,user_id,payment_order_id,transaction_digest,facts_digest,fingerprint,state,created_at,resolved_at,billing_acknowledged_at,retained_until,legal_hold,hold_review_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, id, user, order, input.TransactionDigest, input.FactsDigest, input.Fingerprint(), accountState, created, resolved, accountAck, until, hold, review); err != nil {
			t.Fatal(err)
		}
		if _, err := bc.Exec(ctx, `INSERT INTO payment_credit_attempts(id,payer_user_id,payment_order_id,transaction_digest,facts_digest,fingerprint,state,created_at,resolved_at,account_acknowledged_at,retained_until,legal_hold,hold_review_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, id, user, order, input.TransactionDigest, input.FactsDigest, input.Fingerprint(), billingState, created, resolved, ack, until, hold, review); err != nil {
			t.Fatal(err)
		}
		return id
	}
	protected := []uuid.UUID{current.ID, seed("early"), seed("held"), seed("pending"), seed("unacknowledged")}
	var eligible []uuid.UUID
	for i := 0; i < 102; i++ {
		eligible = append(eligible, seed("expired"))
	}
	guarded := func(c *pgx.Conn, sql string, args ...any) {
		t.Helper()
		_, err := c.Exec(ctx, sql, args...)
		var failure *pgconn.PgError
		if !errorsAsPaymentCreditGuard(err, &failure) {
			t.Fatal("retention/immutability guard did not reject mutation", err)
		}
	}
	guarded(ac, `DELETE FROM account_payment_credit_admissions WHERE id=$1`, protected[1])
	guarded(bc, `DELETE FROM payment_credit_attempts WHERE id=$1`, protected[1])
	guarded(ac, `UPDATE account_payment_credit_admissions SET retained_until=retained_until-interval '1 day' WHERE id=$1`, protected[1])
	guarded(bc, `UPDATE payment_credit_attempts SET retained_until=retained_until-interval '1 day' WHERE id=$1`, protected[1])
	guarded(ac, `TRUNCATE account_payment_credit_admissions`)
	owner, err := bc.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	_, err = owner.Exec(ctx, `TRUNCATE public.payment_credit_attempts`)
	var failure *pgconn.PgError
	if !errorsAsPaymentCreditGuard(err, &failure) {
		t.Fatal("owner statement truncate bypassed evidence guard", err)
	}
	if err = owner.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	accountLock, err := ac.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer accountLock.Rollback(ctx)
	billingLock, err := bc.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer billingLock.Rollback(ctx)
	if _, err = accountLock.Exec(ctx, `SELECT id FROM account_payment_credit_admissions WHERE id=$1 FOR UPDATE`, eligible[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = billingLock.Exec(ctx, `SELECT id FROM payment_credit_attempts WHERE id=$1 FOR UPDATE`, eligible[0]); err != nil {
		t.Fatal(err)
	}
	count := func(c *pgx.Conn, table string) int {
		t.Helper()
		var n int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	purge := func() {
		t.Helper()
		if err := ar.PurgeExpiredPaymentCreditAdmissions(ctx); err != nil {
			t.Fatal(err)
		}
		if err := br.PurgeExpiredPaymentCreditAttempts(ctx, ar); err != nil {
			t.Fatal(err)
		}
	}
	purge()
	if count(ac, "account_payment_credit_admissions") != 7 || count(bc, "payment_credit_attempts") != 7 {
		t.Fatal("runtime purge exceeded100 or failed eligible rows")
	}
	purge()
	if count(ac, "account_payment_credit_admissions") != 6 || count(bc, "payment_credit_attempts") != 6 {
		t.Fatal("runtime purge did not skip locked row and continue")
	}
	if err = accountLock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = billingLock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	purge()
	if count(ac, "account_payment_credit_admissions") != 5 || count(bc, "payment_credit_attempts") != 5 {
		t.Fatal("released eligible row did not purge")
	}
	for _, id := range protected {
		var a, b int
		if err := ac.QueryRow(ctx, `SELECT count(*) FROM account_payment_credit_admissions WHERE id=$1`, id).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if err := bc.QueryRow(ctx, `SELECT count(*) FROM payment_credit_attempts WHERE id=$1`, id).Scan(&b); err != nil || a != 1 || b != 1 {
			t.Fatal("purge removed active/held/unknown evidence", err)
		}
	}
	if _, err = ac.Exec(ctx, `UPDATE account_payment_credit_admissions SET legal_hold=false,hold_review_at=NULL WHERE id=$1`, protected[2]); err != nil {
		t.Fatal(err)
	}
	if _, err = bc.Exec(ctx, `UPDATE payment_credit_attempts SET legal_hold=false,hold_review_at=NULL WHERE id=$1`, protected[2]); err != nil {
		t.Fatal(err)
	}
	purge()
	if count(ac, "account_payment_credit_admissions") != 4 || count(bc, "payment_credit_attempts") != 4 {
		t.Fatal("expired released hold did not clear")
	}
	// Four nonempty batches clear 103 admissions/attempts in each domain.
	// No subject, order, provider or deleted payload is retained in the proof.
	for _, domain := range []struct {
		connection *pgx.Conn
		table      string
	}{
		{ac, "account_payment_credit_retention_batches"}, {bc, "payment_credit_retention_batches"},
	} {
		c, table := domain.connection, pgx.Identifier{domain.table}.Sanitize()
		var batches, records, proofs int
		var valid bool
		if err := c.QueryRow(ctx, `SELECT count(*),sum(purged_record_count),sum(purged_batch_count),bool_and(policy_version=1 AND result='PURGED' AND range_start<=range_end AND range_end<=completed_at AND retained_until>=completed_at+interval '3 years') FROM `+table).Scan(&batches, &records, &proofs, &valid); err != nil || batches != 4 || records != 103 || proofs != 0 || !valid {
			t.Fatal("missing or invalid atomic clearance evidence", domain.table, batches, records, proofs, valid, err)
		}
		guarded(c, `UPDATE `+table+` SET purged_record_count=0`)
		guarded(c, `UPDATE `+table+` SET retained_until=retained_until-interval '1 day'`)
		guarded(c, `DELETE FROM `+table)
		var unexpected int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name NOT IN ('id','policy_version','purged_record_count','purged_batch_count','range_start','range_end','result','completed_at','retained_until','legal_hold','hold_review_at')`, domain.table).Scan(&unexpected); err != nil || unexpected != 0 {
			t.Fatal("clearance proof gained source/content fields", err)
		}
		old := now.AddDate(-4, 0, 0)
		for _, held := range []bool{false, true} {
			if _, err := c.Exec(ctx, `INSERT INTO `+table+`(id,policy_version,purged_record_count,purged_batch_count,range_start,range_end,result,completed_at,retained_until,legal_hold,hold_review_at) VALUES($1,1,1,0,$2,$2,'PURGED',$2,$3,$4,$5)`, uuid.New(), old, old.AddDate(3, 0, 0), held, now.AddDate(0, 0, 1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	purge()
	for _, domain := range []struct {
		connection *pgx.Conn
		table      string
	}{
		{ac, "account_payment_credit_retention_batches"}, {bc, "payment_credit_retention_batches"},
	} {
		var batches, oldUnheld, retired int
		table := pgx.Identifier{domain.table}.Sanitize()
		if err := domain.connection.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE retained_until<=clock_timestamp() AND NOT legal_hold),sum(purged_batch_count) FROM `+table).Scan(&batches, &oldUnheld, &retired); err != nil || batches != 6 || oldUnheld != 0 || retired != 1 {
			t.Fatal("proof expiry or hold did not converge", batches, oldUnheld, retired, err)
		}
	}
	purge() // An idle tick must not recursively create a fresh proof.
	if count(ac, "account_payment_credit_retention_batches") != 6 || count(bc, "payment_credit_retention_batches") != 6 {
		t.Fatal("empty purge generated audit rows")
	}
	for _, migration := range am {
		if migration.Version == 95 {
			guarded(ac, migration.DownSQL)
		}
	}
	err = DownOne(ctx, bc, bm)
	if !errorsAsPaymentCreditGuard(err, &failure) || !strings.Contains(err.Error(), "payment credit attempts are forward-only") {
		t.Fatal("public Billing DownOne did not reach the new explicit refusal", err)
	}
	migrations.Unlock(ctx, bc)
	if count(ac, "account_payment_credit_admissions") != 4 || count(bc, "payment_credit_attempts") != 4 {
		t.Fatal("refused rollback changed evidence")
	}

	t.Run("late-acknowledgement-survives-billing-retention", func(t *testing.T) {
		id := seed("ack-reply-lost")
		var input paymentcredit.Request
		if err := ac.QueryRow(ctx, `SELECT id,user_id,payment_order_id,transaction_digest,facts_digest FROM account_payment_credit_admissions WHERE id=$1`, id).Scan(&input.ID, &input.UserID, &input.OrderID, &input.TransactionDigest, &input.FactsDigest); err != nil {
			t.Fatal(err)
		}
		decision, err := ar.CheckPaymentCreditRetention(ctx, input)
		if err != nil || decision.State != "RETENTION_RETAIN" {
			t.Fatal("lost final ACK must keep the existing remote receipt", decision.State, err)
		}
		wrong := input
		wrong.FactsDigest = paymentcredit.Digest([]byte("foreign-retention-facts"))
		if _, err = ar.CheckPaymentCreditRetention(ctx, wrong); !errors.Is(err, account.ErrConflict) {
			t.Fatal("retention check ignored original fact binding", err)
		}
		if err = br.PurgeExpiredPaymentCreditAttempts(ctx, nil); err == nil {
			t.Fatal("missing retention authority was an implicit purge permit")
		}
		due := func() {
			t.Helper()
			if _, err := bc.Exec(ctx, `UPDATE payment_credit_attempts SET next_retention_check_at=clock_timestamp() WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
		due()
		if err = br.PurgeExpiredPaymentCreditAttempts(ctx, ar); err != nil {
			t.Fatal(err)
		}
		var remaining int
		if err = bc.QueryRow(ctx, `SELECT count(*) FROM payment_credit_attempts WHERE id=$1`, id).Scan(&remaining); err != nil || remaining != 1 {
			t.Fatal("remote evidence vanished before the lost ACK recovered", remaining, err)
		}
		// Park only other owned scheduler rows so this tick proves that the
		// already-known terminal result resumes ACK, not funds resolution.
		if _, err = ac.Exec(ctx, `UPDATE account_payment_credit_admissions SET next_attempt_at=clock_timestamp()+interval '1 hour' WHERE id<>$1`, id); err != nil {
			t.Fatal(err)
		}
		resolver := &creditBoundaryResolver{repository: br}
		if err = ar.ReconcilePaymentCredits(ctx, resolver); err != nil {
			t.Fatal(err)
		}
		var acknowledged bool
		if err = ac.QueryRow(ctx, `SELECT state='ABORTED' AND billing_acknowledged_at IS NOT NULL FROM account_payment_credit_admissions WHERE id=$1`, id).Scan(&acknowledged); err != nil || !acknowledged || resolver.resolveCalls != 0 || resolver.ackCalls != 1 {
			t.Fatal("known outcome did not finish metadata-only ACK recovery", acknowledged, resolver.resolveCalls, resolver.ackCalls, err)
		}
		decision, err = ar.CheckPaymentCreditRetention(ctx, input)
		if err != nil || decision.State != "RETENTION_RELEASE" {
			t.Fatal("confirmed final ACK did not release expired remote audit evidence", decision.State, err)
		}
		due()
		if err = br.PurgeExpiredPaymentCreditAttempts(ctx, ar); err != nil {
			t.Fatal(err)
		}
		if err = bc.QueryRow(ctx, `SELECT count(*) FROM payment_credit_attempts WHERE id=$1`, id).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatal("confirmed old remote evidence did not retire", remaining, err)
		}
		if err = ar.ReconcilePaymentCredits(ctx, resolver); err != nil || resolver.resolveCalls != 0 || resolver.ackCalls != 1 {
			t.Fatal("next tick revived a retired ACK", err)
		}
		var balance int64
		var journals, packages int
		if err = bc.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM entitlement_packages) FROM wallets WHERE id=$1`, wallet.ID).Scan(&balance, &journals, &packages); err != nil || balance != 0 || journals != 0 || packages != 0 {
			t.Fatal("retention/ACK recovery created financial effects", balance, journals, packages, err)
		}
	})
	t.Run("waiting-old-ack-batch-does-not-starve-later-clearance", func(t *testing.T) {
		waiting := make([]uuid.UUID, 0, 101)
		for i := 0; i < 101; i++ {
			waiting = append(waiting, seed("ack-reply-lost"))
		}
		ready := seed("expired")
		if _, err := bc.Exec(ctx, `UPDATE payment_credit_attempts SET next_retention_check_at=clock_timestamp()-interval '2 seconds' WHERE id=ANY($1::uuid[])`, waiting); err != nil {
			t.Fatal(err)
		}
		if _, err := bc.Exec(ctx, `UPDATE payment_credit_attempts SET next_retention_check_at=clock_timestamp()-interval '1 second' WHERE id=$1`, ready); err != nil {
			t.Fatal(err)
		}
		for tick := 0; tick < 2; tick++ {
			if err := br.PurgeExpiredPaymentCreditAttempts(ctx, ar); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := bc.QueryRow(ctx, `SELECT count(*) FROM payment_credit_attempts WHERE id=$1`, ready).Scan(&n); err != nil || n != 0 {
			t.Fatal("old waiting batch starved later eligible evidence", n, err)
		}
		if err := bc.QueryRow(ctx, `SELECT count(*) FROM payment_credit_attempts WHERE id=ANY($1::uuid[])`, waiting).Scan(&n); err != nil || n != 101 {
			t.Fatal("waiting financial acknowledgements were discarded", n, err)
		}
	})
}

func errorsAsPaymentCreditGuard(err error, result **pgconn.PgError) bool {
	return errors.As(err, result) && (*result).Code == "55000"
}
