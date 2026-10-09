const LANGUAGE_BY_LOCALE = new Map([
  ['zh', 'zh'], ['zh-cn', 'zh'], ['zh-hans', 'zh'],
  ['en', 'en'], ['en-us', 'en'], ['en-gb', 'en'],
  ['ja', 'ja'], ['ja-jp', 'ja'],
]);

export function portalSessionHandoffLanguage(requestedLocale, browserLocale, storage) {
  const requestedLanguage = LANGUAGE_BY_LOCALE.get(String(requestedLocale || '').trim().toLowerCase()) || '';
  if (requestedLanguage) {
    try {
      storage.setItem('pinvou.account.language', requestedLanguage);
    } catch {
      // Storage restrictions must not block consumption of a valid one-time ticket.
    }
  }
  const browserLanguage = String(browserLocale || '').trim().toLowerCase();
  const browserPrimary = browserLanguage.split('-', 1)[0];
  const browserFallback = ['zh', 'ja'].includes(browserPrimary) ? browserPrimary : 'en';
  return requestedLanguage || browserFallback;
}

export function takePortalSessionHandoffTicket(href, replaceState) {
  const cleanURL = new URL(href);
  const ticket = cleanURL.hash.startsWith('#ticket=') ? cleanURL.hash.slice(8) : '';
  cleanURL.hash = '';
  cleanURL.searchParams.delete('locale');
  replaceState(null, '', cleanURL.pathname + cleanURL.search);
  return ticket;
}
