#!/usr/bin/env bash
set -euo pipefail

: "${KEYCLOAK_NODE_ONE_URL:?set first Keycloak node URL}"
: "${KEYCLOAK_NODE_TWO_URL:?set second Keycloak node URL}"
: "${KEYCLOAK_ADMIN_USERNAME:?set Keycloak bootstrap administrator username}"
: "${KEYCLOAK_ADMIN_PASSWORD:?set Keycloak bootstrap administrator password}"
: "${KEYCLOAK_PROVISIONER_CLIENT_SECRET:?set provisioner client secret}"

workspace="$(mktemp -d)"
trap 'rm -rf -- "$workspace"' EXIT

uuid_v7() {
  python3 - <<'PY'
import secrets, time, uuid
millis = int(time.time() * 1000)
value = (millis << 80) | (7 << 76) | (secrets.randbits(12) << 64) | (2 << 62) | secrets.randbits(62)
print(uuid.UUID(int=value))
PY
}

token() {
  local realm=$1 client=$2 secret=$3 username=${4:-} password=${5:-}
  local args=(--silent --show-error --fail --request POST
    "$KEYCLOAK_NODE_ONE_URL/realms/$realm/protocol/openid-connect/token"
    --data-urlencode "client_id=$client")
  if [[ -n "$secret" ]]; then args+=(--data-urlencode "client_secret=$secret"); fi
  if [[ -n "$username" ]]; then
    args+=(--data-urlencode grant_type=password --data-urlencode "username=$username" --data-urlencode "password=$password")
  else
    args+=(--data-urlencode grant_type=client_credentials)
  fi
  curl "${args[@]}" | jq -er .access_token
}

admin_token="$(token master admin-cli '' "$KEYCLOAK_ADMIN_USERNAME" "$KEYCLOAK_ADMIN_PASSWORD")"
provisioner_token="$(token pinvou pinvou-account-provisioner "$KEYCLOAK_PROVISIONER_CLIENT_SECRET")"
username=identity-security-gate
old_password="Aa1!$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')"
new_password="Bb2!$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))')"
rejected_password="$(python3 -c 'import secrets; print(secrets.token_hex(5))')"

curl --silent --show-error --fail --request PUT "$KEYCLOAK_NODE_ONE_URL/admin/realms/pinvou" \
  --header "Authorization: Bearer $admin_token" --header 'Content-Type: application/json' \
  --data '{"directGrantFlow":"direct grant","passwordPolicy":"length(16)"}' >/dev/null
curl --silent --show-error --request POST "$KEYCLOAK_NODE_ONE_URL/admin/realms/pinvou/users" \
  --header "Authorization: Bearer $admin_token" --header 'Content-Type: application/json' \
  --data "{\"username\":\"$username\",\"enabled\":true}" >/dev/null
subject="$(curl --silent --show-error --fail --get "$KEYCLOAK_NODE_ONE_URL/admin/realms/pinvou/users" \
  --header "Authorization: Bearer $admin_token" --data-urlencode "username=$username" --data-urlencode exact=true | jq -er '.[0].id')"
curl --silent --show-error --fail --request PUT "$KEYCLOAK_NODE_ONE_URL/admin/realms/pinvou/users/$subject/reset-password" \
  --header "Authorization: Bearer $admin_token" --header 'Content-Type: application/json' \
  --data "{\"type\":\"password\",\"temporary\":false,\"value\":\"$old_password\"}" >/dev/null
user_token="$(token pinvou pinvou-desktop-dev '' "$username" "$old_password")"

operation_one="$(uuid_v7)"
status="$(curl --silent --show-error --output "$workspace/unauthorized.json" --write-out '%{http_code}' --request POST \
  "$KEYCLOAK_NODE_ONE_URL/realms/pinvou/pinvou-identity-security/users/$subject/profile-change" \
  --header "Authorization: Bearer $user_token" --header 'Content-Type: application/json' \
  --data "{\"operationId\":\"$operation_one\",\"fenceVersion\":1,\"field\":\"USERNAME\",\"value\":\"forbidden-name\"}")"
[[ "$status" == 403 ]]

# Keycloak rejects this credential through the configured realm policy after
# the provider has entered the transaction. The credential and common marker
# must both roll back.
status="$(curl --silent --show-error --output "$workspace/rejected.json" --write-out '%{http_code}' --request POST \
  "$KEYCLOAK_NODE_ONE_URL/realms/pinvou/pinvou-identity-security/users/$subject/password-reset" \
  --header "Authorization: Bearer $provisioner_token" --header 'Content-Type: application/json' \
  --data "{\"operationId\":\"$operation_one\",\"fenceVersion\":1,\"password\":\"$rejected_password\"}")"
[[ "$status" != 204 ]]
token pinvou pinvou-desktop-dev '' "$username" "$old_password" >/dev/null
user_json="$(curl --silent --show-error --fail "$KEYCLOAK_NODE_TWO_URL/admin/realms/pinvou/users/$subject" --header "Authorization: Bearer $admin_token")"
[[ "$(jq -r '.attributes.pinvou_identity_security_operation_id[0] // ""' <<<"$user_json")" == '' ]]

operation_two="$(uuid_v7)"
curl --silent --show-error --fail --request POST \
  "$KEYCLOAK_NODE_ONE_URL/realms/pinvou/pinvou-identity-security/users/$subject/password-reset" \
  --header "Authorization: Bearer $provisioner_token" --header 'Content-Type: application/json' \
  --data "{\"operationId\":\"$operation_two\",\"fenceVersion\":2,\"password\":\"$new_password\"}" >/dev/null
token pinvou pinvou-desktop-dev '' "$username" "$new_password" >/dev/null
user_json="$(curl --silent --show-error --fail "$KEYCLOAK_NODE_TWO_URL/admin/realms/pinvou/users/$subject" --header "Authorization: Bearer $admin_token")"
[[ "$(jq -r '.attributes.pinvou_identity_security_operation_id[0]' <<<"$user_json")" == "$operation_two" ]]
[[ "$(jq -r '.attributes.pinvou_identity_security_fence_version[0]' <<<"$user_json")" == 2 ]]

operation_four="$(uuid_v7)"
curl --silent --show-error --fail --request POST \
  "$KEYCLOAK_NODE_TWO_URL/realms/pinvou/pinvou-identity-security/users/$subject/profile-change" \
  --header "Authorization: Bearer $provisioner_token" --header 'Content-Type: application/json' \
  --data "{\"operationId\":\"$operation_four\",\"fenceVersion\":4,\"field\":\"USERNAME\",\"value\":\"identity-security-new\"}" >/dev/null
operation_three="$(uuid_v7)"
status="$(curl --silent --show-error --output "$workspace/stale.json" --write-out '%{http_code}' --request POST \
  "$KEYCLOAK_NODE_ONE_URL/realms/pinvou/pinvou-identity-security/users/$subject/profile-change" \
  --header "Authorization: Bearer $provisioner_token" --header 'Content-Type: application/json' \
  --data "{\"operationId\":\"$operation_three\",\"fenceVersion\":3,\"field\":\"MOBILE\",\"value\":\"+8613800138001\"}")"
[[ "$status" == 409 ]]
[[ "$(jq -r .code "$workspace/stale.json")" == IDENTITY_SECURITY_FENCE_CONFLICT ]]
user_json="$(curl --silent --show-error --fail "$KEYCLOAK_NODE_ONE_URL/admin/realms/pinvou/users/$subject" --header "Authorization: Bearer $admin_token")"
[[ "$(jq -r .username <<<"$user_json")" == identity-security-new ]]
[[ "$(jq -r '.attributes.phone_number[0] // ""' <<<"$user_json")" == '' ]]
[[ "$(jq -r '.attributes.pinvou_identity_security_operation_id[0]' <<<"$user_json")" == "$operation_four" ]]

# Node two now has the fence-4 user in its cache. A fence-5 mobile update on
# node one must invalidate that cache after commit, including both the target
# field and the common marker.
operation_five="$(uuid_v7)"
curl --silent --show-error --fail --request POST \
  "$KEYCLOAK_NODE_ONE_URL/realms/pinvou/pinvou-identity-security/users/$subject/profile-change" \
  --header "Authorization: Bearer $provisioner_token" --header 'Content-Type: application/json' \
  --data "{\"operationId\":\"$operation_five\",\"fenceVersion\":5,\"field\":\"MOBILE\",\"value\":\"+8613800138002\"}" >/dev/null
user_json="$(curl --silent --show-error --fail "$KEYCLOAK_NODE_TWO_URL/admin/realms/pinvou/users/$subject" --header "Authorization: Bearer $admin_token")"
[[ "$(jq -r '.attributes.phone_number[0]' <<<"$user_json")" == +8613800138002 ]]
[[ "$(jq -r '.attributes.pinvou_identity_security_operation_id[0]' <<<"$user_json")" == "$operation_five" ]]
[[ "$(jq -r '.attributes.pinvou_identity_security_fence_version[0]' <<<"$user_json")" == 5 ]]

# A realm-scoped token cannot use a subject id to acquire a lock through a
# different realm route.
status="$(curl --silent --show-error --output "$workspace/cross-realm.json" --write-out '%{http_code}' --request POST \
  "$KEYCLOAK_NODE_ONE_URL/realms/master/pinvou-identity-security/users/$subject/profile-change" \
  --header "Authorization: Bearer $provisioner_token" --header 'Content-Type: application/json' \
  --data "{\"operationId\":\"$operation_three\",\"fenceVersion\":6,\"field\":\"USERNAME\",\"value\":\"cross-realm\"}")"
[[ "$status" == 401 || "$status" == 403 || "$status" == 404 ]]

printf 'Keycloak identity-security runtime gate passed across two nodes\n'
