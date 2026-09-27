<!doctype html>
<html lang="${locale.currentLanguageTag!'zh-CN'}">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light">
  <title>${msg("pinvouLoginTitle")}</title>
  <link rel="stylesheet" href="${url.resourcesPath}/css/pinvou-login.css">
</head>
<body>
  <main class="pv-auth-page">
    <section class="pv-auth-card" aria-labelledby="pv-auth-title">
      <header class="pv-auth-header">
        <p class="pv-brand">PINVOU ACCOUNT PLATFORM</p>
        <h1 id="pv-auth-title"><#if pinvouMode == "register">${msg("pinvouRegisterTitle")}<#elseif pinvouMode == "reset">${msg("pinvouResetPasswordTitle")}<#else>${msg("pinvouLoginTitle")}</#if></h1>
        <p>${msg("pinvouLoginSubtitle")}</p>
      </header>

      <form id="pv-auth-form" action="${url.loginAction}" method="post" novalidate>
        <input type="hidden" name="mode" value="${pinvouMode}">
        <input type="hidden" name="intent" id="pv-intent" value="submit">
        <input type="hidden" name="target_mode" id="pv-target-mode" value="">
        <input type="hidden" name="device_id" id="pv-device-id" value="">

        <div class="pv-tabs" role="tablist" aria-label="${msg("pinvouLoginMethod")}">
          <button type="submit" name="mode_tab" value="password" data-mode="password" class="${(pinvouMode == 'password')?then('active','')}" role="tab" aria-selected="${(pinvouMode == 'password')?c}" formnovalidate>${msg("pinvouPasswordLogin")}</button>
          <button type="submit" name="mode_tab" value="sms" data-mode="sms" class="${(pinvouMode == 'sms')?then('active','')}" role="tab" aria-selected="${(pinvouMode == 'sms')?c}" formnovalidate>${msg("pinvouSmsLogin")}</button>
        </div>

        <#if message?has_content>
          <div class="pv-message ${message.type!'error'}" role="${(message.type == 'error')?then('alert','status')}">${message.summary}</div>
        </#if>

        <div class="pv-form-fields">
          <#if pinvouMode == "password">
            <label class="pv-field">
              <span>${msg("pinvouAccountLabel")}</span>
              <input id="pv-account" name="username" type="text" autocomplete="username" maxlength="64" placeholder="${msg('pinvouAccountPlaceholder')}" required autofocus>
            </label>
            <label class="pv-field">
              <span>${msg("pinvouPasswordLabel")}</span>
              <span class="pv-password-wrap">
                <input id="pv-password" name="password" type="password" autocomplete="current-password" maxlength="128" placeholder="${msg('pinvouPasswordPlaceholder')}" required>
                <span class="pv-password-reveal-output" data-password-reveal-output role="status" aria-live="polite" aria-atomic="true" aria-hidden="true" hidden></span>
                <button type="button" class="pv-eye" data-toggle-password aria-label="${msg('pinvouShowPassword')}" aria-pressed="false" disabled hidden>
                  <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M2.5 12s3.4-6 9.5-6 9.5 6 9.5 6-3.4 6-9.5 6-9.5-6-9.5-6Z"/><circle cx="12" cy="12" r="2.6"/></svg>
                </button>
              </span>
            </label>
            <div class="pv-options">
              <label><input id="pv-remember" type="checkbox"> <span>${msg("pinvouRememberAccount")}</span></label>
              <button type="submit" data-mode="reset" formnovalidate>${msg("pinvouForgotPassword")}</button>
            </div>
          <#elseif pinvouMode == "sms">
            <label class="pv-field">
              <span>${msg("pinvouMobileLabel")}</span>
              <span class="pv-inline-control">
                <span class="pv-country">+86</span>
                <input name="mobile" type="tel" inputmode="tel" autocomplete="tel-national" pattern="1[3-9][0-9]{9}" maxlength="11" value="${pinvouMobile}" placeholder="${msg('pinvouMobilePlaceholder')}" <#if pinvouSmsSent || pinvouCaptchaId?has_content>readonly</#if> required autofocus>
                <button type="submit" class="pv-code-button" data-request-sms data-cooldown="${pinvouRetryAfter}" formnovalidate><#if pinvouSmsSent>${msg("pinvouResendCode")}<#else>${msg("pinvouGetCode")}</#if></button>
              </span>
            </label>
            <label class="pv-field">
              <span>${msg("pinvouCodeLabel")}</span>
              <input name="sms_code" type="text" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" placeholder="${msg('pinvouCodePlaceholder')}" required>
            </label>
          <#elseif pinvouMode == "register">
            <label class="pv-field">
              <span>${msg("pinvouUsernameLabel")}</span>
              <input name="username" type="text" autocomplete="username" minlength="3" maxlength="32" pattern="[A-Za-z][A-Za-z0-9._-]{2,31}" value="${pinvouUsername}" placeholder="${msg('pinvouUsernamePlaceholder')}" <#if pinvouPasswordAccepted>readonly</#if> required autofocus>
            </label>
            <label class="pv-field">
              <span>${msg("pinvouMobileLabel")}</span>
              <span class="pv-inline-control">
                <span class="pv-country">+86</span>
                <input name="mobile" type="tel" inputmode="tel" autocomplete="tel-national" pattern="1[3-9][0-9]{9}" maxlength="11" value="${pinvouMobile}" placeholder="${msg('pinvouMobilePlaceholder')}" <#if pinvouPasswordAccepted>readonly</#if> required>
                <button type="submit" class="pv-code-button" data-request-sms data-cooldown="${pinvouRetryAfter}" formnovalidate><#if pinvouSmsSent>${msg("pinvouResendCode")}<#else>${msg("pinvouGetCode")}</#if></button>
              </span>
            </label>
            <#if !pinvouPasswordAccepted>
              <label class="pv-field">
                <span>${msg("pinvouPasswordLabel")}</span>
                <span class="pv-password-wrap"><input id="pv-password" name="password" type="password" autocomplete="new-password" minlength="8" maxlength="128" placeholder="${msg('pinvouNewPasswordPlaceholder')}" required><span class="pv-password-reveal-output" data-password-reveal-output role="status" aria-live="polite" aria-atomic="true" aria-hidden="true" hidden></span><button type="button" class="pv-eye" data-toggle-password aria-label="${msg('pinvouShowPassword')}" aria-pressed="false" disabled hidden><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M2.5 12s3.4-6 9.5-6 9.5 6 9.5 6-3.4 6-9.5 6-9.5-6-9.5-6Z"/><circle cx="12" cy="12" r="2.6"/></svg></button></span>
              </label>
              <label class="pv-field">
                <span>${msg("pinvouConfirmPasswordLabel")}</span>
                <input name="password_confirmation" type="password" autocomplete="new-password" minlength="8" maxlength="128" placeholder="${msg('pinvouConfirmPasswordPlaceholder')}" required>
              </label>
            <#else>
              <div class="pv-password-accepted" role="status">${msg("pinvouPasswordAccepted")}</div>
            </#if>
            <label class="pv-field">
              <span>${msg("pinvouCodeLabel")}</span>
              <input name="sms_code" type="text" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" placeholder="${msg('pinvouCodePlaceholder')}" required>
            </label>
          <#else>
            <label class="pv-field">
              <span>${msg("pinvouMobileLabel")}</span>
              <span class="pv-inline-control">
                <span class="pv-country">+86</span>
                <input name="mobile" type="tel" inputmode="tel" autocomplete="tel-national" pattern="1[3-9][0-9]{9}" maxlength="11" value="${pinvouMobile}" placeholder="${msg('pinvouMobilePlaceholder')}" <#if pinvouPasswordAccepted>readonly</#if> required autofocus>
                <button type="submit" class="pv-code-button" data-request-sms data-cooldown="${pinvouRetryAfter}" formnovalidate><#if pinvouSmsSent>${msg("pinvouResendCode")}<#else>${msg("pinvouGetCode")}</#if></button>
              </span>
            </label>
            <#if !pinvouPasswordAccepted>
              <label class="pv-field">
                <span>${msg("pinvouNewPasswordLabel")}</span>
                <span class="pv-password-wrap"><input id="pv-password" name="password" type="password" autocomplete="new-password" minlength="8" maxlength="128" placeholder="${msg('pinvouNewPasswordPlaceholder')}" required><span class="pv-password-reveal-output" data-password-reveal-output role="status" aria-live="polite" aria-atomic="true" aria-hidden="true" hidden></span><button type="button" class="pv-eye" data-toggle-password aria-label="${msg('pinvouShowPassword')}" aria-pressed="false" disabled hidden><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M2.5 12s3.4-6 9.5-6 9.5 6 9.5 6-3.4 6-9.5 6-9.5-6-9.5-6Z"/><circle cx="12" cy="12" r="2.6"/></svg></button></span>
              </label>
              <label class="pv-field">
                <span>${msg("pinvouConfirmPasswordLabel")}</span>
                <input name="password_confirmation" type="password" autocomplete="new-password" minlength="8" maxlength="128" placeholder="${msg('pinvouConfirmPasswordPlaceholder')}" required>
              </label>
            <#else>
              <div class="pv-password-accepted" role="status">${msg("pinvouResetPasswordAccepted")}</div>
            </#if>
            <label class="pv-field">
              <span>${msg("pinvouCodeLabel")}</span>
              <input name="sms_code" type="text" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" placeholder="${msg('pinvouCodePlaceholder')}" required>
            </label>
          </#if>
        </div>

        <button type="submit" class="pv-submit" data-submit-auth><#if pinvouMode == "register">${msg("pinvouRegisterAndLogin")}<#elseif pinvouMode == "reset">${msg("pinvouResetPasswordButton")}<#else>${msg("pinvouLoginButton")}</#if></button>

        <div class="pv-register-entry">
          <#if pinvouMode == "register" || pinvouMode == "reset">
            <span>${msg("pinvouHasAccount")}</span>
            <button type="submit" data-mode="password" formnovalidate>${msg("pinvouBackToLogin")}</button>
          <#else>
            <span>${msg("pinvouNoAccount")}</span>
            <button type="submit" data-mode="register" formnovalidate>${msg("pinvouRegisterNow")}</button>
          </#if>
        </div>

        <#if pinvouCaptchaId?has_content>
          <div class="pv-captcha-backdrop" role="presentation">
            <section class="pv-captcha" role="dialog" aria-modal="true" aria-labelledby="pv-captcha-title">
              <h2 id="pv-captcha-title">${msg("pinvouCaptchaTitle")}</h2>
              <p>${msg("pinvouCaptchaHelp")}</p>
              <div class="pv-captcha-stage" style="--captcha-width:${pinvouCaptchaWidth}px;--captcha-height:${pinvouCaptchaHeight}px;--piece-y:${pinvouCaptchaPieceY}px">
                <img src="${pinvouCaptchaBackground}" alt="">
                <img class="pv-captcha-piece" src="${pinvouCaptchaPiece}" alt="">
              </div>
              <input id="pv-captcha-offset" name="captcha_offset" type="range" min="0" max="${(pinvouCaptchaWidth?number - pinvouCaptchaPieceWidth?number)?c}" value="0" aria-label="${msg('pinvouCaptchaSlider')}">
              <button type="submit" class="pv-submit" data-send-sms formnovalidate>${msg("pinvouCaptchaConfirm")}</button>
            </section>
          </div>
        </#if>
      </form>
    </section>
  </main>
  <script src="${url.resourcesPath}/js/pinvou-login.js"></script>
</body>
</html>
