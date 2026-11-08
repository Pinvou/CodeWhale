export const accountLayoutCopy = {
  zh: {
    closeDialog: '关闭', edit: '修改', confirmSwitch: '确认切换', currentContext: '当前工作身份', accountDetails: '账号信息',
    recoveryPending: '原请求结果尚未确认，请仅恢复原请求。',
    invoices: '发票', invoicesUnavailable: '当前暂不支持在线开票，请联系平台支持。',
    allPackages: '查看全部权益', packageDetails: '权益详情', remaining: '剩余积分', validUntil: '有效期至',
    packagesSummary: (count, points) => `${count} 个有效权益包 · 剩余 ${points} 积分`,
    statuses: { ACTIVE: '有效', DISABLED: '已停用', CLOSING: '注销处理中', COMPLETED: '已完成', PAID: '已支付', PAID_LATE: '晚到支付', CREATED: '已创建', PENDING: '待支付', CLOSED: '已关闭', FAILED: '失败', MANUAL_REVIEW: '人工复核', EXPIRED: '已到期', CONSUMED: '已用完', REFUNDED: '已退款', FROZEN: '已冻结' },
  },
  en: {
    closeDialog: 'Close', edit: 'Edit', confirmSwitch: 'Confirm switch', currentContext: 'Current identity', accountDetails: 'Account information',
    recoveryPending: 'The original result is unconfirmed. Recover only the original request.',
    invoices: 'Invoices', invoicesUnavailable: 'Online invoicing is currently unavailable. Contact platform support.',
    allPackages: 'View all benefits', packageDetails: 'Benefit details', remaining: 'Available points', validUntil: 'Valid until',
    packagesSummary: (count, points) => `${count} active benefit packages · ${points} points remaining`,
    statuses: { ACTIVE: 'Active', DISABLED: 'Disabled', CLOSING: 'Closing', COMPLETED: 'Completed', PAID: 'Paid', PAID_LATE: 'Late payment', CREATED: 'Created', PENDING: 'Awaiting payment', CLOSED: 'Closed', FAILED: 'Failed', MANUAL_REVIEW: 'Under review', EXPIRED: 'Expired', CONSUMED: 'Used up', REFUNDED: 'Refunded', FROZEN: 'Frozen' },
  },
  ja: {
    closeDialog: '閉じる', edit: '変更', confirmSwitch: '切り替えを確定', currentContext: '現在の利用主体', accountDetails: 'アカウント情報',
    recoveryPending: '元の要求の結果は未確認です。元の要求だけを復旧してください。',
    invoices: '請求書', invoicesUnavailable: '現在オンラインでの請求書発行には対応していません。サポートにお問い合わせください。',
    allPackages: 'すべての特典を表示', packageDetails: '特典の詳細', remaining: '利用可能ポイント', validUntil: '有効期限',
    packagesSummary: (count, points) => `有効な特典 ${count} 件 · 残り ${points} ポイント`,
    statuses: { ACTIVE: '有効', DISABLED: '無効', CLOSING: '削除処理中', COMPLETED: '完了', PAID: '支払済み', PAID_LATE: '遅延支払い', CREATED: '作成済み', PENDING: '支払い待ち', CLOSED: '終了', FAILED: '失敗', MANUAL_REVIEW: '確認中', EXPIRED: '期限切れ', CONSUMED: '使用済み', REFUNDED: '返金済み', FROZEN: '凍結中' },
  },
};

export const adminLayoutCopy = {
  zh: {
    product: 'ACCOUNT OS', primaryNavigation: '管理后台导航', skipToContent: '跳到主要内容', home: '个人中心',
    navGroups: { workbench: '工作台', identity: '身份与组织', finance: '资金与计费', models: '模型与 Token', governance: '治理与安全' },
    organization: { overview: '组织概览', members: '成员管理', units: '组织单元', lifecycle: '生命周期', allStatuses: '全部状态', resetFilters: '重置筛选', allRoles: '全部角色', identifier: '组织 ID', copyIdentifier: '复制 ID', copyFailed: '复制失败，请手动选择并复制。', version: '当前版本', actions: '更多操作', confirmationName: '输入组织名称以确认', accessAndRoles: '查看成员与角色', pageCount: count => `本批 ${count} 个组织`, directoryPagination: { label: '组织列表翻页', first: '首批结果', later: '后续批次', previous: '上一批', next: '下一批', previousUnavailable: '没有可用的上一批记录。', previousResult: '当前未能更新，显示上次成功读取的结果。', appliedFilters: '已应用的筛选：', noFilters: '未设筛选条件', appliedQuery: value => '名称或 ID：' + value, appliedStatus: value => '状态：' + value, limits: limit => `每批最多 ${limit} 个。列表不提供总数或页码。` }, notFound: '组织不存在或已不可访问。', conflict: '组织状态已变化，请刷新后重试。' },
    nav: { overview: '平台总览', users: '用户管理', roles: '角色与权限', organizations: '组织管理', myorg: '我的组织', billing: '账户与计费', payments: '支付与对账', products: '定价与商品', tokens: 'Token Gateway', models: '模型管理', tokenResources: 'Token 资源', modelOperations: '运行与用量', operations: '运营管理', approvals: '审批中心', audit: '审计与导出', system: '系统与安全', settings: '平台设置' },
    readOnly: '只读访问', controlled: '权限控制', unauthorized: '无权访问此页面',
    domains: { account: '身份与组织', billing: '资金与计费', token: '模型与 Token' },
    metrics: { wallets: '钱包总数', routes: '路由总数', reconciliation_pending: '待核对用量', reconciliation_stopped: '已停止核对', users: '用户总数', active_users: '活跃用户', disabled_users: '停用用户', organizations: '组织总数', active_memberships: '活跃成员关系', pending_invitations: '待接受邀请', posted_microcredits: '账面积分', reserved_microcredits: '冻结积分', receivable_microcredits: '应收积分', pending_payment_orders: '待支付订单', review_payment_orders: '待复核订单', pending_adjustments: '待审批调账', token_accounts: 'Token 账户', active_accounts: '活跃 Token 账户', active_credentials: '有效凭证', healthy_routes: '健康路由', usage_requests: '用量请求数', review_usage_requests: '待复核用量' },
    unavailable: '暂时无法读取数据，请重试。', notAvailable: '当前服务未提供此数据。',
    backToDirectory: '返回目录', userID: '用户 ID', createdAt: '创建时间', lastSeen: '最近活跃', memberships: '所属组织', selfStatusBlocked: '不能停用当前登录的管理员。', disableSessionsWarning: '停用后，此用户的现有会话将失效。', confirmAction: '确认操作',
  },
  en: {
    product: 'ACCOUNT OS', primaryNavigation: 'Administration navigation', skipToContent: 'Skip to main content', home: 'Personal center',
    navGroups: { workbench: 'Workbench', identity: 'Identity & organizations', finance: 'Finance & billing', models: 'Models & Tokens', governance: 'Governance & security' },
    organization: { overview: 'Overview', members: 'Members', units: 'Units', lifecycle: 'Lifecycle', allStatuses: 'All statuses', resetFilters: 'Reset filters', allRoles: 'All roles', identifier: 'Organization ID', copyIdentifier: 'Copy ID', copyFailed: 'Copy failed. Select and copy the ID manually.', version: 'Current version', actions: 'More actions', confirmationName: 'Enter the organization name to confirm', accessAndRoles: 'View members and roles', pageCount: count => `${count} in this batch`, directoryPagination: { label: 'Organization list navigation', first: 'First batch', later: 'Later batch', previous: 'Previous batch', next: 'Next batch', previousUnavailable: 'No previous batch is available from this view.', previousResult: 'The list could not be updated. Showing the last successful result.', appliedFilters: 'Applied filters: ', noFilters: 'None', appliedQuery: value => 'Name or ID: ' + value, appliedStatus: value => 'Status: ' + value, limits: limit => `Up to ${limit} organizations per batch. No total or page numbers are available.` }, notFound: 'Organization not found or no longer accessible.', conflict: 'The organization changed. Refresh and try again.' },
    nav: { overview: 'Platform overview', users: 'Users', roles: 'Roles & permissions', organizations: 'Organizations', myorg: 'My organization', billing: 'Accounts & billing', payments: 'Payments & reconciliation', products: 'Pricing & products', tokens: 'Token Gateway', models: 'Models', tokenResources: 'Token resources', modelOperations: 'Operations & usage', operations: 'Operations', approvals: 'Approvals', audit: 'Audit & exports', system: 'System & security', settings: 'Platform settings' },
    readOnly: 'Read-only access', controlled: 'Permission controlled', unauthorized: 'You cannot access this page',
    domains: { account: 'Identity & organizations', billing: 'Finance & billing', token: 'Models & Tokens' },
    metrics: { wallets: 'Wallets', routes: 'Routes', reconciliation_pending: 'Pending reconciliation', reconciliation_stopped: 'Stopped reconciliation', users: 'Total users', active_users: 'Active users', disabled_users: 'Disabled users', organizations: 'Organizations', active_memberships: 'Active memberships', pending_invitations: 'Pending invitations', posted_microcredits: 'Posted points', reserved_microcredits: 'Reserved points', receivable_microcredits: 'Receivable points', pending_payment_orders: 'Awaiting payment', review_payment_orders: 'Orders under review', pending_adjustments: 'Pending adjustments', token_accounts: 'Token accounts', active_accounts: 'Active Token accounts', active_credentials: 'Active credentials', healthy_routes: 'Healthy routes', usage_requests: 'Usage requests', review_usage_requests: 'Usage under review' },
    unavailable: 'Data is temporarily unavailable. Try again.', notAvailable: 'This service does not currently provide this data.',
    backToDirectory: 'Back to directory', userID: 'User ID', createdAt: 'Created at', lastSeen: 'Last active', memberships: 'Organizations', selfStatusBlocked: 'You cannot disable the signed-in administrator.', disableSessionsWarning: 'Disabling this user invalidates their existing sessions.', confirmAction: 'Confirm action',
  },
  ja: {
    product: 'ACCOUNT OS', primaryNavigation: '管理ナビゲーション', skipToContent: 'メインコンテンツへ移動', home: '個人センター',
    navGroups: { workbench: 'ワークベンチ', identity: '認証と組織', finance: '資金と課金', models: 'モデルと Token', governance: 'ガバナンスと安全性' },
    organization: { overview: '組織の概要', members: 'メンバー管理', units: '組織単位', lifecycle: 'ライフサイクル', allStatuses: 'すべての状態', resetFilters: '絞り込みをリセット', allRoles: 'すべてのロール', identifier: '組織 ID', copyIdentifier: 'ID をコピー', copyFailed: 'コピーできません。ID を選択して手動でコピーしてください。', version: '現在のバージョン', actions: 'その他の操作', confirmationName: '組織名を入力して確認', accessAndRoles: 'メンバーとロールを表示', pageCount: count => `表示中の組織：${count} 件`, directoryPagination: { label: '組織一覧のページ移動', first: '最初の結果', later: '続きの結果', previous: '前の結果', next: '次の結果', previousUnavailable: 'この画面から前の結果には戻れません。', previousResult: '一覧を更新できませんでした。前回取得できた結果を表示しています。', appliedFilters: '適用中の絞り込み：', noFilters: '条件なし', appliedQuery: value => '名前または ID：' + value, appliedStatus: value => '状態：' + value, limits: limit => `1 回の表示は最大 ${limit} 件です。総件数とページ番号は表示されません。` }, notFound: '組織が存在しないか、アクセスできません。', conflict: '組織の状態が変わりました。更新して再試行してください。' },
    nav: { overview: 'プラットフォーム概要', users: 'ユーザー管理', roles: 'ロールと権限', organizations: '組織管理', myorg: '自分の組織', billing: 'アカウントと課金', payments: '支払いと照合', products: '料金と商品', tokens: 'Token Gateway', models: 'モデル管理', tokenResources: 'Token リソース', modelOperations: '稼働と利用量', operations: '運用管理', approvals: '承認センター', audit: '監査とエクスポート', system: 'システムと安全性', settings: 'プラットフォーム設定' },
    readOnly: '閲覧専用', controlled: '権限制御', unauthorized: 'このページへのアクセス権限がありません',
    domains: { account: '認証と組織', billing: '資金と課金', token: 'モデルと Token' },
    metrics: { wallets: 'ウォレット数', routes: 'ルート数', reconciliation_pending: '照合待ち', reconciliation_stopped: '停止中の照合', users: 'ユーザー総数', active_users: '有効なユーザー', disabled_users: '無効なユーザー', organizations: '組織数', active_memberships: '有効なメンバー関係', pending_invitations: '承認待ちの招待', posted_microcredits: '帳簿ポイント', reserved_microcredits: '保留ポイント', receivable_microcredits: '未収ポイント', pending_payment_orders: '支払い待ち', review_payment_orders: '確認待ちの注文', pending_adjustments: '承認待ちの調整', token_accounts: 'Token アカウント', active_accounts: '有効な Token アカウント', active_credentials: '有効な資格情報', healthy_routes: '正常なルート', usage_requests: '利用要求数', review_usage_requests: '確認待ちの利用' },
    unavailable: 'データを取得できません。再試行してください。', notAvailable: '現在このサービスはこのデータを提供していません。',
    backToDirectory: '一覧へ戻る', userID: 'ユーザー ID', createdAt: '作成日時', lastSeen: '最終利用', memberships: '所属組織', selfStatusBlocked: 'ログイン中の管理者は無効にできません。', disableSessionsWarning: '無効にすると、このユーザーの既存セッションが失効します。', confirmAction: '操作を確認',
  },
};

// Labels for read-only collections; financial commands keep their current owners.
adminLayoutCopy.zh.financeLists = {"receivables": "应收账款", "refunds": "退款记录", "adjustments": "调账记录", "back": "返回列表", "detailUnavailable": "当前列表中没有该记录，请返回列表调整筛选条件。", "cursorExpired": "分页已过期，请刷新列表。", "allStatuses": "全部状态", "limit": "每页条数", "search": "查询", "receivable_id": "应收 ID", "wallet_id": "钱包 ID", "original_microcredits": "原始积分", "settled_microcredits": "已结清积分", "outstanding_microcredits": "未结清积分", "status": "状态", "operation_id": "退款操作 ID", "operation_type": "退款类型", "payment_order_id": "订单 ID", "amount_fen": "金额", "created_at": "创建时间", "adjustment_id": "调账 ID", "amount_microcredits": "调账积分", "requested_by": "申请人 ID", "q": "关键词", "provider_id": "支付服务商", "created_from": "开始时间", "created_to": "结束时间", "points": "积分", "PARTIALLY_SETTLED": "部分结清", "SETTLED": "已结清", "WRITTEN_OFF": "已核销", "REQUESTED": "待处理", "SUCCEEDED": "已成功", "FAILED": "已失败", "MANUAL_REVIEW": "人工复核", "REVIEW_REQUIRED": "待复核", "RESOLVED": "已解决", "PENDING_APPROVAL": "待审批", "APPROVED": "已批准", "REJECTED": "已拒绝", "POSTED": "已入账", "NORMAL": "普通退款", "FORCED": "强制退款"};
adminLayoutCopy.zh.financeLists.pageCount = count => `本页 ${count} 条`;
adminLayoutCopy.en.financeLists = {"receivables": "Receivables", "refunds": "Refunds", "adjustments": "Adjustments", "back": "Back to list", "detailUnavailable": "This record is not in the current list. Return and adjust the filters.", "cursorExpired": "This page has expired. Refresh the list.", "allStatuses": "All statuses", "limit": "Page size", "search": "Search", "receivable_id": "Receivable ID", "wallet_id": "Wallet ID", "original_microcredits": "Original points", "settled_microcredits": "Settled points", "outstanding_microcredits": "Outstanding points", "status": "Status", "operation_id": "Refund operation ID", "operation_type": "Refund type", "payment_order_id": "Order ID", "amount_fen": "Amount", "created_at": "Created at", "adjustment_id": "Adjustment ID", "amount_microcredits": "Adjustment points", "requested_by": "Requester ID", "q": "Search", "provider_id": "Payment provider", "created_from": "From", "created_to": "Until", "points": "Points", "PARTIALLY_SETTLED": "Partially settled", "SETTLED": "Settled", "WRITTEN_OFF": "Written off", "REQUESTED": "Requested", "SUCCEEDED": "Succeeded", "FAILED": "Failed", "MANUAL_REVIEW": "Manual review", "REVIEW_REQUIRED": "Review required", "RESOLVED": "Resolved", "PENDING_APPROVAL": "Pending approval", "APPROVED": "Approved", "REJECTED": "Rejected", "POSTED": "Posted", "NORMAL": "Normal refund", "FORCED": "Forced refund"};
adminLayoutCopy.en.financeLists.pageCount = count => `${count} records on this page`;
adminLayoutCopy.ja.financeLists = {"receivables": "未収金", "refunds": "返金履歴", "adjustments": "調整履歴", "back": "一覧へ戻る", "detailUnavailable": "現在の一覧に記録がありません。戻って検索条件を変更してください。", "cursorExpired": "ページの有効期限が切れました。更新してください。", "allStatuses": "すべての状態", "limit": "表示件数", "search": "検索", "receivable_id": "未収 ID", "wallet_id": "ウォレット ID", "original_microcredits": "元のポイント", "settled_microcredits": "精算済みポイント", "outstanding_microcredits": "未精算ポイント", "status": "状態", "operation_id": "返金処理 ID", "operation_type": "返金の種類", "payment_order_id": "注文 ID", "amount_fen": "金額", "created_at": "作成日時", "adjustment_id": "調整 ID", "amount_microcredits": "調整ポイント", "requested_by": "申請者 ID", "q": "検索語", "provider_id": "決済サービス", "created_from": "開始日時", "created_to": "終了日時", "points": "ポイント", "PARTIALLY_SETTLED": "一部精算済み", "SETTLED": "精算済み", "WRITTEN_OFF": "償却済み", "REQUESTED": "申請済み", "SUCCEEDED": "成功", "FAILED": "失敗", "MANUAL_REVIEW": "手動確認", "REVIEW_REQUIRED": "確認待ち", "RESOLVED": "解決済み", "PENDING_APPROVAL": "承認待ち", "APPROVED": "承認済み", "REJECTED": "却下済み", "POSTED": "計上済み", "NORMAL": "通常返金", "FORCED": "強制返金"};
adminLayoutCopy.ja.financeLists.pageCount = count => `このページ ${count} 件`;
Object.assign(adminLayoutCopy.zh.financeLists, {"channel": "支付渠道", "owner_type": "钱包类型", "owner_id": "负责人 ID", "entry_type": "账目类型", "business_reference": "业务引用", "anomaly_type": "异常类型", "severity": "严重程度", "next_review_before": "下次复核截止时间", "posting_id": "分录 ID", "journal_entry_id": "凭证 ID", "ledger_account_code": "科目代码", "ledger_account_type": "科目类型", "delta_microcredits": "积分变动", "journal_balance_microcredits": "凭证余额", "description": "说明", "ledger": "钱包账目"});
Object.assign(adminLayoutCopy.en.financeLists, {"channel": "Payment channel", "owner_type": "Wallet type", "owner_id": "Owner ID", "entry_type": "Entry type", "business_reference": "Business reference", "anomaly_type": "Anomaly type", "severity": "Severity", "next_review_before": "Review due before", "posting_id": "Posting ID", "journal_entry_id": "Journal ID", "ledger_account_code": "Account code", "ledger_account_type": "Account type", "delta_microcredits": "Points change", "journal_balance_microcredits": "Journal balance", "description": "Description", "ledger": "Wallet ledger"});
Object.assign(adminLayoutCopy.ja.financeLists, {"channel": "決済チャネル", "owner_type": "ウォレット種類", "owner_id": "担当者 ID", "entry_type": "取引種類", "business_reference": "業務参照", "anomaly_type": "異常の種類", "severity": "重大度", "next_review_before": "次回確認の期限", "posting_id": "仕訳明細 ID", "journal_entry_id": "仕訳 ID", "ledger_account_code": "勘定コード", "ledger_account_type": "勘定種類", "delta_microcredits": "ポイント増減", "journal_balance_microcredits": "仕訳残高", "description": "説明", "ledger": "ウォレット仕訳"});
Object.assign(adminLayoutCopy.zh.financeLists, {"credit_lot_id": "积分批次 ID", "entitlement_package_id": "权益包 ID", "entitlement_component_id": "权益组件 ID", "component_type": "组件类型", "entered_from": "进入复核起始时间", "entered_to": "进入复核结束时间", "overdue": "是否逾期（true / false）"});
Object.assign(adminLayoutCopy.en.financeLists, {"credit_lot_id": "Credit lot ID", "entitlement_package_id": "Benefit package ID", "entitlement_component_id": "Component ID", "component_type": "Component type", "entered_from": "Entered review from", "entered_to": "Entered review until", "overdue": "Overdue (true / false)"});
Object.assign(adminLayoutCopy.ja.financeLists, {"credit_lot_id": "ポイントロット ID", "entitlement_package_id": "特典パッケージ ID", "entitlement_component_id": "コンポーネント ID", "component_type": "コンポーネント種類", "entered_from": "確認開始日時", "entered_to": "確認終了日時", "overdue": "期限超過（true / false）"});
adminLayoutCopy.zh.financeLists.source_type = '来源类型';
adminLayoutCopy.en.financeLists.source_type = 'Source type';
adminLayoutCopy.ja.financeLists.source_type = '取引元の種類';
adminLayoutCopy.zh.financeOverview = {"title": "运营概览", "point": "快照时间", "window": "统计时段", "statuses": "纳入状态", "all": "查看相关记录（未筛选）", "filtered": "查看筛选记录", "units": {"COUNT": "项", "MICROCREDITS": "积分", "CNY_FEN": "人民币元"}, "metrics": {"wallet_count": "钱包数", "posted_microcredits": "账面积分", "reserved_microcredits": "冻结积分", "available_microcredits": "可用积分", "receivable_microcredits": "应收积分", "pending_adjustment_count": "待审批调账", "at_risk_hold_count": "风险用量冻结", "orders_created_count": "新建订单", "payments_received_fen": "收款金额", "late_review_open_count": "待复核晚到支付", "payment_anomaly_open_count": "待处置支付异常", "reconciliation_open_count": "待核对记录", "refund_attention_count": "需关注的退款", "provider_controlled_count": "受控支付服务商", "product_controlled_count": "受控商品", "product_draft_count": "商品草稿", "product_on_sale_count": "在售商品", "product_off_sale_count": "停售商品", "product_deleted_count": "已删除商品", "admission_pending_count": "准入验证待完成", "admission_blocked_count": "准入验证受阻", "price_draft_count": "价格草稿", "price_future_count": "未来价格", "price_active_count": "生效价格", "price_ended_or_cancelled_count": "已结束或取消价格", "metering_active_count": "有效计量单位", "metering_inactive_count": "停用计量单位"}};
adminLayoutCopy.en.financeOverview = {"title": "Operational overview", "point": "Snapshot time", "window": "Reporting window", "statuses": "Included statuses", "all": "View related records (unfiltered)", "filtered": "View filtered records", "units": {"COUNT": "Count", "MICROCREDITS": "Points", "CNY_FEN": "CNY"}, "metrics": {"wallet_count": "Wallets", "posted_microcredits": "Posted points", "reserved_microcredits": "Reserved points", "available_microcredits": "Available points", "receivable_microcredits": "Receivable points", "pending_adjustment_count": "Pending adjustments", "at_risk_hold_count": "At-risk usage holds", "orders_created_count": "Orders created", "payments_received_fen": "Payments received", "late_review_open_count": "Open late-payment reviews", "payment_anomaly_open_count": "Open payment anomalies", "reconciliation_open_count": "Open reconciliation cases", "refund_attention_count": "Refunds requiring attention", "provider_controlled_count": "Controlled payment providers", "product_controlled_count": "Controlled products", "product_draft_count": "Product drafts", "product_on_sale_count": "Products on sale", "product_off_sale_count": "Products off sale", "product_deleted_count": "Deleted products", "admission_pending_count": "Admission pending", "admission_blocked_count": "Admission blocked", "price_draft_count": "Price drafts", "price_future_count": "Future prices", "price_active_count": "Active prices", "price_ended_or_cancelled_count": "Ended or cancelled prices", "metering_active_count": "Active metering units", "metering_inactive_count": "Inactive metering units"}};
adminLayoutCopy.ja.financeOverview = {"title": "運用概要", "point": "取得日時", "window": "集計期間", "statuses": "対象状態", "all": "関連記録を表示（絞り込みなし）", "filtered": "絞り込み結果を表示", "units": {"COUNT": "件", "MICROCREDITS": "ポイント", "CNY_FEN": "人民元"}, "metrics": {"wallet_count": "ウォレット数", "posted_microcredits": "帳簿ポイント", "reserved_microcredits": "保留ポイント", "available_microcredits": "利用可能ポイント", "receivable_microcredits": "未収ポイント", "pending_adjustment_count": "承認待ち調整", "at_risk_hold_count": "リスクのある利用保留", "orders_created_count": "新規注文", "payments_received_fen": "入金額", "late_review_open_count": "遅延入金の確認待ち", "payment_anomaly_open_count": "未処理の決済異常", "reconciliation_open_count": "照合待ち", "refund_attention_count": "確認が必要な返金", "provider_controlled_count": "制限中の決済サービス", "product_controlled_count": "制限中の商品", "product_draft_count": "商品ドラフト", "product_on_sale_count": "販売中の商品", "product_off_sale_count": "販売停止商品", "product_deleted_count": "削除済み商品", "admission_pending_count": "利用審査待ち", "admission_blocked_count": "利用審査の問題", "price_draft_count": "価格ドラフト", "price_future_count": "将来の価格", "price_active_count": "有効な価格", "price_ended_or_cancelled_count": "終了または取消済み価格", "metering_active_count": "有効な計量単位", "metering_inactive_count": "無効な計量単位"}};
accountLayoutCopy.zh.statuses.DELETED = "已注销";
accountLayoutCopy.en.statuses.DELETED = "Deleted";
accountLayoutCopy.ja.statuses.DELETED = "削除済み";
adminLayoutCopy.zh.financeLists.lifecycle_status = '商品状态';
adminLayoutCopy.en.financeLists.lifecycle_status = 'Product status';
adminLayoutCopy.ja.financeLists.lifecycle_status = '商品状態';
adminLayoutCopy.zh.financeLists.include_deleted = '包含已删除商品（true / false）';
adminLayoutCopy.en.financeLists.include_deleted = 'Include deleted products (true / false)';
adminLayoutCopy.ja.financeLists.include_deleted = '削除済み商品を含む（true / false）';
Object.assign(adminLayoutCopy.zh.financeLists, {"allValues": "全部", "true": "是", "false": "否", "DRAFT": "草稿", "ON_SALE": "在售", "OFF_SALE": "停售", "DELETED": "已删除", "PERSONAL": "个人", "ORGANIZATION": "组织", "ORG_UNIT": "组织单元", "RECHARGE": "充值", "USAGE": "用量扣费", "REFUND": "退款", "REVERSAL": "冲正", "ADJUSTMENT": "调账", "EXPIRY": "到期", "RECEIVABLE": "应收", "CHARGEBACK": "拒付", "REPAIR_CREDIT": "修复入账", "FORCED_REFUND": "强制退款", "USAGE_OVERAGE": "用量超额", "DUPLICATE_PAYMENT": "重复支付", "LATE_PAYMENT_CONFLICT": "晚到支付冲突", "REFUND_FAILURE": "退款失败", "PAYMENT_FACT_CONFLICT": "支付事实冲突", "ILLEGAL_TRANSITION": "非法状态转换", "REVIEW_REQUIRED_LATE_PAYMENT": "晚到支付待复核", "PROVIDER_BLIND": "渠道事实不可见", "ARCHIVED_CONFIRMED_UNPAID": "已归档且确认未支付", "RESOLVED_PAID": "已确认支付"});
Object.assign(adminLayoutCopy.en.financeLists, {"allValues": "All", "true": "Yes", "false": "No", "DRAFT": "Draft", "ON_SALE": "On sale", "OFF_SALE": "Off sale", "DELETED": "Deleted", "PERSONAL": "Personal", "ORGANIZATION": "Organization", "ORG_UNIT": "Organization unit", "RECHARGE": "Recharge", "USAGE": "Usage", "REFUND": "Refund", "REVERSAL": "Reversal", "ADJUSTMENT": "Adjustment", "EXPIRY": "Expiry", "RECEIVABLE": "Receivable", "CHARGEBACK": "Chargeback", "REPAIR_CREDIT": "Repair credit", "FORCED_REFUND": "Forced refund", "USAGE_OVERAGE": "Usage overage", "DUPLICATE_PAYMENT": "Duplicate payment", "LATE_PAYMENT_CONFLICT": "Late-payment conflict", "REFUND_FAILURE": "Refund failure", "PAYMENT_FACT_CONFLICT": "Payment fact conflict", "ILLEGAL_TRANSITION": "Illegal transition", "REVIEW_REQUIRED_LATE_PAYMENT": "Late payment requiring review", "PROVIDER_BLIND": "Provider facts unavailable", "ARCHIVED_CONFIRMED_UNPAID": "Archived and confirmed unpaid", "RESOLVED_PAID": "Confirmed paid"});
Object.assign(adminLayoutCopy.ja.financeLists, {"allValues": "すべて", "true": "はい", "false": "いいえ", "DRAFT": "ドラフト", "ON_SALE": "販売中", "OFF_SALE": "販売停止", "DELETED": "削除済み", "PERSONAL": "個人", "ORGANIZATION": "組織", "ORG_UNIT": "組織単位", "RECHARGE": "チャージ", "USAGE": "利用料", "REFUND": "返金", "REVERSAL": "取消仕訳", "ADJUSTMENT": "調整", "EXPIRY": "期限切れ", "RECEIVABLE": "未収", "CHARGEBACK": "チャージバック", "REPAIR_CREDIT": "修復入金", "FORCED_REFUND": "強制返金", "USAGE_OVERAGE": "利用量超過", "DUPLICATE_PAYMENT": "重複決済", "LATE_PAYMENT_CONFLICT": "遅延入金の競合", "REFUND_FAILURE": "返金失敗", "PAYMENT_FACT_CONFLICT": "決済事実の競合", "ILLEGAL_TRANSITION": "不正な状態遷移", "REVIEW_REQUIRED_LATE_PAYMENT": "遅延入金の確認待ち", "PROVIDER_BLIND": "決済サービスの事実を取得不可", "ARCHIVED_CONFIRMED_UNPAID": "未払い確認後に保管済み", "RESOLVED_PAID": "入金確認済み"});
adminLayoutCopy.zh.financeLists.overdue = "是否逾期";
adminLayoutCopy.zh.financeLists.include_deleted = "包含已删除商品";
adminLayoutCopy.en.financeLists.overdue = "Overdue";
adminLayoutCopy.en.financeLists.include_deleted = "Include deleted products";
adminLayoutCopy.ja.financeLists.overdue = "期限超過";
adminLayoutCopy.ja.financeLists.include_deleted = "削除済み商品を含む";


const userStatusCopy = {
  zh: { userStatusTitle: action => `${action}用户`, confirmUserStatus: action => `确认${action}` },
  en: { userStatusTitle: action => `${action} user`, confirmUserStatus: action => `Confirm ${action.toLowerCase()}` },
  ja: { userStatusTitle: action => `ユーザーを${action}`, confirmUserStatus: action => `${action}を確認` },
};
for (const language of ['zh', 'en', 'ja']) Object.assign(adminLayoutCopy[language], userStatusCopy[language]);

adminLayoutCopy.zh.userSearch = { submit: '搜索', refresh: '刷新当前结果', refreshHelp: '刷新仅更新当前结果；修改输入后，请点击搜索或精确查询。' };
adminLayoutCopy.en.userSearch = { submit: 'Search', refresh: 'Refresh results', refreshHelp: 'Refresh updates the current results. Apply edited text with Search or Exact lookup.' };
adminLayoutCopy.ja.userSearch = { submit: '検索', refresh: '検索結果を更新', refreshHelp: '更新は現在の結果に適用されます。入力を変更した後は、検索または完全一致で検索を実行してください。' };

Object.assign(adminLayoutCopy.zh, { userMembershipStatus: '成员状态', viewUserPlatformGrants: '查看该用户的平台授权' });
Object.assign(adminLayoutCopy.en, { userMembershipStatus: 'Membership status', viewUserPlatformGrants: "View this user's platform grants" });
Object.assign(adminLayoutCopy.ja, { userMembershipStatus: 'メンバー状態', viewUserPlatformGrants: 'ユーザーのプラットフォーム権限を表示' });

Object.assign(adminLayoutCopy.zh, { userDetailsTitle: '用户详情', backToUserList: '返回用户列表' });
Object.assign(adminLayoutCopy.en, { userDetailsTitle: 'User details', backToUserList: 'Back to user list' });
Object.assign(adminLayoutCopy.ja, { userDetailsTitle: 'ユーザー詳細', backToUserList: 'ユーザー一覧に戻る' });

Object.assign(adminLayoutCopy.zh, { identifierActions: { ...{"pending":"正在复制…","copied":"已复制完整值","failed":"复制失败，请展开完整值后手动复制。","unavailable":"当前无法使用剪贴板，请展开完整值后手动复制。","showFull":"展开完整值","collapse":"收起完整值"}, copy: label => `复制${label}` } });

Object.assign(adminLayoutCopy.en, { identifierActions: { ...{"pending":"Copying…","copied":"Full value copied","failed":"Copy failed. Expand the full value and copy it manually.","unavailable":"Clipboard unavailable. Expand the full value and copy it manually.","showFull":"Show full value","collapse":"Collapse full value"}, copy: label => `Copy ${label}` } });

Object.assign(adminLayoutCopy.ja, { identifierActions: { ...{"pending":"コピー中…","copied":"完全な値をコピーしました","failed":"コピーできません。完全な値を表示して手動でコピーしてください。","unavailable":"クリップボードを利用できません。完全な値を表示して手動でコピーしてください。","showFull":"完全な値を表示","collapse":"完全な値を折りたたむ"}, copy: label => `${label}をコピー` } });

Object.assign(adminLayoutCopy.zh, { userIdentifierHelp: {"id":"系统内定位该用户的唯一标识，用于用户详情与角色关联。","code":"用于管理后台按完整用户编码精确查找该用户。"} });

Object.assign(adminLayoutCopy.en, { userIdentifierHelp: {"id":"Unique internal identifier for this user, used for user details and role assignments.","code":"Use the full user code to find this user with the admin exact-code lookup."} });

Object.assign(adminLayoutCopy.ja, { userIdentifierHelp: {"id":"ユーザー詳細とロールの関連付けに使う、このユーザー固有の内部識別子です。","code":"管理画面の完全一致検索でこのユーザーを探すためのユーザーコードです。"} });

Object.assign(adminLayoutCopy.zh.identifierActions, {"failedExpanded":"复制失败，请选中完整值后手动复制。","unavailableExpanded":"当前无法使用剪贴板，请选中完整值后手动复制。"});

Object.assign(adminLayoutCopy.en.identifierActions, {"failedExpanded":"Copy failed. Select the full value and copy it manually.","unavailableExpanded":"Clipboard unavailable. Select the full value and copy it manually."});

Object.assign(adminLayoutCopy.ja.identifierActions, {"failedExpanded":"コピーできません。完全な値を選択して手動でコピーしてください。","unavailableExpanded":"クリップボードを利用できません。完全な値を選択して手動でコピーしてください。"});

adminLayoutCopy.zh.userResults = { ...{"directory":"用户目录结果","exact":"精确编码查询结果","noCondition":"未设置目录搜索条件","capacity":"目录每次最多返回25条；接口不提供总数或页码。","continuation":"当前为游标后续批次，具体页码不可得。","firstBatch":"当前为首批结果。","hasNext":"本次返回了后续批次游标。","noNext":"本次未返回后续批次游标。","exactInfo":"精确编码查询不参与目录分页。","retained":"当前显示的是之前读取的结果，请以刷新或重试后的结果为准。","backToFirst":"返回首批","pagination":"用户目录批次导航"}, returned: count => `本页 ${count} 条`, condition: value => `已读取条件：${value}` };
adminLayoutCopy.en.userResults = { ...{"directory":"User directory results","exact":"Exact user-code result","noCondition":"No directory search condition","capacity":"The directory returns up to 25 records per request. Total count and page numbers are not available.","continuation":"This is a cursor continuation batch. Its page number is not available.","firstBatch":"This is the first result batch.","hasNext":"This response includes a continuation cursor.","noNext":"This response has no continuation cursor.","exactInfo":"Exact-code lookup is separate from directory pagination.","retained":"Previously read results are still shown. Refresh or retry to confirm the current data.","backToFirst":"Back to first batch","pagination":"User directory batch navigation"}, returned: count => `${count} records in this batch`, condition: value => `Read condition: ${value}` };
adminLayoutCopy.ja.userResults = { ...{"directory":"ユーザー一覧の検索結果","exact":"ユーザーコード完全一致の結果","noCondition":"一覧の検索条件なし","capacity":"一覧は1回に最大25件を返します。総件数とページ番号は提供されません。","continuation":"カーソルによる続きの結果です。ページ番号は取得できません。","firstBatch":"最初の結果です。","hasNext":"今回の応答には続きのカーソルがあります。","noNext":"今回の応答には続きのカーソルがありません。","exactInfo":"コード完全一致検索は一覧のページ送りとは別です。","retained":"以前取得した結果を表示しています。更新または再試行して現在のデータを確認してください。","backToFirst":"最初の結果へ","pagination":"ユーザー一覧の結果移動"}, returned: count => `今回の結果：${count}件`, condition: value => `取得済みの条件：${value}` };

adminLayoutCopy.zh.organization.overviewFacts = { activeMembers: '有效成员关系', activeUnits: '状态为有效的组织单元', unavailable: '暂无统计', membersUnavailable: '当前没有该组织的目录统计。', unitsUnavailable: '当前无法确认有效组织单元数量。', directorySnapshot: '来自最近一次成功读取的组织目录。按成员关系状态统计，不代表账号可用。', unitsSource: '来自本次读取的组织结构。按单元自身状态统计，不代表在停用组织中仍可使用。', technical: '技术信息', loading: "读取中", readFailed: "读取失败", directoryLoading: "正在读取组织目录。", directoryFailed: "未能读取组织目录。可以返回列表重试。", unitsLoading: "正在读取组织结构。", unitsFailed: "未能读取组织结构。", deletedSource: "组织已注销，不再提供此项统计。", readDenied: "权限不足", deniedSource: "当前权限不允许查看组织统计。", sessionUnavailable: "登录状态失效", sessionSource: "当前登录状态无法验证。请重新登录，或重新打开管理后台后再查看统计。", sessionChecking: "正在确认登录状态", sessionCheckingSource: "请等账号确认完成，再刷新查看统计。", sessionUnconfirmed: "登录状态尚未确认", sessionUnconfirmedSource: "请重新加载此页面，确认登录状态后查看统计。", readUnconfirmed: "权限待确认", unconfirmedSource: "暂不能确认读取权限。请刷新；如果目录未能读取，可返回目录重试。" };
adminLayoutCopy.en.organization.overviewFacts = { activeMembers: 'Active membership records', activeUnits: 'Organizational units marked active', unavailable: 'Unavailable', membersUnavailable: 'Directory statistics for this organization are unavailable.', unitsUnavailable: 'The number of active units could not be confirmed.', directorySnapshot: 'From the last successful directory read. Counts membership status, regardless of account availability.', unitsSource: 'From the current organization structure. Counts each unit’s own status, even in a disabled organization.', technical: 'Technical information', loading: "Loading", readFailed: "Could not load", directoryLoading: "Reading the organization directory.", directoryFailed: "The directory could not be read. Return to the list to retry.", unitsLoading: "Reading the organization structure.", unitsFailed: "The organization structure could not be read.", deletedSource: "This statistic is no longer available for deleted organizations.", readDenied: "Access denied", deniedSource: "Your current permissions do not allow access to organization statistics.", sessionUnavailable: "Session unavailable", sessionSource: "Sign in again or reopen the admin portal to view these statistics.", sessionChecking: "Checking sign-in", sessionCheckingSource: "Wait for the account check to finish, then refresh the statistics.", sessionUnconfirmed: "Sign-in unconfirmed", sessionUnconfirmedSource: "Reload this page to confirm your sign-in and view these statistics.", readUnconfirmed: "Access not confirmed", unconfirmedSource: "Refresh to confirm access. If the directory could not be read, return to the list to retry." };
adminLayoutCopy.ja.organization.overviewFacts = { activeMembers: '状態が有効なメンバー所属', activeUnits: '状態が有効な組織単位', unavailable: '取得できません', membersUnavailable: 'この組織の一覧統計は取得できません。', unitsUnavailable: '有効な組織単位の数を確認できません。', directorySnapshot: '前回取得できた組織一覧に基づきます。所属の状態を数え、アカウントの利用可否は表しません。', unitsSource: '今回取得した組織構造に基づきます。無効な組織でも、各単位自体の状態を数えます。', technical: '技術情報', loading: "読み込み中", readFailed: "読み込み失敗", directoryLoading: "組織一覧を読み込んでいます。", directoryFailed: "組織一覧を取得できませんでした。一覧に戻って再試行できます。", unitsLoading: "組織構造を読み込んでいます。", unitsFailed: "組織構造を取得できませんでした。", deletedSource: "削除済みの組織では、この統計は表示されません。", readDenied: "権限がありません", deniedSource: "現在の権限では組織の統計を表示できません。", sessionUnavailable: "ログイン状態を確認できません", sessionSource: "再ログインするか、管理ポータルを開き直して統計を確認してください。", sessionChecking: "ログイン状態を確認中", sessionCheckingSource: "アカウントの確認後に更新して統計を表示してください。", sessionUnconfirmed: "ログイン状態は未確認です", sessionUnconfirmedSource: "このページを再読み込みしてログイン状態を確認すると、統計を表示できます。", readUnconfirmed: "権限を確認できません", unconfirmedSource: "更新して権限を確認してください。組織一覧を取得できない場合は、一覧に戻って再試行できます。" };

adminLayoutCopy.zh.organization.unitMove = { currentPath: '当前位置', pathUnavailable: '无法确认当前路径', placeholder: '选择新的上级单元', changed: '组织结构已变化，请关闭并重新打开后选择。', moveChooseTarget: '请选择新的上级单元。', moveNoTargets: '没有可移动到的其他位置。', moveNoChange: '所选位置是当前上级。请选择其他位置。', moveInvalidTarget: '该目标当前不可用。', moveUnavailable: '当前无法确认可用的移动目标。' };
adminLayoutCopy.en.organization.unitMove = { currentPath: 'Current location', pathUnavailable: 'The current path could not be confirmed', placeholder: 'Choose a new parent unit', changed: 'The organization structure changed. Close and reopen to choose again.', moveChooseTarget: 'Choose a new parent unit.', moveNoTargets: 'There is no other available location.', moveNoChange: 'This is the current parent. Choose a different location.', moveInvalidTarget: 'This target is currently unavailable.', moveUnavailable: 'Available move targets could not be confirmed.' };
adminLayoutCopy.ja.organization.unitMove = { currentPath: '現在の場所', pathUnavailable: '現在のパスを確認できません', placeholder: '新しい上位単位を選択', changed: '組織構造が変更されました。閉じてから開き直し、選択してください。', moveChooseTarget: '新しい上位単位を選択してください。', moveNoTargets: '移動先として選べる別の場所がありません。', moveNoChange: '現在の上位単位です。別の場所を選択してください。', moveInvalidTarget: 'この移動先は現在使用できません。', moveUnavailable: '選択できる移動先を確認できません。' };

Object.assign(adminLayoutCopy.zh.organization.unitMove, { conflictOption: '已有同名单元', moveNameConflict: '所选层级已有同名单元，请选择其他位置。', recoveryTarget: '原请求的移动目标', possibleConflictOption: '可能重名，由系统在提交时核对', currentParentID: '当前上级单元 ID', sourceOrganization: '所属组织', sourceUnit: '待移动单元' });
Object.assign(adminLayoutCopy.en.organization.unitMove, { conflictOption: 'Same name already present', moveNameConflict: 'A unit with this name already exists at this level. Choose a different location.', recoveryTarget: 'Destination in the original request', possibleConflictOption: 'Possible name conflict; checked on submit', currentParentID: 'Current parent unit ID', sourceOrganization: 'Organization', sourceUnit: 'Unit to move' });
Object.assign(adminLayoutCopy.ja.organization.unitMove, { conflictOption: '同名の単位あり', moveNameConflict: '選んだ階層に同名の単位があります。別の場所を選択してください。', recoveryTarget: '元のリクエストの移動先', possibleConflictOption: '同名の可能性があります。送信時に確認します', currentParentID: '現在の親単位 ID', sourceOrganization: '所属組織', sourceUnit: '移動する単位' });
Object.assign(adminLayoutCopy.zh.organization.unitMove, {
  proposedPath: '拟移动到',
  proposedUnknown: '尚未确认移动后路径',
  impactTitle: '对下级单元的影响',
  knownImpact: count => count ? `按已加载的结构，${count} 个未删除的下级单元（含间接下级）的路径将一同变更。` : '已加载的结构中没有未删除的下级单元。',
  partialImpact: count => `已确认 ${count} 个未删除的下级单元的路径将变更；无法确认总数。`,
  unknownImpact: '暂无法确认下级单元的影响。',
  previewStale: '此结构的移动预览已失效。请关闭弹窗，刷新组织结构后重新核对。',
});
Object.assign(adminLayoutCopy.en.organization.unitMove, {
  proposedPath: 'Proposed location',
  proposedUnknown: 'The proposed path is not confirmed',
  impactTitle: 'Effect on descendant units',
  knownImpact: count => count === 1 ? 'In the loaded structure, the path of one non-deleted descendant unit will change.' : count > 1 ? `In the loaded structure, the paths of ${count} non-deleted descendant units will change.` : 'The loaded structure contains no non-deleted descendant units.',
  partialImpact: count => count === 1 ? 'The path of one non-deleted descendant unit is known to change. The total could not be confirmed.' : `The paths of ${count} non-deleted descendant units are known to change. The total could not be confirmed.`,
  unknownImpact: 'The effect on descendant units could not be confirmed.',
  previewStale: 'This move preview is out of date. Close this dialog, refresh the organization, and check the preview again.',
});
Object.assign(adminLayoutCopy.ja.organization.unitMove, {
  proposedPath: '移動先のパス',
  proposedUnknown: '移動先のパスは確認できていません',
  impactTitle: '下位単位への影響',
  knownImpact: count => count ? `取得済みの構造では、削除されていない下位単位（間接的な下位を含む）${count} 件のパスが変わります。` : '取得済みの構造には、削除されていない下位単位はありません。',
  partialImpact: count => `削除されていない下位単位 ${count} 件のパスが変わることを確認できました。総数は確認できません。`,
  unknownImpact: '下位単位への影響は確認できません。',
  previewStale: 'この移動プレビューは無効になりました。ダイアログを閉じ、組織情報を更新してから再度確認してください。',
});

adminLayoutCopy.zh.organization.lifecycleTasks = {
  statusTitle: '停用与启用', statusPurpose: '管理成员能否在该组织身份下工作。', currentStatus: '当前组织状态', unknownStatus: '暂无法确认状态',
  conditions: '执行条件', effect: '操作影响', noStatusPermission: '当前账号没有管理组织状态的权限。', statusCondition: '系统会在提交时核对当前状态和适用的检查结果。', statusUnavailable: '当前状态无法发起停用或启用。',
  disableEffect: '停用会暂停有效成员身份，并将当前组织会话切回个人身份；组织不会被删除。',
  enableEffect: '启用会恢复该组织已暂停的成员身份；现有个人会话不会自动切回组织。',
  enableCondition: "提交时核对组织状态和适用条件；已关闭的组织钱包会阻止启用。", usageEffect: "钱包与新用量", disableUsageEffect: "停用成功后，未关闭的组织钱包会被冻结，新的组织用量请求会被拒绝。已受理请求仍按原有规则处理。", enableUsageEffect: "启用成功后，已有组织钱包的生命周期状态恢复为有效；启用不会新建钱包。新的用量请求仍须满足原有权限、额度和准入条件。",
  transferPurpose: '将组织的所有者角色交给另一名成员。', noTransferPermission: '当前账号没有转让所有权的权限。',
  transferCondition: '组织须处于有效状态。接收人须为本组织的有效成员且账号有效，由系统在提交时核验。',
  transferDisabled: '该组织已停用，不能发起新的所有权转让。', transferUnavailable: '当前组织状态无法发起新的所有权转让。',
  transferEffect: '转让给另一名成员成功后，对方获得所有者角色，原所有者失去该角色。',
};
adminLayoutCopy.en.organization.lifecycleTasks = {
  statusTitle: 'Disable or enable the organization', statusPurpose: 'Control whether members can work in this organization context.', currentStatus: 'Current organization status', unknownStatus: 'Status could not be confirmed',
  conditions: 'Requirements', effect: 'Effect', noStatusPermission: 'Your account cannot manage the organization status.', statusCondition: 'When you submit, the system checks the current status and any other applicable requirements.', statusUnavailable: 'The organization cannot be disabled or enabled from its current status.',
  disableEffect: 'Disabling suspends active memberships and switches current organization sessions to personal context. It does not delete the organization.',
  enableEffect: 'Enabling restores suspended memberships in this organization. Current personal sessions do not switch back automatically.',
  enableCondition: "When you submit, the system checks the organization status and applicable requirements. A closed organization wallet blocks enabling.", usageEffect: "Wallet and new usage", disableUsageEffect: "Once disabled, organization wallets that are not closed are frozen, and new organization usage requests are rejected. Previously accepted requests follow the existing rules.", enableUsageEffect: "If the organization already has a wallet, enabling restores its lifecycle status to active after the checks pass. Enabling does not create a wallet. Existing permission, quota, and admission rules still apply to new usage.",
  transferPurpose: 'Pass the organization’s owner role to another member.', noTransferPermission: 'Your account cannot transfer ownership.',
  transferCondition: 'The organization must be active. The recipient must have an active account and an active membership in this organization. The system checks this when you submit.',
  transferDisabled: 'The organization is disabled. You cannot start a new ownership transfer.', transferUnavailable: 'A new ownership transfer is unavailable for the current organization status.',
  transferEffect: 'After a successful transfer to another member, they gain the owner role and the previous owner loses it.',
};
adminLayoutCopy.ja.organization.lifecycleTasks = {
  statusTitle: '組織の無効化と有効化', statusPurpose: 'メンバーがこの組織のコンテキストで作業できる状態を管理します。', currentStatus: '現在の組織の状態', unknownStatus: '状態を確認できません',
  conditions: '実行条件', effect: '操作の影響', noStatusPermission: '現在のアカウントには組織の状態を管理する権限がありません。', statusCondition: '送信時に、現在の状態と適用される条件がシステムによって確認されます。', statusUnavailable: '現在の状態では無効化も有効化もできません。',
  disableEffect: '無効化すると、有効なメンバー資格が停止され、現在の組織セッションは個人のコンテキストに切り替わります。組織は削除されません。',
  enableEffect: '有効化すると、この組織の停止中のメンバー資格が復元されます。現在の個人セッションは自動的に組織に切り替わりません。',
  enableCondition: "送信時に組織の状態と適用される条件を確認します。組織のウォレットが閉鎖済みの場合は有効化できません。", usageEffect: "ウォレットと新規利用", disableUsageEffect: "無効化が完了すると、閉鎖されていない組織ウォレットが凍結され、組織の新規利用リクエストは拒否されます。受理済みのリクエストには従来のルールが適用されます。", enableUsageEffect: "有効化が完了すると、既存の組織ウォレットの状態が有効に戻ります。有効化によってウォレットは新規作成されません。利用には従来の権限、割当、受け付け条件が適用されます。",
  transferPurpose: '組織の所有者ロールを別のメンバーに引き継ぎます。', noTransferPermission: '現在のアカウントには所有権を譲渡する権限がありません。',
  transferCondition: '組織が有効である必要があります。譲渡先は、この組織の有効なメンバーで、アカウントも有効である必要があります。送信時に確認されます。',
  transferDisabled: '組織が無効です。新しい所有権の譲渡を開始できません。', transferUnavailable: '組織の現在の状態では新しい所有権の譲渡を開始できません。',
  transferEffect: '別のメンバーへの譲渡が完了すると、そのメンバーが所有者ロールを取得し、以前の所有者はそのロールを失います。',
};

Object.assign(adminLayoutCopy.zh.organization.lifecycleTasks, {"contextTitle":"生命周期概况","contextOwner":"当前所有者","contextOwnerUnverified":"现有成员结果无法确认所有者","contextOwnerReadFailed":"成员读取失败，无法确认所有者","contextHistory":"此页不提供状态变更记录。"});
Object.assign(adminLayoutCopy.en.organization.lifecycleTasks, {"contextTitle":"Lifecycle overview","contextOwner":"Current owner","contextOwnerUnverified":"The available member results do not confirm the owner","contextOwnerReadFailed":"Could not load members to confirm the owner","contextHistory":"Status history is not provided in this view."});
Object.assign(adminLayoutCopy.ja.organization.lifecycleTasks, {"contextTitle":"ライフサイクルの概要","contextOwner":"現在の所有者","contextOwnerUnverified":"利用可能なメンバー情報から所有者を確認できません","contextOwnerReadFailed":"メンバーを読み込めなかったため所有者を確認できません","contextHistory":"この画面では状態変更履歴は表示されません。"});
