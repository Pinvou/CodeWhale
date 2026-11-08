package com.pinvou.identity;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.mockito.Mockito.*;
import org.junit.jupiter.api.Test;
import org.keycloak.authentication.AuthenticationFlowContext;

final class ClientRiskSourceTest {
    @Test void arbitraryHeadersCannotOverrideServerResolvedSource() {
        AuthenticationFlowContext context = mock(AuthenticationFlowContext.class, RETURNS_DEEP_STUBS);
        when(context.getSession().getContext().getConnection().getRemoteAddr()).thenReturn("198.51.100.42");
        when(context.getHttpRequest().getHttpHeaders().getHeaderString("X-Forwarded-For")).thenReturn("203.0.113.99");
        when(context.getHttpRequest().getHttpHeaders().getHeaderString("X-Pinvou-Client-Source")).thenReturn("203.0.113.88");
        assertEquals("198.51.100.42", ClientRiskSource.from(context));
        verify(context.getHttpRequest().getHttpHeaders(), never()).getHeaderString("X-Forwarded-For");
        verify(context.getHttpRequest().getHttpHeaders(), never()).getHeaderString("X-Pinvou-Client-Source");
    }
    @Test void missingAddressStaysAnExplicitUnknown() {
        AuthenticationFlowContext context = mock(AuthenticationFlowContext.class, RETURNS_DEEP_STUBS);
        when(context.getSession().getContext().getConnection().getRemoteAddr()).thenReturn(null);
        assertEquals("source:unknown", ClientRiskSource.from(context));
    }
}
