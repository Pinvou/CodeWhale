export const ledgerRepairCopy = {
  "zh": {
    "title": "发起财务修复审批",
    "lead": "选择命令并填写目标引用，读取服务端冻结事实后再提交审批草稿。页面不会自行推算任何金额或账务事实。",
    "command": "修复命令",
    "targets": {
      "LEDGER_REPAIR": "目标过账 ID"
    },
    "fields": {
      "target_journal_entry_id": "账目分录 ID",
      "target_posting_id": "过账 ID",
      "wrong_ledger_account_id": "错误平台账户 ID",
      "correct_ledger_account_id": "正确平台账户 ID",
      "amount_microcredits": "金额（微积分）",
      "target": "目标",
      "reason_code": "固定原因码"
    },
    "readFacts": "读取冻结事实",
    "reading": "正在读取…",
    "frozenFacts": "服务端冻结事实",
    "criticalFacts": "临界版本",
    "financialFacts": "财务快照",
    "factsUnavailable": "该目标当前无法给出可证明的冻结事实，修复命令可能尚未激活或事实不满足前置条件。",
    "factsFailed": "读取冻结事实失败，请稍后重试。",
    "invalidInput": "请按字段格式填写完整的目标引用（UUID 或日期）。",
    "reason": "申请理由",
    "reasonHint": "理由需 10 到 2000 字符，写入审批审计。",
    "requestKey": "请求键",
    "requestKeyHint": "请求键用于幂等，同一目标请复用同一键。",
    "createDraft": "创建审批草稿",
    "creating": "正在创建…",
    "createdHint": "已创建审批草稿：",
    "invalidDraft": "冻结事实或理由/请求键不满足闭合要求，草稿未提交。",
    "draftFailed": "创建审批草稿失败，请稍后重试。",
    "controlChanged": "审批控制已变化，请重新读取冻结事实。"
  },
  "en": {
    "title": "Start a financial repair approval",
    "lead": "Choose the command, enter the target references, read the server-frozen facts, then submit the approval draft. The page never derives an amount or an accounting fact.",
    "command": "Repair command",
    "targets": {
      "LEDGER_REPAIR": "Target posting ID"
    },
    "fields": {
      "target_journal_entry_id": "Journal entry ID",
      "target_posting_id": "Posting ID",
      "wrong_ledger_account_id": "Wrong ledger account ID",
      "correct_ledger_account_id": "Correct ledger account ID",
      "amount_microcredits": "Amount (microcredits)",
      "target": "Target",
      "reason_code": "Fixed reason code"
    },
    "readFacts": "Read frozen facts",
    "reading": "Reading…",
    "frozenFacts": "Server-frozen facts",
    "criticalFacts": "Critical versions",
    "financialFacts": "Financial snapshot",
    "factsUnavailable": "This target has no provable frozen facts right now; the repair command may not be activated or the facts do not meet the preconditions.",
    "factsFailed": "Reading the frozen facts failed. Try again later.",
    "invalidInput": "Complete every target reference in the required format (UUID or date).",
    "reason": "Reason",
    "reasonHint": "The reason must be 10 to 2000 characters and is written to the approval audit.",
    "requestKey": "Request key",
    "requestKeyHint": "The request key makes the action idempotent; reuse the same key for the same target.",
    "createDraft": "Create approval draft",
    "creating": "Creating…",
    "createdHint": "Approval draft created:",
    "invalidDraft": "The frozen facts, reason or request key do not meet the closed contract; nothing was submitted.",
    "draftFailed": "Creating the approval draft failed. Try again later.",
    "controlChanged": "The approval control changed; read the frozen facts again."
  },
  "ja": {
    "title": "財務修復の承認を起動",
    "lead": "コマンドを選び対象参照を入力し、サーバーが固定した事実を読み込んでから承認ドラフトを提出します。画面は金額や会計事実を推計しません。",
    "command": "修復コマンド",
    "targets": {
      "LEDGER_REPAIR": "対象仕訳転記 ID"
    },
    "fields": {
      "target_journal_entry_id": "仕訳 ID",
      "target_posting_id": "転記 ID",
      "wrong_ledger_account_id": "誤った勘定 ID",
      "correct_ledger_account_id": "正しい勘定 ID",
      "amount_microcredits": "金額（マイクロクレジット）",
      "target": "対象",
      "reason_code": "固定理由コード"
    },
    "readFacts": "固定事実を読み込む",
    "reading": "読み込み中…",
    "frozenFacts": "サーバー固定事実",
    "criticalFacts": "クリティカル版",
    "financialFacts": "財務スナップショット",
    "factsUnavailable": "この対象では証明可能な固定事実がありません。修復コマンドが未激活か、前提条件を満たしていない可能性があります。",
    "factsFailed": "固定事実の読み込みに失敗しました。後でもう一度お試しください。",
    "invalidInput": "対象参照を所定の形式（UUID または日付）で入力してください。",
    "reason": "申請理由",
    "reasonHint": "理由は 10〜2000 文字で、承認監査に記録されます。",
    "requestKey": "リクエストキー",
    "requestKeyHint": "リクエストキーは冪等性のためです。同じ対象では同じキーを使用してください。",
    "createDraft": "承認ドラフトを作成",
    "creating": "作成中…",
    "createdHint": "承認ドラフトを作成しました:",
    "invalidDraft": "固定事実・理由・リクエストキーが閉じた契約を満たさないため、送信しませんでした。",
    "draftFailed": "承認ドラフトの作成に失敗しました。後でもう一度お試しください。",
    "controlChanged": "承認制御が変更されました。固定事実を再読み込みしてください。"
  }
};

const recoveryCopy = {
  zh: { retry: '使用原请求重试', unknown: '创建草稿的结果尚未确认，请保留原事实和请求键。', permissions: '当前权限不足；未确认的原请求仍保留。', sourceChanged: '请求被拒绝，请检查冻结事实和审批条件。', recoveryUnavailable: '恢复记录无法可靠读取或保存；未发送新请求。', recoveryPending: '刷新或重新认证后仍仅重放原草稿请求，不会自动提交审批。', saved: '草稿已确认创建。', requestKeyHint: '本地请求键最多 111 个 ASCII 可见字符；加 approval: 后的公开审批键最多 120 个字符。' },
  en: { retry: 'Retry the original request', unknown: 'Draft creation is unconfirmed. Keep the original facts and request key.', permissions: 'Current permission is insufficient. The unconfirmed original request is retained.', sourceChanged: 'The request was rejected. Check the frozen facts and approval conditions.', recoveryUnavailable: 'The recovery record cannot be reliably read or saved. No new request was sent.', recoveryPending: 'After refresh or authentication, only the original draft request is replayed; approval is never submitted automatically.', saved: 'Draft creation is confirmed.', requestKeyHint: 'The local key allows up to 111 visible ASCII characters; the public approval key including approval: allows up to 120.' },
  ja: { retry: '元のリクエストで再試行', unknown: 'ドラフト作成の結果は未確認です。元の事実とリクエストキーを保持してください。', permissions: '現在の権限が不足しています。未確認の元のリクエストは保持されます。', sourceChanged: 'リクエストが拒否されました。固定事実と承認条件を確認してください。', recoveryUnavailable: '復元記録を確実に読み書きできないため、新しいリクエストは送信していません。', recoveryPending: '再読み込みや再認証後も元のドラフト要求のみを再実行し、承認を自動提出しません。', saved: 'ドラフトの作成を確認しました。', requestKeyHint: 'ローカルキーは表示可能な ASCII 111 文字以内です。approval: を含む公開承認キーは 120 文字以内です。' },
};
for (const language of ['zh', 'en', 'ja']) Object.assign(ledgerRepairCopy[language], recoveryCopy[language]);
ledgerRepairCopy.zh.requestKeyHint = '本地请求键最多 111 个 UTF-8 字节；加 approval: 后的公开审批键最多 120 个字节。';
ledgerRepairCopy.en.requestKeyHint = 'The local key allows up to 111 UTF-8 bytes; the public approval key including approval: allows up to 120 bytes.';
ledgerRepairCopy.ja.requestKeyHint = 'ローカルキーは UTF-8 で 111 バイト以内です。approval: を含む公開承認キーは 120 バイト以内です。';

const authenticationCopy = {
  zh: { recentAuthenticationRequired: '\u9700\u8981\u91cd\u65b0\u8ba4\u8bc1\u540e\u7ee7\u7eed\u539f\u8349\u7a3f\u8bf7\u6c42\u3002', reauthenticate: '\u91cd\u65b0\u8ba4\u8bc1' },
  en: { recentAuthenticationRequired: 'Authenticate again to continue the original draft request.', reauthenticate: 'Authenticate again' },
  ja: { recentAuthenticationRequired: '\u518d\u8a8d\u8a3c\u3057\u3066\u5143\u306e\u30c9\u30e9\u30d5\u30c8\u8981\u6c42\u3092\u7d9a\u884c\u3057\u3066\u304f\u3060\u3055\u3044\u3002', reauthenticate: '\u518d\u8a8d\u8a3c' },
};
for (const language of ['zh', 'en', 'ja']) Object.assign(ledgerRepairCopy[language], authenticationCopy[language]);
