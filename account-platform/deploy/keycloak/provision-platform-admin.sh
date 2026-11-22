#!/bin/sh
set +x
set -eu
umask 077
unset KC_CLI_PASSWORD

KEYCLOAK_ADMIN_USERNAME=${KEYCLOAK_ADMIN_USERNAME:-${KC_BOOTSTRAP_ADMIN_USERNAME:-}}
KEYCLOAK_ADMIN_PASSWORD=${KEYCLOAK_ADMIN_PASSWORD:-${KC_BOOTSTRAP_ADMIN_PASSWORD:-}}
: "${KEYCLOAK_ADMIN_USERNAME:?set KEYCLOAK_ADMIN_USERNAME or KC_BOOTSTRAP_ADMIN_USERNAME}"
: "${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD or KC_BOOTSTRAP_ADMIN_PASSWORD}"
: "${PINVOU_ADMIN_USERNAME:?set PINVOU_ADMIN_USERNAME}"
: "${PINVOU_ADMIN_PASSWORD:?set PINVOU_ADMIN_PASSWORD}"

KCADM_BINARY=/opt/keycloak/bin/kcadm.sh
KCADM_CONFIG=$(mktemp "${TMPDIR:-/tmp}/pinvou-admin-kcadm.XXXXXXXX")
trap 'rm -f "$KCADM_CONFIG"' 0
trap 'exit 1' HUP INT TERM
kcadm() (
  unset KEYCLOAK_ADMIN_PASSWORD KC_BOOTSTRAP_ADMIN_PASSWORD PINVOU_ADMIN_PASSWORD
  "$KCADM_BINARY" "$@" --config "$KCADM_CONFIG"
)
KCADM=kcadm
REALM=pinvou
ROLE=platform-admin

# Keycloak 26.7 reads KC_CLI_PASSWORD when the password option is omitted.
# Scope it to this invocation; do not put a secret in argv (including env argv).
if ! (
  KC_CLI_PASSWORD=$KEYCLOAK_ADMIN_PASSWORD
  export KC_CLI_PASSWORD
  "$KCADM" config credentials \
    --server http://127.0.0.1:8080 \
    --realm master \
    --user "$KEYCLOAK_ADMIN_USERNAME"
) >/dev/null 2>&1; then
  printf '%s\n' 'Keycloak administrator authentication failed' >&2
  exit 1
fi

if ! "$KCADM" get "realms/$REALM/roles/$ROLE" >/dev/null 2>&1; then
  "$KCADM" create "realms/$REALM/roles" \
    -s "name=$ROLE" \
    -s 'description=Protected access to the Pinvou account platform operations console' >/dev/null
fi

user_id=$("$KCADM" get users -r "$REALM" -q "username=$PINVOU_ADMIN_USERNAME" --fields id,username --format csv --noquotes | {
  while IFS=, read -r id username; do
    if [ "$username" = "$PINVOU_ADMIN_USERNAME" ]; then
      printf '%s' "$id"
      break
    fi
  done
})
if [ -z "$user_id" ]; then
  admin_email=${PINVOU_ADMIN_EMAIL:-${PINVOU_ADMIN_USERNAME}@users.invalid}
  user_id=$("$KCADM" create users -r "$REALM" \
    -s "username=$PINVOU_ADMIN_USERNAME" \
    -s "email=$admin_email" \
    -s emailVerified=true \
    -s firstName=Platform \
    -s lastName=Administrator \
    -s enabled=true \
    -i)
fi

# set-password uses the same supported environment variable for its new value.
if ! (
  KC_CLI_PASSWORD=$PINVOU_ADMIN_PASSWORD
  export KC_CLI_PASSWORD
  "$KCADM" set-password -r "$REALM" --userid "$user_id"
) >/dev/null 2>&1; then
  printf '%s\n' 'Platform administrator password update failed' >&2
  exit 1
fi
"$KCADM" add-roles -r "$REALM" \
  --uid "$user_id" \
  --rolename "$ROLE" >/dev/null

printf 'Platform administrator login identity provisioned; Account business authorization is unchanged\n'
