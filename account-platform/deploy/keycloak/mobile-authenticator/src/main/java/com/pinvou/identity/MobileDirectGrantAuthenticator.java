package com.pinvou.identity;

import jakarta.ws.rs.core.MediaType;
import jakarta.ws.rs.core.MultivaluedMap;
import jakarta.ws.rs.core.Response;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Map;
import java.util.Optional;
import java.util.regex.Pattern;
import java.util.stream.Stream;
import org.jboss.logging.Logger;
import org.keycloak.authentication.AuthenticationFlowContext;
import org.keycloak.authentication.AuthenticationFlowError;
import org.keycloak.authentication.Authenticator;
import org.keycloak.authentication.authenticators.util.AuthenticatorUtils;
import org.keycloak.events.Errors;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserCredentialModel;
import org.keycloak.models.UserModel;
import org.keycloak.models.utils.KeycloakModelUtils;

public final class MobileDirectGrantAuthenticator implements Authenticator {
    private static final Logger LOG = Logger.getLogger(MobileDirectGrantAuthenticator.class);
    private static final Pattern MOBILE = Pattern.compile("^1[3-9][0-9]{9}$");
    private static final Pattern CHALLENGE = Pattern.compile("^[0-9a-fA-F-]{36}$");
    private static final Pattern CODE = Pattern.compile("^[0-9]{6}$");
    private static final Pattern SUBJECT = Pattern.compile("^[A-Za-z0-9._:-]{1,128}$");
    private static final HttpClient HTTP = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).build();

    @Override
    public void authenticate(AuthenticationFlowContext context) {
        MultivaluedMap<String, String> form = context.getHttpRequest().getDecodedFormParameters();
        String login = trim(form.getFirst("username"));
        String mobile = normalizeMobile(login);
        String challenge = trim(form.getFirst("sms_challenge_id"));
        String code = trim(form.getFirst("sms_code"));
        boolean smsLogin = !challenge.isEmpty() || !code.isEmpty();
        UserModel user;
        boolean valid;
        if (smsLogin) {
            Optional<String> subject = !mobile.isEmpty() && CHALLENGE.matcher(challenge).matches()
                && CODE.matcher(code).matches() ? verifySMS(mobile, challenge, code) : Optional.empty();
            user = subject.map(value -> context.getSession().users().getUserById(context.getRealm(), value)).orElse(null);
            valid = user != null;
        } else {
            user = findUser(context, login, mobile);
            String password = form.getFirst("password");
            valid = user != null && password != null && !password.isEmpty()
                && user.credentialManager().isValid(UserCredentialModel.password(password));
        }
        if (user == null) {
            AuthenticatorUtils.dummyHash(context);
            fail(context, AuthenticationFlowError.INVALID_USER);
            return;
        }
        if (!user.isEnabled() || AuthenticatorUtils.getDisabledByBruteForceEventError(context, user) != null) {
            fail(context, AuthenticationFlowError.USER_DISABLED);
            return;
        }

        if (!valid) {
            fail(context, AuthenticationFlowError.INVALID_CREDENTIALS);
            return;
        }
        context.setUser(user);
        context.getEvent().user(user);
        context.success();
    }

    private UserModel findUser(AuthenticationFlowContext context, String login, String mobile) {
        if (login.isEmpty()) return null;
        if (mobile.isEmpty()) {
            return KeycloakModelUtils.findUserByNameOrEmail(context.getSession(), context.getRealm(), login);
        }
        try (Stream<UserModel> users = context.getSession().users()
                .searchForUserByUserAttributeStream(context.getRealm(), "phone_number", mobile)) {
            return users.findFirst().orElse(null);
        }
    }

    private Optional<String> verifySMS(String mobile, String challenge, String code) {
        String endpoint = System.getenv("PINVOU_SMS_VERIFY_URL");
        String serviceToken = System.getenv("PINVOU_INTERNAL_SERVICE_TOKEN");
        if (endpoint == null || serviceToken == null || endpoint.isBlank() || serviceToken.length() < 32) {
            LOG.error("SMS verification endpoint is not configured");
            return Optional.empty();
        }
        String body = "{\"mobile\":\"" + mobile + "\",\"challenge_id\":\"" + challenge + "\",\"code\":\"" + code + "\"}";
        try {
            HttpRequest request = HttpRequest.newBuilder(URI.create(endpoint))
                .timeout(Duration.ofSeconds(5)).header("Content-Type", "application/json")
                .header("Authorization", "Bearer " + serviceToken)
                .POST(HttpRequest.BodyPublishers.ofString(body, StandardCharsets.UTF_8)).build();
            HttpResponse<Void> response = HTTP.send(request, HttpResponse.BodyHandlers.discarding());
            String subject = response.headers().firstValue("X-Pinvou-Identity-Subject").orElse("");
            if (response.statusCode() == 204 && SUBJECT.matcher(subject).matches()) return Optional.of(subject);
            LOG.warnf("SMS verification was rejected by the account service with HTTP %d", response.statusCode());
            return Optional.empty();
        } catch (Exception error) {
            LOG.warnf("SMS verification request failed: %s", error.getClass().getSimpleName());
            return Optional.empty();
        }
    }

    private static String normalizeMobile(String value) {
        String national = trim(value);
        if (national.startsWith("+86")) national = national.substring(3);
        else if (national.startsWith("86")) national = national.substring(2);
        return MOBILE.matcher(national).matches() ? "+86" + national : "";
    }

    private void fail(AuthenticationFlowContext context, AuthenticationFlowError error) {
        context.getEvent().error(Errors.INVALID_USER_CREDENTIALS);
        Response response = Response.status(Response.Status.UNAUTHORIZED)
            .type(MediaType.APPLICATION_JSON_TYPE)
            .entity(Map.of("error", "invalid_grant", "error_description", "Invalid user credentials"))
            .build();
        context.failureChallenge(error, response);
    }

    private static String trim(String value) { return value == null ? "" : value.trim(); }
    @Override public void action(AuthenticationFlowContext context) { authenticate(context); }
    @Override public boolean requiresUser() { return false; }
    @Override public boolean configuredFor(KeycloakSession session, RealmModel realm, UserModel user) { return true; }
    @Override public void setRequiredActions(KeycloakSession session, RealmModel realm, UserModel user) {}
    @Override public void close() {}
}
