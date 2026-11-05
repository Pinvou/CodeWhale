package com.pinvou.identity;

import org.keycloak.authentication.AuthenticationFlowContext;

/** Consumes only the address resolved by Keycloak's configured trusted proxy policy. */
final class ClientRiskSource {
    private ClientRiskSource() {}

    static String from(AuthenticationFlowContext context) {
        String address = context.getSession().getContext().getConnection().getRemoteAddr();
        return address == null || address.isBlank() ? "source:unknown" : address.trim();
    }
}
