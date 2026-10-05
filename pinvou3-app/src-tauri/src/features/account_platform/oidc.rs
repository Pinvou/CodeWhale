//! OIDC protocol verification and the native credential record. No token is
//! returned through the desktop command protocol.
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use jsonwebtoken::{Algorithm, DecodingKey, Validation, decode, decode_header, jwk::JwkSet};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256, Sha384, Sha512};

use super::{AccountPlatformConfig, MAX_RESPONSE_BYTES};

#[derive(Clone, Serialize, Deserialize)]
pub(super) struct IdentityBinding {
    pub issuer: String,
    pub client_id: String,
    pub token_url: String,
    pub subject: String,
    pub nonce: Option<String>,
}

impl IdentityBinding {
    pub fn matches(&self, config: &AccountPlatformConfig) -> bool {
        self.issuer == config.issuer
            && self.client_id == config.client_id
            && self.token_url == config.token_url.as_str()
            && !self.subject.is_empty()
            && self.nonce.as_ref().is_none_or(|nonce| !nonce.is_empty())
    }
}

#[derive(Serialize, Deserialize)]
pub(super) struct CredentialRecord {
    pub refresh_token: String,
    pub identity: IdentityBinding,
}

impl CredentialRecord {
    pub fn parse(value: &str, config: &AccountPlatformConfig) -> Result<Self, String> {
        let record: Self =
            serde_json::from_str(value).map_err(|_| "authentication_required".to_string())?;
        if !record.identity.matches(config) || record.refresh_token.trim().is_empty() {
            return Err("authentication_required".into());
        }
        Ok(record)
    }
}

#[derive(Deserialize)]
pub(super) struct TokenResponse {
    pub access_token: String,
    pub refresh_token: Option<String>,
    pub expires_in: u64,
    pub token_type: String,
    pub id_token: Option<String>,
    #[serde(skip, default = "Instant::now")]
    pub received_at: Instant,
}

impl TokenResponse {
    pub fn expires_at(&self) -> Result<Instant, String> {
        if !self.token_type.eq_ignore_ascii_case("Bearer")
            || !valid_bearer(&self.access_token)
            || self.expires_in == 0
            || self
                .refresh_token
                .as_ref()
                .is_some_and(|v| v.trim().is_empty())
        {
            return Err("oidc_token_response_invalid".into());
        }
        self.received_at
            .checked_add(Duration::from_secs(self.expires_in))
            .filter(|expiry| *expiry > Instant::now())
            .ok_or_else(|| "oidc_token_response_invalid".into())
    }
}

fn valid_bearer(value: &str) -> bool {
    !value.is_empty()
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-._~+/=".contains(&b))
}

#[derive(Clone, Deserialize)]
struct Claims {
    sub: String,
    exp: u64,
    iat: u64,
    aud: serde_json::Value,
    #[serde(default)]
    azp: Option<String>,
    #[serde(default)]
    nonce: Option<String>,
    #[serde(default)]
    at_hash: Option<String>,
}

pub(super) async fn verify_identity(
    client: &reqwest::Client,
    config: &AccountPlatformConfig,
    tokens: &TokenResponse,
    previous: Option<&IdentityBinding>,
    login_nonce: Option<&str>,
) -> Result<IdentityBinding, String> {
    tokens.expires_at()?;
    let Some(id_token) = tokens.id_token.as_deref() else {
        return previous
            .filter(|identity| identity.matches(config))
            .cloned()
            .ok_or_else(|| "oidc_token_response_invalid".into());
    };
    let invalid = || "oidc_id_token_invalid".to_string();
    let header = decode_header(id_token).map_err(|_| invalid())?;
    if !matches!(
        header.alg,
        Algorithm::RS256
            | Algorithm::RS384
            | Algorithm::RS512
            | Algorithm::PS256
            | Algorithm::PS384
            | Algorithm::PS512
            | Algorithm::ES256
            | Algorithm::ES384
            | Algorithm::EdDSA
    ) {
        return Err(invalid());
    }
    let response = client
        .get(config.jwks_url.clone())
        .timeout(Duration::from_secs(10))
        .send()
        .await
        .map_err(|_| "oidc_jwks_unavailable".to_string())?;
    if !response.status().is_success() {
        return Err("oidc_jwks_unavailable".into());
    }
    let raw = read_bounded(response).await?;
    let keys: JwkSet = serde_json::from_slice(&raw).map_err(|_| invalid())?;
    let matching: Vec<_> = keys
        .keys
        .iter()
        .filter(|key| {
            let Ok(value) = serde_json::to_value(key) else {
                return false;
            };
            header
                .kid
                .as_ref()
                .is_none_or(|kid| value.get("kid").and_then(|v| v.as_str()) == Some(kid))
                && value.get("use").is_none_or(|v| v == "sig")
                && value.get("key_ops").is_none_or(|v| {
                    v.as_array()
                        .is_some_and(|ops| ops.iter().any(|op| op == "verify"))
                })
                && value
                    .get("alg")
                    .is_none_or(|v| v.as_str() == Some(format!("{:?}", header.alg).as_str()))
        })
        .collect();
    if matching.len() != 1 {
        return Err(invalid());
    }
    let key = DecodingKey::from_jwk(matching[0]).map_err(|_| invalid())?;
    let mut validation = Validation::new(header.alg);
    validation.set_issuer(&[config.issuer.as_str()]);
    validation.set_audience(&[config.client_id.as_str()]);
    validation.set_required_spec_claims(&["iss", "aud", "exp", "sub"]);
    // jsonwebtoken checks the standard verification fields above. The typed
    // Claims structure additionally requires OIDC's numeric issued-at claim.
    validation.leeway = 0;
    validation.validate_nbf = true;
    let claims = decode::<Claims>(id_token, &key, &validation)
        .map_err(|_| invalid())?
        .claims;
    let audience_count = match &claims.aud {
        serde_json::Value::String(_) => 1,
        serde_json::Value::Array(values) if values.iter().all(|v| v.is_string()) => values.len(),
        _ => return Err(invalid()),
    };
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| invalid())?
        .as_secs();
    if claims.exp <= now
        || claims.iat > claims.exp
        || claims.sub.trim().is_empty()
        || audience_count == 0
        || claims.azp.as_deref().is_some_and(|v| v != config.client_id)
        || (audience_count > 1 && claims.azp.as_deref() != Some(config.client_id.as_str()))
    {
        return Err(invalid());
    }
    let nonce = match previous {
        Some(identity) => {
            if !identity.matches(config)
                || identity.subject != claims.sub
                || claims
                    .nonce
                    .as_deref()
                    .is_some_and(|v| Some(v) != identity.nonce.as_deref())
            {
                return Err(invalid());
            }
            identity.nonce.clone()
        }
        None => {
            // ROPC has no authorization request and sends no nonce. If a caller
            // supplies one, it must be present and exact in the signed token.
            match login_nonce {
                Some(expected)
                    if !expected.is_empty() && claims.nonce.as_deref() == Some(expected) =>
                {
                    Some(expected.to_string())
                }
                None if claims.nonce.is_none() => None,
                _ => return Err(invalid()),
            }
        }
    };
    if let Some(hash) = claims.at_hash {
        let digest = match header.alg {
            Algorithm::RS256 | Algorithm::PS256 | Algorithm::ES256 => {
                Sha256::digest(tokens.access_token.as_bytes()).to_vec()
            }
            Algorithm::RS384 | Algorithm::PS384 | Algorithm::ES384 => {
                Sha384::digest(tokens.access_token.as_bytes()).to_vec()
            }
            Algorithm::RS512 | Algorithm::PS512 => {
                Sha512::digest(tokens.access_token.as_bytes()).to_vec()
            }
            _ => return Err(invalid()),
        };
        if hash != URL_SAFE_NO_PAD.encode(&digest[..digest.len() / 2]) {
            return Err(invalid());
        }
    }
    Ok(IdentityBinding {
        issuer: config.issuer.clone(),
        client_id: config.client_id.clone(),
        token_url: config.token_url.to_string(),
        subject: claims.sub,
        nonce,
    })
}

pub(super) async fn read_bounded(mut response: reqwest::Response) -> Result<Vec<u8>, String> {
    if response
        .content_length()
        .is_some_and(|n| n > MAX_RESPONSE_BYTES as u64)
    {
        return Err("account_platform_response_too_large".into());
    }
    let mut result = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|_| "account_platform_response_invalid".to_string())?
    {
        if chunk.len() > MAX_RESPONSE_BYTES - result.len() {
            return Err("account_platform_response_too_large".into());
        }
        result.extend_from_slice(&chunk);
    }
    Ok(result)
}
