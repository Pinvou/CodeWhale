import { accountLayoutCopy, adminLayoutCopy } from './account-layout-i18n.js';
import { accountClosureCopy } from './account-closure-i18n.js';
import { registrationRepairCopy } from './registration-repair-i18n.js';
import { tokenManagementCopy } from './token-management-i18n.js';

// The existing token dictionary also contains owner types, priorities and
// evidence kinds. Those are not automatically recognized as status codes.
const tokenStatusCodes = [
  'ACTIVE', 'DISABLED', 'FROZEN', 'PUBLISHED', 'DEPRECATED', 'REMOVED', 'READY', 'PENDING', 'FAILED',
  'REPAIR_REQUIRED', 'HEALTHY', 'DEGRADED', 'UNHEALTHY', 'STALE', 'UNKNOWN', 'PROBING', 'RECEIVED',
  'HOLD_RESERVING', 'HOLD_RESERVED', 'DISPATCH_INTENT_PERSISTED', 'UPSTREAM_DISPATCHING', 'STREAMING',
  'SUCCEEDED', 'CANCELLED_BEFORE_DISPATCH', 'REVIEW', 'HOLD_PENDING', 'HOLD_ACTIVE', 'USAGE_PENDING',
  'SETTLED', 'RELEASED', 'SETTLEMENT_PENDING', 'QUEUED', 'PROVISIONING', 'VERIFIED', 'LEASE_EXPIRED',
  'SLA_EXCEEDED', 'MANUAL_REPAIR', 'UNAVAILABLE', 'DRAFT', 'SYNCHRONIZED', 'AVAILABLE', 'COVERED',
  'MISSING', 'BLOCKED', 'PENDING_APPROVAL', 'SCHEDULED', 'EXECUTING', 'APPLIED', 'CANCELLED', 'EXPIRED',
  'INVALIDATED', 'PLAN_SUPERSEDED', 'OPEN', 'RESOLVED', 'LONG_TERM_PLAN', 'RUNNING', 'TIMED_OUT',
  'CANDIDATE', 'RETIRING', 'RETIRED', 'REVOKED', 'VALID', 'INVALID', 'USER_STOPPED',
];

// Shared display vocabulary; contextual dictionaries take precedence in consumers.
// Columns are explicit zh/en/ja translations, never a fallback to another language.
const additions = [
  ['PENDING', '待处理', 'Pending', '処理待ち'],
  ['COMPLETED', '已完成', 'Completed', '完了'],
  ['DELETED', '已删除', 'Deleted', '削除済み'],
  ['REMOVED', '已移除', 'Removed', '削除済み'],
  ['CANCELLED', '已取消', 'Cancelled', 'キャンセル済み'],
  ['CLOSING', '关闭处理中', 'Closing', '終了処理中'],
  ['NONE', '无', 'None', 'なし'],
  ['UNKNOWN', '状态未知', 'Status unknown', '状態不明'],
  ['BLOCKED', '存在阻断', 'Blocked', '処理保留'],
  ['MISSING', '缺失', 'Missing', '未設定'],
  ['AVAILABLE', '可用', 'Available', '利用可能'],
  ['SMS_PENDING', '等待短信验证', 'Awaiting SMS verification', 'SMS 認証待ち'],
  ['BOOTSTRAP_PENDING', '等待初始化', 'Initialization pending', '初期設定待ち'],
  ['REGISTRATION_IN_PROGRESS', '注册处理中', 'Registration in progress', '登録処理中'],
  ['IDENTITY_CREATED', '身份已创建', 'Identity created', '認証情報を作成済み'],
  ['ACCOUNT_COMMITTING', '账号提交中', 'Saving account', 'アカウント保存中'],
  ['ACCOUNT_COMMITTED', '账号已提交', 'Account saved', 'アカウント保存済み'],
  ['WALLET_PROVISIONING', '钱包开通中', 'Provisioning wallet', 'ウォレット作成中'],
  ['COMPENSATION_PENDING', '补偿待完成', 'Compensation pending', '補償処理待ち'],
  ['COMPENSATION', '补偿处理中', 'Compensating', '補償処理中'],
  ['COMPENSATED', '补偿已完成', 'Compensation completed', '補償処理完了'],
  ['IDENTITY_UPDATED', '身份信息已更新', 'Identity updated', '認証情報を更新済み'],
  ['RETRY', '等待重试', 'Awaiting retry', '再試行待ち'],
  ['LEASED', '已领取处理', 'Claimed for processing', '処理を取得済み'],
  ['SUBMITTED', '已提交审批', 'Submitted for approval', '承認申請済み'],
  ['APPROVED', '已批准', 'Approved', '承認済み'],
  ['REJECTED', '已拒绝', 'Rejected', '却下済み'],
  ['DENIED', '已拒绝', 'Denied', '拒否済み'],
  ['WITHDRAWN', '已撤回', 'Withdrawn', '取り下げ済み'],
  ['EXECUTED', '已执行', 'Executed', '実行済み'],
  ['EXECUTION_FAILED', '执行失败', 'Execution failed', '実行失敗'],
  ['REQUESTED', '已申请', 'Requested', '申請済み'],
  ['POSTED', '已记账', 'Posted', '記帳済み'],
  ['COMMITTED', '已确认', 'Committed', '確定済み'],
  ['CREDITED', '已入账', 'Credited', '入金反映済み'],
  ['CREDIT_FAILED', '入账失败', 'Credit failed', '入金反映失敗'],
  ['REPAIRED', '已修复', 'Repaired', '修復済み'],
  ['PARTIALLY_REFUNDED', '部分退款', 'Partially refunded', '一部返金済み'],
  ['LATE_PAID', '迟到付款', 'Late payment', '遅延支払い'],
  ['IGNORED', '已忽略', 'Ignored', '対応不要として記録済み'],
  ['IN_REVIEW', '复核中', 'Under review', '確認中'],
  ['INACTIVE', '未启用', 'Inactive', '無効'],
  ['ENDED', '已结束', 'Ended', '終了済み'],
  ['GENERATING', '生成中', 'Generating', '生成中'],
  ['DOWNLOADED', '已下载', 'Downloaded', 'ダウンロード済み'],
  ['SUSPENDED', '已暂停', 'Suspended', '一時停止中'],
  ['DELETION_PENDING', '等待删除', 'Deletion pending', '削除待ち'],
  ['RECLAIMED_ACCOUNT_CLOSED', '因账户注销已回收', 'Reclaimed after account closure', 'アカウント終了により回収済み'],
  ['NORMAL', '正常', 'Normal', '正常'],
  ['CLEAR', '无未结应收', 'No open receivable', '未精算の債権なし'],
  ['PROCESSING', '处理中', 'Processing', '処理中'],
  ['REVIEW_REQUIRED', '需要人工复核', 'Manual review required', '手動確認が必要'],
  ['RECEIVABLE_OPEN', '应收未结清', 'Open receivable', '債権の精算待ち'],
  ['OUT_OF_BAND', '在此界面外操作', 'Handled outside this interface', 'この画面の外で対応'],
];

const unknown = { zh: '未知状态', en: 'Unknown status', ja: '不明な状態' };
const missing = { zh: '未提供状态', en: 'Status not provided', ja: '状態情報なし' };
const audit = [
  ['PENDING', '等待生成', 'Queued for generation', '生成待ち'],
  ['GENERATING', '正在生成', 'Generating', '生成中'],
  ['READY', '可下载', 'Ready to download', 'ダウンロード可能'],
  ['FAILED', '生成失败', 'Generation failed', '生成失敗'],
  ['EXPIRED', '文件已过期', 'File expired', 'ファイル期限切れ'],
  ['DOWNLOADED', '已下载', 'Downloaded', 'ダウンロード済み'],
];
const anomalies = [
  ['OPEN', '待处置', 'Awaiting resolution', '対処待ち'],
  ['RESOLVED', '处置已记录', 'Resolution recorded', '対処記録済み'],
];
function translatedStatusCopy(language, index) {
  const tokenStatuses = Object.fromEntries(tokenStatusCodes.map(code => [code, tokenManagementCopy[language].modelToken.enumLabels[code]]));
  const labels = {
    ...tokenStatuses,
    ...accountLayoutCopy[language].statuses,
    ...registrationRepairCopy[language].statuses,
    ...accountClosureCopy[language].statuses,
    ...Object.fromEntries(additions.map(row => [row[0], row[index + 1]])),
    ...Object.fromEntries(['ON_SALE', 'OFF_SALE'].map(code => [code, adminLayoutCopy[language].financeLists[code]])),
  };
  return {
    unknown: unknown[language], missing: missing[language], labels, tokenStatuses,
    accountUsers: {
      ...Object.fromEntries(['ACTIVE', 'DISABLED', 'CLOSING', 'DELETED'].map(code => [code, accountLayoutCopy[language].statuses[code]])),
      CLOSING: ['注销处理中', 'Account closure in progress', '退会処理中'][index],
      DELETED: ['已注销', 'Account closed', '退会済み'][index],
      PROVISIONING: ['开通中', 'Provisioning', '開設処理中'][index],
      BOOTSTRAP_PENDING: labels.BOOTSTRAP_PENDING,
      REPAIR_REQUIRED: labels.REPAIR_REQUIRED,
    },
    paymentOrders: Object.fromEntries(['CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'REFUNDED', 'PARTIALLY_REFUNDED', 'MANUAL_REVIEW', 'EXPIRED']
      .map(code => [code, code === 'PENDING' ? accountLayoutCopy[language].statuses.PENDING : labels[code]])),
    auditExports: Object.fromEntries(audit.map(row => [row[0], row[index + 1]])),
    anomalies: Object.fromEntries(anomalies.map(row => [row[0], row[index + 1]])),
    latePayments: Object.fromEntries(['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND', 'ARCHIVED_CONFIRMED_UNPAID', 'RESOLVED_PAID']
      .map(code => [code, adminLayoutCopy[language].financeLists[code]])),
  };
}

export const accountStatusCopy = {
  zh: /* @__PURE__ */ translatedStatusCopy('zh', 0),
  en: /* @__PURE__ */ translatedStatusCopy('en', 1),
  ja: /* @__PURE__ */ translatedStatusCopy('ja', 2),
};
