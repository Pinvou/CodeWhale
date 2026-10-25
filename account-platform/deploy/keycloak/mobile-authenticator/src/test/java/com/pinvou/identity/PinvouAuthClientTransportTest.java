package com.pinvou.identity;

import static org.junit.jupiter.api.Assertions.*;
import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.Map;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.Executors;
import org.junit.jupiter.api.Test;

final class PinvouAuthClientTransportTest {
    private static final String USER = "8d16f56e-83ca-4c93-bfcb-797a8076089b";
    @Test void registrationFunnelPreservesEachTrustedSourceAndDeviceOverRealHTTP() throws Exception {
        var received = new java.util.concurrent.CopyOnWriteArrayList<String>();
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            received.add(exchange.getRequestURI().getPath() + "|"
                + exchange.getRequestHeaders().getFirst("X-Pinvou-Client-Source") + "|"
                + exchange.getRequestHeaders().getFirst("X-Device-ID"));
            assertNull(exchange.getRequestHeaders().getFirst("Authorization"), "public bootstrap must not acquire a service credential");
            var payload=new com.fasterxml.jackson.databind.ObjectMapper().readTree(exchange.getRequestBody());
            if (exchange.getRequestURI().getPath().equals("/v1/registration/password-prechecks")) {
                assertEquals("PUBLIC_SYNTHETIC_GRANT",exchange.getRequestHeaders().getFirst("X-Pinvou-Password-Preparation"));
                assertEquals(1,payload.size());assertEquals("fixture-password",payload.get("password").asText());
            } else {
                assertFalse(payload.has("password"),"Account must never receive password");
                assertNull(exchange.getRequestHeaders().getFirst("X-Pinvou-Password-Preparation"));
                if (exchange.getRequestURI().getPath().endsWith("sms/challenges")) {
                    assertEquals("PUBLIC_SYNTHETIC_GRANT", payload.get("password_preparation_token").asText());
                    assertEquals("fixture-handle", payload.get("password_handle").asText());
                    assertEquals("fixture-policy", payload.get("password_policy_version").asText());
                }
            }
            byte[] body = (exchange.getRequestURI().getPath().endsWith("password-prechecks")
                ? "{\"password_handle\":\"fixture-handle\",\"password_policy_version\":\"fixture-policy\"}"
                : exchange.getRequestURI().getPath().endsWith("password-preparation-authorizations")
                ? "{\"authorization_token\":\"PUBLIC_SYNTHETIC_GRANT\"}" : "{}").getBytes(StandardCharsets.UTF_8);
            int status = exchange.getRequestURI().getPath().endsWith("/sms/challenges") ? 202 : 201;
            exchange.sendResponseHeaders(status, body.length);
            exchange.getResponseBody().write(body); exchange.close();
        });
        server.start();
        try {
            String origin = "http://127.0.0.1:" + server.getAddress().getPort();
            var client = new PinvouAuthClient(Map.of("PINVOU_ACCOUNT_AUTH_URL", origin, "PINVOU_IDENTITY_AUTH_URL", origin)::get);
            for (String source : new String[]{"198.51.100.11", "198.51.100.12"}) {
                client.createCaptcha("+8613800000000", "REGISTER", source, "fixture-device");
                var authorization = client.authorizePasswordPreparation(java.util.UUID.randomUUID().toString(),"+8613800000000","REGISTER","fixture-captcha",128,source,"fixture-device");
                String token = (String) authorization.get("authorization_token");
                var reference = client.precheckPassword("fixture-password", token, source, "fixture-device");
                client.sendSMS(Map.of("username", "fixture", "mobile", "+8613800000000", "password_handle", reference.get("password_handle"), "password_policy_version", reference.get("password_policy_version"), "captcha_id", "fixture-captcha", "captcha_offset", 128, "purpose", "REGISTER", "password_preparation_token", token), source, "fixture-device");
            }
            assertEquals(java.util.List.of(
                "/v1/auth/captcha/challenges|198.51.100.11|fixture-device",
                "/v1/auth/password-preparation-authorizations|198.51.100.11|fixture-device",
                "/v1/registration/password-prechecks|198.51.100.11|fixture-device",
                "/v1/auth/sms/challenges|198.51.100.11|fixture-device",
                "/v1/auth/captcha/challenges|198.51.100.12|fixture-device",
                "/v1/auth/password-preparation-authorizations|198.51.100.12|fixture-device",
                "/v1/registration/password-prechecks|198.51.100.12|fixture-device",
                "/v1/auth/sms/challenges|198.51.100.12|fixture-device"), received);
        } finally { server.stop(0); }
    }
    @Test void resetFunnelKeepsThePasswordOnTheIdentityLegOverRealHTTP() throws Exception {
        var received = new java.util.concurrent.CopyOnWriteArrayList<String>();
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            String path = exchange.getRequestURI().getPath();
            var payload = new com.fasterxml.jackson.databind.ObjectMapper().readTree(exchange.getRequestBody());
            received.add(path);
            assertNull(exchange.getRequestHeaders().getFirst("Authorization"));
            assertEquals("198.51.100.13", exchange.getRequestHeaders().getFirst("X-Pinvou-Client-Source"));
            assertEquals("fixture-device", exchange.getRequestHeaders().getFirst("X-Device-ID"));
            String response = "{}";
            int status = 201;
            if (path.endsWith("password-prechecks")) {
                assertEquals("PUBLIC_SYNTHETIC_GRANT", exchange.getRequestHeaders().getFirst("X-Pinvou-Password-Preparation"));
                assertEquals(1, payload.size());
                assertEquals("fixture-password", payload.get("password").asText());
                response = "{\"password_handle\":\"reset-handle\",\"password_policy_version\":\"fixture-policy\"}";
            } else {
                assertFalse(payload.has("password"));
                if (!path.endsWith("password-resets")) assertEquals("PASSWORD_RESET", payload.get("purpose").asText());
                if (path.endsWith("password-preparation-authorizations")) {
                    response = "{\"authorization_token\":\"PUBLIC_SYNTHETIC_GRANT\"}";
                } else if (path.endsWith("sms/challenges")) {
                    assertEquals("PUBLIC_SYNTHETIC_GRANT", payload.get("password_preparation_token").asText());
                    assertEquals("reset-handle", payload.get("password_handle").asText());
                    assertEquals("fixture-policy", payload.get("password_policy_version").asText());
                    status = 202;
                } else if (path.endsWith("password-resets")) {
                    assertEquals("reset-handle", payload.get("password_handle").asText());
                    assertEquals("fixture-policy", payload.get("password_policy_version").asText());
                    status = 204;
                }
            }
            byte[] body = response.getBytes(StandardCharsets.UTF_8);
            exchange.sendResponseHeaders(status, status == 204 ? -1 : body.length);
            if (status != 204) exchange.getResponseBody().write(body);
            exchange.close();
        });
        server.start();
        try {
            String origin = "http://127.0.0.1:" + server.getAddress().getPort();
            var client = new PinvouAuthClient(Map.of("PINVOU_ACCOUNT_AUTH_URL", origin, "PINVOU_IDENTITY_AUTH_URL", origin)::get);
            String source = "198.51.100.13", device = "fixture-device", mobile = "+8613800000000";
            client.createCaptcha(mobile, "PASSWORD_RESET", source, device);
            var authorization = client.authorizePasswordPreparation(java.util.UUID.randomUUID().toString(), mobile, "PASSWORD_RESET", "fixture-captcha", 128, source, device);
            String token = (String) authorization.get("authorization_token");
            var reference = client.precheckPassword("fixture-password", token, source, device);
            client.sendSMS(Map.of("mobile", mobile, "purpose", "PASSWORD_RESET", "captcha_id", "fixture-captcha", "captcha_offset", 128, "password_preparation_token", token, "password_handle", reference.get("password_handle"), "password_policy_version", reference.get("password_policy_version")), source, device);
            client.resetPassword(Map.of("mobile", mobile, "challenge_id", "fixture-sms", "code", "000000", "password_handle", reference.get("password_handle"), "password_policy_version", reference.get("password_policy_version")), source, device);
            assertEquals(java.util.List.of("/v1/auth/captcha/challenges", "/v1/auth/password-preparation-authorizations", "/v1/registration/password-prechecks", "/v1/auth/sms/challenges", "/v1/auth/password-resets"), received);
        } finally { server.stop(0); }
    }

    @Test void preparationRateLimitHasABoundedHint() throws Exception {
        for (String hint : new String[]{"17", "999999", "bad"}) {
            HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            server.createContext("/", exchange -> {
                exchange.getResponseHeaders().set("Retry-After", hint);
                byte[] body = "{\"code\":\"RATE_LIMITED\"}".getBytes(StandardCharsets.UTF_8);
                exchange.sendResponseHeaders(429, body.length);
                exchange.getResponseBody().write(body); exchange.close();
            });
            server.start();
            try {
                var client = new PinvouAuthClient(Map.of("PINVOU_ACCOUNT_AUTH_URL", "http://127.0.0.1:" + server.getAddress().getPort())::get);
                var error = assertThrows(PinvouAuthClient.AuthClientException.class, () -> client.authorizePasswordPreparation(java.util.UUID.randomUUID().toString(), "+8613800000000", "REGISTER", "fixture-captcha", 128, "198.51.100.13", "fixture-device"));
                assertEquals("RATE_LIMITED", error.code());
                assertEquals(hint.equals("17") ? 17 : hint.equals("999999") ? 86400 : 0, error.retryAfterSeconds());
            } finally { server.stop(0); }
        }
    }

    // A public preparation fixture cannot require the internal workload Bearer.
    // Handler assertion/transport failures are surfaced on the JUnit thread so
    // they cannot masquerade as the intended malformed-reference 503.
    @Test void malformedPasswordReferenceACKIsUnavailableOverPublicTransport() throws Exception {
        for (String body : new String[]{"{}", "{\"password_handle\":\"\",\"password_policy_version\":\"fixture-policy\"}", "{\"password_handle\":\"fixture-handle\",\"password_policy_version\":\"\"}"}) {
            var failure = new java.util.concurrent.atomic.AtomicReference<Throwable>();
            var delivered = new AtomicBoolean();
            var received = new AtomicInteger();
            var handled = new java.util.concurrent.CountDownLatch(1);
            HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            server.createContext("/", exchange -> {
                try {
                    received.incrementAndGet();
                    assertEquals("/v1/registration/password-prechecks", exchange.getRequestURI().getPath());
                    assertNull(exchange.getRequestHeaders().getFirst("Authorization"));
                    assertEquals("PUBLIC_SYNTHETIC_GRANT", exchange.getRequestHeaders().getFirst("X-Pinvou-Password-Preparation"));
                    assertEquals("198.51.100.13", exchange.getRequestHeaders().getFirst("X-Pinvou-Client-Source"));
                    assertEquals("fixture-device", exchange.getRequestHeaders().getFirst("X-Device-ID"));
                    var payload = new com.fasterxml.jackson.databind.ObjectMapper().readTree(exchange.getRequestBody());
                    assertEquals(1, payload.size()); assertEquals("fixture-password", payload.get("password").asText());
                    byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
                    exchange.sendResponseHeaders(201, bytes.length); exchange.getResponseBody().write(bytes); delivered.set(true);
                } catch (Throwable error) { failure.compareAndSet(null, error); }
                finally { exchange.close(); handled.countDown(); }
            });
            server.start();
            try {
                var client = new PinvouAuthClient(Map.of("PINVOU_IDENTITY_AUTH_URL", "http://127.0.0.1:" + server.getAddress().getPort())::get);
                var error = assertThrows(PinvouAuthClient.AuthClientException.class, () -> client.precheckPassword("fixture-password", "PUBLIC_SYNTHETIC_GRANT", "198.51.100.13", "fixture-device"));
                assertEquals(503, error.status()); assertEquals("AUTH_SERVICE_UNAVAILABLE", error.code());
            } finally {
                try { assertTrue(handled.await(5, java.util.concurrent.TimeUnit.SECONDS), "public fixture handler must complete"); } finally { server.stop(0); }
                assertNull(failure.get(), "public fixture must not fail before delivering the intended ACK");
                assertTrue(delivered.get(), "the intended 201 malformed body must actually be delivered");
                assertEquals(1, received.get());
            }
        }
    }

    @Test void passwordPreparationThreeSuccessfulACKsKeepThePublicBoundary() throws Exception {
        var failure = new java.util.concurrent.atomic.AtomicReference<Throwable>();
        var received = new java.util.concurrent.CopyOnWriteArrayList<String>();
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            try {
                String path = exchange.getRequestURI().getPath(); received.add(path);
                assertNull(exchange.getRequestHeaders().getFirst("Authorization"));
                assertEquals("198.51.100.13", exchange.getRequestHeaders().getFirst("X-Pinvou-Client-Source"));
                assertEquals("fixture-device", exchange.getRequestHeaders().getFirst("X-Device-ID"));
                var payload = new com.fasterxml.jackson.databind.ObjectMapper().readTree(exchange.getRequestBody());
                String response; int status;
                if (path.equals("/v1/auth/password-preparation-authorizations")) {
                    assertEquals("REGISTER", payload.get("purpose").asText());
                    assertFalse(payload.has("password")); assertEquals(5, payload.size());
                    response = "{\"authorization_token\":\"PUBLIC_SYNTHETIC_GRANT\"}"; status = 201;
                } else if (path.equals("/v1/registration/password-prechecks")) {
                    assertEquals("PUBLIC_SYNTHETIC_GRANT", exchange.getRequestHeaders().getFirst("X-Pinvou-Password-Preparation"));
                    assertEquals(1, payload.size()); assertEquals("fixture-password", payload.get("password").asText());
                    response = "{\"password_handle\":\"fixture-handle\",\"password_policy_version\":\"fixture-policy\"}"; status = 201;
                } else {
                    assertEquals("/v1/auth/sms/challenges", path); assertFalse(payload.has("password"));
                    assertEquals("PUBLIC_SYNTHETIC_GRANT", payload.get("password_preparation_token").asText());
                    assertEquals("fixture-handle", payload.get("password_handle").asText());
                    assertEquals("fixture-policy", payload.get("password_policy_version").asText());
                    response = "{\"challenge_id\":\"fixture-sms\"}"; status = 202;
                }
                byte[] bytes = response.getBytes(StandardCharsets.UTF_8); exchange.sendResponseHeaders(status, bytes.length); exchange.getResponseBody().write(bytes);
            } catch (Throwable error) { failure.compareAndSet(null, error); }
            finally { exchange.close(); }
        });
        server.start();
        try {
            String origin = "http://127.0.0.1:" + server.getAddress().getPort();
            var client = new PinvouAuthClient(Map.of("PINVOU_ACCOUNT_AUTH_URL", origin, "PINVOU_IDENTITY_AUTH_URL", origin)::get);
            String token = (String) client.authorizePasswordPreparation(java.util.UUID.randomUUID().toString(), "+8613800000000", "REGISTER", "fixture-captcha", 128, "198.51.100.13", "fixture-device").get("authorization_token");
            var reference = client.precheckPassword("fixture-password", token, "198.51.100.13", "fixture-device");
            assertEquals("fixture-sms", client.sendSMS(Map.of("mobile", "+8613800000000", "purpose", "REGISTER", "password_preparation_token", token, "password_handle", reference.get("password_handle"), "password_policy_version", reference.get("password_policy_version")), "198.51.100.13", "fixture-device").get("challenge_id"));
            assertEquals(java.util.List.of("/v1/auth/password-preparation-authorizations", "/v1/registration/password-prechecks", "/v1/auth/sms/challenges"), received);
        } finally { server.stop(0); assertNull(failure.get(), "public handler assertion failures must reach the test thread"); }
    }

    private PinvouAuthClient client(HttpServer server) {
        Map<String, String> env = Map.of("PINVOU_ACCOUNT_AUTH_URL", "http://127.0.0.1:" + server.getAddress().getPort(),
            "PINVOU_SMS_VERIFY_URL", "http://127.0.0.1:" + server.getAddress().getPort() + "/verify",
            "PINVOU_OIDC_ISSUER", "https://identity.example.test/realms/pinvou",
            "PINVOU_INTERNAL_SERVICE_TOKEN", "controlled-test-workload-token");
        return new PinvouAuthClient(env::get);
    }
    private HttpServer server(int status, String body) throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            assertEquals("Bearer controlled-test-workload-token", exchange.getRequestHeaders().getFirst("Authorization"));
            exchange.getResponseHeaders().set("X-Pinvou-Identity-Subject", "controlled-subject");
            byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
            exchange.sendResponseHeaders(status, status == 204 ? -1 : bytes.length);
            if (status != 204) exchange.getResponseBody().write(bytes);
            exchange.close();
        });
        server.start(); return server;
    }
    @Test void actualAuthorizationFactsAndUnknownExtensionsAreAccepted() throws Exception {
        HttpServer s = server(200, "{\"allowed\":true,\"user_id\":\"" + USER + "\",\"permission\":\"personal.session.login\",\"permission_scope\":\"PERSONAL\",\"future\":{\"safe\":true}}");
        try { assertTrue(client(s).authorizeLogin("controlled-subject").allowed()); } finally { s.stop(0); }
    }
    @Test void smsNeedsTheActual204SubjectHeader() throws Exception {
        HttpServer s = server(204, "");
        try { assertEquals("controlled-subject", client(s).verifySMS("+8613800000000", "fixture-challenge", "000000", "198.51.100.1", "fixture-device")); } finally { s.stop(0); }
        s = server(200, "{}");
        try { HttpServer current = s; assertThrows(PinvouAuthClient.AuthClientException.class, () -> client(current).verifySMS("+8613800000000", "fixture-challenge", "000000", "198.51.100.1", "fixture-device")); } finally { s.stop(0); }
    }
    @Test void malformedAndIncompleteAuthorityCannotAuthorize() throws Exception {
        for (String body : new String[]{"{}", "{\"allowed\":true,\"user_id\":\"wrong\"}", "{\"allowed\":true,\"user_id\":\"00000000-0000-0000-0000-000000000000\",\"permission\":\"personal.session.login\",\"permission_scope\":\"PERSONAL\"}"}) {
            HttpServer s = server(200, body);
            try { assertFalse(client(s).authorizeLogin("controlled-subject").allowed()); } finally { s.stop(0); }
        }
        for (String body : new String[]{"{}{}", "{\"allowed\":true,\"allowed\":false}", " ".repeat(65_537)}) {
            HttpServer s = server(200, body);
            try { assertThrows(PinvouAuthClient.AuthClientException.class, () -> client(s).authorizeLogin("controlled-subject")); } finally { s.stop(0); }
        }
    }
    @Test void redirectsNeverForwardTheWorkloadCredential() throws Exception {
        AtomicInteger reached = new AtomicInteger();
        HttpServer target = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        target.createContext("/", exchange -> { reached.incrementAndGet(); exchange.sendResponseHeaders(204, -1); exchange.close(); }); target.start();
        HttpServer redirect = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        redirect.createContext("/", exchange -> { exchange.getResponseHeaders().set("Location", "http://127.0.0.1:" + target.getAddress().getPort()); exchange.sendResponseHeaders(307, -1); exchange.close(); }); redirect.start();
        try { assertThrows(PinvouAuthClient.AuthClientException.class, () -> client(redirect).authorizeLogin("controlled-subject")); assertEquals(0, reached.get()); }
        finally { redirect.stop(0); target.stop(0); }
    }
    @Test void invalidEndpointShapesFailBeforeAnyRequest() {
        for (String url : new String[]{"ftp://identity.example.test", "http://user@identity.example.test", "http://identity.example.test?", "http://identity.example.test#", "http:/missing-host"}) {
            assertThrows(PinvouAuthClient.AuthClientException.class, () -> PinvouAuthClient.validatedURL(url));
        }
        assertTrue(PinvouAuthClient.nonzeroUUID(USER)); assertFalse(PinvouAuthClient.nonzeroUUID("1-1-1-1-1"));
    }
    @Test void dependencyErrorCodeCannotBecomeASecretLogValue() throws Exception {
        HttpServer s = server(400, "{\"code\":\"untrusted-payload-value\"}");
        try {
            var error = assertThrows(PinvouAuthClient.AuthClientException.class, () -> client(s).authorizeLogin("controlled-subject"));
            assertEquals("AUTH_REQUEST_FAILED", error.code());
            assertEquals("AUTH_REQUEST_FAILED", error.getMessage());
        } finally { s.stop(0); }
    }

    @Test void deadlineIncludesBodyAfterSuccessfulResponseHeaders() throws Exception {
        var workers = Executors.newCachedThreadPool();
        HttpServer s = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        s.setExecutor(workers);
        byte[] body = authority().getBytes(StandardCharsets.UTF_8);
        s.createContext("/", exchange -> {
            try {
                exchange.sendResponseHeaders(200, body.length);
                exchange.getResponseBody().write(body, 0, 1);
                exchange.getResponseBody().flush();
                Thread.sleep(10_000);
                exchange.getResponseBody().write(body, 1, body.length - 1);
            } catch (InterruptedException interrupted) {
                Thread.currentThread().interrupt();
            } catch (java.io.IOException cancelled) {
                // The caller's body deadline cancels this owned exchange.
            } finally { exchange.close(); }
        });
        s.start();
        try {
            long started = System.nanoTime();
            var error = assertThrows(PinvouAuthClient.AuthClientException.class,
                () -> client(s).authorizeLogin("controlled-subject"));
            assertEquals("AUTH_SERVICE_UNAVAILABLE", error.code());
            assertTrue((System.nanoTime() - started) / 1_000_000 < 9_500,
                "successful headers must not extend the eight-second complete-body deadline");
        } finally { s.stop(0); workers.shutdownNow(); }
    }

    @Test void exactBodyBoundAcceptsAuthorityAndChunkedOverflowFailsBeforeCompletion() throws Exception {
        String valid = authority();
        HttpServer s = server(200, valid + " ".repeat(65_536 - valid.length()));
        try { assertTrue(client(s).authorizeLogin("controlled-subject").allowed()); }
        finally { s.stop(0); }

        var workers = Executors.newCachedThreadPool();
        HttpServer overflow = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        overflow.setExecutor(workers);
        overflow.createContext("/", exchange -> {
            try {
                exchange.sendResponseHeaders(200, 0);
                exchange.getResponseBody().write(" ".repeat(65_537).getBytes(StandardCharsets.UTF_8));
                exchange.getResponseBody().flush();
                Thread.sleep(10_000);
            } catch (InterruptedException interrupted) {
                Thread.currentThread().interrupt();
            } catch (java.io.IOException cancelled) {
                // The subscriber rejects the body without waiting for EOF.
            } finally { exchange.close(); }
        });
        overflow.start();
        try {
            long started = System.nanoTime();
            assertThrows(PinvouAuthClient.AuthClientException.class,
                () -> client(overflow).authorizeLogin("controlled-subject"));
            assertTrue((System.nanoTime() - started) / 1_000_000 < 7_000,
                "overflow must cancel before the eight-second deadline or server EOF");
        } finally { overflow.stop(0); workers.shutdownNow(); }
    }

    @Test void interruptedCallerPreservesItsInterruptFlag() throws Exception {
        HttpServer s = server(200, authority());
        AtomicBoolean rejected = new AtomicBoolean();
        AtomicBoolean retained = new AtomicBoolean();
        Thread caller = new Thread(() -> {
            Thread.currentThread().interrupt();
            try { client(s).authorizeLogin("controlled-subject"); }
            catch (PinvouAuthClient.AuthClientException error) { rejected.set(true); }
            finally { retained.set(Thread.currentThread().isInterrupted()); }
        });
        try {
            caller.start(); caller.join(3_000);
            assertFalse(caller.isAlive()); assertTrue(rejected.get()); assertTrue(retained.get());
        } finally { caller.interrupt(); s.stop(0); caller.join(3_000); }
    }

    private String authority() {
        return "{\"allowed\":true,\"user_id\":\"" + USER + "\",\"permission\":\"personal.session.login\",\"permission_scope\":\"PERSONAL\"}";
    }

}
