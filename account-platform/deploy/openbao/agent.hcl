# One agent per service; bootstrap and sink mounts MUST be disjoint.
pid_file = "/tmp/bao-agent.pid"
log_level = "warn"

vault {
  address = "https://openbao:8200"
  ca_cert = "/bao/trust/ca.crt"
}

auto_auth {
  method "approle" {
    mount_path = "auth/approle"
    config = {
      role_id_file_path = "/bao/bootstrap/role-id"
      secret_id_file_path = "/bao/bootstrap/secret-id"
      remove_secret_id_file_after_reading = false
    }
  }
  sink "file" {
    config = {
      path = "/bao/token/token"
      # File sink expects an integer; 384 decimal is Unix 0600.
      mode = 384
    }
  }
}
