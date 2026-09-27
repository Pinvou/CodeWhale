#!/bin/sh
set -eu

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

KCADM=/opt/keycloak/bin/kcadm.sh
REALM=${KEYCLOAK_REALM:-pinvou}
DESKTOP_CLIENT=${PINVOU_OIDC_CLIENT_ID:-pinvou-desktop-dev}
PROVISIONER_CLIENT=${KEYCLOAK_PROVISIONER_CLIENT_ID:-pinvou-account-provisioner}
FLOW=pinvou-mobile-direct-grant
PROVIDER=pinvou-mobile-direct-grant
BROWSER_FLOW=pinvou-browser
BROWSER_PROVIDER=pinvou-browser-authenticator
ADMIN_SERVER=${KEYCLOAK_ADMIN_SERVER:-http://127.0.0.1:8080}

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
if [ -z "$client_id" ]; then
  client_id=$("$KCADM" create clients -r "$REALM" -i \
    -s "clientId=$PROVISIONER_CLIENT" -s enabled=true -s publicClient=false \
    -s serviceAccountsEnabled=true -s standardFlowEnabled=false -s directAccessGrantsEnabled=false \
    -s "secret=$KEYCLOAK_PROVISIONER_CLIENT_SECRET")
else
  "$KCADM" update "clients/$client_id" -r "$REALM" -s enabled=true -s publicClient=false \
    -s serviceAccountsEnabled=true -s "secret=$KEYCLOAK_PROVISIONER_CLIENT_SECRET" >/dev/null
fi
service_user=$("$KCADM" get "clients/$client_id/service-account-user" -r "$REALM" --fields id --format csv --noquotes | tail -n 1)
realm_management=$("$KCADM" get clients -r "$REALM" -q clientId=realm-management --fields id,clientId --format csv --noquotes | find_id_by_name realm-management)
for role in manage-users view-users query-users; do
  "$KCADM" add-roles -r "$REALM" --uid "$service_user" --cid "$realm_management" --rolename "$role" >/dev/null 2>&1 || true
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
        "edit": [ "admin", "user" ]
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
for spec in 'pinvou-phone-number|phone_number|String' 'pinvou-phone-number-verified|phone_number_verified|boolean'; do
  name=${spec%%|*}; rest=${spec#*|}; attribute=${rest%%|*}; json_type=${rest##*|}
  if ! "$KCADM" get "clients/$desktop_id/protocol-mappers/models" -r "$REALM" --fields name --format csv --noquotes | grep -qx "$name"; then
    "$KCADM" create "clients/$desktop_id/protocol-mappers/models" -r "$REALM" \
      -s "name=$name" -s protocol=openid-connect -s protocolMapper=oidc-usermodel-attribute-mapper \
      -s "config.\"user.attribute\"=$attribute" -s "config.\"claim.name\"=$attribute" \
      -s "config.\"jsonType.label\"=$json_type" -s 'config."id.token.claim"=true' \
      -s 'config."access.token.claim"=true' -s 'config."userinfo.token.claim"=true' >/dev/null
  fi
done

printf 'Mobile authentication provisioned for realm %s\n' "$REALM"
