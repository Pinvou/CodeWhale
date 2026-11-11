import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { statusLabel, tokenStatusLabel, accountUserStatusLabel, paymentOrderStatusLabel, auditExportStatusLabel } from '../src/features/account-platform/status-label.js';
import { accountStatusCopy, accountPlatformCopy, adminPlatformCopy, financeCopy, accountClosureCopy, tokenManagementCopy } from '../src/shared/i18n.js';

const languages = ['zh', 'en', 'ja'];
const sql = file => readFileSync(new URL(`../../account-platform/migrations/${file}`, import.meta.url), 'utf8');
const values = expression => [...expression.matchAll(/'([^']+)'/g)].map(match => match[1]);

test('published user, current/legacy payment and audit-export contracts have explicit labels in all three languages', () => {
  const userSQL = sql('account/000027_account_closure_saga.up.sql');
  const userStates = values(userSQL.match(/ADD CONSTRAINT users_status_check\s+CHECK \(status IN \(([^)]+)\)/)[1]);
  const exportSQL = sql('account/000032_audit_export_protection.up.sql');
  const exportStates = values(exportSQL.match(/status TEXT NOT NULL CHECK \(status IN \(([^)]+)\)/)[1]);
  const paymentSQL = sql('billing/000049_local_payment_expiry.up.sql');
  const paymentStates = values(paymentSQL.match(/ADD CONSTRAINT payment_orders_status_check\s+CHECK \(status IN \(([^)]+)\)/)[1]);
  const legacyPaymentSQL = sql('billing/000003_payments.up.sql');
  const legacyPaymentStates = values(legacyPaymentSQL.match(/\bstatus TEXT NOT NULL CHECK \(status IN \(([^)]+)\)/)[1]);
  const readablePaymentStates = [...new Set([...paymentStates, ...legacyPaymentStates])];
  const catalogSQL = sql('billing/000043_recharge_product_catalog.up.sql');
  const catalogStates = values(catalogSQL.match(/lifecycle_status TEXT NOT NULL CHECK \(lifecycle_status IN \(([^)]+)\)/)[1]);
  const verificationStates = values(catalogSQL.match(/\bstatus TEXT NOT NULL CHECK \(status IN \(([^)]+)\)/)[1]);
  const lateStates = values(sql('billing/000028_late_payment_review_lifecycle.up.sql').match(/review_status TEXT NOT NULL CHECK \(review_status IN \(([^)]+)\)/)[1]);
  const anomalyStates = values(sql('billing/000029_payment_anomaly_reconciliation.up.sql').match(/CREATE TABLE payment_anomaly_cases[\s\S]*?\bstatus TEXT NOT NULL CHECK \(status IN \(([^)]+)\)/)[1]);
  for (const language of languages) {
    for (const [states, labels, format] of [[userStates, accountStatusCopy[language].accountUsers, accountUserStatusLabel], [readablePaymentStates, accountStatusCopy[language].paymentOrders, paymentOrderStatusLabel], [exportStates, accountStatusCopy[language].auditExports, auditExportStatusLabel]]) {
      assert.deepEqual(Object.keys(labels).sort(), states.toSorted());
      for (const code of states) {
        const label = format(code, language);
        assert.equal(typeof label, 'string');
        assert.ok(label && label !== code && !label.startsWith(accountStatusCopy[language].unknown), `${language}/${code}`);
      }
    }
    for (const code of [...catalogStates, ...verificationStates]) assert.ok(!statusLabel(code, language).startsWith(accountStatusCopy[language].unknown), `${language}/catalog/${code}`);
    for (const [states, labels] of [[lateStates, accountStatusCopy[language].latePayments], [anomalyStates, accountStatusCopy[language].anomalies]]) {
      assert.deepEqual(Object.keys(labels).sort(), states.toSorted());
      assert.ok(Object.values(labels).every(label => typeof label === 'string' && label.length > 0));
    }
  }
});

test('payment waiting, generic pending and export readiness retain distinct meanings', () => {
  const expected = {
    zh: ['待处理', '待支付', '可下载'],
    en: ['Pending', 'Awaiting payment', 'Ready to download'],
    ja: ['処理待ち', '支払い待ち', 'ダウンロード可能'],
  };
  for (const language of languages) {
    assert.deepEqual([statusLabel('PENDING', language), paymentOrderStatusLabel('PENDING', language), auditExportStatusLabel('READY', language)], expected[language]);
    assert.ok(auditExportStatusLabel('PAID', language).startsWith(accountStatusCopy[language].unknown));
    assert.ok(paymentOrderStatusLabel('READY', language).startsWith(accountStatusCopy[language].unknown));
  }
  assert.equal(statusLabel('RESOLVED', 'en', accountStatusCopy.en.anomalies, true), 'Resolution recorded');
  assert.equal(statusLabel('PENDING', 'en'), 'Pending');
  assert.equal(accountUserStatusLabel('DELETED', 'en'), 'Account closed');
  assert.equal(statusLabel('DELETED', 'en'), 'Deleted');
});

test('unknown and prototype names retain diagnostic codes without inherited labels', () => {
  for (const language of languages) for (const code of ['FUTURE_REVIEW', 'constructor', 'toString', '__proto__', 'PERSONAL', 'P0', '<img src=x onerror=alert(1)>', ' PAID ']) {
    const label = statusLabel(code, language);
    assert.equal(typeof label, 'string');
    assert.ok(label.startsWith(accountStatusCopy[language].unknown));
    assert.ok(label.includes(code));
  }
  const labels = Object.create({ FUTURE_REVIEW: 'Succeeded' });
  assert.equal(statusLabel('FUTURE_REVIEW', 'en', labels), 'Unknown status (FUTURE_REVIEW)');
});

test('malformed values are never coerced or treated as known statuses', () => {
  const hostile = { toString() { throw new Error('must not coerce'); } };
  for (const language of languages) for (const value of [undefined, null, '', false, 0, [], hostile, Symbol('status'), () => 'ACTIVE']) {
    assert.equal(statusLabel(value, language), accountStatusCopy[language].missing);
  }
});

test('token status vocabulary excludes mixed owner and priority labels while retaining status semantics', () => {
  for (const language of languages) {
    const mixed = tokenManagementCopy[language].modelToken.enumLabels;
    for (const code of ['PERSONAL', 'P0']) {
      assert.equal(typeof mixed[code], 'string');
      assert.equal(tokenStatusLabel(code, language), `${accountStatusCopy[language].unknown} (${code})`);
    }
    for (const code of Object.keys(accountStatusCopy[language].tokenStatuses)) {
      assert.equal(tokenStatusLabel(code, language), mixed[code]);
      assert.ok(typeof mixed[code] === 'string' && mixed[code].length > 0);
    }
  }
});

test('unknown previews bound Unicode code points and expose direction/control marks', () => {
  const label = statusLabel('💡'.repeat(10000), 'en');
  assert.equal(label, `Unknown status (${'💡'.repeat(120)}…)`);
  assert.equal(statusLabel('NEW\u202e\nSTATE', 'en'), 'Unknown status (NEW\\u202e\\u000aSTATE)');
});

test('supported language copy and contextual labels remain tied to the selected language', () => {
  for (const language of languages) {
    for (const copy of [accountPlatformCopy[language], adminPlatformCopy[language], financeCopy[language], accountClosureCopy[language], tokenManagementCopy[language].modelToken, tokenManagementCopy[language].pricingLifecycle]) assert.equal(copy.statusLanguage, language);
    assert.equal(statusLabel('COMPLETED', language, accountClosureCopy[language].stages), accountClosureCopy[language].stages.COMPLETED);
    for (const other of languages) assert.deepEqual(Object.keys(accountStatusCopy[language].labels).sort(), Object.keys(accountStatusCopy[other].labels).sort());
    assert.ok(Object.values(accountStatusCopy[language].labels).every(label => typeof label === 'string' && label.length > 0));
  }
});
