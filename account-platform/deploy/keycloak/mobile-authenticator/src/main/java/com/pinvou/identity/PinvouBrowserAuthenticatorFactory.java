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

public final class PinvouBrowserAuthenticatorFactory implements AuthenticatorFactory {
    public static final String ID = "pinvou-browser-authenticator";
    private static final PinvouBrowserAuthenticator INSTANCE = new PinvouBrowserAuthenticator();

    @Override public String getId() { return ID; }
    @Override public String getDisplayType() { return "Pinvou browser password, SMS, or registration"; }
    @Override public String getReferenceCategory() { return "pinvou-browser"; }
    @Override public boolean isConfigurable() { return false; }
    @Override public AuthenticationExecutionModel.Requirement[] getRequirementChoices() {
        return new AuthenticationExecutionModel.Requirement[]{AuthenticationExecutionModel.Requirement.ALTERNATIVE, AuthenticationExecutionModel.Requirement.REQUIRED};
    }
    @Override public boolean isUserSetupAllowed() { return false; }
    @Override public String getHelpText() { return "Renders the Pinvou browser login and performs password, verified mobile SMS, and verified mobile registration flows."; }
    @Override public List<ProviderConfigProperty> getConfigProperties() { return Collections.emptyList(); }
    @Override public Authenticator create(KeycloakSession session) { return INSTANCE; }
    @Override public void init(Config.Scope config) {}
    @Override public void postInit(KeycloakSessionFactory factory) {}
    @Override public void close() {}
}
