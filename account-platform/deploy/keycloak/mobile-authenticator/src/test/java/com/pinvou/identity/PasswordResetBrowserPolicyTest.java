package com.pinvou.identity;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.Test;

final class PasswordResetBrowserPolicyTest {
    @Test
    void resetRequestsUseTheDedicatedSmsPurpose() {
        assertEquals("PASSWORD_RESET", PinvouBrowserAuthenticator.purpose("reset"));
        assertEquals("LOGIN", PinvouBrowserAuthenticator.purpose("sms"));
    }

    @Test
    void consumedOrMismatchedReferencesRestartTheWholeResetForm() {
        assertTrue(PinvouBrowserAuthenticator.shouldRestartPasswordReset("reset", "PASSWORD_REFERENCE_INVALID"));
        assertTrue(PinvouBrowserAuthenticator.shouldRestartPasswordReset("reset", "PASSWORD_REFERENCE_CONFLICT"));
        assertFalse(PinvouBrowserAuthenticator.shouldRestartPasswordReset("reset", "SMS_CODE_INVALID"));
        assertFalse(PinvouBrowserAuthenticator.shouldRestartPasswordReset("register", "PASSWORD_REFERENCE_INVALID"));
    }

    @Test
    void identityPasswordPolicyCodesHaveAnActionableMessage() {
        assertEquals("pinvouPasswordInvalid", PinvouBrowserAuthenticator.messageFor("PASSWORD_INVALID"));
        assertEquals("pinvouPasswordInvalid", PinvouBrowserAuthenticator.messageFor("PASSWORD_EDGE_SPACE_NOT_ALLOWED"));
    }
}
