#!/bin/sh
set -eu

KEYCLOAK_ADMIN_USERNAME=${KEYCLOAK_ADMIN_USERNAME:-${KC_BOOTSTRAP_ADMIN_USERNAME:-}}
KEYCLOAK_ADMIN_PASSWORD=${KEYCLOAK_ADMIN_PASSWORD:-${KC_BOOTSTRAP_ADMIN_PASSWORD:-}}
: "${KEYCLOAK_ADMIN_USERNAME:?set KEYCLOAK_ADMIN_USERNAME or KC_BOOTSTRAP_ADMIN_USERNAME}"
: "${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD or KC_BOOTSTRAP_ADMIN_PASSWORD}"
: "${PINVOU_ADMIN_USERNAME:?set PINVOU_ADMIN_USERNAME}"
: "${PINVOU_ADMIN_PASSWORD:?set PINVOU_ADMIN_PASSWORD}"

KCADM=/opt/keycloak/bin/kcadm.sh
REALM=pinvou
ROLE=platform-admin

"$KCADM" config credentials \
  --server http://127.0.0.1:8080 \
  --realm master \
  --user "$KEYCLOAK_ADMIN_USERNAME" \
  --password "$KEYCLOAK_ADMIN_PASSWORD" >/dev/null

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

"$KCADM" set-password -r "$REALM" \
  --userid "$user_id" \
  --new-password "$PINVOU_ADMIN_PASSWORD" >/dev/null
"$KCADM" add-roles -r "$REALM" \
  --uid "$user_id" \
  --rolename "$ROLE" >/dev/null

printf 'Platform administrator provisioned: %s\n' "$PINVOU_ADMIN_USERNAME"
