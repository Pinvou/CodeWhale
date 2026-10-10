export const accountClosedGrantCopy = {
  "zh": {
    "title": "回收注销账号的剩余额度",
    "grant": "额度授予标识",
    "single": "整笔回收剩余额度",
    "batch": "批量回收剩余额度",
    "grants": "额度授予标识（逗号分隔）",
    "lead": "仅已完成匿名化且未过期、未冻结、没有预占的剩余额度可以整笔回收。"
  },
  "en": {
    "title": "Reclaim closed-account quota",
    "grant": "Quota grant ID",
    "single": "Reclaim full remainder",
    "batch": "Reclaim quota batch",
    "grants": "Quota grant IDs (comma separated)",
    "lead": "Only the full, unexpired remainder of an anonymized account can be reclaimed. Reserved or frozen quota remains blocked."
  },
  "ja": {
    "title": "解約アカウントの利用枠を回収",
    "grant": "利用枠付与の識別子",
    "single": "残額を全額回収",
    "batch": "利用枠を一括回収",
    "grants": "利用枠付与の識別子（カンマ区切り）",
    "lead": "匿名化済みで有効期限内の残額のみ全額回収できます。予約済みまたは凍結中の利用枠は回収できません。"
  }
};
Object.assign(accountClosedGrantCopy.zh, { reclaimed: '已回收', rejected: '未回收：请核对注销状态、到期、冻结或预占阻断' });
Object.assign(accountClosedGrantCopy.en, { reclaimed: 'Reclaimed', rejected: 'Not reclaimed: check closure, expiry, freeze and reserved-credit blockers' });
Object.assign(accountClosedGrantCopy.ja, { reclaimed: '回収済み', rejected: '未回収：退会状態、期限、凍結、予約残高の阻止要因を確認してください' });
Object.assign(accountClosedGrantCopy.zh, { recoveryUnavailable: '无法安全保存或读取原回收凭据，已停止发送请求。请恢复会话存储后再试。', recoveryPending: '原回收结果尚未确认；只能凭原请求重试。' });
Object.assign(accountClosedGrantCopy.en, { recoveryUnavailable: 'The original reclaim proof cannot be read or saved safely. Requests are blocked until session storage is restored.', recoveryPending: 'The original reclaim result is unconfirmed. Retry only the original request.' });
Object.assign(accountClosedGrantCopy.ja, { recoveryUnavailable: '元の回収証跡を安全に保存・読み取りできないため送信を停止しました。セッション保存を復旧してから再試行してください。', recoveryPending: '元の回収結果は未確認です。元の要求でのみ再試行できます。' });
