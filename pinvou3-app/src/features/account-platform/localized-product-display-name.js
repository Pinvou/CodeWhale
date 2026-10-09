function currentProductName(item, language) {
  const normalizedLanguage = String(language || '').trim().toLowerCase();
  const languageKey = normalizedLanguage.startsWith('zh')
    ? 'zh_cn'
    : normalizedLanguage.startsWith('ja')
      ? 'ja'
      : 'en';
  return item?.[`current_plan_name_${languageKey}`];
}

export function localizedProductDisplayName(item, language, stableIDField) {
  for (const candidate of [currentProductName(item, language), item?.plan_name, item?.[stableIDField]]) {
    const value = String(candidate || '').trim();
    if (value) return value;
  }
  return '';
}
