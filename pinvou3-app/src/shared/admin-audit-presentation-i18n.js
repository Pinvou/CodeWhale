export const auditPrivacyCopy = {
  "zh": {
    "defaultRedacted": "新导出默认脱敏。只有获得未脱敏审计导出权限才可选此项。",
    "recentAuth": "导出时可能需要重新验证身份；验证身份不会增加权限。",
    "permissionUnavailable": "当前没有未脱敏导出权限。",
    "savedSelectionUnavailable": "保留的选择为未脱敏，当前没有相应权限。"
  },
  "en": {
    "defaultRedacted": "New exports are redacted by default. This option requires permission to export unredacted audit data.",
    "recentAuth": "Export may require you to verify your identity again. Verification does not grant permissions.",
    "permissionUnavailable": "You do not currently have permission for unredacted exports.",
    "savedSelectionUnavailable": "The retained selection is unredacted. You do not currently have that permission."
  },
  "ja": {
    "defaultRedacted": "新規エクスポートでは既定で機密情報を除きます。この項目を選ぶには機密情報を含む監査データのエクスポート権限が必要です。",
    "recentAuth": "エクスポート時に再認証が必要な場合があります。再認証で権限が追加されることはありません。",
    "permissionUnavailable": "現在、機密情報を含むエクスポート権限がありません。",
    "savedSelectionUnavailable": "保持された選択は機密情報を含みます。現在その権限はありません。"
  }
};

Object.assign(auditPrivacyCopy.zh, {includeUnredacted: '包含未脱敏的个人字段'});
Object.assign(auditPrivacyCopy.en, {includeUnredacted: 'Include unredacted personal fields'});
Object.assign(auditPrivacyCopy.ja, {includeUnredacted: '機密情報を含む個人項目を含める'});

export const auditExportPresentation = {
  "zh": {
    "confirmTitle": "确认审计导出",
    "confirmLead": "以下是实际将提交的条件。确认后才发送请求；文件是否可用以导出状态为准。",
    "all": "不限",
    "timeZone": "输入采用的浏览器时区（上方时间已转换为 UTC）",
    "zoneUnavailable": "时区名称不可用，以列出的 UTC 时间为准。",
    "privacy": "数据范围",
    "unredacted": "包含未脱敏个人字段",
    "redacted": "已脱敏",
    "reason": "导出原因",
    "cancel": "取消",
    "confirmSubmit": "确认并提交导出",
    "reviewButton": "核对导出条件",
    "submitting": "正在提交已确认的导出请求…",
    "unknown": "导出请求结果尚未确认。重试仍核对同一请求。",
    "ticketExpiry": "下载票据有效至",
    "statusUnknown": "暂无法确认状态",
    "status": {
      "PENDING": "排队中",
      "GENERATING": "生成中",
      "READY": "文件已生成",
      "DOWNLOADED": "下载票据已使用",
      "FAILED": "导出失败",
      "EXPIRED": "已过期"
    },
    "limits": [
      [
        "范围",
        "最多 31 天、100,000 行、500 MiB"
      ],
      [
        "每位操作者",
        "最多同时 2 个、每个 UTC 自然日 10 个"
      ],
      [
        "文件保留上限",
        "24 小时"
      ],
      [
        "下载票据",
        "15 分钟，成功使用一次"
      ]
    ]
  },
  "en": {
    "confirmTitle": "Confirm audit export",
    "confirmLead": "Review the exact filters that will be submitted. The request is sent after confirmation. The export status shows when a file is available.",
    "all": "Any",
    "timeZone": "Browser input time zone (times above are converted to UTC)",
    "zoneUnavailable": "The time zone name is unavailable; use the UTC times shown.",
    "privacy": "Data scope",
    "unredacted": "Includes unredacted personal fields",
    "redacted": "Redacted",
    "reason": "Export reason",
    "cancel": "Cancel",
    "confirmSubmit": "Confirm and submit export",
    "reviewButton": "Review export filters",
    "submitting": "Submitting the confirmed export request…",
    "unknown": "The export request outcome is unconfirmed. Retry checks the same request.",
    "ticketExpiry": "Download ticket expires",
    "statusUnknown": "Status could not be confirmed",
    "status": {
      "PENDING": "Queued",
      "GENERATING": "Generating",
      "READY": "File generated",
      "DOWNLOADED": "Download ticket used",
      "FAILED": "Export failed",
      "EXPIRED": "Expired"
    },
    "limits": [
      [
        "Scope",
        "Up to 31 days, 100,000 rows and 500 MiB"
      ],
      [
        "Per operator",
        "Up to 2 concurrent and 10 per UTC day"
      ],
      [
        "Maximum file retention",
        "24 hours"
      ],
      [
        "Download ticket",
        "15 minutes, one successful use"
      ]
    ]
  },
  "ja": {
    "confirmTitle": "監査エクスポートの確認",
    "confirmLead": "実際に送信される条件を確認してください。確認後に送信し、ファイルの利用可否はエクスポート状態で確認できます。",
    "all": "指定なし",
    "timeZone": "入力時のブラウザーのタイムゾーン（上記は UTC に変換済み）",
    "zoneUnavailable": "タイムゾーン名を取得できません。表示された UTC 時刻を確認してください。",
    "privacy": "データの範囲",
    "unredacted": "機密情報を含む個人項目あり",
    "redacted": "機密情報を除く",
    "reason": "エクスポートの理由",
    "cancel": "キャンセル",
    "confirmSubmit": "確認して送信",
    "reviewButton": "エクスポート条件を確認",
    "submitting": "確認済みのエクスポートを送信中…",
    "unknown": "送信結果は未確認です。再試行すると同じリクエストを確認します。",
    "ticketExpiry": "ダウンロード票の有効期限",
    "statusUnknown": "状態を確認できません",
    "status": {
      "PENDING": "待機中",
      "GENERATING": "生成中",
      "READY": "ファイル生成済み",
      "DOWNLOADED": "ダウンロード票は使用済み",
      "FAILED": "エクスポート失敗",
      "EXPIRED": "期限切れ"
    },
    "limits": [
      [
        "範囲",
        "最大 31 日、100,000 行、500 MiB"
      ],
      [
        "操作者ごと",
        "同時に最大 2 件、UTC の 1 日に最大 10 件"
      ],
      [
        "ファイルの最大保持時間",
        "24 時間"
      ],
      [
        "ダウンロード票",
        "15 分、使用成功は 1 回"
      ]
    ]
  }
};
