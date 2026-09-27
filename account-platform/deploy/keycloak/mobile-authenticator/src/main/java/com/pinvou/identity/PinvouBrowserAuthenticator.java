package com.pinvou.identity;

import jakarta.ws.rs.core.MultivaluedMap;
import jakarta.ws.rs.core.Response;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.List;
import java.util.regex.Pattern;
import java.util.stream.Stream;
import org.jboss.logging.Logger;
import org.keycloak.authentication.AuthenticationFlowContext;
import org.keycloak.authentication.AuthenticationFlowError;
import org.keycloak.authentication.Authenticator;
import org.keycloak.authentication.authenticators.util.AuthenticatorUtils;
import org.keycloak.events.Errors;
import org.keycloak.forms.login.LoginFormsProvider;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserCredentialModel;
import org.keycloak.models.UserModel;
import org.keycloak.models.utils.KeycloakModelUtils;

public final class PinvouBrowserAuthenticator implements Authenticator {
    private static final Logger LOG = Logger.getLogger(PinvouBrowserAuthenticator.class);
    private static final Pattern MOBILE = Pattern.compile("^1[3-9][0-9]{9}$");
    private static final Pattern CODE = Pattern.compile("^[0-9]{6}$");
    private static final Pattern USERNAME = Pattern.compile("^[A-Za-z][A-Za-z0-9._-]{2,31}$");
    private static final Pattern DEVICE = Pattern.compile("^[A-Za-z0-9._:-]{8,128}$");
    private static final String TEMPLATE = "pinvou-login.ftl";
    private static final String NOTE_PREFIX = "pinvou.browser.";
    private static final String REGISTRATION_OPERATION_ATTRIBUTE = "pinvou_registration_operation_id";
    private static final PinvouAuthClient CLIENT = new PinvouAuthClient();

    @Override
    public void authenticate(AuthenticationFlowContext context) {
        render(context, mode(context), "");
    }

    @Override
    public void action(AuthenticationFlowContext context) {
        MultivaluedMap<String, String> form = context.getHttpRequest().getDecodedFormParameters();
        String requestedMode = safeMode(form.getFirst("mode"));
        String intent = trim(form.getFirst("intent"));
        if ("switch_mode".equals(intent)) {
            clearState(context);
            render(context, safeMode(form.getFirst("target_mode")), "");
            return;
        }
        context.getAuthenticationSession().setAuthNote(note("mode"), requestedMode);
        try {
            switch (intent) {
                case "request_sms" -> requestSMS(context, form, requestedMode);
                case "send_sms" -> sendSMS(context, form, requestedMode);
                case "submit" -> submit(context, form, requestedMode);
                default -> render(context, requestedMode, "pinvouInvalidRequest");
            }
        } catch (PinvouAuthClient.AuthClientException error) {
            LOG.warnf("Pinvou browser authentication dependency rejected a request: %s", error.code());
            if ("CAPTCHA_INVALID".equals(error.code()) || "SMS_RATE_LIMITED".equals(error.code())) clearCaptcha(context);
            if ("REGISTRATION_RESTART_REQUIRED".equals(error.code())) clearState(context);
            render(context, requestedMode, messageFor(error.code()));
        } catch (RuntimeException error) {
            LOG.errorf("Pinvou browser authentication failed safely: %s", error.getClass().getSimpleName());
            render(context, requestedMode, "pinvouServiceUnavailable");
        }
    }

    private void requestSMS(AuthenticationFlowContext context, MultivaluedMap<String, String> form, String mode)
        throws PinvouAuthClient.AuthClientException {
        if ("password".equals(mode)) {
            render(context, mode, "pinvouInvalidRequest");
            return;
        }
        String mobile = normalizeMobile(form.getFirst("mobile"));
        String device = normalizeDevice(form.getFirst("device_id"));
        if (mobile.isEmpty()) {
            render(context, mode, "pinvouMobileInvalid");
            return;
        }
        put(context, "mobile", mobile);
        put(context, "device", device);
        if ("register".equals(mode)) {
            String username = trim(form.getFirst("username"));
            if (!USERNAME.matcher(username).matches()) {
                render(context, mode, "pinvouUsernameInvalid");
                return;
            }
            if (get(context, "password_handle").isEmpty()) {
                String password = form.getFirst("password");
                String confirmation = form.getFirst("password_confirmation");
                if (password == null || password.length() < 8 || password.length() > 128 || !password.equals(confirmation)) {
                    render(context, mode, "pinvouPasswordInvalid");
                    return;
                }
                Map<String, Object> reference = CLIENT.precheckPassword(password);
                String handle = PinvouAuthClient.string(reference.get("password_handle"));
                String policy = PinvouAuthClient.string(reference.get("password_policy_version"));
                CLIENT.precheckRegistration(username, mobile, handle, policy);
                put(context, "password_handle", handle);
                put(context, "password_policy", policy);
            }
            put(context, "username", username);
        }
        Map<String, Object> captcha = CLIENT.createCaptcha(mobile, purpose(mode));
        put(context, "captcha_id", PinvouAuthClient.string(captcha.get("captcha_id")));
        put(context, "captcha_background", PinvouAuthClient.string(captcha.get("background_image")));
        put(context, "captcha_piece", PinvouAuthClient.string(captcha.get("piece_image")));
        put(context, "captcha_width", PinvouAuthClient.string(captcha.get("width")));
        put(context, "captcha_height", PinvouAuthClient.string(captcha.get("height")));
        put(context, "captcha_piece_width", PinvouAuthClient.string(captcha.get("piece_width")));
        put(context, "captcha_piece_height", PinvouAuthClient.string(captcha.get("piece_height")));
        put(context, "captcha_piece_y", PinvouAuthClient.string(captcha.get("piece_y")));
        render(context, mode, "");
    }

    private void sendSMS(AuthenticationFlowContext context, MultivaluedMap<String, String> form, String mode)
        throws PinvouAuthClient.AuthClientException {
        String mobile = get(context, "mobile");
        String captchaID = get(context, "captcha_id");
        int offset;
        try { offset = Integer.parseInt(trim(form.getFirst("captcha_offset"))); }
        catch (NumberFormatException error) { offset = -1; }
        if (mobile.isEmpty() || captchaID.isEmpty() || offset < 0) {
            clearCaptcha(context);
            render(context, mode, "pinvouCaptchaInvalid");
            return;
        }
        Map<String, Object> request = new LinkedHashMap<>();
        request.put("username", "register".equals(mode) ? get(context, "username") : "");
        request.put("mobile", mobile);
        request.put("password_handle", "register".equals(mode) ? get(context, "password_handle") : "");
        request.put("password_policy_version", "register".equals(mode) ? get(context, "password_policy") : "");
        request.put("purpose", purpose(mode));
        request.put("captcha_id", captchaID);
        request.put("captcha_offset", offset);
        Map<String, Object> response = CLIENT.sendSMS(request, source(context), get(context, "device"));
        put(context, "sms_challenge", PinvouAuthClient.string(response.get("challenge_id")));
        put(context, "sms_sent", "true");
        put(context, "retry_after", PinvouAuthClient.string(response.getOrDefault("retry_after_seconds", "60")));
        if ("register".equals(mode)) {
            put(context, "registration_operation", PinvouAuthClient.string(response.get("registration_operation_id")));
            put(context, "registration_token", PinvouAuthClient.string(response.get("registration_query_token")));
        }
        clearCaptcha(context);
        render(context, mode, "pinvouCodeSent");
    }

    private void submit(AuthenticationFlowContext context, MultivaluedMap<String, String> form, String mode)
        throws PinvouAuthClient.AuthClientException {
        if ("password".equals(mode)) {
            authenticatePassword(context, form);
            return;
        }
        String mobile = get(context, "mobile");
        String challenge = get(context, "sms_challenge");
        String code = trim(form.getFirst("sms_code"));
        if (mobile.isEmpty() || challenge.isEmpty() || !CODE.matcher(code).matches()) {
            render(context, mode, "pinvouCodeInvalid");
            return;
        }
        if ("register".equals(mode)) {
            Map<String, Object> request = new LinkedHashMap<>();
            request.put("username", get(context, "username"));
            request.put("mobile", mobile);
            request.put("challenge_id", challenge);
            request.put("code", code);
            request.put("password_handle", get(context, "password_handle"));
            request.put("password_policy_version", get(context, "password_policy"));
            request.put("registration_operation_id", get(context, "registration_operation"));
            request.put("registration_query_token", get(context, "registration_token"));
            Map<String, Object> result = CLIENT.register(request, source(context), get(context, "device"));
            String operationID = get(context, "registration_operation");
            UserModel user = findRegistrationUser(context, operationID);
            PinvouAuthClient.LoginAuthorization authorization = user == null
                ? new PinvouAuthClient.LoginAuthorization(false, "")
                : CLIENT.authorizeLogin(user.getId());
            if (!RegistrationLoginPolicy.allows(
                PinvouAuthClient.string(result.get("status")),
                operationID,
                PinvouAuthClient.string(result.get("registration_operation_id")),
                PinvouAuthClient.string(result.get("user_id")),
                authorization.accountUserID(),
                authorization.allowed(),
                user != null,
                user != null && user.isEnabled()
            )) {
                clearState(context);
                render(context, "password", "pinvouRegistrationRecovering");
                return;
            }
            complete(context, user);
            return;
        }
        String subject = CLIENT.verifySMS(mobile, challenge, code, source(context), get(context, "device"));
        UserModel user = context.getSession().users().getUserById(context.getRealm(), subject);
        if (user == null || !user.isEnabled()) {
            credentialFailure(context, mode);
            return;
        }
        succeed(context, user);
    }

    private void authenticatePassword(AuthenticationFlowContext context, MultivaluedMap<String, String> form)
        throws PinvouAuthClient.AuthClientException {
        String login = trim(form.getFirst("username"));
        String password = form.getFirst("password");
        UserModel user = findUser(context, login);
        if (user == null) AuthenticatorUtils.dummyHash(context);
        boolean allowed = PasswordLoginGate.allows(
            password,
            user != null,
            candidate -> user != null && user.credentialManager().isValid(UserCredentialModel.password(candidate)),
            () -> user.isEnabled(),
            () -> AuthenticatorUtils.getDisabledByBruteForceEventError(context, user) != null,
            () -> CLIENT.authorizeLogin(user.getId())
        );
        if (!allowed) {
            credentialFailure(context, "password");
            return;
        }
        complete(context, user);
    }

    private UserModel findRegistrationUser(AuthenticationFlowContext context, String operationID) {
        if (operationID.isBlank()) return null;
        try (Stream<UserModel> users = context.getSession().users()
            .searchForUserByUserAttributeStream(context.getRealm(), REGISTRATION_OPERATION_ATTRIBUTE, operationID)) {
            List<UserModel> candidates = users
                .filter(user -> operationID.equals(user.getFirstAttribute(REGISTRATION_OPERATION_ATTRIBUTE)))
                .limit(2)
                .toList();
            return RegistrationUserSelector.uniqueExactMatch(
                candidates,
                operationID,
                user -> user.getFirstAttribute(REGISTRATION_OPERATION_ATTRIBUTE)
            );
        }
    }

    private UserModel findUser(AuthenticationFlowContext context, String login) {
        String mobile = normalizeMobile(login);
        if (mobile.isEmpty()) return login.isEmpty() ? null : KeycloakModelUtils.findUserByNameOrEmail(context.getSession(), context.getRealm(), login);
        try (Stream<UserModel> users = context.getSession().users()
            .searchForUserByUserAttributeStream(context.getRealm(), "phone_number", mobile)) {
            return users.findFirst().orElse(null);
        }
    }

    private void succeed(AuthenticationFlowContext context, UserModel user) throws PinvouAuthClient.AuthClientException {
        if (!CLIENT.authorizeLogin(user.getId()).allowed()) {
            clearState(context);
            credentialFailure(context, "password");
            return;
        }
        complete(context, user);
    }

    private void complete(AuthenticationFlowContext context, UserModel user) {
        clearState(context);
        context.setUser(user);
        context.getEvent().user(user);
        context.success();
    }

    private void credentialFailure(AuthenticationFlowContext context, String mode) {
        context.getEvent().error(Errors.INVALID_USER_CREDENTIALS);
        Response response = form(context, mode, "pinvouInvalidCredentials").createForm(TEMPLATE);
        context.failureChallenge(AuthenticationFlowError.INVALID_CREDENTIALS, response);
    }

    private void render(AuthenticationFlowContext context, String mode, String message) {
        context.challenge(form(context, mode, message).createForm(TEMPLATE));
    }

    private LoginFormsProvider form(AuthenticationFlowContext context, String mode, String message) {
        LoginFormsProvider form = context.form()
            .setAttribute("pinvouMode", mode)
            .setAttribute("pinvouUsername", get(context, "username"))
            .setAttribute("pinvouMobile", nationalMobile(get(context, "mobile")))
            .setAttribute("pinvouPasswordAccepted", !get(context, "password_handle").isEmpty())
            .setAttribute("pinvouSmsSent", "true".equals(get(context, "sms_sent")))
            .setAttribute("pinvouRetryAfter", get(context, "retry_after"))
            .setAttribute("pinvouCaptchaId", get(context, "captcha_id"))
            .setAttribute("pinvouCaptchaBackground", get(context, "captcha_background"))
            .setAttribute("pinvouCaptchaPiece", get(context, "captcha_piece"))
            .setAttribute("pinvouCaptchaWidth", get(context, "captcha_width"))
            .setAttribute("pinvouCaptchaHeight", get(context, "captcha_height"))
            .setAttribute("pinvouCaptchaPieceWidth", get(context, "captcha_piece_width"))
            .setAttribute("pinvouCaptchaPieceHeight", get(context, "captcha_piece_height"))
            .setAttribute("pinvouCaptchaPieceY", get(context, "captcha_piece_y"));
        if (message == null || message.isBlank()) return form;
        if ("pinvouCodeSent".equals(message) || "pinvouRegistrationRecovering".equals(message)) return form.setInfo(message);
        return form.setError(message);
    }

    private static String messageFor(String code) {
        return switch (code) {
            case "MOBILE_INVALID" -> "pinvouMobileInvalid";
            case "USERNAME_INVALID" -> "pinvouUsernameInvalid";
            case "USERNAME_ALREADY_REGISTERED", "USERNAME_TAKEN" -> "pinvouUsernameTaken";
            case "MOBILE_ALREADY_REGISTERED", "PHONE_IN_USE" -> "pinvouMobileTaken";
            case "CAPTCHA_INVALID" -> "pinvouCaptchaInvalid";
            case "SMS_RATE_LIMITED" -> "pinvouSmsRateLimited";
            case "SMS_CODE_INVALID" -> "pinvouCodeInvalid";
            case "PASSWORD_POLICY_VIOLATION", "PASSWORD_REFERENCE_INVALID", "PASSWORD_REFERENCE_CONFLICT" -> "pinvouPasswordInvalid";
            case "REGISTRATION_RESTART_REQUIRED" -> "pinvouRegistrationRestart";
            default -> "pinvouServiceUnavailable";
        };
    }

    private static String source(AuthenticationFlowContext context) {
        String forwarded = trim(context.getHttpRequest().getHttpHeaders().getHeaderString("X-Forwarded-For"));
        if (!forwarded.isEmpty()) return forwarded.split(",", 2)[0].trim();
        String remote = context.getSession().getContext().getConnection().getRemoteAddr();
        return remote == null || remote.isBlank() ? "source:unknown" : remote.trim();
    }

    private static String normalizeDevice(String value) {
        value = trim(value);
        return DEVICE.matcher(value).matches() ? value : "web-device-absent";
    }

    private static String purpose(String mode) { return "register".equals(mode) ? "REGISTER" : "LOGIN"; }
    private static String safeMode(String value) { return "sms".equals(value) || "register".equals(value) ? value : "password"; }
    private static String trim(String value) { return value == null ? "" : value.trim(); }
    private static String normalizeMobile(String value) {
        String national = trim(value);
        if (national.startsWith("+86")) national = national.substring(3);
        else if (national.startsWith("86")) national = national.substring(2);
        return MOBILE.matcher(national).matches() ? "+86" + national : "";
    }
    private static String nationalMobile(String value) { return value != null && value.startsWith("+86") ? value.substring(3) : trim(value); }
    private static String note(String key) { return NOTE_PREFIX + key; }
    private static String get(AuthenticationFlowContext context, String key) {
        String value = context.getAuthenticationSession().getAuthNote(note(key));
        return value == null ? "" : value;
    }
    private static void put(AuthenticationFlowContext context, String key, String value) {
        context.getAuthenticationSession().setAuthNote(note(key), value == null ? "" : value);
    }
    private static String mode(AuthenticationFlowContext context) { return safeMode(get(context, "mode")); }
    private static void clearCaptcha(AuthenticationFlowContext context) {
        for (String key : new String[]{"captcha_id", "captcha_background", "captcha_piece", "captcha_width", "captcha_height", "captcha_piece_width", "captcha_piece_height", "captcha_piece_y"}) {
            context.getAuthenticationSession().removeAuthNote(note(key));
        }
    }
    private static void clearState(AuthenticationFlowContext context) {
        for (String key : new String[]{"mode", "username", "mobile", "device", "password_handle", "password_policy", "sms_challenge", "sms_sent", "retry_after", "registration_operation", "registration_token"}) {
            context.getAuthenticationSession().removeAuthNote(note(key));
        }
        clearCaptcha(context);
    }

    @Override public boolean requiresUser() { return false; }
    @Override public boolean configuredFor(KeycloakSession session, RealmModel realm, UserModel user) { return true; }
    @Override public void setRequiredActions(KeycloakSession session, RealmModel realm, UserModel user) {}
    @Override public void close() {}
}
