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
            byte[] body = "{}".getBytes(StandardCharsets.UTF_8);
            int status = exchange.getRequestURI().getPath().endsWith("precheck") ? 200 : 201;
            exchange.sendResponseHeaders(status, body.length);
            exchange.getResponseBody().write(body); exchange.close();
        });
        server.start();
        try {
            String origin = "http://127.0.0.1:" + server.getAddress().getPort();
            var client = new PinvouAuthClient(Map.of("PINVOU_ACCOUNT_AUTH_URL", origin, "PINVOU_IDENTITY_AUTH_URL", origin)::get);
            for (String source : new String[]{"198.51.100.11", "198.51.100.12"}) {
                client.precheckPassword("fixture-password", source, "fixture-device");
                client.precheckRegistration("fixture", "+8613800000000", "fixture-handle", "fixture-policy", source, "fixture-device");
                client.createCaptcha("+8613800000000", "REGISTER", source, "fixture-device");
            }
            assertEquals(java.util.List.of(
                "/v1/registration/password-prechecks|198.51.100.11|fixture-device",
                "/v1/auth/registration/precheck|198.51.100.11|fixture-device",
                "/v1/auth/captcha/challenges|198.51.100.11|fixture-device",
                "/v1/registration/password-prechecks|198.51.100.12|fixture-device",
                "/v1/auth/registration/precheck|198.51.100.12|fixture-device",
                "/v1/auth/captcha/challenges|198.51.100.12|fixture-device"), received);
        } finally { server.stop(0); }
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
