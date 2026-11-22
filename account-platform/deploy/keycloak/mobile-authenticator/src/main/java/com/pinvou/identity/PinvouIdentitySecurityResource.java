package com.pinvou.identity;

import jakarta.ws.rs.Consumes;
import jakarta.ws.rs.NotFoundException;
import jakarta.ws.rs.POST;
import jakarta.ws.rs.Path;
import jakarta.ws.rs.PathParam;
import jakarta.ws.rs.Produces;
import jakarta.ws.rs.WebApplicationException;
import jakarta.ws.rs.core.Context;
import jakarta.ws.rs.core.HttpHeaders;
import jakarta.ws.rs.core.MediaType;
import jakarta.ws.rs.core.Response;
import jakarta.ws.rs.core.UriInfo;
import jakarta.persistence.LockModeType;
import org.keycloak.connections.jpa.JpaConnectionProvider;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.ModelDuplicateException;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserCredentialModel;
import org.keycloak.models.UserModel;
import org.keycloak.models.jpa.entities.UserAttributeEntity;
import org.keycloak.models.jpa.entities.UserEntity;
import org.keycloak.services.managers.AppAuthManager;
import org.keycloak.services.managers.AuthenticationManager;
import org.keycloak.services.resources.admin.AdminAuth;
import org.keycloak.services.resources.admin.fgap.AdminPermissions;

import java.util.Map;
import java.util.UUID;

@Path("/")
@Consumes(MediaType.APPLICATION_JSON)
@Produces(MediaType.APPLICATION_JSON)
public final class PinvouIdentitySecurityResource {
    static final String OPERATION_ATTRIBUTE = "pinvou_identity_security_operation_id";
    static final String FENCE_ATTRIBUTE = "pinvou_identity_security_fence_version";

    private final KeycloakSession session;

    @Context HttpHeaders headers;
    @Context UriInfo uriInfo;

    PinvouIdentitySecurityResource(KeycloakSession session) {
        this.session = session;
    }

    @POST
    @Path("users/{subject}/password-reset")
    public Response resetPassword(@PathParam("subject") String subject, PasswordResetRequest request) {
        RealmModel realm = session.getContext().getRealm();
        AdminAuth admin = authenticateAdmin(realm);
        if (request == null || request.operationId == null || request.operationId.version() != 7 || request.fenceVersion <= 0) {
            return problem(Response.Status.BAD_REQUEST, "PASSWORD_RESET_COMMAND_INVALID");
        }

        UserModel candidate = realmUser(realm, subject);
        AdminPermissions.evaluator(session, realm, admin).users().requireResetPassword(candidate);
        // Permission and realm scope are established before acquiring a
        // database lock. Otherwise an authenticated caller without access to
        // this realm could use arbitrary global subject ids as a lock DoS.
        LockedUser locked = lockRealmUser(realm, subject, candidate);
        UserModel user = locked.user();
        AdminPermissions.evaluator(session, realm, admin).users().requireResetPassword(user);

        FenceDecision decision = evaluateFence(locked.fenceVersion(), locked.operationId(), request.operationId, request.fenceVersion, request.password != null);
        if (decision == FenceDecision.CONFLICT) {
            return problem(Response.Status.CONFLICT, "PASSWORD_REFERENCE_CONFLICT");
        }
        if (decision == FenceDecision.APPLIED) {
            return Response.noContent().build();
        }
        if (decision == FenceDecision.NOT_APPLIED) {
            return problem(Response.Status.NOT_FOUND, "PASSWORD_RESET_NOT_APPLIED");
        }
        if (request.password.length() < 8 || request.password.length() > 128) {
            return problem(Response.Status.BAD_REQUEST, "PASSWORD_INVALID");
        }
        if (!user.credentialManager().updateCredential(UserCredentialModel.password(request.password))) {
            throw new WebApplicationException(Response.Status.INTERNAL_SERVER_ERROR);
        }
        setCommonFence(user, request.operationId, request.fenceVersion);
        // Compatibility markers remain admin-only and are written in the same
        // Keycloak transaction as the credential and common monotonic fence.
        user.setSingleAttribute("pinvou_password_reset_operation_id", request.operationId.toString());
        user.setSingleAttribute("pinvou_password_reset_fence_version", Long.toString(request.fenceVersion));
        return Response.noContent().build();
    }

    @POST
    @Path("users/{subject}/profile-change")
    public Response updateProfile(@PathParam("subject") String subject, ProfileChangeRequest request) {
        RealmModel realm = session.getContext().getRealm();
        AdminAuth admin = authenticateAdmin(realm);
        if (!validProfileRequest(request)) {
            return problem(Response.Status.BAD_REQUEST, "PROFILE_CHANGE_COMMAND_INVALID");
        }
        UserModel candidate = realmUser(realm, subject);
        AdminPermissions.evaluator(session, realm, admin).users().requireManage(candidate);
        LockedUser locked = lockRealmUser(realm, subject, candidate);
        UserModel user = locked.user();
        AdminPermissions.evaluator(session, realm, admin).users().requireManage(user);

        FenceDecision decision = evaluateFence(
                locked.fenceVersion(),
                locked.operationId(),
                request.operationId,
                request.fenceVersion,
                true);
        if (decision == FenceDecision.CONFLICT) {
            return problem(Response.Status.CONFLICT, "IDENTITY_SECURITY_FENCE_CONFLICT");
        }
        if (decision == FenceDecision.APPLIED) {
            return Response.noContent().build();
        }
        try {
            if ("USERNAME".equals(request.field)) {
                user.setUsername(request.value);
            } else {
                user.setSingleAttribute("phone_number", request.value);
                user.setSingleAttribute("phone_number_verified", "true");
            }
        } catch (ModelDuplicateException duplicate) {
            return problem(Response.Status.CONFLICT, "USERNAME_ALREADY_REGISTERED");
        }
        setCommonFence(user, request.operationId, request.fenceVersion);
        return Response.noContent().build();
    }

    private static boolean validProfileRequest(ProfileChangeRequest request) {
        if (request == null || request.operationId == null || request.operationId.version() != 7 || request.fenceVersion <= 0 || request.value == null) {
            return false;
        }
        if ("USERNAME".equals(request.field)) {
            return request.value.length() >= 3 && request.value.length() <= 64 && request.value.equals(request.value.trim());
        }
        return "MOBILE".equals(request.field) && request.value.matches("\\+861[3-9][0-9]{9}");
    }

    private UserModel realmUser(RealmModel realm, String subject) {
        UserModel user = session.users().getUserById(realm, subject);
        if (user == null) throw new NotFoundException();
        return user;
    }

    private LockedUser lockRealmUser(RealmModel realm, String subject, UserModel cacheAwareUser) {
        JpaConnectionProvider jpa = session.getProvider(JpaConnectionProvider.class);
        UserEntity entity = jpa == null ? null : jpa.getEntityManager().find(UserEntity.class, subject, LockModeType.PESSIMISTIC_WRITE);
        if (entity == null || !realm.getId().equals(entity.getRealmId())) throw new NotFoundException();
        // The locked JPA row is authoritative only for the fence decision.
        // Mutations must continue through session.users() so Keycloak's
        // UserCacheSession registers the cluster invalidation on commit.
        jpa.getEntityManager().refresh(entity, LockModeType.PESSIMISTIC_WRITE);
        if (!subject.equals(cacheAwareUser.getId())) throw new NotFoundException();
        return new LockedUser(
                cacheAwareUser,
                parseFence(entityAttribute(entity, FENCE_ATTRIBUTE)),
                entityAttribute(entity, OPERATION_ATTRIBUTE));
    }

    private static String entityAttribute(UserEntity entity, String name) {
        if (entity.getAttributes() == null) return null;
        return entity.getAttributes().stream()
                .filter(attribute -> name.equals(attribute.getName()))
                .map(UserAttributeEntity::getValue)
                .findFirst()
                .orElse(null);
    }

    private static void setCommonFence(UserModel user, UUID operationId, long fenceVersion) {
        user.setSingleAttribute(OPERATION_ATTRIBUTE, operationId.toString());
        user.setSingleAttribute(FENCE_ATTRIBUTE, Long.toString(fenceVersion));
    }

    private AdminAuth authenticateAdmin(RealmModel realm) {
        AuthenticationManager.AuthResult auth = new AppAuthManager.BearerTokenAuthenticator(session)
                .setRealm(realm)
                .setUriInfo(uriInfo)
                .setConnection(session.getContext().getConnection())
                .setHeaders(headers)
                .setRequest(session.getContext().getHttpRequest())
                .authenticate();
        if (auth == null) throw new WebApplicationException(Response.Status.UNAUTHORIZED);
        return new AdminAuth(realm, auth.getToken(), auth.getUser(), auth.getClient());
    }

    private static long parseFence(String value) {
        if (value == null || value.isBlank()) return 0;
        try {
            long parsed = Long.parseLong(value);
            return Math.max(parsed, 0);
        } catch (NumberFormatException ignored) {
            throw new WebApplicationException(Response.Status.CONFLICT);
        }
    }

    enum FenceDecision { APPLIED, WRITE, NOT_APPLIED, CONFLICT }

    static FenceDecision evaluateFence(long currentFence, String currentOperation, UUID operationId, long fenceVersion, boolean hasPassword) {
        if (currentFence > fenceVersion || currentFence == fenceVersion && !operationId.toString().equals(currentOperation)) {
            return FenceDecision.CONFLICT;
        }
        if (currentFence == fenceVersion && operationId.toString().equals(currentOperation)) {
            return FenceDecision.APPLIED;
        }
        return hasPassword ? FenceDecision.WRITE : FenceDecision.NOT_APPLIED;
    }

    private static Response problem(Response.Status status, String code) {
        return Response.status(status).entity(Map.of("code", code)).build();
    }

    public static final class PasswordResetRequest {
        public UUID operationId;
        public long fenceVersion;
        public String password;
    }

    public static final class ProfileChangeRequest {
        public UUID operationId;
        public long fenceVersion;
        public String field;
        public String value;
    }

    private record LockedUser(UserModel user, long fenceVersion, String operationId) {}
}
