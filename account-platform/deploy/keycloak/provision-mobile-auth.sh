#!/bin/sh
set -eu
umask 077

KEYCLOAK_ADMIN_USERNAME=${KEYCLOAK_ADMIN_USERNAME:-${KC_BOOTSTRAP_ADMIN_USERNAME:-}}
KEYCLOAK_ADMIN_PASSWORD=${KEYCLOAK_ADMIN_PASSWORD:-${KC_BOOTSTRAP_ADMIN_PASSWORD:-}}
if [ -z "$KEYCLOAK_ADMIN_USERNAME" ] && [ -n "${KEYCLOAK_ADMIN_USERNAME_FILE:-}" ]; then
  KEYCLOAK_ADMIN_USERNAME=$(cat "$KEYCLOAK_ADMIN_USERNAME_FILE")
fi
if [ -z "$KEYCLOAK_ADMIN_PASSWORD" ] && [ -n "${KEYCLOAK_ADMIN_PASSWORD_FILE:-}" ]; then
  KEYCLOAK_ADMIN_PASSWORD=$(cat "$KEYCLOAK_ADMIN_PASSWORD_FILE")
fi
if [ -z "${KEYCLOAK_PROVISIONER_CLIENT_SECRET:-}" ] && [ -n "${KEYCLOAK_PROVISIONER_CLIENT_SECRET_FILE:-}" ]; then
  KEYCLOAK_PROVISIONER_CLIENT_SECRET=$(cat "$KEYCLOAK_PROVISIONER_CLIENT_SECRET_FILE")
fi
: "${KEYCLOAK_ADMIN_USERNAME:?set KEYCLOAK_ADMIN_USERNAME or KC_BOOTSTRAP_ADMIN_USERNAME}"
: "${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD or KC_BOOTSTRAP_ADMIN_PASSWORD}"
: "${KEYCLOAK_PROVISIONER_CLIENT_SECRET:?set KEYCLOAK_PROVISIONER_CLIENT_SECRET}"

KCADM_BINARY=/opt/keycloak/bin/kcadm.sh
KCADM_CONFIG=$(mktemp "${TMPDIR:-/tmp}/pinvou-kcadm.XXXXXXXX")
trap 'rm -f "$KCADM_CONFIG"' 0 HUP INT TERM
kcadm() {
  "$KCADM_BINARY" "$@" --config "$KCADM_CONFIG"
}
KCADM=kcadm
REALM=${KEYCLOAK_REALM:-pinvou}
DESKTOP_CLIENT=${PINVOU_OIDC_CLIENT_ID:-pinvou-desktop-dev}
PROVISIONER_CLIENT=${KEYCLOAK_PROVISIONER_CLIENT_ID:-pinvou-account-provisioner}
FLOW=pinvou-mobile-direct-grant
PROVIDER=pinvou-mobile-direct-grant
BROWSER_FLOW=pinvou-browser
BROWSER_PROVIDER=pinvou-browser-authenticator
ADMIN_SERVER=${KEYCLOAK_ADMIN_SERVER:-http://127.0.0.1:8080}

PORTAL_CLIENT=${PORTAL_OIDC_CLIENT_ID:-pinvou-portal}
HANDOFF_CLIENT=${PORTAL_HANDOFF_OIDC_CLIENT_ID:-pinvou-portal-handoff}
ACCOUNT_AUDIENCE=${PINVOU_OIDC_AUDIENCE:-$DESKTOP_CLIENT}
PORTAL_URL=${PORTAL_PUBLIC_URL:?set PORTAL_PUBLIC_URL}
PORTAL_URL=${PORTAL_URL%/}
HANDOFF_ENABLED=${PORTAL_SESSION_HANDOFF:-false}
for id in "$REALM" "$DESKTOP_CLIENT" "$PROVISIONER_CLIENT" "$PORTAL_CLIENT" "$HANDOFF_CLIENT" "$ACCOUNT_AUDIENCE"; do
  case "$id" in ''|*[!A-Za-z0-9._-]*) printf 'OIDC identifiers must be nonempty safe names\n' >&2; exit 1;; esac
done
if [ "$PORTAL_CLIENT" = "$DESKTOP_CLIENT" ] || [ "$HANDOFF_CLIENT" = "$PORTAL_CLIENT" ] || [ "$HANDOFF_CLIENT" = "$DESKTOP_CLIENT" ] || [ "$PROVISIONER_CLIENT" = "$DESKTOP_CLIENT" ] || [ "$PROVISIONER_CLIENT" = "$PORTAL_CLIENT" ] || [ "$PROVISIONER_CLIENT" = "$HANDOFF_CLIENT" ]; then
  printf 'Desktop, online Portal, handoff and provisioner must be distinct clients\n' >&2; exit 1
fi
case "$PORTAL_URL" in http://*|https://*) ;; *) printf 'PORTAL_PUBLIC_URL must be HTTP(S)\n' >&2; exit 1;; esac
case "$PORTAL_URL" in *'@'*|*'?'*|*'#'*|*'"'*|*'\'*|*[[:space:]]*) printf 'PORTAL_PUBLIC_URL contains forbidden URL characters\n' >&2; exit 1;; esac
authority=${PORTAL_URL#*://}
authority=${authority%%/*}
case "$authority" in ''|:*|*'"'*|*'\'*) printf 'PORTAL_PUBLIC_URL must contain an authority\n' >&2; exit 1;; esac
for secret in "$KEYCLOAK_PROVISIONER_CLIENT_SECRET"; do
  count=$(printf '%s' "$secret" | wc -c)
  if [ "$count" -lt 32 ] || [ "$count" -gt 4096 ] || [ "$(printf '%s' "$secret" | LC_ALL=C tr -d '[:print:]' | wc -c)" -ne 0 ]; then
    printf 'Provisioner secret must be one value of 32 to 4096 bytes\n' >&2; exit 1
  fi
done
case "$HANDOFF_ENABLED" in true|false) ;; *) printf 'PORTAL_SESSION_HANDOFF must be true or false\n' >&2; exit 1;; esac
HANDOFF_SECRET=''
if [ "$HANDOFF_ENABLED" = true ]; then
  : "${PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE:?set the handoff secret file}"
  HANDOFF_SECRET=$(cat "$PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE")
  count=$(printf '%s' "$HANDOFF_SECRET" | wc -c)
  if [ "$count" -lt 32 ] || [ "$count" -gt 4096 ] || [ "$(printf '%s' "$HANDOFF_SECRET" | LC_ALL=C tr -d '[:print:]' | wc -c)" -ne 0 ]; then
    printf 'Handoff secret must be one value of 32 to 4096 bytes\n' >&2; exit 1
  fi
fi

# F04 credential entry is an explicitly installed, separate public online client.
CREDENTIAL_URL=${CREDENTIAL_PORTAL_PUBLIC_URL:-}
CREDENTIAL_CLIENT=${CREDENTIAL_OIDC_CLIENT_ID:-pinvou-credentials}
if [ -n "$CREDENTIAL_URL" ]; then
  CREDENTIAL_URL=${CREDENTIAL_URL%/}
  case "$CREDENTIAL_CLIENT" in ''|*[!A-Za-z0-9._-]*) printf 'Credential client identifier is invalid\n' >&2; exit 1;; esac
  for existing_client in "$DESKTOP_CLIENT" "$PORTAL_CLIENT" "$HANDOFF_CLIENT" "$PROVISIONER_CLIENT"; do
    if [ "$CREDENTIAL_CLIENT" = "$existing_client" ]; then printf 'Credential entry requires its own client\n' >&2; exit 1; fi
  done
  # The independent credential server enforces exactly these production origins.
  case "$CREDENTIAL_URL" in https://credentials.pinvou.com|https://credentials-uat.pinvou.com) ;; *) printf 'Credential public origin is not approved\n' >&2; exit 1;; esac
  if [ "$CREDENTIAL_URL" = "$PORTAL_URL" ]; then printf 'Credential and Portal origins must differ\n' >&2; exit 1; fi
fi

if [ -n "${KEYCLOAK_ADMIN_TRUSTSTORE:-}" ]; then
  KEYCLOAK_ADMIN_TRUSTSTORE_PASSWORD=${KEYCLOAK_ADMIN_TRUSTSTORE_PASSWORD:-}
  if [ -z "$KEYCLOAK_ADMIN_TRUSTSTORE_PASSWORD" ] && [ -n "${KEYCLOAK_ADMIN_TRUSTSTORE_PASSWORD_FILE:-}" ]; then
    KEYCLOAK_ADMIN_TRUSTSTORE_PASSWORD=$(cat "$KEYCLOAK_ADMIN_TRUSTSTORE_PASSWORD_FILE")
  fi
  : "${KEYCLOAK_ADMIN_TRUSTSTORE_PASSWORD:?set truststore password or password file}"
  "$KCADM" config truststore --trustpass "$KEYCLOAK_ADMIN_TRUSTSTORE_PASSWORD" "$KEYCLOAK_ADMIN_TRUSTSTORE" >/dev/null
fi

find_id_by_name() {
  wanted=$1
  while IFS=, read -r id name _rest; do
    if [ "$name" = "$wanted" ]; then
      printf '%s\n' "$id"
      return 0
    fi
  done
}

attempt=0
until "$KCADM" config credentials --server "$ADMIN_SERVER" --realm master \
  --user "$KEYCLOAK_ADMIN_USERNAME" --password "$KEYCLOAK_ADMIN_PASSWORD" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge "${KEYCLOAK_PROVISION_MAX_ATTEMPTS:-60}" ]; then
    printf 'Keycloak realm provisioning could not authenticate after %s attempts\n' "$attempt" >&2
    exit 1
  fi
  sleep 2
done

client_id=$("$KCADM" get clients -r "$REALM" -q "clientId=$PROVISIONER_CLIENT" --fields id,clientId --format csv --noquotes | find_id_by_name "$PROVISIONER_CLIENT")
escaped_provisioner_secret=$(printf '%s' "$KEYCLOAK_PROVISIONER_CLIENT_SECRET" | sed 's/\\/\\\\/g; s/"/\\"/g')
provisioner_json=$(cat <<JSON
{"clientId":"$PROVISIONER_CLIENT","enabled":true,"protocol":"openid-connect","publicClient":false,"serviceAccountsEnabled":true,"standardFlowEnabled":false,"directAccessGrantsEnabled":false,"implicitFlowEnabled":false,"secret":"$escaped_provisioner_secret","attributes":{"standard.token.exchange.enabled":"false"}}
JSON
)
if [ -z "$client_id" ]; then
  client_id=$(printf '%s' "$provisioner_json" | "$KCADM" create clients -r "$REALM" -f - -i)
else
  printf '%s' "$provisioner_json" | "$KCADM" update "clients/$client_id" -r "$REALM" -f - >/dev/null
fi
service_user=$("$KCADM" get "clients/$client_id/service-account-user" -r "$REALM" --fields id --format csv --noquotes | tail -n 1)
realm_management=$("$KCADM" get clients -r "$REALM" -q clientId=realm-management --fields id,clientId --format csv --noquotes | find_id_by_name realm-management)
for role in manage-users view-users query-users; do
  "$KCADM" add-roles -r "$REALM" --uid "$service_user" --cid "$realm_management" --rolename "$role" >/dev/null
done

flow_id=$("$KCADM" get authentication/flows -r "$REALM" --fields id,alias --format csv --noquotes | find_id_by_name "$FLOW")
if [ -z "$flow_id" ]; then
  flow_id=$("$KCADM" create authentication/flows -r "$REALM" -i \
    -s "alias=$FLOW" -s 'description=Pinvou mobile password or SMS direct grant' \
    -s providerId=basic-flow -s topLevel=true -s builtIn=false)
fi
if ! "$KCADM" get "authentication/flows/$FLOW/executions" -r "$REALM" --format csv --noquotes | grep -q "$PROVIDER"; then
  "$KCADM" create "authentication/flows/$FLOW/executions/execution" -r "$REALM" -s "provider=$PROVIDER" >/dev/null
fi
execution_id=$("$KCADM" get "authentication/flows/$FLOW/executions" -r "$REALM" --fields id,providerId --format csv --noquotes | find_id_by_name "$PROVIDER")
"$KCADM" update "authentication/flows/$FLOW/executions" -r "$REALM" \
  -s "id=$execution_id" \
  -s requirement=REQUIRED -n >/dev/null
browser_flow_id=$("$KCADM" get authentication/flows -r "$REALM" --fields id,alias --format csv --noquotes | find_id_by_name "$BROWSER_FLOW")
if [ -z "$browser_flow_id" ]; then
  browser_flow_id=$("$KCADM" create authentication/flows -r "$REALM" -i \
    -s "alias=$BROWSER_FLOW" -s 'description=Pinvou browser cookie, password, SMS, verified mobile registration, and password recovery' \
    -s providerId=basic-flow -s topLevel=true -s builtIn=false)
fi
if ! "$KCADM" get "authentication/flows/$BROWSER_FLOW/executions" -r "$REALM" --format csv --noquotes | grep -q 'auth-cookie'; then
  "$KCADM" create "authentication/flows/$BROWSER_FLOW/executions/execution" -r "$REALM" -s provider=auth-cookie >/dev/null
fi
if ! "$KCADM" get "authentication/flows/$BROWSER_FLOW/executions" -r "$REALM" --format csv --noquotes | grep -q "$BROWSER_PROVIDER"; then
  "$KCADM" create "authentication/flows/$BROWSER_FLOW/executions/execution" -r "$REALM" -s "provider=$BROWSER_PROVIDER" >/dev/null
fi
for browser_provider in auth-cookie "$BROWSER_PROVIDER"; do
  browser_execution_id=$("$KCADM" get "authentication/flows/$BROWSER_FLOW/executions" -r "$REALM" --fields id,providerId --format csv --noquotes | find_id_by_name "$browser_provider")
  "$KCADM" update "authentication/flows/$BROWSER_FLOW/executions" -r "$REALM" -s "id=$browser_execution_id" -s requirement=ALTERNATIVE -n >/dev/null
done
"$KCADM" update "realms/$REALM" -s "directGrantFlow=$FLOW" -s "browserFlow=$BROWSER_FLOW" \
  -s registrationAllowed=false -s resetPasswordAllowed=false -s rememberMe=true -s loginTheme=pinvou \
  -s internationalizationEnabled=true -s 'supportedLocales=["zh-CN","en","ja"]' -s defaultLocale=zh-CN >/dev/null

# BEGIN PINVOU_REALM_HARDENING
# Apply to the existing production realm, not only to the development import.
# Read back each non-secret field; a partial or ignored update fails closed.
"$KCADM" update "realms/$REALM" \
  -s bruteForceProtected=true -s permanentLockout=false -s failureFactor=10 \
  -s maxFailureWaitSeconds=900 -s minimumQuickLoginWaitSeconds=60 \
  -s waitIncrementSeconds=60 -s quickLoginCheckMilliSeconds=1000 -s maxDeltaTimeSeconds=43200 \
  -s accessTokenLifespan=600 -s revokeRefreshToken=true -s refreshTokenMaxReuse=0 >/dev/null
"$KCADM" update events/config -r "$REALM" \
  -s eventsEnabled=true -s eventsExpiration=2592000 -s adminEventsEnabled=true \
  -s 'enabledEventTypes=["LOGIN","LOGIN_ERROR","LOGOUT","REGISTER","REGISTER_ERROR","UPDATE_PASSWORD","UPDATE_PASSWORD_ERROR","REFRESH_TOKEN","REFRESH_TOKEN_ERROR"]' \
  -s adminEventsDetailsEnabled=false >/dev/null
require_realm_setting() {
  resource=$1
  field=$2
  expected=$3
  if [ "$resource" = "realms/$REALM" ]; then
    readback=$("$KCADM" get "$resource" --fields "$field" --format csv --noquotes) || {
      printf 'Keycloak hardening readback unavailable for %s\n' "$field" >&2; exit 1;
    }
  else
    readback=$("$KCADM" get "$resource" -r "$REALM" --fields "$field" --format csv --noquotes) || {
      printf 'Keycloak hardening readback unavailable for %s\n' "$field" >&2; exit 1;
    }
  fi
  actual=''
  while IFS= read -r line; do actual=$line; done <<PINVOU_READBACK
$readback
PINVOU_READBACK
  if [ "$actual" != "$expected" ]; then
    printf 'Keycloak hardening readback failed for %s\n' "$field" >&2
    exit 1
  fi
}
for setting in bruteForceProtected:true permanentLockout:false failureFactor:10 \
  maxFailureWaitSeconds:900 minimumQuickLoginWaitSeconds:60 waitIncrementSeconds:60 \
  quickLoginCheckMilliSeconds:1000 maxDeltaTimeSeconds:43200 accessTokenLifespan:600 \
  revokeRefreshToken:true refreshTokenMaxReuse:0; do
  require_realm_setting "realms/$REALM" "${setting%%:*}" "${setting#*:}"
done
for setting in eventsEnabled:true eventsExpiration:2592000 adminEventsEnabled:true adminEventsDetailsEnabled:false; do
  require_realm_setting events/config "${setting%%:*}" "${setting#*:}"
done
event_types=$("$KCADM" get events/config -r "$REALM" --fields enabledEventTypes) || {
  printf 'Keycloak audit event types readback unavailable\n' >&2; exit 1;
}
for event_type in LOGIN LOGIN_ERROR LOGOUT REGISTER REGISTER_ERROR UPDATE_PASSWORD UPDATE_PASSWORD_ERROR REFRESH_TOKEN REFRESH_TOKEN_ERROR; do
  case "$event_types" in
    *\""$event_type"\"*) ;;
    *) printf 'Keycloak required audit event missing: %s\n' "$event_type" >&2; exit 1;;
  esac
done
# END PINVOU_REALM_HARDENING

# Mobile-only accounts do not have email or legal-name fields at registration
# time. Keep those standard attributes available for later profile editing, but
# do not let Keycloak's VERIFY_PROFILE trigger block password or SMS grants.
# Phone attributes are managed by the account service and remain read-only to
# the end user.
"$KCADM" update users/profile -r "$REALM" -f - >/dev/null <<'PINVOU_MOBILE_USER_PROFILE'
{
  "attributes": [
    {
      "name": "username",
      "displayName": "${username}",
      "validations": {
        "length": { "min": 3, "max": 255 },
        "username-prohibited-characters": {},
        "up-username-not-idn-homograph": {}
      },
      "permissions": {
        "view": [ "admin", "user" ],
        "edit": [ "admin" ]
      },
      "multivalued": false
    },
    {
      "name": "email",
      "displayName": "${email}",
      "validations": {
        "email": {},
        "length": { "max": 255 }
      },
      "permissions": {
        "view": [ "admin", "user" ],
        "edit": [ "admin", "user" ]
      },
      "multivalued": false
    },
    {
      "name": "firstName",
      "displayName": "${firstName}",
      "validations": {
        "length": { "max": 255 },
        "person-name-prohibited-characters": {}
      },
      "permissions": {
        "view": [ "admin", "user" ],
        "edit": [ "admin", "user" ]
      },
      "multivalued": false
    },
    {
      "name": "lastName",
      "displayName": "${lastName}",
      "validations": {
        "length": { "max": 255 },
        "person-name-prohibited-characters": {}
      },
      "permissions": {
        "view": [ "admin", "user" ],
        "edit": [ "admin", "user" ]
      },
      "multivalued": false
    },
    {
      "name": "phone_number",
      "displayName": "Phone number",
      "validations": {
        "length": { "max": 16 }
      },
      "permissions": {
        "view": [ "admin", "user" ],
        "edit": [ "admin" ]
      },
      "multivalued": false
    },
    {
      "name": "phone_number_verified",
      "displayName": "Phone number verified",
      "permissions": {
        "view": [ "admin", "user" ],
        "edit": [ "admin" ]
      },
      "multivalued": false
    },
    {
      "name": "pinvou_registration_operation_id",
      "displayName": "Registration operation",
      "permissions": { "view": [ "admin" ], "edit": [ "admin" ] },
      "multivalued": false
    },
    {
      "name": "pinvou_bootstrap_managed",
      "displayName": "Bootstrap identity ownership",
      "permissions": { "view": [ "admin" ], "edit": [ "admin" ] },
      "multivalued": false
    },
    {
      "name": "pinvou_identity_security_operation_id",
      "displayName": "Identity security operation",
      "permissions": {
        "view": [ "admin" ],
        "edit": [ "admin" ]
      },
      "multivalued": false
    },
    {
      "name": "pinvou_identity_security_fence_version",
      "displayName": "Identity security fence",
      "permissions": {
        "view": [ "admin" ],
        "edit": [ "admin" ]
      },
      "multivalued": false
    },
    {
      "name": "pinvou_password_reset_operation_id",
      "displayName": "Password reset operation",
      "permissions": {
        "view": [ "admin" ],
        "edit": [ "admin" ]
      },
      "multivalued": false
    },
    {
      "name": "pinvou_password_reset_fence_version",
      "displayName": "Password reset fence",
      "permissions": {
        "view": [ "admin" ],
        "edit": [ "admin" ]
      },
      "multivalued": false
    }
  ],
  "groups": [
    {
      "name": "user-metadata",
      "displayHeader": "User metadata",
      "displayDescription": "Attributes which refer to user metadata"
    }
  ]
}
PINVOU_MOBILE_USER_PROFILE

desktop_id=$("$KCADM" get clients -r "$REALM" -q "clientId=$DESKTOP_CLIENT" --fields id,clientId --format csv --noquotes | find_id_by_name "$DESKTOP_CLIENT")
if [ -z "$desktop_id" ]; then
  printf 'The selected desktop client must already be imported from its realm configuration\n' >&2; exit 1
fi
for spec in 'pinvou-phone-number|phone_number|String' 'pinvou-phone-number-verified|phone_number_verified|boolean'; do
  name=${spec%%|*}; rest=${spec#*|}; attribute=${rest%%|*}; json_type=${rest##*|}
  mapper_id=$("$KCADM" get "clients/$desktop_id/protocol-mappers/models" -r "$REALM" --fields id,name --format csv --noquotes | find_id_by_name "$name")
  if [ -z "$mapper_id" ]; then
    action=create; endpoint="clients/$desktop_id/protocol-mappers/models"
  else
    action=update; endpoint="clients/$desktop_id/protocol-mappers/models/$mapper_id"
  fi
  "$KCADM" "$action" "$endpoint" -r "$REALM" \
    -s "name=$name" -s protocol=openid-connect -s protocolMapper=oidc-usermodel-attribute-mapper \
    -s "config.\"user.attribute\"=$attribute" -s "config.\"claim.name\"=$attribute" \
    -s "config.\"jsonType.label\"=$json_type" -s 'config."id.token.claim"=true' \
    -s 'config."access.token.claim"=true' -s 'config."userinfo.token.claim"=true' >/dev/null
done

# Public online Portal uses code + PKCE. Only the separate server-side client
# can exchange a desktop access token; neither client requests offline access.
portal_id=$("$KCADM" get clients -r "$REALM" -q "clientId=$PORTAL_CLIENT" --fields id,clientId --format csv --noquotes | find_id_by_name "$PORTAL_CLIENT")
portal_json=$(cat <<JSON
{"clientId":"$PORTAL_CLIENT","enabled":true,"protocol":"openid-connect","publicClient":true,"standardFlowEnabled":true,"directAccessGrantsEnabled":false,"implicitFlowEnabled":false,"serviceAccountsEnabled":false,"redirectUris":["$PORTAL_URL/auth/callback"],"webOrigins":[],"attributes":{"pkce.code.challenge.method":"S256","standard.token.exchange.enabled":"false"},"defaultClientScopes":["basic","roles","profile","email"],"optionalClientScopes":[]}
JSON
)
if [ -z "$portal_id" ]; then
  portal_id=$(printf '%s' "$portal_json" | "$KCADM" create clients -r "$REALM" -f - -i)
else
  printf '%s' "$portal_json" | "$KCADM" update "clients/$portal_id" -r "$REALM" -f - >/dev/null
fi

# The Account verifier keeps its existing audience. Both Portal token types
# explicitly carry it, while the desktop additionally carries handoff audience.
ensure_audience() {
  owner=$1
  name=$2
  audience=$3
  mapper_id=$("$KCADM" get "clients/$owner/protocol-mappers/models" -r "$REALM" --fields id,name --format csv --noquotes | find_id_by_name "$name")
  if [ -z "$mapper_id" ]; then
    action=create; endpoint="clients/$owner/protocol-mappers/models"
  else
    action=update; endpoint="clients/$owner/protocol-mappers/models/$mapper_id"
  fi
  "$KCADM" "$action" "$endpoint" -r "$REALM" \
    -s "name=$name" -s protocol=openid-connect -s protocolMapper=oidc-audience-mapper \
    -s "config.\"included.client.audience\"=$audience" -s 'config."access.token.claim"=true' >/dev/null
}
ensure_audience "$portal_id" pinvou-account-audience "$ACCOUNT_AUDIENCE"
"$KCADM" update "clients/$desktop_id" -r "$REALM" -s 'attributes."standard.token.exchange.enabled"=false' -s 'optionalClientScopes=["offline_access"]' >/dev/null
if [ "$HANDOFF_ENABLED" = true ]; then
  handoff_id=$("$KCADM" get clients -r "$REALM" -q "clientId=$HANDOFF_CLIENT" --fields id,clientId --format csv --noquotes | find_id_by_name "$HANDOFF_CLIENT")
  # Secret is supplied on stdin rather than in a process argument or log.
  escaped_secret=$(printf '%s' "$HANDOFF_SECRET" | sed 's/\\/\\\\/g; s/"/\\"/g')
  handoff_json=$(cat <<JSON
{"clientId":"$HANDOFF_CLIENT","enabled":true,"protocol":"openid-connect","publicClient":false,"clientAuthenticatorType":"client-secret","secret":"$escaped_secret","standardFlowEnabled":false,"directAccessGrantsEnabled":false,"implicitFlowEnabled":false,"serviceAccountsEnabled":false,"redirectUris":[],"webOrigins":[],"attributes":{"standard.token.exchange.enabled":"true","standard.token.exchange.enableRefreshRequestedTokenType":"false"},"defaultClientScopes":["basic","roles"],"optionalClientScopes":[]}
JSON
)
  if [ -z "$handoff_id" ]; then
    handoff_id=$(printf '%s' "$handoff_json" | "$KCADM" create clients -r "$REALM" -f - -i)
  else
    printf '%s' "$handoff_json" | "$KCADM" update "clients/$handoff_id" -r "$REALM" -f - >/dev/null
  fi
  ensure_audience "$desktop_id" pinvou-portal-handoff-audience "$HANDOFF_CLIENT"
  ensure_audience "$handoff_id" pinvou-account-audience "$ACCOUNT_AUDIENCE"
fi

if [ -n "$CREDENTIAL_URL" ]; then
  credential_id=$("$KCADM" get clients -r "$REALM" -q "clientId=$CREDENTIAL_CLIENT" --fields id,clientId --format csv --noquotes | find_id_by_name "$CREDENTIAL_CLIENT")
  credential_json=$(cat <<JSON
{"clientId":"$CREDENTIAL_CLIENT","enabled":true,"protocol":"openid-connect","publicClient":true,"standardFlowEnabled":true,"directAccessGrantsEnabled":false,"implicitFlowEnabled":false,"serviceAccountsEnabled":false,"redirectUris":["$CREDENTIAL_URL/oidc/callback"],"webOrigins":[],"attributes":{"pkce.code.challenge.method":"S256","standard.token.exchange.enabled":"false"},"defaultClientScopes":["basic","roles","profile"],"optionalClientScopes":[]}
JSON
)
  if [ -z "$credential_id" ]; then
    credential_id=$(printf '%s' "$credential_json" | "$KCADM" create clients -r "$REALM" -f - -i)
  else
    printf '%s' "$credential_json" | "$KCADM" update "clients/$credential_id" -r "$REALM" -f - >/dev/null
  fi
  ensure_audience "$credential_id" pinvou-credential-audience "$CREDENTIAL_CLIENT"
fi

printf 'Mobile authentication and Portal clients provisioned for realm %s\n' "$REALM"
