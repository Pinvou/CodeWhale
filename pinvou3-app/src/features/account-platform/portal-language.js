export const PORTAL_LANGUAGE_STORAGE_KEY = 'pinvou.account.language';

const SUPPORTED_LANGUAGES = new Set(['zh', 'en', 'ja']);
const HTML_LANGUAGE = { zh: 'zh-CN', en: 'en', ja: 'ja-JP' };
const LANGUAGE_BY_LOCALE = new Map([
  ['zh', 'zh'], ['zh-cn', 'zh'], ['zh-hans', 'zh'],
  ['en', 'en'], ['en-us', 'en'], ['en-gb', 'en'],
  ['ja', 'ja'], ['ja-jp', 'ja'],
]);

export function normalizePortalLanguage(value) {
  return LANGUAGE_BY_LOCALE.get(String(value || '').trim().toLowerCase()) || '';
}

export function browserPortalLanguage(value) {
  const primary = String(value || '').trim().toLowerCase().split('-', 1)[0];
  return SUPPORTED_LANGUAGES.has(primary) ? primary : '';
}

export function consumePortalLanguage(href, storage, replaceState) {
  const url = new URL(href);
  const hasLocale = url.searchParams.has('locale');
  const language = normalizePortalLanguage(url.searchParams.get('locale'));
  if (language) storage.setItem(PORTAL_LANGUAGE_STORAGE_KEY, language);
  if (hasLocale) {
    url.searchParams.delete('locale');
    replaceState(undefined, '', url.pathname + url.search + url.hash);
  }
  return language;
}

export function storedPortalLanguage(storage) {
  const language = storage.getItem(PORTAL_LANGUAGE_STORAGE_KEY);
  return SUPPORTED_LANGUAGES.has(language) ? language : '';
}

export function portalHTMLLanguage(language) {
  return HTML_LANGUAGE[language] || 'en';
}
