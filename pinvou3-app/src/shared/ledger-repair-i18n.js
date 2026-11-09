export const ledgerRepairCopy = {
  "zh": {
    "title": "发起财务修复审批",
    "usageSettlementRepairTitle": "用量结算修复",
    "writeOffTitle": "应收核销",
    "lead": "选择命令并填写目标引用，读取服务端冻结事实后再提交审批草稿。页面不会自行推算任何金额或账务事实。",
    "command": "修复命令",
    "targets": {
      "LEDGER_REPAIR": "目标过账 ID",
      "RECEIVABLE_WRITE_OFF": "应收 ID",
      "PAYMENT_FINANCIAL_REPAIR": "支付订单 ID",
      "REFUND_EXCEPTION_REPAIR": "退款 ID",
      "CHARGEBACK_FINANCIAL_REPAIR": "拒付案件 ID",
      "RECONCILIATION_FINANCIAL_REPAIR": "对账案件 ID",
      "USAGE_SETTLEMENT_REPAIR": "用量冻结 ID"
    },
    "fields": {
      "usage_request_id": "用量请求 ID",
      "hold_id": "用量冻结 ID",
      "loss_review_id": "用量损失复核 ID",
      "evidence_kind": "证据类型",
      "expected_microcredits": "应结算金额（微积分）",
      "settled_microcredits": "本次计划结算金额（微积分）",
      "settlement_microcredits": "补结算金额（微积分）",
      "overage_microcredits": "超额金额（微积分）",
      "allocations_digest": "分配摘要",
      "hold_updated_at": "冻结更新时间",
      "loss_review_updated_at": "损失复核更新时间",
      "receivable_id": "应收 ID",
      "write_off_microcredits": "核销金额（微积分）",
      "expected_version": "预期应收版本",
      "expected_receivable_version": "预期应收版本",
      "target_journal_entry_id": "账目分录 ID",
      "target_posting_id": "过账 ID",
      "wrong_ledger_account_id": "错误平台账户 ID",
      "correct_ledger_account_id": "正确平台账户 ID",
      "amount_microcredits": "金额（微积分）",
      "target": "目标",
      "reason_code": "固定原因码",
      "payment_order_id": "支付订单 ID",
      "payment_evidence_id": "支付证据 ID",
      "amount_fen": "支付金额（分）",
      "credit_microcredits": "入账金额（微积分）",
      "applied_to_receivables": "用于应收的金额（微积分）",
      "wallet_version": "钱包版本",
      "wallet_id": "钱包 ID",
      "provider_transaction_id": "支付渠道交易 ID",
      "payment_order_updated_at": "订单更新时间",
      "facts_digest": "事实摘要",
      "refund_id": "退款 ID",
      "anomaly_case_id": "异常案件 ID",
      "provider_refund_id": "退款渠道引用",
      "revoked_microcredits": "回收金额（微积分）",
      "shortfall_microcredits": "短缺金额（微积分）",
      "package_id": "权益包 ID",
      "refund_updated_at": "退款更新时间",
      "anomaly_version": "异常版本",
      "chargeback_case_id": "拒付案件 ID",
      "expected_case_version": "预期拒付案件版本",
      "chargeback_case_version": "拒付案件版本",
      "base_shortfall_microcredits": "BASE 短缺金额（微积分）",
      "component_digest": "组件摘要",
      "provider_chargeback_id": "支付渠道拒付引用",
      "reconciliation_case_id": "对账案件 ID",
      "business_date": "上海业务日期（YYYY-MM-DD）",
      "run_id": "对账运行 ID",
      "provider_snapshot_digest": "渠道快照摘要",
      "currency": "币种",
      "resolution_code": "固定处置码",
      "ledger_effect": "账务效果"
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
    "requestKeyHint": "本地请求键最多 111 个 UTF-8 字节；加 approval: 后的公开审批键最多 120 个字节。",
    "createDraft": "创建审批草稿",
    "creating": "正在创建…",
    "createdHint": "已创建审批草稿：",
    "invalidDraft": "冻结事实或理由/请求键不满足闭合要求，草稿未提交。",
    "draftFailed": "创建审批草稿失败，请稍后重试。",
    "controlChanged": "审批控制已变化，请重新读取冻结事实。",
    "paymentRepairTitle": "支付财务修复",
    "refundRepairTitle": "退款例外修复",
    "chargebackRepairTitle": "拒付财务修复",
    "reconciliationRepairTitle": "对账证据确认",
    "reconciliationLead": "适用于渠道有付款记录、平台无对应记录且渠道退款证据已确认的差异。加载证据并核对后提交审批；本申请需经审批并执行成功，才会据此结案匹配差异。",
    "ledgerEffectNone": "执行会登记证据引用与结案结果，并结案关联的人工复核案件（如有）。不会补记账、增加钱包余额或发起渠道操作。",
    "businessDateHint": "按对账运行的上海业务日期填写（UTC+08:00），不按浏览器时区换算。可选取日期，或手动输入 YYYY-MM-DD。",
    "dateManualEntry": "手动输入日期",
    "dateUseCalendar": "使用日期选择器",
    "dateCalendarUnavailableHelp": "日期选择器不支持当前值；请手动填写，或清空日期后切换。"
  },
  "en": {
    "title": "Start a financial repair approval",
    "usageSettlementRepairTitle": "Usage settlement repair",
    "writeOffTitle": "Receivable write-off",
    "lead": "Choose the command, enter the target references, read the server-frozen facts, then submit the approval draft. The page never derives an amount or an accounting fact.",
    "command": "Repair command",
    "targets": {
      "LEDGER_REPAIR": "Target posting ID",
      "RECEIVABLE_WRITE_OFF": "Receivable ID",
      "PAYMENT_FINANCIAL_REPAIR": "Payment order ID",
      "REFUND_EXCEPTION_REPAIR": "Refund ID",
      "CHARGEBACK_FINANCIAL_REPAIR": "Chargeback case ID",
      "RECONCILIATION_FINANCIAL_REPAIR": "Reconciliation case ID",
      "USAGE_SETTLEMENT_REPAIR": "Usage hold ID"
    },
    "fields": {
      "usage_request_id": "Usage request ID",
      "hold_id": "Usage hold ID",
      "loss_review_id": "Usage loss review ID",
      "evidence_kind": "Evidence kind",
      "expected_microcredits": "Expected amount (microcredits)",
      "settled_microcredits": "Planned settlement amount (microcredits)",
      "settlement_microcredits": "Settlement amount (microcredits)",
      "overage_microcredits": "Overage (microcredits)",
      "allocations_digest": "Allocations digest",
      "hold_updated_at": "Hold updated at",
      "loss_review_updated_at": "Loss review updated at",
      "receivable_id": "Receivable ID",
      "write_off_microcredits": "Write-off amount (microcredits)",
      "expected_version": "Expected receivable version",
      "expected_receivable_version": "Expected receivable version",
      "target_journal_entry_id": "Journal entry ID",
      "target_posting_id": "Posting ID",
      "wrong_ledger_account_id": "Wrong ledger account ID",
      "correct_ledger_account_id": "Correct ledger account ID",
      "amount_microcredits": "Amount (microcredits)",
      "target": "Target",
      "reason_code": "Fixed reason code",
      "payment_order_id": "Payment order ID",
      "payment_evidence_id": "Payment evidence ID",
      "amount_fen": "Payment amount (fen)",
      "credit_microcredits": "Credit amount (microcredits)",
      "applied_to_receivables": "Applied to receivables (microcredits)",
      "wallet_version": "Wallet version",
      "wallet_id": "Wallet ID",
      "provider_transaction_id": "Provider transaction ID",
      "payment_order_updated_at": "Order updated at",
      "facts_digest": "Facts digest",
      "refund_id": "Refund ID",
      "anomaly_case_id": "Anomaly case ID",
      "provider_refund_id": "Provider refund reference",
      "revoked_microcredits": "Revoked amount (microcredits)",
      "shortfall_microcredits": "Shortfall (microcredits)",
      "package_id": "Entitlement package ID",
      "refund_updated_at": "Refund updated at",
      "anomaly_version": "Anomaly version",
      "chargeback_case_id": "Chargeback case ID",
      "expected_case_version": "Expected chargeback case version",
      "chargeback_case_version": "Chargeback case version",
      "base_shortfall_microcredits": "BASE shortfall (microcredits)",
      "component_digest": "Component digest",
      "provider_chargeback_id": "Provider chargeback reference",
      "reconciliation_case_id": "Reconciliation case ID",
      "business_date": "Shanghai business date (YYYY-MM-DD)",
      "run_id": "Reconciliation run ID",
      "provider_snapshot_digest": "Provider snapshot digest",
      "currency": "Currency",
      "resolution_code": "Fixed resolution code",
      "ledger_effect": "Ledger effect"
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
    "requestKeyHint": "The local key allows up to 111 UTF-8 bytes; the public approval key including approval: allows up to 120 bytes.",
    "createDraft": "Create approval draft",
    "creating": "Creating…",
    "createdHint": "Approval draft created:",
    "invalidDraft": "The frozen facts, reason or request key do not meet the closed contract; nothing was submitted.",
    "draftFailed": "Creating the approval draft failed. Try again later.",
    "controlChanged": "The approval control changed; read the frozen facts again.",
    "paymentRepairTitle": "Payment financial repair",
    "refundRepairTitle": "Refund exception repair",
    "chargebackRepairTitle": "Chargeback financial repair",
    "reconciliationRepairTitle": "Reconciliation evidence review",
    "reconciliationLead": "For a provider payment missing internally with confirmed provider refund evidence. Load and review the evidence, then submit for approval. This request closes the matched difference only after approval and successful execution.",
    "ledgerEffectNone": "Execution records the evidence reference and closure result, including closure of any linked manual review case. It does not post ledger entries, credit wallets, or initiate provider operations.",
    "businessDateHint": "Use the reconciliation run’s Shanghai business date (UTC+08:00), without browser time zone conversion. Pick a date or enter YYYY-MM-DD manually.",
    "dateManualEntry": "Enter date manually",
    "dateUseCalendar": "Use date picker",
    "dateCalendarUnavailableHelp": "The date picker cannot represent this value. Enter it manually, or clear the date before switching."
  },
  "ja": {
    "title": "財務修復の承認を起動",
    "usageSettlementRepairTitle": "使用量決済修復",
    "writeOffTitle": "売掛金の償却",
    "lead": "コマンドを選び対象参照を入力し、サーバーが固定した事実を読み込んでから承認ドラフトを提出します。画面は金額や会計事実を推計しません。",
    "command": "修復コマンド",
    "targets": {
      "LEDGER_REPAIR": "対象仕訳転記 ID",
      "RECEIVABLE_WRITE_OFF": "売掛金 ID",
      "PAYMENT_FINANCIAL_REPAIR": "支払注文 ID",
      "REFUND_EXCEPTION_REPAIR": "返金 ID",
      "CHARGEBACK_FINANCIAL_REPAIR": "チャージバック案件 ID",
      "RECONCILIATION_FINANCIAL_REPAIR": "照合案件 ID",
      "USAGE_SETTLEMENT_REPAIR": "使用量保留 ID"
    },
    "fields": {
      "usage_request_id": "使用量リクエスト ID",
      "hold_id": "使用量保留 ID",
      "loss_review_id": "使用量損失審査 ID",
      "evidence_kind": "証拠種別",
      "expected_microcredits": "決済予定額（マイクロクレジット）",
      "settled_microcredits": "今回の予定決済額（マイクロクレジット）",
      "settlement_microcredits": "追加決済額（マイクロクレジット）",
      "overage_microcredits": "超過額（マイクロクレジット）",
      "allocations_digest": "配分ダイジェスト",
      "hold_updated_at": "保留更新日時",
      "loss_review_updated_at": "損失審査更新日時",
      "receivable_id": "売掛金 ID",
      "write_off_microcredits": "償却額（マイクロクレジット）",
      "expected_version": "予想売掛金版",
      "expected_receivable_version": "予想売掛金版",
      "target_journal_entry_id": "仕訳 ID",
      "target_posting_id": "転記 ID",
      "wrong_ledger_account_id": "誤った勘定 ID",
      "correct_ledger_account_id": "正しい勘定 ID",
      "amount_microcredits": "金額（マイクロクレジット）",
      "target": "対象",
      "reason_code": "固定理由コード",
      "payment_order_id": "支払注文 ID",
      "payment_evidence_id": "支払証拠 ID",
      "amount_fen": "支払金額（分）",
      "credit_microcredits": "計上額（マイクロクレジット）",
      "applied_to_receivables": "売掛金充当額（マイクロクレジット）",
      "wallet_version": "ウォレット版",
      "wallet_id": "ウォレット ID",
      "provider_transaction_id": "決済取引 ID",
      "payment_order_updated_at": "注文更新時刻",
      "facts_digest": "事実ダイジェスト",
      "refund_id": "返金 ID",
      "anomaly_case_id": "異常案件 ID",
      "provider_refund_id": "決済返金参照",
      "revoked_microcredits": "回収額（マイクロクレジット）",
      "shortfall_microcredits": "不足額（マイクロクレジット）",
      "package_id": "権利パッケージ ID",
      "refund_updated_at": "返金更新時刻",
      "anomaly_version": "異常版",
      "chargeback_case_id": "チャージバック案件 ID",
      "expected_case_version": "予想チャージバック案件版",
      "chargeback_case_version": "チャージバック案件版",
      "base_shortfall_microcredits": "BASE 不足額（マイクロクレジット）",
      "component_digest": "構成要素ダイジェスト",
      "provider_chargeback_id": "決済チャージバック参照",
      "reconciliation_case_id": "照合案件 ID",
      "business_date": "上海の業務日（YYYY-MM-DD）",
      "run_id": "照合実行 ID",
      "provider_snapshot_digest": "決済スナップショットダイジェスト",
      "currency": "通貨",
      "resolution_code": "固定解決コード",
      "ledger_effect": "台帳への効果"
    },
    "readFacts": "固定事実を読み込む",
    "reading": "読み込み中…",
    "frozenFacts": "サーバー固定事実",
    "criticalFacts": "クリティカル版",
    "financialFacts": "財務スナップショット",
    "factsUnavailable": "この対象では証明可能な固定事実がありません。修復コマンドがまだ有効化されていないか、前提条件を満たしていない可能性があります。",
    "factsFailed": "固定事実の読み込みに失敗しました。後でもう一度お試しください。",
    "invalidInput": "対象参照を所定の形式（UUID または日付）で入力してください。",
    "reason": "申請理由",
    "reasonHint": "理由は 10〜2000 文字で、承認監査に記録されます。",
    "requestKey": "リクエストキー",
    "requestKeyHint": "ローカルキーは UTF-8 で 111 バイト以内です。approval: を含む公開承認キーは 120 バイト以内です。",
    "createDraft": "承認ドラフトを作成",
    "creating": "作成中…",
    "createdHint": "承認ドラフトを作成しました:",
    "invalidDraft": "固定事実・理由・リクエストキーが閉じた契約を満たさないため、送信しませんでした。",
    "draftFailed": "承認ドラフトの作成に失敗しました。後でもう一度お試しください。",
    "controlChanged": "承認制御が変更されました。固定事実を再読み込みしてください。",
    "paymentRepairTitle": "支払財務修復",
    "refundRepairTitle": "返金例外修復",
    "chargebackRepairTitle": "チャージバック財務修復",
    "reconciliationRepairTitle": "照合証拠の確認",
    "reconciliationLead": "決済事業者に支払記録があり、内部記録がなく、事業者の返金証拠を確認できる差異が対象です。証拠を読み込んで照合し、承認申請します。この申請による差異の終了は、承認と実行の成功後に行われます。",
    "ledgerEffectNone": "実行では返金証拠の参照と終了結果を記録し、関連する手動確認案件があれば併せて終了します。仕訳の追加、ウォレットへの加算、決済事業者の操作は行いません。",
    "businessDateHint": "照合実行の上海業務日（UTC+08:00）を指定します。ブラウザーの時間帯では変換しません。日付を選ぶか、YYYY-MM-DD で手入力できます。",
    "dateManualEntry": "日付を手入力",
    "dateUseCalendar": "日付選択を使用",
    "dateCalendarUnavailableHelp": "現在の値は日付選択で扱えません。手入力するか、日付を空にしてから切り替えてください。"
  }
};

const recoveryCopy = {
  zh: { retry: '使用原请求重试', unknown: '创建草稿的结果尚未确认，请保留原事实和请求键。', permissions: '当前权限不足；未确认的原请求仍保留。', sourceChanged: '请求被拒绝，请检查冻结事实和审批条件。', recoveryUnavailable: '恢复记录无法可靠读取或保存；未发送新请求。', recoveryPending: '刷新或重新认证后仍仅重放原草稿请求，不会自动提交审批。', saved: '草稿已确认创建。' },
  en: { retry: 'Retry the original request', unknown: 'Draft creation is unconfirmed. Keep the original facts and request key.', permissions: 'Current permission is insufficient. The unconfirmed original request is retained.', sourceChanged: 'The request was rejected. Check the frozen facts and approval conditions.', recoveryUnavailable: 'The recovery record cannot be reliably read or saved. No new request was sent.', recoveryPending: 'After refresh or authentication, only the original draft request is replayed; approval is never submitted automatically.', saved: 'Draft creation is confirmed.' },
  ja: { retry: '元のリクエストで再試行', unknown: 'ドラフト作成の結果は未確認です。元の事実とリクエストキーを保持してください。', permissions: '現在の権限が不足しています。未確認の元のリクエストは保持されます。', sourceChanged: 'リクエストが拒否されました。固定事実と承認条件を確認してください。', recoveryUnavailable: '復元記録を確実に読み書きできないため、新しいリクエストは送信していません。', recoveryPending: '再読み込みや再認証後も元のドラフト要求のみを再実行し、承認を自動提出しません。', saved: 'ドラフトの作成を確認しました。' },
};
for (const language of ['zh', 'en', 'ja']) Object.assign(ledgerRepairCopy[language], recoveryCopy[language]);

const ledgerAmountCopy = {
  zh: {
    amountPointsLabel: '金额（积分）',
    amountPointsHint: '1 积分 = 10,000 微积分。最多 4 位小数，使用小数点，不含空格或千位分隔符。',
    amountPointsEquivalent: '内部记账值：{amount} 微积分。',
    amountPointsInvalid: '请输入大于 0 且不超过 922337203685477.5807 的积分金额，最多 4 位小数；不支持正负号、空格、千位分隔符或科学计数法。',
  },
  en: {
    amountPointsLabel: 'Amount (points)',
    amountPointsHint: '1 point = 10,000 microcredits. Use a decimal point and up to 4 decimal places, without spaces or grouping separators.',
    amountPointsEquivalent: 'Internal amount: {amount} microcredits.',
    amountPointsInvalid: 'Enter points greater than 0 and no more than 922337203685477.5807, with up to 4 decimal places; signs, spaces, grouping separators and exponent notation are unsupported.',
  },
  ja: {
    amountPointsLabel: '金額（ポイント）',
    amountPointsHint: '1 ポイント = 10,000 マイクロクレジット。小数点を使用し、小数部は 4 桁以内で、空白や桁区切りを含めないでください。',
    amountPointsEquivalent: '内部金額：{amount} マイクロクレジット。',
    amountPointsInvalid: '0 より大きく 922337203685477.5807 以下のポイントを小数部 4 桁以内で入力してください。符号、空白、桁区切り、指数表記は使用できません。',
  },
};
for (const language of ['zh', 'en', 'ja']) Object.assign(ledgerRepairCopy[language], ledgerAmountCopy[language]);

const authenticationCopy = {
  zh: { recentAuthenticationRequired: '\u9700\u8981\u91cd\u65b0\u8ba4\u8bc1\u540e\u7ee7\u7eed\u539f\u8349\u7a3f\u8bf7\u6c42\u3002', reauthenticate: '\u91cd\u65b0\u8ba4\u8bc1' },
  en: { recentAuthenticationRequired: 'Authenticate again to continue the original draft request.', reauthenticate: 'Authenticate again' },
  ja: { recentAuthenticationRequired: '\u518d\u8a8d\u8a3c\u3057\u3066\u5143\u306e\u30c9\u30e9\u30d5\u30c8\u8981\u6c42\u3092\u7d9a\u884c\u3057\u3066\u304f\u3060\u3055\u3044\u3002', reauthenticate: '\u518d\u8a8d\u8a3c' },
};
for (const language of ['zh', 'en', 'ja']) Object.assign(ledgerRepairCopy[language], authenticationCopy[language]);

ledgerRepairCopy.zh.writeOffRecentAuthenticationRequired = '需要重新认证。当前页面保留原事实和请求键；门户跳转后，未知操作请到审批中心打开原草稿，用 EDIT 核验并继续，不要另建请求。';
ledgerRepairCopy.en.writeOffRecentAuthenticationRequired = 'Authenticate again. This page retains the original facts and key. After portal navigation, open the original draft in the approval center and use EDIT to verify and continue an unconfirmed operation; do not create another request.';
ledgerRepairCopy.ja.writeOffRecentAuthenticationRequired = '再認証が必要です。このページでは元の事実とキーを保持します。ポータル移動後、未確認の操作は承認センターで元のドラフトを開き、EDIT で確認して続行してください。別の要求を作成しないでください。';

const reconciliationCaseContextCopy = {
  "zh": {
    "caseRepairStart": "核对此案件的支付证据",
    "caseContextTitle": "案件来源（仅供核对）",
    "caseContextHelp": "这是案件原始对账批次中的记录，不代表证据已匹配或可以结案。核对渠道、账期和金额后，继续读取服务端事实验证支付证据。",
    "caseContextRead": "读取案件来源",
    "caseContextReading": "正在读取案件来源…",
    "caseContextDenied": "没有读取对账案件的权限；未加载案件来源。原有手填和审批前校验入口仍可使用。",
    "caseContextUnavailable": "案件来源未能确认。请重试读取；不要把未加载信息当作匹配结果。",
    "caseContextMissing": "来源金额缺失或无法解析",
    "evidenceMatchTitle": "审批前证据匹配记录（已通过）",
    "evidenceMatchHelp": "以下来自原服务端审批前校验及冻结投影。此校验记录不代表审批或结案执行已完成；执行时仍会重新核验。",
    "caseContextLabels": {
      "case_id": "对账案件 ID",
      "reconciliation_case_id": "对账案件 ID",
      "reconciliation_run_id": "案件原始对账批次 ID",
      "run_id": "案件原始对账批次 ID",
      "case_type": "差异类型",
      "status": "案件状态",
      "case_version": "案件版本",
      "provider_id": "供应商",
      "provider_version": "供应商版本",
      "channel": "支付渠道",
      "business_date": "案件原始批次业务日期",
      "merchant_order_no": "商户订单号",
      "amount_fen": "渠道记录金额（分）",
      "currency": "币种",
      "provider_transaction_id": "支付渠道交易 ID",
      "payment_evidence_id": "已校验支付证据 ID"
    }
  },
  "en": {
    "caseRepairStart": "Check payment evidence for this case",
    "caseContextTitle": "Case source (for review only)",
    "caseContextHelp": "These records belong to the case’s original reconciliation run. They do not establish an evidence match or eligibility for closure. Review the channel, business date and amount, then read the owner facts to validate the payment evidence.",
    "caseContextRead": "Read case source",
    "caseContextReading": "Reading case source…",
    "caseContextDenied": "You do not have permission to read reconciliation cases. Case source was not loaded. The existing manual input and approval precheck remain available.",
    "caseContextUnavailable": "The case source could not be confirmed. Retry the read; unloaded information is not a matching result.",
    "caseContextMissing": "Source amount is missing or unusable",
    "evidenceMatchTitle": "Evidence precheck record (passed)",
    "evidenceMatchHelp": "These facts come from the original server precheck and frozen projection. Passing this precheck does not establish that approval or closure execution is complete; execution revalidates the facts.",
    "caseContextLabels": {
      "case_id": "Reconciliation case ID",
      "reconciliation_case_id": "Reconciliation case ID",
      "reconciliation_run_id": "Case original reconciliation run ID",
      "run_id": "Case original reconciliation run ID",
      "case_type": "Difference type",
      "status": "Case status",
      "case_version": "Case version",
      "provider_id": "Provider",
      "provider_version": "Provider version",
      "channel": "Payment channel",
      "business_date": "Case original run business date",
      "merchant_order_no": "Merchant order number",
      "amount_fen": "Provider record amount (fen)",
      "currency": "Currency",
      "provider_transaction_id": "Provider payment transaction ID",
      "payment_evidence_id": "Validated payment evidence ID"
    }
  },
  "ja": {
    "caseRepairStart": "この案件の支払証拠を確認",
    "caseContextTitle": "案件の出所（照合用）",
    "caseContextHelp": "案件の元の照合実行に属する記録です。証拠の一致や結案資格を示すものではありません。チャネル・業務日・金額を照合してから、サーバーの事前検証で支払証拠を確認してください。",
    "caseContextRead": "案件の出所を読み込む",
    "caseContextReading": "案件の出所を読み込み中…",
    "caseContextDenied": "照合案件の読取権限がありません。出所は読み込まれていません。従来の手入力と承認前検証は引き続き利用できます。",
    "caseContextUnavailable": "案件の出所を確認できませんでした。再読込してください。未取得の情報は一致結果ではありません。",
    "caseContextMissing": "元の金額が未記録または解析不能です",
    "evidenceMatchTitle": "証拠の事前検証記録（合格）",
    "evidenceMatchHelp": "元のサーバー事前検証と凍結投影の記録です。この記録は承認や結案実行の完了を意味しません。実行時に事実が再検証されます。",
    "caseContextLabels": {
      "case_id": "照合案件 ID",
      "reconciliation_case_id": "照合案件 ID",
      "reconciliation_run_id": "案件の元の照合実行 ID",
      "run_id": "案件の元の照合実行 ID",
      "case_type": "差異の種類",
      "status": "案件状態",
      "case_version": "案件バージョン",
      "provider_id": "プロバイダー",
      "provider_version": "プロバイダーバージョン",
      "channel": "支払チャネル",
      "business_date": "案件の元の実行の業務日",
      "merchant_order_no": "加盟店注文番号",
      "amount_fen": "チャネル記録金額（分）",
      "currency": "通貨",
      "provider_transaction_id": "支払チャネル取引 ID",
      "payment_evidence_id": "検証済み支払証拠 ID"
    }
  }
};
for (const language of ['zh', 'en', 'ja']) Object.assign(ledgerRepairCopy[language], reconciliationCaseContextCopy[language]);

const reconciliationCaseHostCopy = {
  "zh": {
    "caseTargetLocked": "案件已固定。如需处理其他案件，请返回案件列表选择。"
  },
  "en": {
    "caseTargetLocked": "This case is fixed. To work on another case, select it from the case list."
  },
  "ja": {
    "caseTargetLocked": "対象案件は固定されています。別の案件を処理するには、案件一覧から選択してください。"
  }
};
for (const language of ['zh', 'en', 'ja']) Object.assign(ledgerRepairCopy[language], reconciliationCaseHostCopy[language]);

const reconciliationCaseDateCopy = {
  "zh": {
    "caseDateUse": "采用案件业务日期",
    "caseDateHelp": "空白或上次自动带入的日期会随案件来源更新；手动填写的日期会保留。可主动采用此案件原始批次的业务日期。"
  },
  "en": {
    "caseDateUse": "Use case business date",
    "caseDateHelp": "An empty date or the previous automatic date follows the accepted case source. Manual edits are preserved. You can explicitly use the business date of this case’s original run."
  },
  "ja": {
    "caseDateUse": "案件の業務日を使用",
    "caseDateHelp": "空欄または前回自動入力した日付は、取得した案件の出所に合わせて更新されます。手動入力は保持されます。案件の元の実行の業務日を明示的に採用できます。"
  }
};
for (const language of ['zh', 'en', 'ja']) Object.assign(ledgerRepairCopy[language], reconciliationCaseDateCopy[language]);

ledgerRepairCopy.zh.ledgerTargetEntry = {
  task: '修正平台账户的入账归属', help: '根据已获权的账务资料填写完整 ID。读取服务端快照后，再核对分录和两侧账户。',
  fields: {
    target_resource_id: { label: '待修复过账 ID', help: '需要更正归属的过账记录。' },
    target_journal_entry_id: { label: '关联账目分录 ID', help: '包含该笔过账的账目分录。' },
    wrong_ledger_account_id: { label: '当前入账的平台账户 ID', help: '原过账错误计入的账户。' },
    correct_ledger_account_id: { label: '应入账的平台账户 ID', help: '更正后应归属的账户。' },
  },
};
ledgerRepairCopy.en.ledgerTargetEntry = {
  task: 'Correct the platform account assigned to a posting', help: 'Enter full IDs from records you are authorized to view. Check the journal entry and both accounts after reading the server snapshot.',
  fields: {
    target_resource_id: { label: 'Posting ID to correct', help: 'The posting recorded under the wrong account.' },
    target_journal_entry_id: { label: 'Related journal entry ID', help: 'The journal entry that contains this posting.' },
    wrong_ledger_account_id: { label: 'Platform account ID currently posted to', help: 'The account that received the original posting in error.' },
    correct_ledger_account_id: { label: 'Intended platform account ID', help: 'The account that should receive the posting.' },
  },
};
ledgerRepairCopy.ja.ledgerTargetEntry = {
  task: '転記先のプラットフォーム口座を訂正', help: '閲覧権限のある帳簿から ID をすべて入力します。サーバーのスナップショット取得後に、仕訳と両方の口座を確認してください。',
  fields: {
    target_resource_id: { label: '訂正する転記 ID', help: '誤った口座に記録された転記です。' },
    target_journal_entry_id: { label: '関連する仕訳 ID', help: 'この転記を含む仕訳です。' },
    wrong_ledger_account_id: { label: '現在の転記先口座 ID', help: '誤って転記されたプラットフォーム口座です。' },
    correct_ledger_account_id: { label: '正しい転記先口座 ID', help: '本来の転記先となるプラットフォーム口座です。' },
  },
};

ledgerRepairCopy.zh.ledgerImpact = {
  refreshing: "正在重新读取这笔更正的预览。",
  title: '受影响的账户', original: '当前入账账户', intended: '应入账账户', accountType: '账户类型',
  requested: '申请更正金额', points: value => value + ' 积分', notProvided: '未提供', identifiers: '查看冻结标识和参数',
  limit: '快照未提供账户增减方向和更正前后的余额。这里显示的是申请金额，实际结果以审批执行和账本为准。',
  types: { PAYMENT_CLEARING: '支付清算', USAGE_REVENUE: '用量收入', WALLET_LIABILITY: '钱包负债', RECEIVABLE_ASSET: '应收资产', ADJUSTMENT: '调账', REFUND_CLEARING: '退款清算', CHARGEBACK_CLEARING: '拒付清算' },
};
ledgerRepairCopy.en.ledgerImpact = {
  refreshing: "Refreshing the preview for this correction.",
  title: 'Accounts affected', original: 'Account currently posted to', intended: 'Intended account', accountType: 'Account type',
  requested: 'Requested correction amount', points: value => value + ' points', notProvided: 'Not provided', identifiers: 'View frozen IDs and parameters',
  limit: 'The snapshot does not provide the direction of each balance change or before and after balances. This is the requested amount. Check approval execution and the ledger for the actual result.',
  types: { PAYMENT_CLEARING: 'Payment clearing', USAGE_REVENUE: 'Usage revenue', WALLET_LIABILITY: 'Wallet liability', RECEIVABLE_ASSET: 'Receivable asset', ADJUSTMENT: 'Adjustment', REFUND_CLEARING: 'Refund clearing', CHARGEBACK_CLEARING: 'Chargeback clearing' },
};
ledgerRepairCopy.ja.ledgerImpact = {
  refreshing: "この訂正のプレビューを再取得しています。",
  title: '影響を受ける口座', original: '現在の転記先口座', intended: '正しい転記先口座', accountType: '口座種別',
  requested: '訂正申請額', points: value => value + ' ポイント', notProvided: '未提供', identifiers: '取得時の ID とパラメーターを表示',
  limit: 'スナップショットに各口座の増減方向や訂正前後の残高は含まれていません。表示額は申請額です。結果は承認の実行と台帳で確認してください。',
  types: { PAYMENT_CLEARING: '支払い精算', USAGE_REVENUE: '利用収益', WALLET_LIABILITY: 'ウォレット負債', RECEIVABLE_ASSET: '未収資産', ADJUSTMENT: '調整', REFUND_CLEARING: '返金精算', CHARGEBACK_CLEARING: 'チャージバック精算' },
};

ledgerRepairCopy.zh.ledgerWorkflow = { title: '财务修复步骤', steps: ["核对修复目标","读取快照并核对影响","创建审批草稿"], phaseReading: "正在读取快照", phaseReview: "可核对影响", approvalNext: "草稿创建成功后，在审批中心继续处理。", load: '读取服务端快照', loading: '正在读取服务端快照…' };
ledgerRepairCopy.en.ledgerWorkflow = { title: 'Financial repair steps', steps: ["Identify the target","Read the snapshot and check the impact","Create approval draft"], phaseReading: "Reading the snapshot", phaseReview: "Ready to check the impact", approvalNext: "After the draft is created, continue in the Approval Center.", load: 'Read server snapshot', loading: 'Reading server snapshot…' };
ledgerRepairCopy.ja.ledgerWorkflow = { title: '財務修復の手順', steps: ["修復対象を確認","スナップショットを取得して影響を確認","承認の下書きを作成"], phaseReading: "スナップショットを取得中", phaseReview: "影響を確認できます", approvalNext: "下書きの作成後は承認センターで手続きを続けます。", load: 'サーバーのスナップショットを取得', loading: 'サーバーのスナップショットを取得中…' };

ledgerRepairCopy.zh.receivableTarget = {
  intro: '用同一条应收记录的完整 ID 和当前版本核对核销对象。版本用于检测记录变化。',
  fields: { target_resource_id: { label: '待核销应收 ID', help: '输入要核销的那条应收记录。' }, expected_version: { label: '记录版本（核对用）', help: '输入已从获准的账务记录核实的当前版本。本页不会自动填入版本。' } },
  confirmed: '快照对应的应收记录', version: '快照记录版本', recheck: '请核对原应收记录的 ID 和当前版本，再重新读取。',
};
ledgerRepairCopy.en.receivableTarget = {
  intro: 'Verify the write-off target using the full ID and current version of the same receivable. The version detects changes to the record.',
  fields: { target_resource_id: { label: 'Receivable ID to write off', help: 'Enter the ID of the exact receivable record.' }, expected_version: { label: 'Record version for verification', help: 'Enter the current version you verified from an authorized financial record. This form does not fill it in automatically.' } },
  confirmed: 'Receivable in this snapshot', version: 'Record version in this snapshot', recheck: 'Check the ID and current version of the original receivable, then read again.',
};
ledgerRepairCopy.ja.receivableTarget = {
  intro: '同じ未収金の完全な ID と現在の版で対象を確認します。版は記録の変更を検出するために使います。',
  fields: { target_resource_id: { label: '償却する未収金の ID', help: '償却対象の未収金記録の ID を入力してください。' }, expected_version: { label: '記録の版（確認用）', help: '参照権限のある会計記録で確認した現在の版を入力してください。このフォームには自動入力されません。' } },
  confirmed: 'スナップショットの未収金', version: 'スナップショットの記録の版', recheck: '元の未収金の ID と現在の版を確認してから、再度取得してください。',
};

ledgerRepairCopy.zh.receivableAmounts = {
  title: '核销金额核对', field: { label: '申请核销金额（微积分）', help: '填写大于零的整数，不得超过该记录的未核销余额。10,000 微积分等于 1 积分。', invalid: '请输入 1 到 9223372036854775807 之间的整数微积分。' },
  outstanding: '快照中的未核销余额', requested: '申请核销金额', points: value => value + ' 积分',
  full: '拟全额核销', partial: '拟部分核销', limit: '这些金额来自已读取的快照，核销后的余额以审批执行和应收记录为准。',
};
ledgerRepairCopy.en.receivableAmounts = {
  title: 'Review write-off amounts', field: { label: 'Requested write-off (microcredits)', help: 'Enter a positive whole amount no higher than this record’s outstanding balance. 10,000 microcredits equal 1 point.', invalid: 'Enter a whole number from 1 to 9223372036854775807 microcredits.' },
  outstanding: 'Outstanding balance in this snapshot', requested: 'Requested write-off', points: value => value + ' points',
  full: 'Requested in full', partial: 'Requested in part', limit: 'These amounts are from the snapshot. Check approval execution and the receivable record for the balance after write-off.',
};
ledgerRepairCopy.ja.receivableAmounts = {
  title: '償却額の確認', field: { label: '申請する償却額（マイクロクレジット）', help: 'この記録の未償却残高を超えない正の整数を入力してください。10,000 マイクロクレジットは 1 ポイントです。', invalid: '1 から 9223372036854775807 までの整数をマイクロクレジットで入力してください。' },
  outstanding: 'スナップショットの未償却残高', requested: '申請する償却額', points: value => value + ' ポイント',
  full: '全額の償却を申請', partial: '一部の償却を申請', limit: '金額は取得済みのスナップショットに基づきます。償却後の残高は承認の実行結果と未収金記録で確認してください。',
};

ledgerRepairCopy.zh.receivableTask = { action: '当前任务', name: '应收核销', lead: '填写目标与金额，读取服务端快照并核对后，提交核销审批。', submit: '提交核销审批', submitting: '正在提交审批…', submitHelp: '系统会按当前状态创建草稿或更新已有的可编辑草稿，并在审批状态允许时提交。审批通过且执行完成后，才会核销应收。', recoveryHelp: '使用原请求确认提交结果。结果明确前，当前单据和已填内容会保留。', submissionConfirmed: '已确认提交核销审批。请在审批中心查看最新状态。' };
ledgerRepairCopy.en.receivableTask = { action: 'Current task', name: 'Receivable write-off', lead: 'Enter the target and amount, read and review the server snapshot, then submit the write-off for approval.', submit: 'Submit for write-off approval', submitting: 'Submitting for approval…', submitHelp: 'The system creates a draft or updates an existing editable draft and submits it when the approval status allows. The receivable is written off only after approval and execution.', recoveryHelp: 'Use the original request to confirm the submission result. The current receivable and your entries stay in place until the result is confirmed.', submissionConfirmed: 'Write-off approval submission confirmed. Check the Approval Center for the latest status.' };
ledgerRepairCopy.ja.receivableTask = { action: '現在のタスク', name: '未収金の償却', lead: '対象と金額を入力し、サーバーのスナップショットを取得して確認してから、償却の承認を申請します。', submit: '償却の承認を申請', submitting: '承認を申請中…', submitHelp: '現在の状態に応じて下書きを作成するか、編集可能な既存の下書きを更新し、承認状態が許可する場合に申請します。承認後、処理が実行されてから未収金が償却されます。', recoveryHelp: '元のリクエストで申請結果を確認します。結果が確認されるまで、対象の未収金と入力内容は保持されます。', submissionConfirmed: '償却の承認申請を確認しました。最新の状態は承認センターで確認してください。' };

ledgerRepairCopy.zh.paymentRelation = {
  "intro": "输入要核对的支付订单和这笔支付已经保存的凭证。读取后会核对它们是否属于同一笔支付。",
  "title": "订单与凭证核对",
  "fields": [
    "支付订单 ID",
    "已保存的支付凭证 ID",
    "渠道交易流水",
    "订单金额（分）"
  ],
  "limit": "此快照未提供币种、渠道名称或实时收款状态。",
  "reading": "正在核对当前输入…",
  "matched": "已核对本次读取的关联。",
  "pending": "仍在确认上一次审批请求的结果。",
  "waiting": "请读取快照以核对当前输入。"
};
ledgerRepairCopy.en.paymentRelation = {
  "intro": "Enter the payment order and the evidence already recorded for that payment. The snapshot checks that they refer to the same payment.",
  "title": "Order and evidence",
  "fields": [
    "Payment order ID",
    "Recorded payment evidence ID",
    "Provider transaction reference",
    "Order amount (minor units)"
  ],
  "limit": "This snapshot does not include the currency, provider name, or live payment status.",
  "reading": "Checking the current references…",
  "matched": "These references match in the loaded snapshot.",
  "pending": "The previous approval request is still being confirmed.",
  "waiting": "Load the snapshot to check the current references."
};
ledgerRepairCopy.ja.paymentRelation = {
  "intro": "対象の支払い注文と、その支払いについて記録済みの証拠を入力してください。スナップショットで同じ支払いかを確認します。",
  "title": "注文と証拠の確認",
  "fields": [
    "支払い注文 ID",
    "記録済みの支払い証拠 ID",
    "決済事業者の取引参照",
    "注文額（最小通貨単位）"
  ],
  "limit": "このスナップショットには通貨、決済事業者名、現在の支払い状態が含まれません。",
  "reading": "現在の参照を照合中…",
  "matched": "取得したスナップショットでこれらの参照が一致しました。",
  "pending": "前回の承認リクエストの結果を確認中です。",
  "waiting": "スナップショットを取得して現在の参照を照合してください。"
};

ledgerRepairCopy.zh.paymentImpact = {
  "title": "拟议账务影响",
  "gate": "当前只核对快照。提交的是审批请求，处理进度以审批详情为准。",
  "missing": "此快照未提供",
  "unit": "积分",
  "amount": "最小货币单位",
  "ref": "审批请求",
  "state": "审批状态",
  "unknown": "请查看审批详情",
  "states": {
    "SUBMITTED": "已提交，待审批",
    "APPROVED": "已批准，待执行",
    "EXECUTING": "正在执行",
    "EXECUTED": "执行完成",
    "EXECUTION_FAILED": "执行失败"
  },
  "scenario": "适用于支付渠道已确认收款、本地账务尚未入账的订单。",
  "fields": [
    "拟补记积分",
    "拟抵扣应收积分",
    "目标钱包",
    "当前钱包余额"
  ]
};
ledgerRepairCopy.en.paymentImpact = {
  "title": "Proposed ledger impact",
  "gate": "This is a snapshot. Submission starts an approval request; check its details for processing status.",
  "missing": "Not provided in this snapshot",
  "unit": "points",
  "amount": "minor currency units",
  "ref": "Approval request",
  "state": "Approval status",
  "unknown": "See approval details",
  "states": {
    "SUBMITTED": "Submitted, awaiting approval",
    "APPROVED": "Approved, awaiting execution",
    "EXECUTING": "Executing",
    "EXECUTED": "Execution complete",
    "EXECUTION_FAILED": "Execution failed"
  },
  "scenario": "Use this for an order the payment provider confirmed as paid but the local ledger has yet to credit.",
  "fields": [
    "Points to credit",
    "Points to apply to receivables",
    "Destination wallet",
    "Current wallet balance"
  ]
};
ledgerRepairCopy.ja.paymentImpact = {
  "title": "予定される台帳への影響",
  "gate": "これはスナップショットです。送信すると承認申請が開始されます。進捗は申請の詳細で確認してください。",
  "missing": "このスナップショットにはありません",
  "unit": "ポイント",
  "amount": "通貨の最小単位",
  "ref": "承認申請",
  "state": "承認状況",
  "unknown": "承認申請の詳細を確認",
  "states": {
    "SUBMITTED": "送信済み・承認待ち",
    "APPROVED": "承認済み・実行待ち",
    "EXECUTING": "実行中",
    "EXECUTED": "実行完了",
    "EXECUTION_FAILED": "実行失敗"
  },
  "scenario": "決済事業者が入金を確認済みで、ローカル台帳に未計上の注文に使用します。",
  "fields": [
    "計上予定のポイント",
    "未収分に充当するポイント",
    "対象ウォレット",
    "現在のウォレット残高"
  ]
};

ledgerRepairCopy.zh.paymentTask = {
  "identity": "核对对象和证据",
  "approval": "提交审批请求",
  "command": "命令代码",
  "reading": "正在加载核对快照…",
  "read": "加载核对快照",
  "submitting": "正在提交审批请求…",
  "badID": "请输入完整、无首尾空格的 UUID。",
  "evidence": "填写已持久化的支付证据 UUID。",
  "anomaly": "填写这笔退款对应的异常案件 UUID。",
  "approvalHelp": "核对上方快照后填写审批理由及请求标识。",
  "title": "支付入账补记",
  "target": "填写平台支付订单 UUID；商户订单号不能用于此字段。",
  "factsFailed": "无法加载支付补记的核对快照。请检查订单和支付证据后重试。"
};
ledgerRepairCopy.en.paymentTask = {
  "identity": "Check the target and evidence",
  "approval": "Submit for approval",
  "command": "Command code",
  "reading": "Loading verification snapshot…",
  "read": "Load verification snapshot",
  "submitting": "Submitting approval request…",
  "badID": "Enter the full UUID without leading or trailing spaces.",
  "evidence": "Enter the UUID of the stored payment evidence.",
  "anomaly": "Enter the UUID of the anomaly case for this refund.",
  "approvalHelp": "Review the snapshot above, then provide the reason and request reference.",
  "title": "Credit a confirmed payment",
  "target": "Enter the platform payment order UUID. The merchant order number is not accepted here.",
  "factsFailed": "Could not load the payment verification snapshot. Check the order and evidence, then retry."
};
ledgerRepairCopy.ja.paymentTask = {
  "identity": "対象と証拠を確認",
  "approval": "承認を申請",
  "command": "コマンドコード",
  "reading": "確認用スナップショットを読み込み中…",
  "read": "確認用スナップショットを読み込む",
  "submitting": "承認申請を送信中…",
  "badID": "前後に空白を付けず、完全な UUID を入力してください。",
  "evidence": "保存済みの決済証拠の UUID を入力してください。",
  "anomaly": "この返金に対応する異常案件の UUID を入力してください。",
  "approvalHelp": "上のスナップショットを確認し、申請理由と申請識別子を入力してください。",
  "title": "確認済みの決済を追加入金",
  "target": "プラットフォームの決済注文 UUID を入力します。加盟店の注文番号は使えません。",
  "factsFailed": "決済の確認用スナップショットを読み込めませんでした。注文と決済証拠を確認して再試行してください。"
};

ledgerRepairCopy.zh.refundRelation = {
  "intro": "请提供这次退款、已保存的支付凭证及对应异常案件。系统会在同一次读取中核对三个对象。",
  "title": "退款、凭证与案件核对",
  "fields": [
    "退款 ID",
    "已保存的支付凭证 ID",
    "异常案件 ID",
    "渠道退款流水"
  ],
  "limit": "此快照未提供原支付订单详情或可选凭证列表。",
  "reading": "正在核对当前输入…",
  "matched": "已核对本次读取的关联。",
  "pending": "仍在确认上一次审批请求的结果。",
  "waiting": "请读取快照以核对当前输入。"
};
ledgerRepairCopy.en.refundRelation = {
  "intro": "Provide the refund, its recorded payment evidence, and the related exception case. The snapshot checks the three references together.",
  "title": "Refund, evidence and exception case",
  "fields": [
    "Refund ID",
    "Recorded payment evidence ID",
    "Exception case ID",
    "Provider refund reference"
  ],
  "limit": "This snapshot does not include the original payment order details or a list of eligible evidence.",
  "reading": "Checking the current references…",
  "matched": "These references match in the loaded snapshot.",
  "pending": "The previous approval request is still being confirmed.",
  "waiting": "Load the snapshot to check the current references."
};
ledgerRepairCopy.ja.refundRelation = {
  "intro": "対象の返金、記録済みの支払い証拠、対応する例外案件を入力してください。スナップショットで三つの参照をまとめて照合します。",
  "title": "返金・証拠・例外案件の確認",
  "fields": [
    "返金 ID",
    "記録済みの支払い証拠 ID",
    "例外案件 ID",
    "決済事業者の返金参照"
  ],
  "limit": "元の支払い注文の詳細や選択可能な証拠の一覧は、このスナップショットに含まれません。",
  "reading": "現在の参照を照合中…",
  "matched": "取得したスナップショットでこれらの参照が一致しました。",
  "pending": "前回の承認リクエストの結果を確認中です。",
  "waiting": "スナップショットを取得して現在の参照を照合してください。"
};
