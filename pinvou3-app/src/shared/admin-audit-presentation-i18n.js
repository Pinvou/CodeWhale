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
