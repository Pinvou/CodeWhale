package com.pinvou.identity;

import java.util.function.BooleanSupplier;

/**
 * Executes the credential and Account authorization gates in login order.
 * State suppliers are deliberately lazy: for an existing user the real
 * credential check must complete before disabled or brute-force state is read.
 */
final class PasswordLoginGate {
    private PasswordLoginGate() {}

    @FunctionalInterface
    interface PasswordVerifier {
        boolean verify(String password);
    }

    @FunctionalInterface
    interface LoginAuthorizer {
        PinvouAuthClient.LoginAuthorization authorize() throws PinvouAuthClient.AuthClientException;
    }

    static boolean allows(
        String password,
        boolean userPresent,
        PasswordVerifier verifier,
        BooleanSupplier userEnabled,
        BooleanSupplier bruteForceBlocked,
        LoginAuthorizer authorizer
    ) throws PinvouAuthClient.AuthClientException {
        if (!userPresent || password == null || password.isEmpty()) return false;
        if (!verifier.verify(password)) return false;
        if (!userEnabled.getAsBoolean()) return false;
        if (bruteForceBlocked.getAsBoolean()) return false;
        PinvouAuthClient.LoginAuthorization authorization = authorizer.authorize();
        return authorization != null && authorization.allowed() && !authorization.accountUserID().isBlank();
    }
}
