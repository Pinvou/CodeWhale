package com.pinvou.identity;

import java.util.Collections;
import java.util.List;
import org.keycloak.Config;
import org.keycloak.authentication.Authenticator;
import org.keycloak.authentication.AuthenticatorFactory;
import org.keycloak.models.AuthenticationExecutionModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.provider.ProviderConfigProperty;

public final class MobileDirectGrantAuthenticatorFactory implements AuthenticatorFactory {
    public static final String ID = "pinvou-mobile-direct-grant";
    private static final MobileDirectGrantAuthenticator INSTANCE = new MobileDirectGrantAuthenticator();

    @Override public String getId() { return ID; }
    @Override public String getDisplayType() { return "Pinvou mobile password or SMS"; }
    @Override public String getReferenceCategory() { return "password-or-sms"; }
    @Override public boolean isConfigurable() { return false; }
    @Override public AuthenticationExecutionModel.Requirement[] getRequirementChoices() {
        return new AuthenticationExecutionModel.Requirement[]{AuthenticationExecutionModel.Requirement.REQUIRED};
    }
    @Override public boolean isUserSetupAllowed() { return false; }
    @Override public String getHelpText() { return "Authenticates a mainland China mobile using a password or a one-time SMS challenge."; }
    @Override public List<ProviderConfigProperty> getConfigProperties() { return Collections.emptyList(); }
    @Override public Authenticator create(KeycloakSession session) { return INSTANCE; }
    @Override public void init(Config.Scope config) {}
    @Override public void postInit(KeycloakSessionFactory factory) {}
    @Override public void close() {}
}
