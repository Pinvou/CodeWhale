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
import java.util.regex.Pattern;
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
    private static final Pattern MOBILE = Pattern.compile("^1[3-9][0-9]{9}$");
    private static final Pattern CHALLENGE = Pattern.compile("^[0-9a-fA-F-]{36}$");
    private static final Pattern CODE = Pattern.compile("^[0-9]{6}$");
    private static final HttpClient HTTP = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).build();

    @Override
    public void authenticate(AuthenticationFlowContext context) {
        MultivaluedMap<String, String> form = context.getHttpRequest().getDecodedFormParameters();
        String username = trim(form.getFirst("username"));
        UserModel user = username.isEmpty() ? null
            : KeycloakModelUtils.findUserByNameOrEmail(context.getSession(), context.getRealm(), username);
        if (user == null) {
            AuthenticatorUtils.dummyHash(context);
            fail(context, AuthenticationFlowError.INVALID_USER);
            return;
        }
        if (!user.isEnabled() || AuthenticatorUtils.getDisabledByBruteForceEventError(context, user) != null) {
            fail(context, AuthenticationFlowError.USER_DISABLED);
            return;
        }

        String challenge = trim(form.getFirst("sms_challenge_id"));
        String code = trim(form.getFirst("sms_code"));
        boolean valid;
        if (!challenge.isEmpty() || !code.isEmpty()) {
            valid = MOBILE.matcher(username).matches() && CHALLENGE.matcher(challenge).matches()
                && CODE.matcher(code).matches() && verifySMS(username, challenge, code);
        } else {
            String password = form.getFirst("password");
            valid = password != null && !password.isEmpty() && user.credentialManager().isValid(UserCredentialModel.password(password));
        }
        if (!valid) {
            fail(context, AuthenticationFlowError.INVALID_CREDENTIALS);
            return;
        }
        context.setUser(user);
        context.getEvent().user(user);
        context.success();
    }

    private boolean verifySMS(String username, String challenge, String code) {
        String endpoint = System.getenv("PINVOU_SMS_VERIFY_URL");
        String serviceToken = System.getenv("PINVOU_INTERNAL_SERVICE_TOKEN");
        if (endpoint == null || serviceToken == null || endpoint.isBlank() || serviceToken.length() < 32) return false;
        String body = "{\"mobile\":\"+86" + username + "\",\"challenge_id\":\"" + challenge + "\",\"code\":\"" + code + "\"}";
        try {
            HttpRequest request = HttpRequest.newBuilder(URI.create(endpoint))
                .timeout(Duration.ofSeconds(5)).header("Content-Type", "application/json")
                .header("Authorization", "Bearer " + serviceToken)
                .POST(HttpRequest.BodyPublishers.ofString(body, StandardCharsets.UTF_8)).build();
            HttpResponse<Void> response = HTTP.send(request, HttpResponse.BodyHandlers.discarding());
            return response.statusCode() == 204;
        } catch (Exception ignored) {
            return false;
        }
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
