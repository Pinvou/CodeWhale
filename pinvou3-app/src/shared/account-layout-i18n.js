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

Object.assign(adminLayoutCopy.zh.organization.lifecycleTasks, {"disablePaymentLabel":"组织充值订单","disablePaymentEffect":"停用成功后，该组织不能创建新的充值订单。已创建的订单请按原流程核对。"});
Object.assign(adminLayoutCopy.en.organization.lifecycleTasks, {"disablePaymentLabel":"Organization recharge orders","disablePaymentEffect":"After disabling succeeds, the organization cannot create new recharge orders. Check existing orders through the usual process."});
Object.assign(adminLayoutCopy.ja.organization.lifecycleTasks, {"disablePaymentLabel":"組織のチャージ注文","disablePaymentEffect":"無効化が完了すると、この組織では新しいチャージ注文を作成できません。作成済みの注文は従来の手順で確認してください。"});
Object.assign(adminLayoutCopy.zh.organization.lifecycleTasks, { contextTitle: "生命周期概况", contextOwner: "当前所有者", contextOwnerUnverified: "现有成员结果无法确认所有者", contextOwnerReadFailed: "成员读取失败，无法确认所有者", contextHistory: "此页不提供状态变更记录。", disablePrecheckInfo: "此处不提供停用前的检查结果。提交时，系统会核对组织状态是否仍允许停用。" });
Object.assign(adminLayoutCopy.en.organization.lifecycleTasks, { contextTitle: "Lifecycle overview", contextOwner: "Current owner", contextOwnerUnverified: "The available member results do not confirm the owner", contextOwnerReadFailed: "Could not load members to confirm the owner", contextHistory: "Status history is not provided in this view.", disablePrecheckInfo: "This view does not provide a precheck result. The organization’s current state is checked when you submit." });
Object.assign(adminLayoutCopy.ja.organization.lifecycleTasks, { contextTitle: "ライフサイクルの概要", contextOwner: "現在の所有者", contextOwnerUnverified: "利用可能なメンバー情報から所有者を確認できません", contextOwnerReadFailed: "メンバーを読み込めなかったため所有者を確認できません", contextHistory: "この画面では状態変更履歴は表示されません。", disablePrecheckInfo: "この画面には事前確認の結果は表示されません。組織を無効化できる状態かどうかは、送信時に確認されます。" });
Object.assign(adminLayoutCopy.zh.organization.lifecycleTasks, { ownerPicker: {"searchScope":"搜索全平台账号：支持账号名、显示名称或完整用户 ID。成员资格在提交时核验。","searchLabel":"搜索新所有者账号","memberRoleLabel":"组织角色","memberInactiveFact":"已读取的组织成员状态不是有效；账号状态未确认。","memberActiveFact":"已读取的组织成员状态为有效；账号状态未确认。","pendingTarget":"本次转让请求的目标","searchPlaceholder":"输入账号名、显示名称或完整用户 ID","directHelp":"当前权限无法搜索账号。输入已知的完整用户 ID；成员资格在提交时核验。","selectLabel":"选择新所有者","choose":"请选择新所有者","denied":"没有读取账号候选的权限。","pageScope":"当前查询批次","notSearched":"尚未搜索账号。","noResults":"本次查询未找到匹配账号。","noMore":"此批没有可显示的账号。","retryPage":"重试此批","firstPage":"返回首批","unknownRole":"组织角色未确认","activeFact":"已读取的账号与成员状态均为有效，提交时会核验。","inactiveFact":"现有记录显示该账号或成员不是有效状态，提交时会核验。","unconfirmedFact":"当前资料不足以确认此人的组织成员资格，提交时会核验。"} });
Object.assign(adminLayoutCopy.en.organization.lifecycleTasks, { ownerPicker: {"searchScope":"Search platform accounts by username, display name, or full user ID. Membership is verified on submission.","searchLabel":"Search accounts for the new owner","memberRoleLabel":"Organization role","memberInactiveFact":"The available organization membership record is not active. Account status is unconfirmed.","memberActiveFact":"The available organization membership record is active. Account status is unconfirmed.","pendingTarget":"Target of this transfer request","searchPlaceholder":"Username, display name, or full user ID","directHelp":"Your permissions do not allow account search. Enter a known full user ID. Membership is verified on submission.","selectLabel":"Select the new owner","choose":"Select a new owner","denied":"You do not have permission to read account candidates.","pageScope":"Current search batch","notSearched":"No account search yet.","noResults":"No accounts matched this search.","noMore":"No accounts in this batch.","retryPage":"Retry this batch","firstPage":"Return to first batch","unknownRole":"Organization role unconfirmed","activeFact":"The available account and membership records are both active. They are verified on submission.","inactiveFact":"An available account or membership record is not active. It is checked on submission.","unconfirmedFact":"The available records do not confirm organization membership. It is checked on submission."} });
Object.assign(adminLayoutCopy.ja.organization.lifecycleTasks, { ownerPicker: {"searchScope":"ユーザー名、表示名、または完全なユーザー ID でプラットフォーム全体を検索します。組織のメンバー資格は送信時に確認されます。","searchLabel":"新しい所有者のアカウントを検索","memberRoleLabel":"組織のロール","memberInactiveFact":"取得済みの組織メンバーは有効ではありません。アカウントの状態は未確認です。","memberActiveFact":"取得済みの組織メンバーは有効です。アカウントの状態は未確認です。","pendingTarget":"この譲渡リクエストの対象","searchPlaceholder":"ユーザー名、表示名、または完全なユーザー ID","directHelp":"アカウントの検索権限がありません。既知の完全なユーザー ID を入力してください。組織のメンバー資格は送信時に確認されます。","selectLabel":"新しい所有者を選択","choose":"新しい所有者を選択","denied":"候補アカウントを閲覧する権限がありません。","pageScope":"現在の検索結果の範囲","notSearched":"アカウントはまだ検索されていません。","noResults":"この検索に一致するアカウントはありません。","noMore":"この範囲に表示できるアカウントはありません。","retryPage":"この結果を再取得","firstPage":"最初の結果に戻る","unknownRole":"組織のロールは未確認","activeFact":"取得済みのアカウントとメンバー資格は有効です。送信時に確認されます。","inactiveFact":"取得済みのアカウントまたはメンバー資格は有効ではありません。送信時に確認されます。","unconfirmedFact":"現在のデータでは組織のメンバー資格を確認できません。送信時に確認されます。"} });
Object.assign(adminLayoutCopy.zh.organization.lifecycleTasks, {"transferTargetLabel":"转让目标","transferTargetPending":"尚未选择可核对的目标","transferRoleChanges":"若转让给不同的成员，所选成员将成为所有者，原所有者将失去所有者角色。双方其他角色不由此次转让修改。","transferRecovery":"此处没有撤销操作。再次转让仍需满足当时的权限和成员资格。"});
Object.assign(adminLayoutCopy.en.organization.lifecycleTasks, {"transferTargetLabel":"Transfer target","transferTargetPending":"No target identified yet","transferRoleChanges":"If ownership is transferred to a different member, the selected member gains the Owner role and the previous owner loses it. This transfer does not modify their other roles.","transferRecovery":"There is no undo action here. Another transfer must meet the permissions and membership requirements at that time."});
Object.assign(adminLayoutCopy.ja.organization.lifecycleTasks, {"transferTargetLabel":"譲渡先","transferTargetPending":"確認できる譲渡先がまだ選択されていません","transferRoleChanges":"別のメンバーに譲渡すると、選択したメンバーに所有者ロールが付与され、従来の所有者から同ロールが削除されます。双方の他のロールはこの譲渡では変更されません。","transferRecovery":"この画面には取り消し操作がありません。再度譲渡するには、その時点の権限とメンバー資格が必要です。"});
Object.assign(adminLayoutCopy.zh.organization.lifecycleTasks, {"transferSelectToContinue":"请先选择新所有者。组织成员资格将在提交时核验。","transferIDToContinue":"请输入新所有者的完整用户 ID。组织成员资格将在提交时核验。","transferReview":"核对转让"});
Object.assign(adminLayoutCopy.en.organization.lifecycleTasks, {"transferSelectToContinue":"Select the new owner. Membership is verified on submission.","transferIDToContinue":"Enter the new owner’s full user ID. Membership is verified on submission.","transferReview":"Review transfer"});
Object.assign(adminLayoutCopy.ja.organization.lifecycleTasks, {"transferSelectToContinue":"新しい所有者を選択してください。メンバー資格は送信時に確認されます。","transferIDToContinue":"新しい所有者の完全なユーザー ID を入力してください。メンバー資格は送信時に確認されます。","transferReview":"譲渡内容を確認"});

Object.assign(adminLayoutCopy.zh.organization.lifecycleTasks, {"transferWaiting":"正在提交转让请求，请稍候。"});
Object.assign(adminLayoutCopy.en.organization.lifecycleTasks, {"transferWaiting":"Submitting the transfer request. Please wait."});
Object.assign(adminLayoutCopy.ja.organization.lifecycleTasks, {"transferWaiting":"譲渡リクエストを送信しています。しばらくお待ちください。"});

Object.assign(adminLayoutCopy.zh.financeOverview, { metricScope: '查看指标口径' });
Object.assign(adminLayoutCopy.en.financeOverview, { metricScope: 'View metric scope' });
Object.assign(adminLayoutCopy.ja.financeOverview, { metricScope: '集計対象を確認' });

adminLayoutCopy.zh.walletDirectory = {
  types: { PERSONAL: '个人钱包', ORGANIZATION: '组织钱包', ORG_UNIT: '组织单元钱包' },
  typeUnavailable: '钱包类型未提供', accountStatus: '账户状态', riskStatus: '风险状态', unavailable: '未提供',
  account: { ACTIVE: '有效', FROZEN: '已冻结', CLOSED: '已关闭' },
  risk: { CLEAR: '无未结应收', RECEIVABLE_OPEN: '有未结应收' },
  select: (type, id) => `选择${type} · ${id}`,
};
adminLayoutCopy.en.walletDirectory = {
  types: { PERSONAL: 'Personal wallet', ORGANIZATION: 'Organization wallet', ORG_UNIT: 'Organization unit wallet' },
  typeUnavailable: 'Wallet type unavailable', accountStatus: 'Account status', riskStatus: 'Risk status', unavailable: 'Not provided',
  account: { ACTIVE: 'Active', FROZEN: 'Frozen', CLOSED: 'Closed' },
  risk: { CLEAR: 'No open receivable', RECEIVABLE_OPEN: 'Open receivable' },
  select: (type, id) => `Select ${type} · ${id}`,
};
adminLayoutCopy.ja.walletDirectory = {
  types: { PERSONAL: '個人ウォレット', ORGANIZATION: '組織ウォレット', ORG_UNIT: '組織単位のウォレット' },
  typeUnavailable: 'ウォレット種別は未提供', accountStatus: '口座の状態', riskStatus: 'リスク状態', unavailable: '未提供',
  account: { ACTIVE: '有効', FROZEN: '凍結中', CLOSED: '閉鎖済み' },
  risk: { CLEAR: '未決済の債権なし', RECEIVABLE_OPEN: '未決済の債権あり' },
  select: (type, id) => `${type}を選択 · ${id}`,
};

adminLayoutCopy.zh.walletAdjustmentForm = {
  title: '申请钱包调账', approval: '将创建调账记录并提交审批。余额是否变更以审批执行和账本为准。',
  walletHelp: '从上方钱包列表选择，或输入完整钱包 ID。', snapshot: '所选钱包在当前列表中的最近读数',
  noSnapshot: '当前列表没有此钱包的余额读数。', posted: '账面积分', available: '可用积分', unavailable: '未提供',
  amount: '调整金额（微积分）', amountHelp: '输入非零整数。正数增加，负数扣减；10,000 微积分等于 1 积分。',
  invalidAmount: '请输入范围内的非零整数金额。', increase: '增加', decrease: '扣减',
  change: (direction, points) => `本次申请：${direction} ${points} 积分`, points: value => `${value} 积分`,
  submissionConfirmed: '已确认调账审批请求。请在审批中心查看处理状态。', recoveryWallet: '待恢复请求的钱包：',
  reasonHelp: '请填写至少 10 个字符。', send: '提交调账审批',
};
adminLayoutCopy.en.walletAdjustmentForm = {
  title: 'Request a wallet adjustment', approval: 'This creates an adjustment record and submits it for approval. Check approval execution and the ledger to confirm a balance change.',
  walletHelp: 'Select a wallet above or enter its full wallet ID.', snapshot: 'Most recent amounts for this wallet in the current list',
  noSnapshot: 'The current list has no balance amounts for this wallet.', posted: 'Posted points', available: 'Available points', unavailable: 'Not provided',
  amount: 'Adjustment amount (microcredits)', amountHelp: 'Enter a nonzero whole number. Positive adds and negative deducts; 10,000 microcredits equal 1 point.',
  invalidAmount: 'Enter a nonzero whole amount within the supported range.', increase: 'Add', decrease: 'Deduct',
  change: (direction, points) => `Requested change: ${direction} ${points} points`, points: value => `${value} points`,
  submissionConfirmed: 'The adjustment approval request was confirmed. Check its status in the approval center.', recoveryWallet: 'Wallet in the original request:',
  reasonHelp: 'Enter at least 10 characters.', send: 'Submit adjustment for approval',
};
adminLayoutCopy.ja.walletAdjustmentForm = {
  title: 'ウォレット調整を申請', approval: '調整記録を作成し、承認を申請します。残高の変更は承認の実行結果と台帳で確認してください。',
  walletHelp: '上の一覧で選択するか、ウォレット ID をすべて入力してください。', snapshot: 'このウォレットの現在の一覧での最終取得値',
  noSnapshot: '現在の一覧にこのウォレットの残高情報はありません。', posted: '帳簿ポイント', available: '利用可能ポイント', unavailable: '未提供',
  amount: '調整額（マイクロクレジット）', amountHelp: '0 以外の整数を入力してください。正の数は加算、負の数は減算です。10,000 マイクロクレジットは 1 ポイントです。',
  invalidAmount: '対応範囲内で 0 以外の整数を入力してください。', increase: '加算', decrease: '減算',
  change: (direction, points) => `申請する変更：${points} ポイントを${direction}`, points: value => `${value} ポイント`,
  submissionConfirmed: '調整の承認申請が確認されました。処理状況は承認センターで確認してください。', recoveryWallet: '元の申請のウォレット：',
  reasonHelp: '10 文字以上で入力してください。', send: '調整の承認を申請',
};

adminLayoutCopy.zh.billingTasks = { title: '账户与计费任务', accounts: '账户与记录', ledger: '账务处理', exceptions: '异常与修复', other: '其他任务' };
adminLayoutCopy.en.billingTasks = { title: 'Billing tasks', accounts: 'Accounts and records', ledger: 'Ledger operations', exceptions: 'Exceptions and repairs', other: 'Other tasks' };
adminLayoutCopy.ja.billingTasks = { title: 'アカウントと請求のタスク', accounts: '口座と記録', ledger: '台帳処理', exceptions: '例外と修復', other: 'その他のタスク' };

adminLayoutCopy.zh.billingTasks.showOverview = '账户与计费概览';
adminLayoutCopy.en.billingTasks.showOverview = 'Accounts and billing overview';
adminLayoutCopy.ja.billingTasks.showOverview = 'アカウントと請求の概要';
adminLayoutCopy.zh.organization.unitDeleteStage = {"ready":"请核对删除对象。下一步会再次确认，此时尚未发送删除请求。","processing":"正在处理已确认的删除请求…","uncertain":"删除结果尚未确认。重试会继续同一条请求。","rejected":"此次删除请求未成功。请查看提示，核对后再确认。"};
adminLayoutCopy.en.organization.unitDeleteStage = {"ready":"Review the unit to delete. The next step asks for final confirmation before sending a request.","processing":"Processing the confirmed deletion request…","uncertain":"The deletion result is unconfirmed. Retry continues the same request.","rejected":"This deletion request did not succeed. Review the message before confirming again."};
adminLayoutCopy.ja.organization.unitDeleteStage = {"ready":"削除対象を確認してください。次の手順で最終確認するまで、削除リクエストは送信されません。","processing":"確定した削除リクエストを処理中です…","uncertain":"削除結果を確認できていません。再試行では同じリクエストを続行します。","rejected":"今回の削除リクエストは成功しませんでした。メッセージを確認してから再度確定してください。"};
adminLayoutCopy.zh.organization.unitDeleteImpact = {"organization":"所属组织","unit":"待删除单元","children":"当前记录中的直属子单元：","unavailable":"暂无法确认","scope":"子单元数来自当前已加载的组织结构，不代表删除资格；成员、角色和其他关联由实际请求检查。","blocked":"系统已确认有资源阻止删除：","child":"仍有直属子单元。请先在组织结构中处理这些单元。","member":"仍有关联成员。请先核对并处理单元成员归属。","role":"仍有角色授权绑定。请先核对并处理该单元的角色授权。","approval":"仍有未结束的审批。请先在审批中心查看并处理。","account":"账户域报告其他关联阻断，请联系相应管理员核对。","billing":"计费域报告关联阻断，请联系计费管理员核对。","token":"令牌域报告关联阻断，请联系令牌管理员核对。","confirmRequest":"本次确认会提交删除请求，系统将核查关联资源。","walletBalance":"单元钱包仍有非零入账余额。请计费管理员核对该钱包余额。","walletReserved":"单元钱包仍有预留余额。请计费管理员核对余额占用。","receivable":"单元仍有未结应收。请计费管理员核对应收记录。","entitlement":"单元仍有关联权益余额。请计费管理员核对对应权益包。","usageHold":"单元仍有待结算的用量预留。请计费管理员核对预留记录。","payment":"单元仍有未完成的支付订单。请计费管理员核对付款及入账状态。","refund":"单元仍有未完成的退款。请计费管理员核对退款状态。","chargeback":"单元仍有未解决的拒付记录。请计费管理员核对拒付处理状态。","forcedRefund":"单元仍有待处理的强制退款记录。请计费管理员核对该记录。","reconciliation":"单元仍有未处理完的对账差异。请计费管理员核对该差异。","usageReview":"单元仍有待复核的用量记录。请计费管理员核对用量复核项。","adjustment":"单元仍有未完成的财务调账。请计费管理员核对调账状态。","tokenAccount":"单元仍关联 Token 账户。请令牌管理员核对账户归属。","tokenUsage":"单元仍有未结算的 Token 用量。请令牌管理员核对待结算请求。"};
adminLayoutCopy.en.organization.unitDeleteImpact = {"organization":"Organization","unit":"Unit to delete","children":"Direct child units in the current records:","unavailable":"Unable to confirm","scope":"This count comes from the loaded organization structure and does not establish deletion eligibility. The deletion request checks memberships, role bindings, and other dependencies.","blocked":"The system confirmed dependencies that prevent deletion:","child":"Direct child units remain. Review and resolve them in the organization structure first.","member":"Member assignments remain. Review the members assigned to this unit.","role":"Role bindings remain. Review and resolve the unit’s role assignments.","approval":"An approval request is still open. Review it in the Approval Center.","account":"The account domain reported another dependency. Ask the responsible administrator to review it.","billing":"The billing domain reported a dependency. Ask the billing administrator to review it.","token":"The token domain reported a dependency. Ask the token administrator to review it.","confirmRequest":"Confirming now submits the deletion request. The system will check for linked resources.","walletBalance":"The unit wallet has a non-zero posted balance. Ask the billing administrator to review that wallet balance.","walletReserved":"The unit wallet still has a reserved balance. Ask the billing administrator to review the reservations.","receivable":"The unit has an outstanding receivable. Ask the billing administrator to review open receivables.","entitlement":"Entitlement amounts remain for this unit. Ask the billing administrator to review the affected packages.","usageHold":"The unit has unsettled usage holds. Ask the billing administrator to review those holds.","payment":"The unit has an unfinished payment order. Ask the billing administrator to review payment and fulfillment.","refund":"The unit has an unfinished refund. Ask the billing administrator to review the refund status.","chargeback":"The unit has an unresolved chargeback. Ask the billing administrator to review the chargeback case.","forcedRefund":"The unit has an unresolved forced refund. Ask the billing administrator to review that case.","reconciliation":"The unit has an open reconciliation difference. Ask the billing administrator to review the discrepancy.","usageReview":"The unit has usage awaiting review. Ask the billing administrator to review the usage record.","adjustment":"The unit has a pending financial adjustment. Ask the billing administrator to review its status.","tokenAccount":"A Token account is still linked to this unit. Ask the Token administrator to review the account association.","tokenUsage":"Token usage has not settled. Ask the Token administrator to review the unsettled usage requests."};
adminLayoutCopy.ja.organization.unitDeleteImpact = {"organization":"所属組織","unit":"削除対象の単位","children":"現在の記録にある直属の子単位：","unavailable":"確認できません","scope":"件数は読み込み済みの組織構造に基づくもので、削除可否を示しません。メンバー、ロール割り当て、その他の関連は削除リクエストで確認されます。","blocked":"削除を妨げる関連リソースが確認されました：","child":"直属の子単位が残っています。先に組織構造で確認して対応してください。","member":"メンバーの割り当てが残っています。この単位のメンバーを確認してください。","role":"ロールの割り当てが残っています。この単位のロールを確認して対応してください。","approval":"処理中の承認申請があります。承認センターで確認してください。","account":"アカウント領域に別の関連リソースがあります。担当の管理者に確認してください。","billing":"請求領域に関連リソースがあります。請求管理者に確認してください。","token":"トークン領域に関連リソースがあります。トークン管理者に確認してください。","confirmRequest":"この確認で削除リクエストを送信します。システムが関連リソースを確認します。","walletBalance":"単位のウォレット残高がゼロではありません。請求管理者に残高の確認を依頼してください。","walletReserved":"単位のウォレットに予約残高があります。請求管理者に予約分の確認を依頼してください。","receivable":"単位に未決済の未収金があります。請求管理者に未収金の確認を依頼してください。","entitlement":"単位の利用権残高が残っています。請求管理者に対象パッケージの確認を依頼してください。","usageHold":"単位に未精算の使用量保留があります。請求管理者に保留の確認を依頼してください。","payment":"単位に未完了の支払い注文があります。請求管理者に支払いと反映状況の確認を依頼してください。","refund":"単位に未完了の返金があります。請求管理者に返金状況の確認を依頼してください。","chargeback":"単位に未解決のチャージバックがあります。請求管理者に対象案件の確認を依頼してください。","forcedRefund":"単位に未解決の強制返金があります。請求管理者に対象案件の確認を依頼してください。","reconciliation":"単位に未解決の照合差異があります。請求管理者に差異の確認を依頼してください。","usageReview":"単位に確認待ちの使用量があります。請求管理者に使用量記録の確認を依頼してください。","adjustment":"単位に未完了の財務調整があります。請求管理者に調整状況の確認を依頼してください。","tokenAccount":"単位に Token アカウントが関連付けられています。Token 管理者に関連付けの確認を依頼してください。","tokenUsage":"単位に未精算の Token 使用量があります。Token 管理者に未精算リクエストの確認を依頼してください。"};
adminLayoutCopy.zh.organization.unitDeleteTrigger = "{action}：{name}，单元 ID {id}";
adminLayoutCopy.en.organization.unitDeleteTrigger = "{action}: {name}, unit ID {id}";
adminLayoutCopy.ja.organization.unitDeleteTrigger = "{action}：{name}、単位 ID {id}";
adminLayoutCopy.zh.organization.unitDeleteUnnamed = "无可显示名称";
adminLayoutCopy.en.organization.unitDeleteUnnamed = "No display name";
adminLayoutCopy.ja.organization.unitDeleteUnnamed = "表示名なし";
adminLayoutCopy.zh.organization.unitEntryPath = "所在分支";
adminLayoutCopy.zh.organization.unitEntryPathUnavailable = "暂无法确认";
adminLayoutCopy.zh.organization.unitEntryID = "单元 ID";
adminLayoutCopy.en.organization.unitEntryPath = "Branch";
adminLayoutCopy.en.organization.unitEntryPathUnavailable = "Unable to confirm";
adminLayoutCopy.en.organization.unitEntryID = "Unit ID";
adminLayoutCopy.ja.organization.unitEntryPath = "所属階層";
adminLayoutCopy.ja.organization.unitEntryPathUnavailable = "確認できません";
adminLayoutCopy.ja.organization.unitEntryID = "単位 ID";
adminLayoutCopy.zh.financeLists.receivableFilter = {"help":"填写钱包详情中的完整钱包 ID；留空可查询当前权限范围。","all":"全部来源","unsupported":"当前来源值不受支持","types":{"USAGE_OVERAGE":"用量超额","CHARGEBACK":"渠道拒付","FORCED_REFUND":"强制退款"}};
adminLayoutCopy.en.financeLists.receivableFilter = {"help":"Use the full wallet ID from its details. Leave blank to search your available scope.","all":"All sources","unsupported":"Current source value is unsupported","types":{"USAGE_OVERAGE":"Usage overage","CHARGEBACK":"Chargeback","FORCED_REFUND":"Forced refund"}};
adminLayoutCopy.ja.financeLists.receivableFilter = {"help":"ウォレット詳細の完全な ID を入力してください。空欄の場合は権限範囲内を検索します。","all":"すべての発生元","unsupported":"現在の発生元の値は未対応です","types":{"USAGE_OVERAGE":"利用量超過","CHARGEBACK":"チャージバック","FORCED_REFUND":"強制返金"}};
adminLayoutCopy.zh.financeLists.receivableTime = {"legend":"创建时间范围","from":"起点（含）","to":"终点（不含）","zone":"显示和填写使用本机时区：{zone}。快捷项只填入时间，点击查询后生效。","local":"设备当前时区","today":"填入今天","yesterday":"填入昨天"};
adminLayoutCopy.en.financeLists.receivableTime = {"legend":"Creation time range","from":"From (inclusive)","to":"Until (exclusive)","zone":"Times are shown and entered in your device time zone: {zone}. Presets fill the fields; select Search to apply.","local":"Current device time zone","today":"Fill today","yesterday":"Fill yesterday"};
adminLayoutCopy.ja.financeLists.receivableTime = {"legend":"作成日時の範囲","from":"開始（含む）","to":"終了（含まない）","zone":"日時の表示と入力には端末のタイムゾーン（{zone}）を使用します。候補は入力欄に反映され、検索を押すと適用されます。","local":"端末の現在のタイムゾーン","today":"今日を入力","yesterday":"昨日を入力"};
adminLayoutCopy.zh.financeLists.receivableResults = {"applied":"已生效筛选","clear":"清除筛选","first":"返回首批","noRecords":"当前权限范围内暂无应收记录。","noMatches":"未找到符合已生效筛选的应收记录。","emptyPage":"当前批次没有应收记录。"};
adminLayoutCopy.en.financeLists.receivableResults = {"applied":"Applied filters","clear":"Clear filters","first":"Return to first batch","noRecords":"There are no receivables in your available scope.","noMatches":"No receivables match the applied filters.","emptyPage":"There are no receivables in this batch."};
adminLayoutCopy.ja.financeLists.receivableResults = {"applied":"適用中の条件","clear":"条件をクリア","first":"最初の一覧に戻る","noRecords":"権限範囲内に未収金の記録がありません。","noMatches":"適用中の条件に一致する未収金の記録がありません。","emptyPage":"この表示範囲に未収金の記録がありません。"};

adminLayoutCopy.zh.adjustmentList = {"invalidStatus":"状态无效","view":"当前视图","applied":"已应用条件","all":"查看全部调账记录","pendingEmpty":"暂无待审批调账记录。","noMatches":"当前筛选条件下没有匹配的调账记录。","noRecords":"暂无调账记录。","emptyPage":"此页没有调账记录，可返回第一页。","first":"返回第一页"};
adminLayoutCopy.en.adjustmentList = {"invalidStatus":"Invalid status","view":"Current view","applied":"Applied filters","all":"View all adjustments","pendingEmpty":"No adjustments are awaiting approval.","noMatches":"No adjustments match the applied filters.","noRecords":"No adjustments have been recorded.","emptyPage":"There are no adjustments on this page. You can return to the first page.","first":"Go to first page"};
adminLayoutCopy.ja.adjustmentList = {"invalidStatus":"無効なステータス","view":"現在の表示","applied":"適用中の条件","all":"すべての調整履歴を表示","pendingEmpty":"承認待ちの調整はありません。","noMatches":"条件に一致する調整履歴はありません。","noRecords":"調整履歴はありません。","emptyPage":"このページには調整履歴がありません。最初のページに戻れます。","first":"最初のページへ"};

adminLayoutCopy.zh.adjustmentFacts = {"amount":"申请调账（积分）","points":"积分","requestTime":"申请时间","effectiveTime":"入账时间","notEffective":"未入账","unavailable":"无法确认","inputZone":"筛选时间使用设备时区"};
adminLayoutCopy.en.adjustmentFacts = {"amount":"Requested adjustment (points)","points":"points","requestTime":"Requested at","effectiveTime":"Posted at","notEffective":"Not posted","unavailable":"Unavailable","inputZone":"Filter time uses the device time zone"};
adminLayoutCopy.ja.adjustmentFacts = {"amount":"申請調整（ポイント）","points":"ポイント","requestTime":"申請日時","effectiveTime":"計上日時","notEffective":"未計上","unavailable":"確認できません","inputZone":"絞り込みは端末のタイムゾーンを使用"};

adminLayoutCopy.zh.adjustmentLookup = {"wallet":"钱包","ownerTypes":{"PERSONAL":"个人钱包","ORGANIZATION":"组织钱包","ORG_UNIT":"组织单元钱包"},"exactID":"精确 ID","findWallet":"查找钱包","findRequester":"查找申请人","walletHint":"钱包 ID 或所有者 ID（可输入部分）","requesterHint":"用户名、显示名称或完整用户 ID","directoryUnavailable":"当前权限不支持人员目录，可使用已知用户 ID。","search":"查找","loading":"正在查找…","forbidden":"没有权限读取此目录。","error":"查找失败，请重试。","noMatches":"未找到匹配对象。","more":"加载更多结果","openWallet":"查看钱包流水","openRequester":"查看申请人资料","relatedHelp":"按权限显示关联入口。没有入口的标识仍可用于精确筛选。"};
adminLayoutCopy.en.adjustmentLookup = {"wallet":"Wallet","ownerTypes":{"PERSONAL":"Personal wallet","ORGANIZATION":"Organization wallet","ORG_UNIT":"Organization unit wallet"},"exactID":"Exact ID","findWallet":"Find a wallet","findRequester":"Find a requester","walletHint":"Wallet or owner ID (partial IDs accepted)","requesterHint":"Username, display name, or full user ID","directoryUnavailable":"Your permissions do not include the user directory. You can use a known user ID.","search":"Find","loading":"Searching…","forbidden":"You do not have permission to read this directory.","error":"The lookup failed. Try again.","noMatches":"No matching results.","more":"Load more results","openWallet":"View wallet ledger","openRequester":"View requester profile","relatedHelp":"Related links depend on your permissions. You can still use an ID for an exact filter."};
adminLayoutCopy.ja.adjustmentLookup = {"wallet":"ウォレット","ownerTypes":{"PERSONAL":"個人ウォレット","ORGANIZATION":"組織ウォレット","ORG_UNIT":"組織単位のウォレット"},"exactID":"完全な ID","findWallet":"ウォレットを探す","findRequester":"申請者を探す","walletHint":"ウォレットまたは所有者の ID（一部でも可）","requesterHint":"ユーザー名、表示名、または完全なユーザー ID","directoryUnavailable":"ユーザー一覧を読む権限がありません。既知のユーザー ID は使用できます。","search":"検索","loading":"検索中…","forbidden":"この一覧を読む権限がありません。","error":"検索に失敗しました。もう一度お試しください。","noMatches":"一致する項目がありません。","more":"さらに読み込む","openWallet":"ウォレット台帳を表示","openRequester":"申請者の情報を表示","relatedHelp":"関連情報へのリンクは権限に応じて表示します。ID は完全一致の絞り込みに使用できます。"};

adminLayoutCopy.zh.orderStatusSummary = {"order":"订单状态","payment":"收款记录","fulfillment":"履约状态","attention":"关注状态","unknown":"未知状态","paymentRecorded":"此单有已确认的收款记录","noRecordedPayment":"当前记录没有确认收款","paymentUnconfirmed":"当前信息无法确认收款","paymentUnderReview":"有已核验的收款证据，待人工复核","expiredPending":"订单已过期，权益仍待发放。这两项状态不能确认渠道最终付款结果。","fulfillmentStates":{"PENDING":"待发放权益","CREDITED":"权益已发放","MANUAL_REVIEW":"待人工复核"},"attentionStates":{"NORMAL":"当前无关注提示","PROCESSING":"处理中","REVIEW_REQUIRED":"需要复核","RECEIVABLE_OPEN":"有待结清应收"}};
adminLayoutCopy.en.orderStatusSummary = {"order":"Order status","payment":"Payment evidence","fulfillment":"Fulfillment status","attention":"Attention status","unknown":"Unknown state","paymentRecorded":"This order has a confirmed payment record","noRecordedPayment":"No confirmed payment in this record","paymentUnconfirmed":"Payment cannot be confirmed from the available information","paymentUnderReview":"Verified payment evidence awaits manual review","expiredPending":"This order has expired and benefits are still pending. Neither status establishes the final provider payment result.","fulfillmentStates":{"PENDING":"Benefits pending","CREDITED":"Benefits delivered","MANUAL_REVIEW":"Manual review required"},"attentionStates":{"NORMAL":"No current attention flag","PROCESSING":"Processing","REVIEW_REQUIRED":"Review required","RECEIVABLE_OPEN":"Outstanding receivable"}};
adminLayoutCopy.ja.orderStatusSummary = {"order":"注文状態","payment":"入金記録","fulfillment":"特典付与の状態","attention":"確認状況","unknown":"不明な状態","paymentRecorded":"この注文には確認済みの入金記録があります","noRecordedPayment":"現在の記録に確認済みの入金はありません","paymentUnconfirmed":"現在の情報から入金を確認できません","paymentUnderReview":"検証済みの入金証拠は手動確認待ちです","expiredPending":"注文は期限切れで、特典は付与待ちです。これらの状態から決済サービスでの最終結果は確認できません。","fulfillmentStates":{"PENDING":"特典付与待ち","CREDITED":"特典付与済み","MANUAL_REVIEW":"手動確認が必要"},"attentionStates":{"NORMAL":"現在の確認フラグなし","PROCESSING":"処理中","REVIEW_REQUIRED":"確認が必要","RECEIVABLE_OPEN":"未精算の未収金あり"}};
