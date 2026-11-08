package com.pinvou.identity;

import org.keycloak.models.KeycloakSession;
import org.keycloak.services.resource.RealmResourceProvider;

final class PinvouIdentitySecurityResourceProvider implements RealmResourceProvider {
    private final KeycloakSession session;

    PinvouIdentitySecurityResourceProvider(KeycloakSession session) {
        this.session = session;
    }

    @Override
    public Object getResource() {
        return new PinvouIdentitySecurityResource(session);
    }

    @Override public void close() {}
}
