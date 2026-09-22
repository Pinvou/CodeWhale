import { portalSessionHandoffCopy } from "./assets/portal-session-handoff-i18n.js";
import { portalSessionHandoffLanguage, takePortalSessionHandoffTicket } from "./assets/portal-session-handoff-url.js";

const requestedLocale = new URLSearchParams(location.search).get("locale")?.toLowerCase();
const localeKey = portalSessionHandoffLanguage(requestedLocale, navigator.language, localStorage);
const locale = localeKey === "zh" ? "zh-CN" : localeKey;
const messages = portalSessionHandoffCopy[localeKey];
document.documentElement.lang = locale;
document.title = messages.title;
document.querySelector("main").textContent = messages.opening;
const ticket = takePortalSessionHandoffTicket(location.href, history.replaceState.bind(history));
if (!ticket) {
  document.querySelector("main").textContent = messages.invalid;
} else {
  fetch("/portal-session-handoff/consume", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ ticket })
  }).then(async (response) => {
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(payload.code || "SESSION_HANDOFF_INVALID");
    location.replace(payload.redirect_to || "/");
  }).catch((error) => {
    document.querySelector("main").textContent = error.message === "SESSION_HANDOFF_UNAVAILABLE" ? messages.unavailable : messages.invalid;
  });
}
