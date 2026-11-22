export const PORTAL_LANGUAGE_STORAGE_KEY = 'pinvou.account.language';
export const ADMIN_PORTAL_LANGUAGE_STORAGE_KEY = 'pinvou.admin.language';

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

// Language persistence is optional; browser privacy and quota policies must not
// prevent a Portal from mounting or changing its in-memory language.
export function optionalPortalLanguageStorage() {
  try {
    return globalThis.localStorage || null;
  } catch {
    return null;
  }
}

export function storePortalLanguage(storage, language, key = PORTAL_LANGUAGE_STORAGE_KEY) {
  try {
    if (!storage) return false;
    storage.setItem(key, language);
    return true;
  } catch {
    return false;
  }
}

export function consumePortalLanguage(href, storage, replaceState) {
  const url = new URL(href);
  const hasLocale = url.searchParams.has('locale');
  const language = normalizePortalLanguage(url.searchParams.get('locale'));
  if (language) storePortalLanguage(storage, language);
  if (hasLocale) {
    url.searchParams.delete('locale');
    replaceState(undefined, '', url.pathname + url.search + url.hash);
  }
  return language;
}

export function storedPortalLanguage(storage, key = PORTAL_LANGUAGE_STORAGE_KEY) {
  try {
    const language = storage?.getItem(key);
    return SUPPORTED_LANGUAGES.has(language) ? language : '';
  } catch {
    return '';
  }
}

export function portalHTMLLanguage(language) {
  return HTML_LANGUAGE[language] || 'en';
}
