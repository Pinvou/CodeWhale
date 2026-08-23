use std::fs::File;
use std::io::{BufReader, Read};
use std::path::Path;

use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
use ring::signature::{UnparsedPublicKey, ED25519};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use super::UpdateInfo;

const PUBLIC_KEY_ENV: &str = "PINVOU_CLIENT_READINESS_PUBLIC_KEY";
const KEY_ID_ENV: &str = "PINVOU_CLIENT_READINESS_KEY_ID";
const ENVIRONMENT_ENV: &str = "PINVOU_ACCOUNT_ENVIRONMENT";
const REQUIRED_CHECKS: [&str; 6] = [
    "GATEWAY_CONTRACT:PASS",
    "ACCOUNT_CONTRACT:PASS",
    "OIDC_METADATA:PASS",
    "MODEL_CATALOG:PASS",
    "MANAGED_ROUTE:PASS",
    "PRICING_FORMULA:PASS",
];

#[derive(Debug, Clone, Serialize, Deserialize)]
pub(super) struct ClientReadinessAttestationClaims {
    version: u32,
    attestation_id: String,
    environment: String,
    artifact_sha256: String,
    client_version: String,
    service_version: String,
    contract_version: String,
    account_contract_version: String,
    catalog_version: i64,
    protocol_code: String,
    check_codes: Vec<String>,
    issued_at: String,
    expires_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub(super) struct ClientReadinessAttestation {
    key_id: String,
    algorithm: String,
    claims: ClientReadinessAttestationClaims,
    signature_base64url: String,
}

#[derive(Serialize)]
struct InstallGateEvidence<'a> {
    schema_version: u32,
    stage: &'a str,
    current_version: &'a str,
    target_version: &'a str,
    artifact_sha256: &'a str,
    attestation_id: &'a str,
    environment: &'a str,
    migration_preflight:
        crate::features::account_platform::client_migration::ClientMigrationPreflight,
    verified_at: String,
}

pub(super) fn verify_install_gate(
    package_path: &str,
    installer_path: Option<&str>,
    info: &UpdateInfo,
) -> Result<(), String> {
    let package = Path::new(package_path);
    if !package.is_file() {
        return Err("更新安装门禁失败：已下载制品不存在".to_string());
    }
    let artifact_sha256 = sha256_file(package)?;
    let attestation = info
        .readiness_attestation
        .as_ref()
        .ok_or_else(|| "更新安装门禁失败：更新清单缺少签名准入证明".to_string())?;
    let public_key = required_env(PUBLIC_KEY_ENV)?;
    let key_id = required_env(KEY_ID_ENV)?;
    let environment = required_env(ENVIRONMENT_ENV)?;
    verify_attestation(
        attestation,
        &public_key,
        &key_id,
        &environment,
        &artifact_sha256,
        &info.latest_version,
        chrono::Utc::now(),
    )?;
    super::platform::verify_package_signature(package, installer_path.map(Path::new))?;
    let migration_preflight = crate::features::account_platform::client_migration::preflight()
        .map_err(|error| format!("更新安装门禁失败：迁移预检失败：{error:#}"))?;
    if !migration_preflight.settings_valid
        || !migration_preflight.session_bindings_valid
        || !migration_preflight.scheduled_profiles_valid
    {
        return Err("更新安装门禁失败：隔离迁移预检未收敛".to_string());
    }
    persist_gate_evidence(InstallGateEvidence {
        schema_version: 1,
        stage: "INSTALL_GATE_VERIFIED",
        current_version: &info.current_version,
        target_version: &info.latest_version,
        artifact_sha256: &artifact_sha256,
        attestation_id: &attestation.claims.attestation_id,
        environment: &environment,
        migration_preflight,
        verified_at: chrono::Utc::now().to_rfc3339(),
    })
}

fn verify_attestation(
    attestation: &ClientReadinessAttestation,
    public_key_base64url: &str,
    expected_key_id: &str,
    expected_environment: &str,
    artifact_sha256: &str,
    client_version: &str,
    now: chrono::DateTime<chrono::Utc>,
) -> Result<(), String> {
    let claims = &attestation.claims;
    let issued_at = chrono::DateTime::parse_from_rfc3339(&claims.issued_at)
        .map_err(|_| "更新安装门禁失败：准入证明签发时间无效".to_string())?
        .with_timezone(&chrono::Utc);
    let expires_at = chrono::DateTime::parse_from_rfc3339(&claims.expires_at)
        .map_err(|_| "更新安装门禁失败：准入证明到期时间无效".to_string())?
        .with_timezone(&chrono::Utc);
    if attestation.algorithm != "Ed25519"
        || attestation.key_id != expected_key_id
        || claims.version != 1
        || claims.environment != expected_environment
        || claims.artifact_sha256 != artifact_sha256
        || claims.client_version != client_version
        || claims.protocol_code != "CHAT"
        || claims.catalog_version <= 0
        || issued_at > now + chrono::Duration::minutes(1)
        || expires_at <= now
        || expires_at > issued_at + chrono::Duration::minutes(15)
        || REQUIRED_CHECKS
            .iter()
            .any(|required| !claims.check_codes.iter().any(|actual| actual == required))
    {
        return Err("更新安装门禁失败：准入证明与当前制品或环境不匹配".to_string());
    }
    let public_key = URL_SAFE_NO_PAD
        .decode(public_key_base64url.trim())
        .map_err(|_| "更新安装门禁失败：准入证明公钥配置无效".to_string())?;
    let signature = URL_SAFE_NO_PAD
        .decode(attestation.signature_base64url.trim())
        .map_err(|_| "更新安装门禁失败：准入证明签名格式无效".to_string())?;
    let payload =
        serde_json::to_vec(claims).map_err(|_| "更新安装门禁失败：准入证明载荷无效".to_string())?;
    UnparsedPublicKey::new(&ED25519, public_key)
        .verify(&payload, &signature)
        .map_err(|_| "更新安装门禁失败：准入证明签名校验失败".to_string())
}

fn sha256_file(path: &Path) -> Result<String, String> {
    let file =
        File::open(path).map_err(|error| format!("更新安装门禁失败：无法读取制品：{error}"))?;
    let mut reader = BufReader::new(file);
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let read = reader
            .read(&mut buffer)
            .map_err(|error| format!("更新安装门禁失败：无法校验制品：{error}"))?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(crate::platform::encoding::hex_lower(&hasher.finalize()))
}

fn required_env(name: &str) -> Result<String, String> {
    std::env::var(name)
        .ok()
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
        .ok_or_else(|| format!("更新安装门禁失败：缺少受信配置 {name}"))
}

fn persist_gate_evidence(evidence: InstallGateEvidence<'_>) -> Result<(), String> {
    let path = crate::platform::paths::pinvou3_home()
        .join("updates")
        .join("account-platform-switch.json");
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)
            .map_err(|error| format!("更新安装门禁失败：无法创建事务目录：{error}"))?;
    }
    let bytes = serde_json::to_vec_pretty(&evidence)
        .map_err(|error| format!("更新安装门禁失败：无法生成事务证据：{error}"))?;
    deepseek_tui::utils::write_atomic(&path, &bytes)
        .map_err(|error| format!("更新安装门禁失败：无法提交事务证据：{error}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use ring::signature::{Ed25519KeyPair, KeyPair};

    #[test]
    fn attestation_is_bound_to_artifact_environment_and_expiry() {
        let seed = [7_u8; 32];
        let pair = Ed25519KeyPair::from_seed_unchecked(&seed).unwrap();
        let now = chrono::Utc::now();
        let mut attestation = ClientReadinessAttestation {
            key_id: "release-key".into(),
            algorithm: "Ed25519".into(),
            claims: ClientReadinessAttestationClaims {
                version: 1,
                attestation_id: "018f47b4-e124-7dca-9c85-6f7688436600".into(),
                environment: "production".into(),
                artifact_sha256: "a".repeat(64),
                client_version: "0.8.0".into(),
                service_version: "0.8.0".into(),
                contract_version: "client-readiness-v1".into(),
                account_contract_version: "account-v1".into(),
                catalog_version: 7,
                protocol_code: "CHAT".into(),
                check_codes: REQUIRED_CHECKS
                    .iter()
                    .map(|value| value.to_string())
                    .collect(),
                issued_at: now.to_rfc3339(),
                expires_at: (now + chrono::Duration::minutes(5)).to_rfc3339(),
            },
            signature_base64url: String::new(),
        };
        attestation.signature_base64url = URL_SAFE_NO_PAD.encode(
            pair.sign(&serde_json::to_vec(&attestation.claims).unwrap())
                .as_ref(),
        );
        let public = URL_SAFE_NO_PAD.encode(pair.public_key().as_ref());
        verify_attestation(
            &attestation,
            &public,
            "release-key",
            "production",
            &"a".repeat(64),
            "0.8.0",
            now,
        )
        .unwrap();
        assert!(verify_attestation(
            &attestation,
            &public,
            "release-key",
            "staging",
            &"a".repeat(64),
            "0.8.0",
            now
        )
        .is_err());
        assert!(verify_attestation(
            &attestation,
            &public,
            "release-key",
            "production",
            &"b".repeat(64),
            "0.8.0",
            now
        )
        .is_err());
    }
}
