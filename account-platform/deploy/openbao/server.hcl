# Non-development single-node Raft. No automatic initialization or unseal.
ui = false
api_addr = "https://openbao:8200"
cluster_addr = "https://openbao:8201"
default_lease_ttl = "5m"
max_lease_ttl = "15m"
unsafe_allow_api_audit_creation = false
log_level = "warn"

storage "raft" {
  path = "/bao/data"
  node_id = "pinvou-kms-1"
  performance_multiplier = 1
}

listener "tcp" {
  address = "0.0.0.0:8200"
  cluster_address = "0.0.0.0:8201"
  tls_cert_file = "/bao/tls/server.crt"
  tls_key_file = "/bao/tls/server.key"
  tls_min_version = "tls12"
}

audit "file" "platform-audit" {
  description = "HMAC-protected platform KMS audit"
  options {
    file_path = "/bao/audit/audit.jsonl"
    mode = "0600"
    log_raw = "false"
    hmac_accessor = "true"
  }
}
