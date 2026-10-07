package com.pinvou.identity;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import jakarta.ws.rs.core.MultivaluedHashMap;
import java.util.LinkedHashMap;
import java.util.Map;
import org.keycloak.authentication.AuthenticationFlowContext;
import org.keycloak.forms.login.LoginFormsProvider;
import org.keycloak.http.HttpRequest;
import org.keycloak.sessions.AuthenticationSessionModel;
import org.junit.jupiter.api.Test;
import org.mockito.Answers;

final class RegistrationSmsRequestPolicyTest {
    @Test
    void missingPasswordHasAnActionableSmsRequestMessage() {
        assertEquals("pinvouPasswordRequiredBeforeSms", PinvouBrowserAuthenticator.registrationPasswordMessage(null, null));
        assertEquals("pinvouPasswordRequiredBeforeSms", PinvouBrowserAuthenticator.registrationPasswordMessage("", ""));
    }

    @Test
    void invalidOrMismatchedPasswordKeepsTheExistingPolicyMessage() {
        assertEquals("pinvouPasswordInvalid", PinvouBrowserAuthenticator.registrationPasswordMessage("short", "short"));
        assertEquals("pinvouPasswordInvalid", PinvouBrowserAuthenticator.registrationPasswordMessage("password-one", "password-two"));
        assertEquals("", PinvouBrowserAuthenticator.registrationPasswordMessage("password-one", "password-one"));
    }

    @Test
    void emptyPasswordSmsRequestPreservesRegistrationFieldsAndRendersActionableError() {
        AuthenticationFlowContext context = mock(AuthenticationFlowContext.class);
        AuthenticationSessionModel session = mock(AuthenticationSessionModel.class);
        HttpRequest request = mock(HttpRequest.class);
        LoginFormsProvider forms = mock(LoginFormsProvider.class, Answers.RETURNS_SELF);
        Map<String, String> notes = new LinkedHashMap<>();
        var parameters = new MultivaluedHashMap<String, String>();
        parameters.putSingle("mode", "register");
        parameters.putSingle("intent", "request_sms");
        parameters.putSingle("username", "ke12_12");
        parameters.putSingle("mobile", "18002237050");
        parameters.putSingle("device_id", "browser-device-1");
        parameters.putSingle("password", "");
        parameters.putSingle("password_confirmation", "");

        when(context.getHttpRequest()).thenReturn(request);
        when(context.getAuthenticationSession()).thenReturn(session);
        when(context.form()).thenReturn(forms);
        when(request.getDecodedFormParameters()).thenReturn(parameters);
        when(session.getAuthNote(org.mockito.ArgumentMatchers.anyString()))
            .thenAnswer(invocation -> notes.get(invocation.getArgument(0, String.class)));
        org.mockito.Mockito.doAnswer(invocation -> {
            notes.put(invocation.getArgument(0, String.class), invocation.getArgument(1, String.class));
            return null;
        }).when(session).setAuthNote(
            org.mockito.ArgumentMatchers.anyString(), org.mockito.ArgumentMatchers.anyString());

        new PinvouBrowserAuthenticator().action(context);

        assertEquals(Map.of(
            "pinvou.browser.mode", "register",
            "pinvou.browser.mobile", "+8618002237050",
            "pinvou.browser.device", "browser-device-1",
            "pinvou.browser.username", "ke12_12"
        ), notes);
        verify(forms).setAttribute("pinvouUsername", "ke12_12");
        verify(forms).setAttribute("pinvouMobile", "18002237050");
        verify(forms).setError("pinvouPasswordRequiredBeforeSms");
        verify(forms).createForm("pinvou-login.ftl");
    }
}
