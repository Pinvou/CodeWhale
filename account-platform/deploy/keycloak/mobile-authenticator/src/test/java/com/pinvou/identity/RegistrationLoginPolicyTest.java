package com.pinvou.identity;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.List;
import org.junit.jupiter.api.Test;

final class RegistrationLoginPolicyTest {
    private static final String USER_ID = "0199a307-45ac-7e12-9f03-1bb29eb08fd4";
    private static final String OTHER_USER_ID = "0199a307-45ac-7e12-9f03-1bb29eb08fd5";
    private static final String OPERATION_ID = "0199a307-45ac-7e12-9f03-1bb29eb08fd6";
    private static final String OTHER_OPERATION_ID = "0199a307-45ac-7e12-9f03-1bb29eb08fd7";

    @Test
    void allowsEveryLoginReadyStatusWhenIdentityAndAccountBindingMatch() {
        for (String status : List.of("ACCOUNT_COMMITTED", "PROVISIONING", "ACTIVE", "REPAIR_REQUIRED")) {
            assertTrue(RegistrationLoginPolicy.allows(status, OPERATION_ID, OPERATION_ID, USER_ID, USER_ID, true, true, true), status);
        }
    }

    @Test
    void rejectsEveryNonReadyRegistrationStatus() {
        for (String status : List.of(
            "SMS_PENDING", "REGISTRATION_IN_PROGRESS", "IDENTITY_CREATING", "IDENTITY_CREATED",
            "ACCOUNT_COMMITTING", "COMPENSATION_PENDING", "COMPENSATED", "EXPIRED", "UNKNOWN", ""
        )) {
            assertFalse(RegistrationLoginPolicy.allows(status, OPERATION_ID, OPERATION_ID, USER_ID, USER_ID, true, true, true), status);
        }
        assertFalse(RegistrationLoginPolicy.allows(null, OPERATION_ID, OPERATION_ID, USER_ID, USER_ID, true, true, true));
    }

    @Test
    void rejectsMissingDisabledOrUnboundIdentity() {
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", OPERATION_ID, OPERATION_ID, USER_ID, USER_ID, true, false, true));
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", OPERATION_ID, OPERATION_ID, USER_ID, USER_ID, true, true, false));
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", OPERATION_ID, OPERATION_ID, "", USER_ID, true, true, true));
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", OPERATION_ID, OPERATION_ID, USER_ID, "", true, true, true));
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", OPERATION_ID, OPERATION_ID, "not-a-uuid", USER_ID, true, true, true));
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", "", OPERATION_ID, USER_ID, USER_ID, true, true, true));
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", OPERATION_ID, "", USER_ID, USER_ID, true, true, true));
    }

    @Test
    void rejectsAKeycloakSubjectAuthorizedForAnotherAccountUser() {
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", OPERATION_ID, OPERATION_ID, USER_ID, OTHER_USER_ID, true, true, true));
    }

    @Test
    void rejectsAResponseForAnotherRegistrationOperation() {
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", OPERATION_ID, OTHER_OPERATION_ID, USER_ID, USER_ID, true, true, true));
    }

    @Test
    void rejectsDeniedAuthorizationEvenWhenTheAccountUserMatches() {
        assertFalse(RegistrationLoginPolicy.allows("PROVISIONING", OPERATION_ID, OPERATION_ID, USER_ID, USER_ID, false, true, true));
    }
}
