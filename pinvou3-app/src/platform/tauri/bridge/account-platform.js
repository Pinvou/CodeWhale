/** Account Platform desktop transport. OAuth tokens remain behind Tauri. */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};

  registry["account-platform"] = function (context) {
    var invoke = context.invoke;
    return {
      session: function () { return invoke("account_platform_session"); },
      login: function (username, password) {
        return invoke("account_platform_login", {
          credentials: { username: username, password: password },
        });
      },
      logout: function () { return invoke("account_platform_logout"); },
      request: function (request) {
        return invoke("account_platform_request", { request: request });
      },
    };
  };
})(window);
