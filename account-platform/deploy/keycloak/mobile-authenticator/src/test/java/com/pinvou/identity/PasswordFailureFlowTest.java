package com.pinvou.identity;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

import jakarta.ws.rs.core.MultivaluedHashMap;
import jakarta.ws.rs.core.Response;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicReference;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;
import org.keycloak.authentication.*;
import org.keycloak.common.ClientConnection;
import org.keycloak.credential.hash.PasswordHashProvider;
import org.keycloak.events.EventBuilder;
import org.keycloak.forms.login.LoginFormsProvider;
import org.keycloak.http.HttpRequest;
import org.keycloak.models.*;
import org.keycloak.services.managers.BruteForceProtector;
import org.keycloak.sessions.AuthenticationSessionModel;

/** Executes the shipped authenticators through Keycloak 26.7's real result,
 * default flow and logFailure. Storage, credential and dependency ports are
 * controlled; the production failure attribution chain is not substituted. */
final class PasswordFailureFlowTest {
    private static final class Fixture {
        final KeycloakSession session = mock(KeycloakSession.class, RETURNS_DEEP_STUBS);
        final KeycloakSessionFactory factory = mock(KeycloakSessionFactory.class);
        final RealmModel realm = mock(RealmModel.class);
        final AuthenticationSessionModel auth = mock(AuthenticationSessionModel.class);
        final UserModel user = mock(UserModel.class, RETURNS_DEEP_STUBS);
        final BruteForceProtector protector = mock(BruteForceProtector.class);
        final PinvouAuthClient client = mock(PinvouAuthClient.class);
        final HttpRequest request = mock(HttpRequest.class);
        final EventBuilder event = mock(EventBuilder.class, RETURNS_SELF);
        final AtomicReference<UserModel> authenticated = new AtomicReference<>();
        final Map<String, String> notes = new HashMap<>();
        final AuthenticationProcessor processor;
        final DefaultAuthenticationFlow flow;
        final AuthenticationExecutionModel execution = new AuthenticationExecutionModel();
        boolean exists = true;
        final MultivaluedHashMap<String, String> form = new MultivaluedHashMap<>();

        Fixture() throws Exception {
            when(realm.isBruteForceProtected()).thenReturn(true);
            when(session.getKeycloakSessionFactory()).thenReturn(factory);
            when(realm.getAuthenticationExecutionsStream(anyString())).thenAnswer(i -> Stream.empty());
            when(user.getId()).thenReturn("fixture-user");
            when(user.getUsername()).thenReturn("fixture");
            when(user.isEnabled()).thenReturn(true);
            when(session.users().searchForUserByUserAttributeStream(eq(realm), eq("phone_number"), anyString()))
                .thenAnswer(i -> exists ? Stream.of(user) : Stream.empty());
            doReturn(mock(PasswordHashProvider.class)).when(session).getProvider(PasswordHashProvider.class);
            when(request.getDecodedFormParameters()).thenReturn(form);
            when(session.getContext().getHttpRequest()).thenReturn(request);
            when(auth.getAuthenticatedUser()).thenAnswer(i -> authenticated.get());
            doAnswer(i -> { authenticated.set(i.getArgument(0)); return null; }).when(auth).setAuthenticatedUser(any());
            when(auth.getAuthNote(anyString())).thenAnswer(i -> notes.get(i.getArgument(0)));
            doAnswer(i -> { notes.put(i.getArgument(0), i.getArgument(1)); return null; }).when(auth).setAuthNote(anyString(), anyString());
            doAnswer(i -> { notes.remove(i.getArgument(0)); return null; }).when(auth).removeAuthNote(anyString());
            when(auth.getExecutionStatus()).thenReturn(new HashMap<>());
            execution.setId("fixture-execution");
            execution.setAuthenticator("pinvou-password");
            execution.setRequirement(AuthenticationExecutionModel.Requirement.REQUIRED);
            processor = spy(new AuthenticationProcessor().setRealm(realm).setSession(session)
                .setAuthenticationSession(auth).setConnection(mock(ClientConnection.class))
                .setRequest(request).setEventBuilder(event));
            doReturn(protector).when(processor).getBruteForceProtector();
            AuthenticationFlowModel model = new AuthenticationFlowModel(); model.setId("fixture-flow");
            flow = new DefaultAuthenticationFlow(processor, model);
            form.putSingle("username", "+8613800000000");
            form.putSingle("password", "fixture-password");
            form.putSingle("mode", "password"); form.putSingle("intent", "submit");
            when(client.authorizeLogin(anyString())).thenReturn(new PinvouAuthClient.LoginAuthorization(true, "fixture-account-user"));
        }

        AuthenticationProcessor.Result attempt(boolean browser) {
            return attempt(browser, false);
        }

        AuthenticationProcessor.Result attempt(boolean browser, boolean expectSuccess) {
            AuthenticatorFactory authenticatorFactory = browser ? new PinvouBrowserAuthenticatorFactory() : new MobileDirectGrantAuthenticatorFactory();
            execution.setAuthenticator(authenticatorFactory.getId());
            doReturn(authenticatorFactory).when(factory).getProviderFactory(Authenticator.class, authenticatorFactory.getId());
            Authenticator authenticator = browser ? new PinvouBrowserAuthenticator(client) : new MobileDirectGrantAuthenticator(client);
            var result = spy(processor.createAuthenticatorContext(execution, authenticator, List.of(execution)));
            LoginFormsProvider forms = mock(LoginFormsProvider.class, RETURNS_SELF);
            when(forms.createForm(anyString())).thenReturn(Response.status(401).build());
            doReturn(forms).when(result).form();
            if (browser) authenticator.action(result); else authenticator.authenticate(result);
            if (expectSuccess) assertEquals(FlowStatus.SUCCESS, result.getStatus());
            else assertNotEquals(FlowStatus.SUCCESS, result.getStatus());
            flow.processResult(result, false);
            return result;
        }

        void assertFailures(int count) {
            verify(protector, times(count)).failedLogin(eq(realm), eq(user), any(), any(), any());
        }
    }

    @Test void wrongKnownPasswordIsCountedThroughTheOfficialFlowAndUnknownRetryCannotReuseIt() throws Exception {
        for (boolean browser : new boolean[]{true, false}) {
            Fixture f = new Fixture();
            when(f.user.credentialManager().isValid(any(UserCredentialModel.class))).thenReturn(false);
            var wrong = f.attempt(browser);
            assertEquals(FlowStatus.FAILURE_CHALLENGE, wrong.getStatus());
            assertSame(f.user, f.authenticated.get()); f.assertFailures(1);
            verify(f.event).user(f.user);
            f.notes.put("ATTEMPTED_USERNAME", "fixture");
            f.exists = false;
            var unknown = f.attempt(browser);
            assertEquals(FlowStatus.CHALLENGE, unknown.getStatus());
            assertNull(f.authenticated.get()); assertFalse(f.notes.containsKey("ATTEMPTED_USERNAME"));
            f.assertFailures(1);
            verify(f.session.getProvider(PasswordHashProvider.class)).encodedCredential(anyString(), anyInt());
        }
    }

    @Test void disabledBlockedAndCorrectPasswordDeniedByAccountAreNotCredentialFailures() throws Exception {
        for (boolean browser : new boolean[]{true, false}) {
            for (String reason : List.of("disabled", "blocked", "account-denied", "dependency")) {
                Fixture f = new Fixture();
                when(f.user.credentialManager().isValid(any(UserCredentialModel.class))).thenReturn(true);
                if (reason.equals("disabled")) when(f.user.isEnabled()).thenReturn(false);
                if (reason.equals("blocked")) when(f.protector.isTemporarilyDisabled(f.session, f.realm, f.user)).thenReturn(true);
                if (reason.equals("account-denied")) when(f.client.authorizeLogin(anyString())).thenReturn(new PinvouAuthClient.LoginAuthorization(false, ""));
                if (reason.equals("dependency")) when(f.client.authorizeLogin(anyString())).thenThrow(new PinvouAuthClient.AuthClientException(503, "AUTH_SERVICE_UNAVAILABLE"));
                f.authenticated.set(f.user); f.notes.put("ATTEMPTED_USERNAME", "fixture");
                var rejected = f.attempt(browser);
                assertEquals(FlowStatus.CHALLENGE, rejected.getStatus(), reason);
                assertNull(f.authenticated.get()); f.assertFailures(0);
            }
        }
    }

    @Test void missingPasswordsAndWrongPasswordsForDisabledUsersDoNotCountAgainstPriorUsers() throws Exception {
        for (boolean browser : new boolean[]{true, false}) {
            for (String reason : List.of("missing", "empty", "disabled", "blocked")) {
                Fixture f = new Fixture();
                when(f.user.credentialManager().isValid(any(UserCredentialModel.class))).thenReturn(false);
                if (reason.equals("missing")) f.form.remove("password");
                if (reason.equals("empty")) f.form.putSingle("password", "");
                if (reason.equals("disabled")) when(f.user.isEnabled()).thenReturn(false);
                if (reason.equals("blocked")) when(f.protector.isTemporarilyDisabled(f.session, f.realm, f.user)).thenReturn(true);
                f.authenticated.set(f.user); f.notes.put("ATTEMPTED_USERNAME", "fixture");
                var rejected = f.attempt(browser);
                assertEquals(FlowStatus.CHALLENGE, rejected.getStatus(), reason);
                assertNull(f.authenticated.get()); f.assertFailures(0);
            }
        }
    }

    @Test void browserRegistrationUsesTheTrustedConnectionForAllPrechecksAndCaptcha() throws Exception {
        Fixture f = new Fixture();
        when(f.session.getContext().getConnection().getRemoteAddr()).thenReturn("198.51.100.21");
        when(f.client.precheckPassword(anyString(), anyString(), anyString()))
            .thenReturn(Map.of("password_handle", "fixture-handle", "password_policy_version", "fixture-policy"));
        when(f.client.createCaptcha(anyString(), anyString(), anyString(), anyString())).thenReturn(Map.of());
        f.form.putSingle("intent", "request_sms"); f.form.putSingle("mode", "register");
        f.form.putSingle("username", "fixture"); f.form.putSingle("mobile", "13800000000");
        f.form.putSingle("password_confirmation", "fixture-password"); f.form.putSingle("device_id", "fixture-device");
        f.attempt(true);
        verify(f.client).precheckPassword("fixture-password", "198.51.100.21", "fixture-device");
        verify(f.client).precheckRegistration("fixture", "+8613800000000", "fixture-handle", "fixture-policy", "198.51.100.21", "fixture-device");
        verify(f.client).createCaptcha("+8613800000000", "REGISTER", "198.51.100.21", "fixture-device");
        f.assertFailures(0);
    }

    @Test void successfulPasswordAndBrowserModeSwitchKeepTheirExistingAuthenticationSemantics() throws Exception {
        for (boolean browser : new boolean[]{true, false}) {
            Fixture f = new Fixture();
            when(f.user.credentialManager().isValid(any(UserCredentialModel.class))).thenReturn(true);
            f.attempt(browser, true);
            assertSame(f.user, f.authenticated.get()); f.assertFailures(0);
        }
        Fixture f = new Fixture();
        f.authenticated.set(f.user);
        f.form.putSingle("intent", "switch_mode"); f.form.putSingle("target_mode", "sms");
        f.attempt(true);
        assertSame(f.user, f.authenticated.get(), "rendering another mode is not a new password attempt");
        f.assertFailures(0);
        verifyNoInteractions(f.client);
    }

    @Test void aWrongPasswordUserCannotConflictWithANewSMSCredentialForAnotherUser() throws Exception {
        Fixture f = new Fixture();
        when(f.user.credentialManager().isValid(any(UserCredentialModel.class))).thenReturn(false);
        f.attempt(true); f.assertFailures(1);
        f.form.putSingle("intent", "switch_mode"); f.form.putSingle("target_mode", "sms");
        f.attempt(true);
        assertSame(f.user, f.authenticated.get(), "pure mode rendering preserves the old attempt");
        UserModel smsUser = mock(UserModel.class);
        when(smsUser.getId()).thenReturn("fixture-sms-user"); when(smsUser.isEnabled()).thenReturn(true);
        when(f.session.users().getUserById(f.realm, "fixture-sms-user")).thenReturn(smsUser);
        when(f.client.verifySMS(anyString(), anyString(), anyString(), anyString(), anyString())).thenReturn("fixture-sms-user");
        f.notes.put("pinvou.browser.mobile", "+8613800000000");
        f.notes.put("pinvou.browser.sms_challenge", "fixture-challenge");
        f.form.putSingle("intent", "submit"); f.form.putSingle("mode", "sms"); f.form.putSingle("sms_code", "000000");
        f.attempt(true, true);
        assertSame(smsUser, f.authenticated.get()); f.assertFailures(1);
    }

    @Test void newRegistrationAndResetSubmissionsClearOnlyFailedPasswordAttribution() throws Exception {
        for (String mode : List.of("register", "reset")) {
            Fixture f = new Fixture();
            when(f.user.credentialManager().isValid(any(UserCredentialModel.class))).thenReturn(false);
            f.attempt(true); f.assertFailures(1);
            f.notes.put("pinvou.browser.mobile", "+8613800000000");
            f.notes.put("pinvou.browser.sms_challenge", "fixture-challenge");
            f.form.putSingle("mode", mode); f.form.putSingle("sms_code", "000000");
            if (mode.equals("register")) {
                String operation = "3eb750de-11fa-47d5-b43b-508d8c520317";
                String account = "7101d101-fbf3-47bd-b786-6f92d29fb3dd";
                UserModel registered = mock(UserModel.class);
                when(registered.getId()).thenReturn("registered-fixture-user"); when(registered.isEnabled()).thenReturn(true);
                when(registered.getFirstAttribute("pinvou_registration_operation_id")).thenReturn(operation);
                when(f.session.users().searchForUserByUserAttributeStream(f.realm, "pinvou_registration_operation_id", operation))
                    .thenAnswer(i -> Stream.of(registered));
                when(f.client.register(anyMap(), anyString(), anyString()))
                    .thenReturn(Map.of("status", "ACTIVE", "registration_operation_id", operation, "user_id", account));
                when(f.client.authorizeLogin("registered-fixture-user")).thenReturn(new PinvouAuthClient.LoginAuthorization(true, account));
                f.notes.put("pinvou.browser.registration_operation", operation);
                f.attempt(true, true);
                assertSame(registered, f.authenticated.get());
            } else {
                f.attempt(true);
                assertNull(f.authenticated.get());
                verify(f.client).resetPassword(anyMap(), anyString(), anyString());
            }
            assertFalse(f.notes.containsKey("pinvou.browser.failed_password_user"));
            f.assertFailures(1);
        }

        // A real authenticated identity with no failed-attempt marker is not
        // cleared merely because a reset form is submitted in that session.
        Fixture authenticated = new Fixture();
        authenticated.authenticated.set(authenticated.user);
        authenticated.notes.put("pinvou.browser.mobile", "+8613800000000");
        authenticated.notes.put("pinvou.browser.sms_challenge", "fixture-challenge");
        authenticated.form.putSingle("mode", "reset"); authenticated.form.putSingle("sms_code", "000000");
        authenticated.attempt(true);
        assertSame(authenticated.user, authenticated.authenticated.get()); authenticated.assertFailures(0);
    }
}
