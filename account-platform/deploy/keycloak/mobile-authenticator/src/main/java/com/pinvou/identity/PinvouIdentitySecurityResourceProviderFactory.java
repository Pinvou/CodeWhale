package com.pinvou.identity;

import org.keycloak.Config;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.services.resource.RealmResourceProvider;
import org.keycloak.services.resource.RealmResourceProviderFactory;

import java.util.List;

public final class PinvouIdentitySecurityResourceProviderFactory implements RealmResourceProviderFactory {
    public static final String ID = "pinvou-identity-security";

    @Override
    public RealmResourceProvider create(KeycloakSession session) {
        return new PinvouIdentitySecurityResourceProvider(session);
    }

    @Override public void init(Config.Scope config) {}
    @Override public void postInit(KeycloakSessionFactory factory) {}
    @Override public void close() {}
    @Override public String getId() { return ID; }
    @Override public List<ProviderConfigProperty> getConfigMetadata() { return List.of(); }
}
