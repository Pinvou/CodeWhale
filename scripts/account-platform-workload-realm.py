"""Emit a secret-free Keycloak workload realm; never contact a server."""

import argparse
import json
import re

SERVICES = (
    "account-service", "identity-service", "billing-service", "token-gateway",
    "keycloak-mobile-authenticator", "token-credential-portal", "token-command-executor",
)
AUDIENCE = "pinvou-account-platform-internal"


def build_realm(name="pinvou-workload", audience=AUDIENCE):
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,63}-workload", name):
        raise ValueError("a separate workload realm name is required")
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9:._-]{0,127}", audience):
        raise ValueError("a bounded internal audience is required")
    clients = []
    for service in SERVICES:
        clients.append({
            "clientId": service,
            "name": service,
            "enabled": True,
            "protocol": "openid-connect",
            "clientAuthenticatorType": "client-secret",
            "publicClient": False,
            "bearerOnly": False,
            "serviceAccountsEnabled": True,
            "standardFlowEnabled": False,
            "implicitFlowEnabled": False,
            "directAccessGrantsEnabled": False,
            "authorizationServicesEnabled": False,
            "fullScopeAllowed": False,
            "redirectUris": [],
            "webOrigins": [],
            "defaultClientScopes": [],
            "optionalClientScopes": [],
            "attributes": {
                "access.token.lifespan": "300",
                "access.token.signed.response.alg": "RS256",
                "use.refresh.tokens": "false",
                "client_credentials.use_refresh_token": "false",
                "oauth2.device.authorization.grant.enabled": "false",
                "oidc.ciba.grant.enabled": "false",
                "standard.token.exchange.enabled": "false",
            },
            "protocolMappers": [{
                "name": "internal-api-audience",
                "protocol": "openid-connect",
                "protocolMapper": "oidc-audience-mapper",
                "consentRequired": False,
                "config": {
                    "included.custom.audience": audience,
                    "access.token.claim": "true",
                    "id.token.claim": "false",
                    "introspection.token.claim": "true",
                },
            }],
        })
    return {
        "realm": name,
        "enabled": True,
        "sslRequired": "all",
        "registrationAllowed": False,
        "resetPasswordAllowed": False,
        "rememberMe": False,
        "loginWithEmailAllowed": False,
        "bruteForceProtected": True,
        "accessTokenLifespan": 300,
        "defaultSignatureAlgorithm": "RS256",
        "revokeRefreshToken": True,
        "refreshTokenMaxReuse": 0,
        # Omitting these causes Keycloak import to create and attach built-in
        # scopes even when each client's scope arrays are explicitly empty.
        "clientScopes": [],
        "defaultDefaultClientScopes": [],
        "defaultOptionalClientScopes": [],
        "clients": clients,
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--realm", default="pinvou-workload")
    parser.add_argument("--audience", default=AUDIENCE)
    args = parser.parse_args()
    try:
        print(json.dumps(build_realm(args.realm, args.audience), indent=2))
    except ValueError as error:
        parser.error(str(error))
