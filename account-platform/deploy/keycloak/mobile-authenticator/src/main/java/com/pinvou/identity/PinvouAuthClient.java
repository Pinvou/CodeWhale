package com.pinvou.identity;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.core.JsonParser;
import com.fasterxml.jackson.databind.DeserializationFeature;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionStage;
import java.util.concurrent.Flow;
import java.util.concurrent.TimeUnit;
import java.util.UUID;
import java.util.function.Function;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.Collections;
import java.util.Map;

final class PinvouAuthClient {
    private static final ObjectMapper JSON = new ObjectMapper()
        .enable(JsonParser.Feature.STRICT_DUPLICATE_DETECTION)
        .enable(DeserializationFeature.FAIL_ON_TRAILING_TOKENS);
    private static final TypeReference<Map<String, Object>> OBJECT_MAP = new TypeReference<>() {};
    private final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3))
        .followRedirects(HttpClient.Redirect.NEVER).build();
    private final Function<String, String> environment;

    PinvouAuthClient() { this(System::getenv); }
    PinvouAuthClient(Function<String, String> environment) { this.environment = environment; }

    Map<String, Object> precheckPassword(String password, String authorization, String source, String device) throws AuthClientException {
        Map<String, Object> reference = post(publicURL("PINVOU_IDENTITY_AUTH_URL", "/v1/registration/password-prechecks"),
            Map.of("password", password), Map.of("X-Pinvou-Password-Preparation", authorization, "X-Pinvou-Client-Source", source, "X-Device-ID", device), 201).body();
        if (string(reference.get("password_handle")).isEmpty() || string(reference.get("password_policy_version")).isEmpty()) {
            throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        }
        return reference;
    }

    Map<String, Object> authorizePasswordPreparation(String operation, String mobile, String purpose, String captcha, int offset, String source, String device) throws AuthClientException {
        return post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/v1/auth/password-preparation-authorizations"),
            Map.of("operation_id", operation, "mobile", mobile, "purpose", purpose, "captcha_id", captcha, "captcha_offset", offset),
            Map.of("X-Pinvou-Client-Source", source, "X-Device-ID", device), 201).body();
    }

    Map<String, Object> createCaptcha(String mobile, String purpose, String source, String device) throws AuthClientException {
        return post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/v1/auth/captcha/challenges"),
            Map.of("mobile", mobile, "purpose", purpose), Map.of("X-Pinvou-Client-Source", source, "X-Device-ID", device), 201).body();
    }

    Map<String, Object> sendSMS(Map<String, Object> body, String source, String device) throws AuthClientException {
        return post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/v1/auth/sms/challenges"), body,
            Map.of("X-Pinvou-Client-Source", source, "X-Device-ID", device), 202).body();
    }

    String verifySMS(String mobile, String challenge, String code, String source, String device) throws AuthClientException {
        Response result = post(requiredURL("PINVOU_SMS_VERIFY_URL"),
            Map.of("mobile", mobile, "challenge_id", challenge, "code", code),
            Map.of("Authorization", "Bearer " + workloadToken(), "X-Pinvou-Client-Source", source, "X-Device-ID", device), 204);
        String subject = result.header("X-Pinvou-Identity-Subject");
        if (result.status() != 204 || !subject.matches("[A-Za-z0-9._:-]{1,128}")) throw new AuthClientException(401, "SMS_CODE_INVALID");
        return subject;
    }

    LoginAuthorization authorizeLogin(String subject) throws AuthClientException {
        String issuer = requiredValue("PINVOU_OIDC_ISSUER");
        Response result = post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/internal/v1/authorization/check"), Map.of(
            "issuer", issuer,
            "subject", subject,
            "permission", "personal.session.login"
        ), Map.of("Authorization", "Bearer " + workloadToken()), 200);
        String userID = string(result.body().get("user_id"));
        boolean allowed = Boolean.TRUE.equals(result.body().get("allowed"))
            && "personal.session.login".equals(string(result.body().get("permission")))
            && "PERSONAL".equals(string(result.body().get("permission_scope")))
            && nonzeroUUID(userID);
        return new LoginAuthorization(allowed, userID);
    }

    Map<String, Object> register(Map<String, Object> body, String source, String device) throws AuthClientException {
        return post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/v1/auth/register"), body,
            Map.of("X-Pinvou-Client-Source", source, "X-Device-ID", device), 201, 202).body();
    }

    void resetPassword(Map<String, Object> body, String source, String device) throws AuthClientException {
        post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/v1/auth/password-resets"), body,
            Map.of("X-Pinvou-Client-Source", source, "X-Device-ID", device), 204);
    }

    private Response post(URI target, Map<String, Object> body, Map<String, String> headers, int... expectedStatuses) throws AuthClientException {
        try {
            HttpRequest.Builder request = HttpRequest.newBuilder(target).timeout(Duration.ofSeconds(8))
                .header("Content-Type", "application/json").header("Accept", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(JSON.writeValueAsString(body), StandardCharsets.UTF_8));
            headers.forEach(request::header);
            // The deadline covers receipt of the complete bounded body, not
            // only response headers. Cancellation closes a stalled exchange.
            CompletableFuture<HttpResponse<byte[]>> pending = http.sendAsync(
                request.build(), ignored -> new BoundedBodySubscriber());
            HttpResponse<byte[]> response;
            try {
                response = pending.get(8, TimeUnit.SECONDS);
            } finally {
                if (!pending.isDone()) pending.cancel(true);
            }
            byte[] bytes = response.body();
            Map<String, Object> payload = bytes.length == 0 ? Collections.emptyMap() : JSON.readValue(bytes, OBJECT_MAP);
            if (response.statusCode() < 200 || response.statusCode() >= 300) {
                throw new AuthClientException(response.statusCode(), string(payload.get("code")), boundedRetryAfter(response.headers().firstValue("Retry-After").orElse("")));
            }
            if (java.util.Arrays.stream(expectedStatuses).noneMatch(status -> status == response.statusCode())) {
                throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
            }
            return new Response(response.statusCode(), payload, response.headers().map());
        } catch (AuthClientException error) {
            throw error;
        } catch (InterruptedException error) {
            Thread.currentThread().interrupt();
            throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        } catch (Exception error) {
            throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        }
    }

    private static int boundedRetryAfter(String value) {
        try {
            long seconds=value.matches("[0-9]{1,18}") ? Long.parseLong(value)
                : java.time.Duration.between(java.time.Instant.now(),java.time.ZonedDateTime.parse(value,java.time.format.DateTimeFormatter.RFC_1123_DATE_TIME).toInstant()).getSeconds();
            return (int)Math.min(86400,Math.max(0,seconds));
        } catch (RuntimeException ignored) { return 0; }
    }

    private static final class BoundedBodySubscriber implements HttpResponse.BodySubscriber<byte[]> {
        private static final int MAX_BYTES = 65_536;
        private final ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        private final CompletableFuture<byte[]> body = new CompletableFuture<>();
        private Flow.Subscription subscription;

        @Override public CompletionStage<byte[]> getBody() { return body; }
        @Override public void onSubscribe(Flow.Subscription incoming) {
            if (subscription != null) { incoming.cancel(); return; }
            subscription = incoming;
            incoming.request(1);
        }
        @Override public void onNext(List<ByteBuffer> buffers) {
            if (body.isDone()) return;
            for (ByteBuffer buffer : buffers) {
                if (buffer.remaining() > MAX_BYTES - bytes.size()) {
                    subscription.cancel();
                    body.completeExceptionally(new IOException("AUTH_SERVICE_UNAVAILABLE"));
                    return;
                }
                byte[] part = new byte[buffer.remaining()];
                buffer.get(part);
                bytes.writeBytes(part);
            }
            subscription.request(1);
        }
        @Override public void onError(Throwable error) { body.completeExceptionally(error); }
        @Override public void onComplete() { body.complete(bytes.toByteArray()); }
    }

    private URI publicURL(String variable, String fallbackPath) throws AuthClientException {
        String configured = environment.apply(variable);
        if (configured == null || configured.isBlank()) throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        String value = configured.trim();
        validatedURL(value);
        return validatedURL(value + (value.endsWith("/") ? fallbackPath.substring(1) : fallbackPath));
    }

    private URI requiredURL(String variable) throws AuthClientException {
        return validatedURL(requiredValue(variable));
    }

    private String requiredValue(String variable) throws AuthClientException {
        String configured = environment.apply(variable);
        if (configured == null || configured.isBlank()) throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        return configured.trim();
    }

    private String workloadToken() throws AuthClientException {
        try {
            String file = environment.apply("PINVOU_WORKLOAD_IDENTITY_TOKEN_FILE");
            String token = file != null && !file.isBlank()
                ? Files.readString(Path.of(file), StandardCharsets.UTF_8).trim()
                : string(environment.apply("PINVOU_INTERNAL_SERVICE_TOKEN")).trim();
            if (token.isBlank()) throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
            return token;
        } catch (AuthClientException error) {
            throw error;
        } catch (Exception error) {
            throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        }
    }

    static URI validatedURL(String value) throws AuthClientException {
        try {
            URI uri = URI.create(value);
            if (!("http".equals(uri.getScheme()) || "https".equals(uri.getScheme())) || uri.getHost() == null
                || uri.getRawUserInfo() != null || uri.getRawQuery() != null || uri.getRawFragment() != null) {
                throw new IllegalArgumentException();
            }
            return uri;
        } catch (IllegalArgumentException error) {
            throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        }
    }

    static boolean nonzeroUUID(String value) {
        if (value == null || !value.matches("[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}")) return false;
        return !UUID.fromString(value).equals(new UUID(0, 0));
    }

    static String string(Object value) { return value == null ? "" : String.valueOf(value); }

    record LoginAuthorization(boolean allowed, String accountUserID) {}

    record Response(int status, Map<String, Object> body, Map<String, java.util.List<String>> headers) {
        String header(String name) {
            return headers.entrySet().stream().filter(item -> item.getKey().equalsIgnoreCase(name))
                .flatMap(item -> item.getValue().stream()).findFirst().orElse("");
        }
    }

    static final class AuthClientException extends Exception {
        private final int status;
        private final String code;
        private final int retryAfterSeconds;
        AuthClientException(int status, String code) { this(status,code,0); }
        AuthClientException(int status, String code, int retryAfterSeconds) {
            super(safeErrorCode(code));
            this.retryAfterSeconds=Math.min(86400,Math.max(0,retryAfterSeconds));
            this.status = status;
            this.code = safeErrorCode(code);
        }
        private static String safeErrorCode(String code) {
            if (code == null) return "AUTH_REQUEST_FAILED";
            return switch (code) {
                case "AUTH_SERVICE_UNAVAILABLE", "SMS_CODE_INVALID", "MOBILE_INVALID", "USERNAME_INVALID",
                    "USERNAME_ALREADY_REGISTERED", "USERNAME_TAKEN", "MOBILE_ALREADY_REGISTERED", "PHONE_IN_USE",
                    "CAPTCHA_INVALID", "PASSWORD_PREPARATION_INVALID", "PASSWORD_PREPARATION_UNAVAILABLE", "PASSWORD_PRECHECK_CAPACITY_REACHED", "RATE_LIMITED", "SMS_RATE_LIMITED", "PASSWORD_POLICY_VIOLATION", "PASSWORD_INVALID",
                    "PASSWORD_EDGE_SPACE_NOT_ALLOWED", "PASSWORD_REFERENCE_INVALID", "PASSWORD_REFERENCE_CONFLICT",
                    "REGISTRATION_RESTART_REQUIRED" -> code;
                default -> "AUTH_REQUEST_FAILED";
            };
        }
        int retryAfterSeconds() { return retryAfterSeconds; }
        int status() { return status; }
        String code() { return code; }
    }
}
