package com.pinvou.identity;

import jakarta.ws.rs.core.MediaType;
import jakarta.ws.rs.core.MultivaluedMap;
import jakarta.ws.rs.core.Response;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.TimeUnit;
import java.util.function.Function;
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
    private static final PinvouAuthClient DEFAULT_CLIENT = new PinvouAuthClient();
    private final PinvouAuthClient client;
    private final Function<String, String> environment;

    public MobileDirectGrantAuthenticator() { this(DEFAULT_CLIENT); }
    MobileDirectGrantAuthenticator(PinvouAuthClient client) { this(client, System::getenv); }
    MobileDirectGrantAuthenticator(PinvouAuthClient client, Function<String, String> environment) {
        this.client = client;
        this.environment = environment;
    }

    @Override
    public void authenticate(AuthenticationFlowContext context) {
        context.clearUser();
        context.getAuthenticationSession().removeAuthNote("ATTEMPTED_USERNAME");
        MultivaluedMap<String, String> form = context.getHttpRequest().getDecodedFormParameters();
        String login = trim(form.getFirst("username"));
        String mobile = normalizeMobile(login);
        String challenge = trim(form.getFirst("sms_challenge_id"));
        String code = trim(form.getFirst("sms_code"));
        boolean smsLogin = !challenge.isEmpty() || !code.isEmpty();
        UserModel user;
        boolean valid;
        boolean passwordSupplied = false;
        boolean smsBindingRequired = false;
        if (smsLogin) {
            SMSVerification verification = !mobile.isEmpty() && CHALLENGE.matcher(challenge).matches()
                && CODE.matcher(code).matches() ? verifySMS(context, mobile, challenge, code) : SMSVerification.INVALID;
            if (verification.serviceFailure() != null) {
                smsServiceFailure(context, verification.serviceFailure());
                return;
            }
            smsBindingRequired = verification.bindingRequired();
            user = verification.subject() == null ? null : context.getSession().users().getUserById(context.getRealm(), verification.subject());
            valid = user != null;
        } else {
            user = findUser(context, login, mobile);
            String password = form.getFirst("password");
            passwordSupplied = password != null && !password.isEmpty();
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
            if (passwordSupplied) {
                context.setUser(user);
                context.getEvent().user(user);
            }
            fail(context, AuthenticationFlowError.INVALID_CREDENTIALS, passwordSupplied);
            return;
        }
        try {
            PinvouAuthClient.LoginAuthorization authorization = smsBindingRequired
                ? client.authorizeLogin(user.getId(), challenge) : client.authorizeLogin(user.getId());
            if (!authorization.allowed()) {
                fail(context, AuthenticationFlowError.INVALID_CREDENTIALS);
                return;
            }
        } catch (PinvouAuthClient.AuthClientException error) {
            LOG.warnf("Login authorization failed safely: %s", error.code());
            if (smsLogin) smsServiceFailure(context, "");
            else fail(context, AuthenticationFlowError.INVALID_CREDENTIALS);
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

    private record SMSVerification(String subject, boolean bindingRequired, String serviceFailure) {
        private static final SMSVerification INVALID = new SMSVerification(null, false, null);
        private static final SMSVerification UNAVAILABLE = new SMSVerification(null, false, "");
        private static final SMSVerification RECOVERING = new SMSVerification(null, false, "REGISTRATION_RECOVERING");
        private static final SMSVerification MANUAL_REVIEW = new SMSVerification(null, false, "REGISTRATION_MANUAL_REVIEW_REQUIRED");
    }

    private static void smsServiceFailure(AuthenticationFlowContext context, String reason) {
        Map<String, String> body = reason.isEmpty()
            ? Map.of("error", "temporarily_unavailable")
            : Map.of("error", "temporarily_unavailable", "error_description", reason);
        context.challenge(Response.status(Response.Status.SERVICE_UNAVAILABLE)
            .type(MediaType.APPLICATION_JSON_TYPE).entity(body).build());
    }

    private SMSVerification verifySMS(AuthenticationFlowContext context, String mobile, String challenge, String code) {
        String endpoint = environment.apply("PINVOU_SMS_VERIFY_URL");
        String tokenFile = environment.apply("PINVOU_WORKLOAD_IDENTITY_TOKEN_FILE");
        String developmentToken = environment.apply("PINVOU_INTERNAL_SERVICE_TOKEN");
        if (endpoint == null || endpoint.isBlank() || ((tokenFile == null || tokenFile.isBlank()) && (developmentToken == null || developmentToken.isBlank()))) {
            LOG.error("SMS verification endpoint is not configured");
            return SMSVerification.UNAVAILABLE;
        }
        String body = "{\"mobile\":\"" + mobile + "\",\"challenge_id\":\"" + challenge + "\",\"code\":\"" + code + "\"}";
        CompletableFuture<HttpResponse<Void>> pending = null;
        try {
            String serviceToken = tokenFile != null && !tokenFile.isBlank()
                ? Files.readString(Path.of(tokenFile), StandardCharsets.UTF_8).trim()
                : developmentToken.trim();
            if (serviceToken.isEmpty()) throw new IllegalStateException("projected workload token is empty");
            String source = ClientRiskSource.from(context);
            String device = boundedHeader(context, "X-Device-ID", "device:absent:" + source);
            HttpRequest request = HttpRequest.newBuilder(PinvouAuthClient.validatedURL(endpoint.trim()))
                .timeout(Duration.ofSeconds(5)).header("Content-Type", "application/json")
                .header("Authorization", "Bearer " + serviceToken)
                .header("X-Pinvou-Client-Source", source).header("X-Device-ID", device)
                .header("X-Pinvou-SMS-Binding", "v1")
                .POST(HttpRequest.BodyPublishers.ofString(body, StandardCharsets.UTF_8)).build();
            pending = HTTP.sendAsync(request, HttpResponse.BodyHandlers.discarding());
            // The deadline includes the response body, not only its headers.
            HttpResponse<Void> response = pending.get(5, TimeUnit.SECONDS);
            String subject = response.headers().firstValue("X-Pinvou-Identity-Subject").orElse("");
            if (response.statusCode() == 204 && SUBJECT.matcher(subject).matches()) {
                return new SMSVerification(subject, response.headers().firstValue("X-Pinvou-SMS-Binding").orElse("").equals("v1"), null);
            }
            if (response.statusCode() == 409) {
                String outcome = response.headers().firstValue("X-Pinvou-SMS-Outcome").orElse("");
                if (SMSVerification.RECOVERING.serviceFailure().equals(outcome)) return SMSVerification.RECOVERING;
                if (SMSVerification.MANUAL_REVIEW.serviceFailure().equals(outcome)) return SMSVerification.MANUAL_REVIEW;
            }
            LOG.warnf("SMS verification was rejected by the account service with HTTP %d", response.statusCode());
            if (response.statusCode() >= 500 || response.statusCode() < 400) return SMSVerification.UNAVAILABLE;
            return SMSVerification.INVALID;
        } catch (InterruptedException error) {
            Thread.currentThread().interrupt();
            LOG.warn("SMS verification request interrupted");
            return SMSVerification.UNAVAILABLE;
        } catch (Exception error) {
            LOG.warnf("SMS verification request failed: %s", error.getClass().getSimpleName());
            return SMSVerification.UNAVAILABLE;
        } finally {
            if (pending != null && !pending.isDone()) pending.cancel(true);
        }
    }

    private static String normalizeMobile(String value) {
        String national = trim(value);
        if (national.startsWith("+86")) national = national.substring(3);
        else if (national.startsWith("86")) national = national.substring(2);
        return MOBILE.matcher(national).matches() ? "+86" + national : "";
    }

    private void fail(AuthenticationFlowContext context, AuthenticationFlowError error) {
        fail(context, error, false);
    }

    private void fail(AuthenticationFlowContext context, AuthenticationFlowError error, boolean countPasswordFailure) {
        context.getEvent().error(Errors.INVALID_USER_CREDENTIALS);
        Response response = Response.status(Response.Status.UNAUTHORIZED)
            .type(MediaType.APPLICATION_JSON_TYPE)
            .entity(Map.of("error", "invalid_grant", "error_description", "Invalid user credentials"))
            .build();
        if (countPasswordFailure) context.failureChallenge(error, response);
        else context.challenge(response);
    }

    private static String trim(String value) { return value == null ? "" : value.trim(); }
    private static String boundedHeader(AuthenticationFlowContext context, String name, String fallback) {
        String value = trim(context.getHttpRequest().getHttpHeaders().getHeaderString(name));
        return value.isEmpty() || value.length() > 128 ? fallback : value;
    }
    @Override public void action(AuthenticationFlowContext context) { authenticate(context); }
    @Override public boolean requiresUser() { return false; }
    @Override public boolean configuredFor(KeycloakSession session, RealmModel realm, UserModel user) { return true; }
    @Override public void setRequiredActions(KeycloakSession session, RealmModel realm, UserModel user) {}
    @Override public void close() {}
}
