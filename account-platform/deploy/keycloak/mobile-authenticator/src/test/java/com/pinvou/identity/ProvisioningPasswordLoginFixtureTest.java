package com.pinvou.identity;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.List;
import org.junit.jupiter.api.Test;

final class ProvisioningPasswordLoginFixtureTest {
    private static final ObjectMapper JSON = new ObjectMapper();

    @Test
    void provisioningUserPasswordMustPassCredentialVerificationThenAccountAuthorization() throws Exception {
        JsonNode fixture = fixture();
        List<String> calls = new ArrayList<>();
        String password = fixturePassword(fixture);
        boolean allowed = PasswordLoginGate.allows(
            password,
            true,
            candidate -> {
                calls.add("password");
                return fixture.required("password_sha256").asText().equals(sha256(candidate));
            },
            () -> {
                calls.add("enabled");
                return true;
            },
            () -> {
                calls.add("brute-force");
                return false;
            },
            () -> {
                calls.add("authorize");
                return new PinvouAuthClient.LoginAuthorization(
                    fixture.required("authorization_allowed").asBoolean(),
                    fixture.required("account_user_id").asText()
                );
            }
        );
        assertTrue(allowed);
        assertEquals(List.of("password", "enabled", "brute-force", "authorize"), calls);
        assertEquals("PROVISIONING", fixture.required("account_status").asText());
    }

    @Test
    void invalidPasswordNeverReachesAccountAuthorization() throws Exception {
        JsonNode fixture = fixture();
        List<String> calls = new ArrayList<>();
        boolean allowed = PasswordLoginGate.allows(
            fixturePassword(fixture) + "-wrong",
            true,
            candidate -> {
                calls.add("password");
                return fixture.required("password_sha256").asText().equals(sha256(candidate));
            },
            () -> {
                calls.add("enabled");
                return true;
            },
            () -> {
                calls.add("brute-force");
                return false;
            },
            () -> {
                calls.add("authorize");
                return new PinvouAuthClient.LoginAuthorization(true, fixture.required("account_user_id").asText());
            }
        );
        assertFalse(allowed);
        assertEquals(List.of("password"), calls);
    }

    @Test
    void validPasswordStillFailsWhenAccountAuthorizationDeniesLogin() throws Exception {
        JsonNode fixture = fixture();
        List<String> calls = new ArrayList<>();
        boolean allowed = PasswordLoginGate.allows(
            fixturePassword(fixture),
            true,
            candidate -> {
                calls.add("password");
                return fixture.required("password_sha256").asText().equals(sha256(candidate));
            },
            () -> {
                calls.add("enabled");
                return true;
            },
            () -> {
                calls.add("brute-force");
                return false;
            },
            () -> {
                calls.add("authorize");
                return new PinvouAuthClient.LoginAuthorization(false, fixture.required("account_user_id").asText());
            }
        );
        assertFalse(allowed);
        assertEquals(List.of("password", "enabled", "brute-force", "authorize"), calls);
    }

    @Test
    void disabledUserStillVerifiesPasswordBeforeStateCheckAndNeverAuthorizes() throws Exception {
        JsonNode fixture = fixture();
        List<String> calls = new ArrayList<>();
        boolean allowed = PasswordLoginGate.allows(
            fixturePassword(fixture),
            true,
            candidate -> {
                calls.add("password");
                return fixture.required("password_sha256").asText().equals(sha256(candidate));
            },
            () -> {
                calls.add("enabled");
                return false;
            },
            () -> {
                calls.add("brute-force");
                return false;
            },
            () -> {
                calls.add("authorize");
                return new PinvouAuthClient.LoginAuthorization(true, fixture.required("account_user_id").asText());
            }
        );
        assertFalse(allowed);
        assertEquals(List.of("password", "enabled"), calls);
    }

    @Test
    void lockedUserStillVerifiesPasswordBeforeBruteForceCheckAndNeverAuthorizes() throws Exception {
        JsonNode fixture = fixture();
        List<String> calls = new ArrayList<>();
        boolean allowed = PasswordLoginGate.allows(
            fixturePassword(fixture),
            true,
            candidate -> {
                calls.add("password");
                return fixture.required("password_sha256").asText().equals(sha256(candidate));
            },
            () -> {
                calls.add("enabled");
                return true;
            },
            () -> {
                calls.add("brute-force");
                return true;
            },
            () -> {
                calls.add("authorize");
                return new PinvouAuthClient.LoginAuthorization(true, fixture.required("account_user_id").asText());
            }
        );
        assertFalse(allowed);
        assertEquals(List.of("password", "enabled", "brute-force"), calls);
    }

    @Test
    void emptyAuthorizationResultFailsClosedAfterCredentialAndStateChecks() throws Exception {
        JsonNode fixture = fixture();
        List<String> calls = new ArrayList<>();
        boolean allowed = PasswordLoginGate.allows(
            fixturePassword(fixture),
            true,
            candidate -> {
                calls.add("password");
                return fixture.required("password_sha256").asText().equals(sha256(candidate));
            },
            () -> {
                calls.add("enabled");
                return true;
            },
            () -> {
                calls.add("brute-force");
                return false;
            },
            () -> {
                calls.add("authorize");
                return null;
            }
        );
        assertFalse(allowed);
        assertEquals(List.of("password", "enabled", "brute-force", "authorize"), calls);
    }

    @Test
    void unknownUserFailsBeforeCredentialOrAccountCallbacks() throws Exception {
        List<String> calls = new ArrayList<>();
        boolean allowed = PasswordLoginGate.allows(
            "irrelevant",
            false,
            candidate -> {
                calls.add("password");
                return false;
            },
            () -> {
                calls.add("enabled");
                return true;
            },
            () -> {
                calls.add("brute-force");
                return false;
            },
            () -> {
                calls.add("authorize");
                return null;
            }
        );
        assertFalse(allowed);
        assertTrue(calls.isEmpty());
    }

    private static JsonNode fixture() throws Exception {
        Path path = Path.of(System.getProperty("user.dir"), "..", "..", "..", "test", "fixtures", "provisioning-password-login.json");
        return JSON.readTree(Files.readString(path.normalize(), StandardCharsets.UTF_8));
    }

    private static String sha256(String value) {
        try {
            return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(value.getBytes(StandardCharsets.UTF_8)));
        } catch (Exception error) {
            throw new IllegalStateException(error);
        }
    }

    private static String fixturePassword(JsonNode fixture) {
        return "fixture:" + fixture.required("username").asText() + ":" + fixture.required("registration_operation_id").asText();
    }
}
