package com.pinvou.identity;

import com.fasterxml.jackson.core.type.TypeReference;
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
    private static final ObjectMapper JSON = new ObjectMapper();
    private static final TypeReference<Map<String, Object>> OBJECT_MAP = new TypeReference<>() {};
    private final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).build();

    Map<String, Object> precheckPassword(String password) throws AuthClientException {
        return post(publicURL("PINVOU_IDENTITY_AUTH_URL", "/v1/registration/password-prechecks"),
            Map.of("password", password), Map.of()).body();
    }

    void precheckRegistration(String username, String mobile, String handle, String policy) throws AuthClientException {
        post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/v1/auth/registration/precheck"), Map.of(
            "username", username, "mobile", mobile, "password_handle", handle, "password_policy_version", policy), Map.of());
    }

    Map<String, Object> createCaptcha(String mobile, String purpose) throws AuthClientException {
        return post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/v1/auth/captcha/challenges"),
            Map.of("mobile", mobile, "purpose", purpose), Map.of()).body();
    }

    Map<String, Object> sendSMS(Map<String, Object> body, String source, String device) throws AuthClientException {
        return post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/v1/auth/sms/challenges"), body,
            Map.of("X-Pinvou-Client-Source", source, "X-Device-ID", device)).body();
    }

    String verifySMS(String mobile, String challenge, String code, String source, String device) throws AuthClientException {
        Response result = post(requiredURL("PINVOU_SMS_VERIFY_URL"),
            Map.of("mobile", mobile, "challenge_id", challenge, "code", code),
            Map.of("Authorization", "Bearer " + workloadToken(), "X-Pinvou-Client-Source", source, "X-Device-ID", device));
        String subject = result.header("X-Pinvou-Identity-Subject");
        if (subject.isBlank()) throw new AuthClientException(401, "SMS_CODE_INVALID");
        return subject;
    }

    Map<String, Object> register(Map<String, Object> body, String source, String device) throws AuthClientException {
        return post(publicURL("PINVOU_ACCOUNT_AUTH_URL", "/v1/auth/register"), body,
            Map.of("X-Pinvou-Client-Source", source, "X-Device-ID", device)).body();
    }

    private Response post(URI target, Map<String, Object> body, Map<String, String> headers) throws AuthClientException {
        try {
            HttpRequest.Builder request = HttpRequest.newBuilder(target).timeout(Duration.ofSeconds(8))
                .header("Content-Type", "application/json").header("Accept", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(JSON.writeValueAsString(body), StandardCharsets.UTF_8));
            headers.forEach(request::header);
            HttpResponse<String> response = http.send(request.build(), HttpResponse.BodyHandlers.ofString(StandardCharsets.UTF_8));
            Map<String, Object> payload = response.body().isBlank() ? Collections.emptyMap() : JSON.readValue(response.body(), OBJECT_MAP);
            if (response.statusCode() < 200 || response.statusCode() >= 300) {
                throw new AuthClientException(response.statusCode(), string(payload.get("code")));
            }
            return new Response(payload, response.headers().map());
        } catch (AuthClientException error) {
            throw error;
        } catch (Exception error) {
            throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        }
    }

    private static URI publicURL(String variable, String fallbackPath) throws AuthClientException {
        String configured = System.getenv(variable);
        if (configured == null || configured.isBlank()) throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        String value = configured.trim();
        return URI.create(value.endsWith("/") ? value.substring(0, value.length() - 1) + fallbackPath : value + fallbackPath);
    }

    private static URI requiredURL(String variable) throws AuthClientException {
        String configured = System.getenv(variable);
        if (configured == null || configured.isBlank()) throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        return URI.create(configured.trim());
    }

    private static String workloadToken() throws AuthClientException {
        try {
            String file = System.getenv("PINVOU_WORKLOAD_IDENTITY_TOKEN_FILE");
            String token = file != null && !file.isBlank()
                ? Files.readString(Path.of(file), StandardCharsets.UTF_8).trim()
                : string(System.getenv("PINVOU_INTERNAL_SERVICE_TOKEN")).trim();
            if (token.isBlank()) throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
            return token;
        } catch (AuthClientException error) {
            throw error;
        } catch (Exception error) {
            throw new AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE");
        }
    }

    static String string(Object value) { return value == null ? "" : String.valueOf(value); }

    record Response(Map<String, Object> body, Map<String, java.util.List<String>> headers) {
        String header(String name) {
            return headers.entrySet().stream().filter(item -> item.getKey().equalsIgnoreCase(name))
                .flatMap(item -> item.getValue().stream()).findFirst().orElse("");
        }
    }

    static final class AuthClientException extends Exception {
        private final int status;
        private final String code;
        AuthClientException(int status, String code) {
            super(code == null || code.isBlank() ? "AUTH_REQUEST_FAILED" : code);
            this.status = status;
            this.code = code == null || code.isBlank() ? "AUTH_REQUEST_FAILED" : code;
        }
        int status() { return status; }
        String code() { return code; }
    }
}
