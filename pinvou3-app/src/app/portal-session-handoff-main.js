import '../shared/legacy-polyfills.js';
import { accountPlatformCopy } from '../shared/i18n.js';
import { portalHTMLLanguage } from '../features/account-platform/portal-language.js';
import { portalSessionHandoffLanguage, takePortalSessionHandoffTicket } from '../features/account-platform/portal-session-handoff-url.js';
import { consumePortalSessionHandoff } from '../features/account-platform/portal-session-handoff.js';

const requestedLocale = new URLSearchParams(location.search).get('locale');
const ticket = takePortalSessionHandoffTicket(location.href, history.replaceState.bind(history));
const locale = portalSessionHandoffLanguage(requestedLocale, navigator.language, localStorage);
const copy = accountPlatformCopy[locale];
const main = document.querySelector('main');
document.documentElement.lang = portalHTMLLanguage(locale);
document.title = copy.sessionHandoff.title;
main.textContent = copy.sessionHandoff.opening;
const failure = reason => {
  main.textContent = reason.message === 'SESSION_HANDOFF_UNAVAILABLE' ? copy.sessionHandoff.unavailable : copy.sessionHandoff.invalid;
  const login = document.createElement('a');
  login.href = '/auth/login?return_to=%2F'; login.textContent = copy.login;
  main.append(document.createElement('br'), login);
};
consumePortalSessionHandoff(ticket).then(path => location.replace(path)).catch(failure);
