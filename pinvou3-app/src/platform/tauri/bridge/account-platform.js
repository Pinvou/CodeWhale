/** Account Platform desktop transport. OAuth tokens remain behind Tauri. */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};

  registry["account-platform"] = function (context) {
    var invoke = context.invoke;
    // This announces invalidated catalog ownership, not another login state.
    // Identity data and OAuth credentials never cross this subscription.
    var identityListeners = new Set();
    var pendingIdentityChanges = 0;
    function identityChanged() {
      identityListeners.forEach(function (listener) {
        try { listener(pendingIdentityChanges > 0); } catch (_) {}
      });
    }
    function lostAuthentication(error) {
      var code = typeof error === 'string' ? error.split(':', 1)[0] : error && error.code;
      return code === 'authentication_required';
    }
    async function identityOperation(operation) {
      pendingIdentityChanges += 1;
      identityChanged();
      try { return await operation(); }
      finally { pendingIdentityChanges -= 1; identityChanged(); }
    }
    async function observedOperation(operation) {
      try {
        var response = await operation();
        if (response && (response.status === 401 || response.authenticated === false)) identityChanged();
        return response;
      } catch (error) {
        if (lostAuthentication(error)) identityChanged();
        throw error;
      }
    }
    function changesIdentity(request) {
      if (!request || request.area !== 'account' || request.method !== 'POST'
          || typeof request.resource !== 'string') return false;
      try {
        // Match the native PreparedRequest URL and allowlisted route, including
        // dot segments and outer slashes; keep the original invoke DTO intact.
        var url = new URL('http://desktop.invalid' + request.resource);
        if (url.hostname !== 'desktop.invalid' || url.href.includes('?') || url.href.includes('#')) return false;
        var route = url.pathname.replace(/^\/+|\/+$/g, '');
        return route === 'v1/sessions/current/active-context' || route === 'v1/account/bootstrap';
      } catch (_) { return false; }
    }
    return {
      subscribeIdentityChange: function (listener) {
        identityListeners.add(listener);
        listener(pendingIdentityChanges > 0);
        return function () { identityListeners.delete(listener); };
      },
      getModelCatalog: function () { return invoke("account_platform_models"); },
      setDefaultModel: function (model) { return invoke("account_platform_set_default_model", { model: model }); },
      session: function () { return observedOperation(function () { return invoke("account_platform_session"); }); },
      login: function (credentials) { return identityOperation(function () { return invoke("account_platform_login", { credentials: credentials }); }); },
      createSliderCaptcha: function (input) { return invoke("account_platform_create_slider_captcha", { input: input }); },
      precheckMobileRegistration: function (input) { return invoke("account_platform_precheck_mobile_registration", { input: input }); },
      sendSms: function (input) { return invoke("account_platform_send_sms", { input: input }); },
      registerMobile: function (input) { return invoke("account_platform_register_mobile", { input: input }); },
      loginSms: function (input) { return identityOperation(function () { return invoke("account_platform_login_sms", { input: input }); }); },
      logout: function () { return identityOperation(function () { return invoke("account_platform_logout"); }); },
      request: function (request) {
        var operation = function () { return invoke("account_platform_request", { request: request }); };
        return changesIdentity(request) ? identityOperation(operation) : observedOperation(operation);
      },
    };
  };
})(window);
