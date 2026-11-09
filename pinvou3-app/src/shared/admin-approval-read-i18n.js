export const approvalReadCopy = {
  "zh": {
    "loading": "正在读取我的申请…",
    "denied": "你没有权限读取我的申请。",
    "signedOut": "登录状态已失效，请重新登录后查看我的申请。",
    "failed": "暂时无法读取我的申请。",
    "previous": "以下为当前条件下上次成功读取的结果。",
    "empty": "当前条件下没有你的申请。",
    "query": "查询",
    "reset": "重置",
    "retryRead": "重试读取",
    "refreshRead": "重新读取",
    "snapshotTime": "上次成功结果 · 查询快照时间：",
    "receivedTime": "上次成功读取 · 浏览器接收时间：",
    "targetID": "目标编号",
    "targetHelp": "输入业务对象的完整编号，精确匹配。支持 UUID 及业务编号，例如支付服务商编号。"
  },
  "en": {
    "loading": "Reading my requests…",
    "denied": "You do not have access to read My requests.",
    "signedOut": "Your sign-in has expired. Sign in again to view My requests.",
    "failed": "My requests could not be loaded.",
    "previous": "Showing the last successful result for these filters.",
    "empty": "No requests of yours match these filters.",
    "query": "Search",
    "reset": "Reset",
    "retryRead": "Retry loading",
    "refreshRead": "Reload results",
    "snapshotTime": "Last successful result · query snapshot:",
    "receivedTime": "Last successful read · received by this browser:",
    "targetID": "Target ID",
    "targetHelp": "Enter the full resource ID for an exact match. UUIDs and business IDs, such as a payment provider ID, are supported."
  },
  "ja": {
    "loading": "自分の申請を読み込み中…",
    "denied": "自分の申請を表示する権限がありません。",
    "signedOut": "ログインの有効期限が切れました。再度ログインして自分の申請を確認してください。",
    "failed": "自分の申請を取得できませんでした。",
    "previous": "この条件で前回正常に取得した結果を表示しています。",
    "empty": "この条件に一致する自分の申請はありません。",
    "query": "検索",
    "reset": "リセット",
    "retryRead": "再読み込みを試す",
    "refreshRead": "結果を再読み込み",
    "snapshotTime": "前回の結果・検索スナップショット時刻：",
    "receivedTime": "前回成功時・ブラウザーの受信時刻：",
    "targetID": "対象 ID",
    "targetHelp": "対象の完全な ID を入力すると、完全一致で検索できます。UUID や決済事業者 ID などの業務 ID に対応しています。"
  }
};

const pendingMessages = {
  "zh": {
    "loading": "正在读取待我审批…",
    "denied": "你没有权限读取待我审批。",
    "signedOut": "登录状态已失效，请重新登录后查看待我审批。",
    "failed": "暂时无法读取待我审批。",
    "empty": "当前查询结果没有需要你审批的申请。",
    "emptyPage": "这一页没有需要你审批的申请。",
    "emptyWithNext": "这一页没有需要你审批的申请，可以继续查看下一页。"
  },
  "en": {
    "loading": "Reading requests awaiting your approval…",
    "denied": "You do not have access to read requests awaiting your approval.",
    "signedOut": "Your sign-in has expired. Sign in again to view requests awaiting your approval.",
    "failed": "Requests awaiting your approval could not be loaded.",
    "empty": "There are no requests awaiting your approval in this result.",
    "emptyPage": "No requests on this page await your approval.",
    "emptyWithNext": "No requests on this page await your approval. You can continue to the next page."
  },
  "ja": {
    "loading": "承認待ちの申請を読み込み中…",
    "denied": "承認待ちの申請を表示する権限がありません。",
    "signedOut": "ログインの有効期限が切れました。再度ログインして承認待ちの申請を確認してください。",
    "failed": "承認待ちの申請を取得できませんでした。",
    "empty": "この検索結果に承認が必要な申請はありません。",
    "emptyPage": "このページに承認が必要な申請はありません。",
    "emptyWithNext": "このページに承認が必要な申請はありません。次のページを確認できます。"
  }
};
const pendingDeadlineFacts = {
  "zh": {
    "expiryFrom": "到期起始日（含）",
    "expiryTo": "到期截止日（不含）",
    "expiryHelp": "按 UTC 日期筛选申请到期时间。可只填写一端。",
    "invalidExpiry": "请输入有效日期，截止日期须晚于起始日期。",
    "requesterID": "申请人 ID",
    "submittedAt": "提交时间",
    "expiresAt": "到期时间",
    "missingFact": "未提供",
    "pageCount": "本页 {count} 条 · 按服务返回顺序"
  },
  "en": {
    "expiryFrom": "Expiry from (inclusive)",
    "expiryTo": "Expiry before (exclusive)",
    "expiryHelp": "Filter expiry by UTC date. Either boundary can be left empty.",
    "invalidExpiry": "Enter valid dates. The end date must be after the start date.",
    "requesterID": "Requester ID",
    "submittedAt": "Submitted at",
    "expiresAt": "Expires at",
    "missingFact": "Not provided",
    "pageCount": "{count} on this page · server order"
  },
  "ja": {
    "expiryFrom": "期限の開始日（当日を含む）",
    "expiryTo": "期限の終了日（当日を含まない）",
    "expiryHelp": "有効期限を UTC の日付で絞り込みます。片方のみでも指定できます。",
    "invalidExpiry": "有効な日付を指定し、終了日を開始日より後にしてください。",
    "requesterID": "申請者 ID",
    "submittedAt": "提出日時",
    "expiresAt": "有効期限",
    "missingFact": "未提供",
    "pageCount": "このページに {count} 件・取得順"
  }
};
export const pendingApprovalReadCopy = Object.fromEntries(Object.entries(pendingMessages).map(([language, messages]) => [language, { ...approvalReadCopy[language], ...messages, ...pendingDeadlineFacts[language] }]));

const executionMessages = {
  "zh": {
    "loading": "正在读取审批执行记录…",
    "denied": "你没有权限读取审批执行记录。",
    "signedOut": "登录状态已失效，请重新登录后查看执行记录。",
    "failed": "暂时无法读取执行列表，无法据此判断任何审批的执行结果。",
    "empty": "当前筛选结果没有审批执行记录。",
    "emptyPage": "这一页没有符合筛选条件的执行记录。",
    "emptyWithNext": "这一页没有符合筛选条件的执行记录，可以继续查看下一页。"
  },
  "en": {
    "loading": "Reading approval execution records…",
    "denied": "You do not have access to read approval execution records.",
    "signedOut": "Your sign-in has expired. Sign in again to view execution records.",
    "failed": "The execution list could not be loaded. This does not tell you whether any approval executed successfully.",
    "empty": "No approval execution records match these filters.",
    "emptyPage": "No execution records on this page match these filters.",
    "emptyWithNext": "No execution records on this page match these filters. You can continue to the next page."
  },
  "ja": {
    "loading": "承認の実行記録を読み込み中…",
    "denied": "承認の実行記録を表示する権限がありません。",
    "signedOut": "ログインの有効期限が切れました。再度ログインして実行記録を確認してください。",
    "failed": "実行一覧を取得できませんでした。このエラーから各承認の実行結果は判断できません。",
    "empty": "この条件に一致する実行記録はありません。",
    "emptyPage": "このページに条件と一致する実行記録はありません。",
    "emptyWithNext": "このページに条件と一致する実行記録はありません。次のページを確認できます。"
  }
};
const executionDetailMessages = {
  "zh": {
    "detailsTitle": "执行详情",
    "attempt": "执行尝试状态",
    "attemptLabels": {
      "EXECUTING": "执行中",
      "EXECUTED": "已执行",
      "FAILED": "执行失败"
    },
    "errorClass": "可展示的错误类别",
    "publicFailure": "执行失败（服务未提供更具体类别）",
    "retryable": "记录中的重试资格",
    "retryableYes": "标记为可重试；实际可用动作仍受审批权限约束。",
    "retryableNo": "标记为不可重试",
    "nextRetry": "下次重试时间",
    "deadline": "执行期限",
    "noDetailValue": "未提供",
    "noAttempt": "该审批详情未提供可查看的执行记录。",
    "detailLoading": "正在读取所选审批的详情…",
    "detailFailed": "暂时无法读取所选审批的执行详情。",
    "detailDenied": "你没有权限读取所选审批的执行详情。",
    "detailSignedOut": "登录状态已失效，请重新登录后查看执行详情。"
  },
  "en": {
    "detailsTitle": "Execution details",
    "attempt": "Attempt status",
    "attemptLabels": {
      "EXECUTING": "Executing",
      "EXECUTED": "Executed",
      "FAILED": "Execution failed"
    },
    "errorClass": "Available error category",
    "publicFailure": "Execution failed (no more specific category was provided)",
    "retryable": "Retry eligibility in the record",
    "retryableYes": "Marked retryable; available actions still depend on approval permissions.",
    "retryableNo": "Marked not retryable",
    "nextRetry": "Next retry time",
    "deadline": "Execution deadline",
    "noDetailValue": "Not provided",
    "noAttempt": "This approval detail does not provide a viewable execution record.",
    "detailLoading": "Reading details for the selected approval…",
    "detailFailed": "The selected approval’s execution details could not be loaded.",
    "detailDenied": "You do not have access to the selected approval’s execution details.",
    "detailSignedOut": "Your sign-in has expired. Sign in again to view execution details."
  },
  "ja": {
    "detailsTitle": "実行詳細",
    "attempt": "実行試行の状態",
    "attemptLabels": {
      "EXECUTING": "実行中",
      "EXECUTED": "実行済み",
      "FAILED": "実行失敗"
    },
    "errorClass": "表示できるエラー分類",
    "publicFailure": "実行失敗（詳細な分類は提供されていません）",
    "retryable": "記録上の再試行可否",
    "retryableYes": "再試行可能と記録されています。利用できる操作は承認権限によって決まります。",
    "retryableNo": "再試行不可と記録されています",
    "nextRetry": "次の再試行時刻",
    "deadline": "実行期限",
    "noDetailValue": "未提供",
    "noAttempt": "この承認詳細では閲覧できる実行記録が提供されていません。",
    "detailLoading": "選択した承認の詳細を読み込み中…",
    "detailFailed": "選択した承認の実行詳細を取得できませんでした。",
    "detailDenied": "選択した承認の実行詳細を表示する権限がありません。",
    "detailSignedOut": "ログインの有効期限が切れました。再度ログインして実行詳細を確認してください。"
  }
};
export const executionApprovalReadCopy = Object.fromEntries(Object.entries(executionMessages).map(([language, messages]) => [language, { ...approvalReadCopy[language], ...messages, ...executionDetailMessages[language] }]));
