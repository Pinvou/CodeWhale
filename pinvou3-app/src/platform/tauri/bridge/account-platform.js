/** Account Platform desktop transport. OAuth tokens remain behind Tauri. */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};

  registry["account-platform"] = function (context) {
    var invoke = context.invoke;
    return {
      session: function () { return invoke("account_platform_session"); },
      openPortal: function (input) { return invoke("account_platform_open_portal", { input: input }); },
      selectCatalogModel: function (input) { return invoke("account_platform_select_catalog_model", { input: input }); },
      selectSessionCatalogModel: function (input) { return invoke("account_platform_select_session_model", { input: input }); },
      modelCatalog: function () { return invoke("account_platform_model_catalog"); },
      branchReplacementSession: function (input) { return invoke("account_platform_branch_replacement_session", { input: input }); },
      login: function (credentials) { return invoke("account_platform_login", { credentials: credentials }); },
      createSliderCaptcha: function (input) { return invoke("account_platform_create_slider_captcha", { input: input }); },
      precheckMobileRegistration: function (input) { return invoke("account_platform_precheck_mobile_registration", { input: input }); },
      sendSms: function (input) { return invoke("account_platform_send_sms", { input: input }); },
      registerMobile: function (input) { return invoke("account_platform_register_mobile", { input: input }); },
      registrationStatus: function () { return invoke("account_platform_registration_status"); },
      loginSms: function (input) { return invoke("account_platform_login_sms", { input: input }); },
      resetPassword: function (input) { return invoke("account_platform_reset_password", { input: input }); },
      logout: function () { return invoke("account_platform_logout"); },
      request: function (request) {
        return invoke("account_platform_request", { request: request });
      },
    };
  };
})(window);
