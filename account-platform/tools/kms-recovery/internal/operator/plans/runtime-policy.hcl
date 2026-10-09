# Same bounded key access, separately assigned to each service AppRole.
path "transit/encrypt/pinvou-token-data" {
  capabilities = ["update"]
  required_parameters = ["plaintext", "associated_data"]
  allowed_parameters = { "plaintext" = [], "associated_data" = [] }
  denied_parameters = { "associated_data" = [""] }
}
path "transit/decrypt/pinvou-token-data" {
  capabilities = ["update"]
  required_parameters = ["ciphertext", "associated_data"]
  allowed_parameters = { "ciphertext" = [], "associated_data" = [] }
  denied_parameters = { "associated_data" = [""] }
}
path "auth/token/lookup-self" { capabilities = ["read"] }
path "auth/token/renew-self" { capabilities = ["update"] }
path "auth/token/revoke-self" { capabilities = ["update"] }
