package com.pinvou.identity;

import java.util.Set;
import java.util.UUID;

/**
 * Fail-closed decision for continuing the same OIDC flow after registration.
 *
 * <p>The registration response identifies the Account user while the Keycloak
 * user identifies an Identity subject.  The Account authorization lookup is
 * the authoritative binding between those two identities, so all three must
 * agree before Keycloak may create a login session.</p>
 */
final class RegistrationLoginPolicy {
    private static final Set<String> LOGIN_READY_STATUSES = Set.of(
        "ACCOUNT_COMMITTED", "PROVISIONING", "ACTIVE", "REPAIR_REQUIRED"
    );

    private RegistrationLoginPolicy() {}

    static boolean allows(
        String status,
        String requestedRegistrationOperationID,
        String returnedRegistrationOperationID,
        String registrationAccountUserID,
        String authorizedAccountUserID,
        boolean authorizationAllowed,
        boolean keycloakUserPresent,
        boolean keycloakUserEnabled
    ) {
        return LOGIN_READY_STATUSES.contains(value(status))
            && authorizationAllowed
            && keycloakUserPresent
            && keycloakUserEnabled
            && sameID(requestedRegistrationOperationID, returnedRegistrationOperationID)
            && sameID(registrationAccountUserID, authorizedAccountUserID);
    }

    private static boolean sameID(String left, String right) {
        try {
            return UUID.fromString(value(left)).equals(UUID.fromString(value(right)));
        } catch (IllegalArgumentException error) {
            return false;
        }
    }

    private static String value(String input) {
        return input == null ? "" : input.trim();
    }
}
