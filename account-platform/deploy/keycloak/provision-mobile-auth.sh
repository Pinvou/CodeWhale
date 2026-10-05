#!/bin/sh
set -eu

KEYCLOAK_ADMIN_USERNAME=${KEYCLOAK_ADMIN_USERNAME:-${KC_BOOTSTRAP_ADMIN_USERNAME:-}}
KEYCLOAK_ADMIN_PASSWORD=${KEYCLOAK_ADMIN_PASSWORD:-${KC_BOOTSTRAP_ADMIN_PASSWORD:-}}
: "${KEYCLOAK_ADMIN_USERNAME:?set KEYCLOAK_ADMIN_USERNAME or KC_BOOTSTRAP_ADMIN_USERNAME}"
: "${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD or KC_BOOTSTRAP_ADMIN_PASSWORD}"
: "${KEYCLOAK_PROVISIONER_CLIENT_SECRET:?set KEYCLOAK_PROVISIONER_CLIENT_SECRET}"

KCADM=/opt/keycloak/bin/kcadm.sh
# Suppress remote details which may contain accounts or credentials.
kc() {
  if ! "$KCADM" "$@" 2>/dev/null; then
    printf '%s\n' 'Mobile authentication provisioning failed' >&2
    return 1
  fi
}
REALM=${KEYCLOAK_REALM:-pinvou}
DESKTOP_CLIENT=${PINVOU_OIDC_CLIENT_ID:-pinvou-desktop-dev}
PROVISIONER_CLIENT=${KEYCLOAK_PROVISIONER_CLIENT_ID:-pinvou-account-provisioner}
FLOW=pinvou-mobile-direct-grant
PROVIDER=pinvou-mobile-direct-grant

find_id_by_name() {
  wanted=$1
  while IFS=, read -r id name _rest; do
    if [ "$name" = "$wanted" ]; then
      printf '%s\n' "$id"
      return 0
    fi
    # The CLI preserves each resource's JSON field order, not --fields order.
    # Flow records can therefore contain alias before id, unlike client rows.
    if [ "$id" = "$wanted" ]; then
      printf '%s\n' "$name"
      return 0
    fi
  done
}

kc config credentials --server http://127.0.0.1:8080 --realm master \
  --user "$KEYCLOAK_ADMIN_USERNAME" --password "$KEYCLOAK_ADMIN_PASSWORD" >/dev/null

records=$(kc get clients -r "$REALM" -q "clientId=$PROVISIONER_CLIENT" --fields id,clientId --format csv --noquotes)
client_id=$(printf '%s\n' "$records" | find_id_by_name "$PROVISIONER_CLIENT")
if [ -z "$client_id" ]; then
  client_id=$(kc create clients -r "$REALM" -i \
    -s "clientId=$PROVISIONER_CLIENT" -s enabled=true -s publicClient=false \
    -s serviceAccountsEnabled=true -s standardFlowEnabled=false -s directAccessGrantsEnabled=false \
    -s "secret=$KEYCLOAK_PROVISIONER_CLIENT_SECRET")
else
  kc update "clients/$client_id" -r "$REALM" -s enabled=true -s publicClient=false \
    -s serviceAccountsEnabled=true -s "secret=$KEYCLOAK_PROVISIONER_CLIENT_SECRET" >/dev/null
fi
records=$(kc get "clients/$client_id/service-account-user" -r "$REALM" --fields id --format csv --noquotes)
service_user=$(printf '%s\n' "$records" | tail -n 1)
records=$(kc get clients -r "$REALM" -q clientId=realm-management --fields id,clientId --format csv --noquotes)
realm_management=$(printf '%s\n' "$records" | find_id_by_name realm-management)
: "${service_user:?service account is unavailable}"
: "${realm_management:?realm management client is unavailable}"
for role in manage-users view-users query-users; do
  kc add-roles -r "$REALM" --uid "$service_user" --cid "$realm_management" --rolename "$role" >/dev/null
  granted=$(kc get "users/$service_user/role-mappings/clients/$realm_management" -r "$REALM" --fields name --format csv --noquotes)
  if ! printf '%s\n' "$granted" | grep -qx "$role"; then
    printf '%s\n' 'Mobile authentication provisioning role verification failed' >&2
    exit 1
  fi
done

records=$(kc get authentication/flows -r "$REALM" --fields id,alias --format csv --noquotes)
flow_id=$(printf '%s\n' "$records" | find_id_by_name "$FLOW")
if [ -z "$flow_id" ]; then
  flow_id=$(kc create authentication/flows -r "$REALM" -i \
    -s "alias=$FLOW" -s 'description=Pinvou mobile password or SMS direct grant' \
    -s providerId=basic-flow -s topLevel=true -s builtIn=false)
fi
records=$(kc get "authentication/flows/$FLOW/executions" -r "$REALM" --format csv --noquotes)
if ! printf '%s\n' "$records" | grep -q "$PROVIDER"; then
  kc create "authentication/flows/$FLOW/executions/execution" -r "$REALM" -s "provider=$PROVIDER" >/dev/null
fi
records=$(kc get "authentication/flows/$FLOW/executions" -r "$REALM" --fields id,providerId --format csv --noquotes)
execution_id=$(printf '%s\n' "$records" | find_id_by_name "$PROVIDER")
kc update "authentication/flows/$FLOW/executions" -r "$REALM" \
  -s "id=$execution_id" \
  -s requirement=REQUIRED -n >/dev/null
kc update "realms/$REALM" -s "directGrantFlow=$FLOW" -s registrationAllowed=false >/dev/null

# Mobile-only accounts do not have email or legal-name fields at registration
# time. Keep those standard attributes available for later profile editing, but
# do not let Keycloak's VERIFY_PROFILE trigger block password or SMS grants.
# Phone attributes are managed by the account service and remain read-only to
# the end user.
kc update users/profile -r "$REALM" -f - >/dev/null <<'PINVOU_MOBILE_USER_PROFILE'
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
      "name": "pinvou_registration_id",
      "displayName": "Registration identifier",
      "validations": { "length": { "min": 36, "max": 36 }, "pattern": { "pattern": "^[0-9a-fA-F-]{36}$" } },
      "permissions": { "view": [ "admin" ], "edit": [ "admin" ] },
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

records=$(kc get clients -r "$REALM" -q "clientId=$DESKTOP_CLIENT" --fields id,clientId --format csv --noquotes)
desktop_id=$(printf '%s\n' "$records" | find_id_by_name "$DESKTOP_CLIENT")
: "${desktop_id:?desktop client is unavailable}"
for spec in 'pinvou-phone-number|phone_number|String' 'pinvou-phone-number-verified|phone_number_verified|boolean'; do
  name=${spec%%|*}; rest=${spec#*|}; attribute=${rest%%|*}; json_type=${rest##*|}
  records=$(kc get "clients/$desktop_id/protocol-mappers/models" -r "$REALM" --fields name --format csv --noquotes)
  if ! printf '%s\n' "$records" | grep -qx "$name"; then
    kc create "clients/$desktop_id/protocol-mappers/models" -r "$REALM" \
      -s "name=$name" -s protocol=openid-connect -s protocolMapper=oidc-usermodel-attribute-mapper \
      -s "config.\"user.attribute\"=$attribute" -s "config.\"claim.name\"=$attribute" \
      -s "config.\"jsonType.label\"=$json_type" -s 'config."id.token.claim"=true' \
      -s 'config."access.token.claim"=true' -s 'config."userinfo.token.claim"=true' >/dev/null
  fi
done

printf '%s\n' 'Mobile authentication provisioned'
