//! Cryptographic primitives for desktop-to-desktop remote control.
//!
//! This module has no transport or connection-state ownership. Callers must
//! first validate wire schemas in `protocol`; the types here bind already
//! validated identities, roles, epochs and sequences to audited primitives.

use std::collections::{BTreeMap, HashSet};
use std::fmt;
#[cfg(test)]
use std::sync::Arc;
#[cfg(test)]
use std::sync::atomic::{AtomicUsize, Ordering};

use aes_gcm::aead::{Aead, KeyInit, Payload};
use aes_gcm::{Aes256Gcm, Nonce};
use base64::Engine as _;
use hkdf::Hkdf;
use ring::rand::{SecureRandom, SystemRandom};
use ring::signature::{self, UnparsedPublicKey};
use serde::Serialize;
use serde::ser::{Error as _, Serializer};
use sha2::{Digest, Sha256};
use sha2_10::Sha256 as Sha256V10;
use x25519_dalek::{PublicKey, StaticSecret};
use zeroize::{Zeroize, Zeroizing};

use super::protocol::{
    ControllerProtocolError, LeaseInstallValidationContext, MAX_DECRYPTED_PAYLOAD_BYTES,
    MAX_ENCRYPTED_FRAME_BYTES, ParsedTranscript, Result, build_receipt_commitment_preimage,
};

const AEAD_NONCE_BYTES: usize = 12;
const AEAD_TAG_BYTES: usize = 16;
const RECEIPT_SECRET_BYTES: usize = 32;
const REPLAY_WINDOW_BITS: u64 = 64;
const MAX_RECEIPT_REPLAYS: usize = 8;
const MAX_BINDING_BYTES: usize = 128;
pub(super) const MAX_SENSITIVE_STRING_BYTES: usize = 8 * 1024;
const SEALED_CODE_VERSION: u8 = 1;
const PAIRING_CODE_BYTES: usize = 7;
const SEALED_CODE_BYTES: usize = 1 + 32 + AEAD_NONCE_BYTES + PAIRING_CODE_BYTES + AEAD_TAG_BYTES;

#[derive(Debug, Eq, PartialEq)]
pub(crate) struct ControllerCryptoError {
    pub(crate) code: &'static str,
    pub(crate) field: &'static str,
}

impl ControllerCryptoError {
    fn new(code: &'static str, field: &'static str) -> Self {
        Self { code, field }
    }
}

impl fmt::Display for ControllerCryptoError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{}: {}", self.code, self.field)
    }
}

impl std::error::Error for ControllerCryptoError {}

type CryptoResult<T> = std::result::Result<T, ControllerCryptoError>;

fn lp(output: &mut Vec<u8>, bytes: &[u8]) -> CryptoResult<()> {
    let length = u32::try_from(bytes.len())
        .map_err(|_| ControllerCryptoError::new("input_too_large", "length_prefix"))?;
    output.extend_from_slice(&length.to_be_bytes());
    output.extend_from_slice(bytes);
    Ok(())
}

fn random_32() -> CryptoResult<Zeroizing<[u8; 32]>> {
    let mut value = Zeroizing::new([0_u8; 32]);
    SystemRandom::new()
        .fill(&mut value[..])
        .map_err(|_| ControllerCryptoError::new("random_unavailable", "csprng"))?;
    Ok(value)
}

fn random_nonce() -> CryptoResult<[u8; AEAD_NONCE_BYTES]> {
    let mut value = [0_u8; AEAD_NONCE_BYTES];
    SystemRandom::new()
        .fill(&mut value)
        .map_err(|_| ControllerCryptoError::new("random_unavailable", "csprng"))?;
    Ok(value)
}

fn shared_secret(secret: &StaticSecret, peer: [u8; 32]) -> CryptoResult<Zeroizing<[u8; 32]>> {
    let shared = secret.diffie_hellman(&PublicKey::from(peer));
    if !shared.was_contributory() {
        return Err(ControllerCryptoError::new("invalid_peer_key", "x25519"));
    }
    Ok(Zeroizing::new(shared.to_bytes()))
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct CodeWrapPublicKey([u8; 32]);

impl CodeWrapPublicKey {
    pub(crate) fn from_bytes(value: [u8; 32]) -> Self {
        Self(value)
    }

    pub(crate) fn to_bytes(self) -> [u8; 32] {
        self.0
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct HandshakePublicKey([u8; 32]);

impl HandshakePublicKey {
    pub(crate) fn from_bytes(value: [u8; 32]) -> Self {
        Self(value)
    }

    pub(crate) fn to_bytes(self) -> [u8; 32] {
        self.0
    }
}

fn hkdf_expand(
    input_key_material: &[u8],
    salt: &[u8],
    domain: &'static [u8],
    fields: &[&[u8]],
) -> CryptoResult<Zeroizing<[u8; 32]>> {
    let mut info = domain.to_vec();
    for field in fields {
        lp(&mut info, field)?;
    }
    let hkdf = Hkdf::<Sha256V10>::new(Some(salt), input_key_material);
    let mut output = Zeroizing::new([0_u8; 32]);
    hkdf.expand(&info, &mut output[..])
        .map_err(|_| ControllerCryptoError::new("key_derivation_failed", "hkdf"))?;
    info.zeroize();
    Ok(output)
}

/// A short-lived key pair used only to wrap the one-time pairing code.
pub(crate) struct CodeWrapKeyPair {
    secret: StaticSecret,
    public: CodeWrapPublicKey,
}

impl fmt::Debug for CodeWrapKeyPair {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("CodeWrapKeyPair([REDACTED])")
    }
}

impl CodeWrapKeyPair {
    pub(crate) fn generate() -> CryptoResult<Self> {
        let bytes = random_32()?;
        let secret = StaticSecret::from(*bytes);
        Ok(Self {
            public: CodeWrapPublicKey(PublicKey::from(&secret).to_bytes()),
            secret,
        })
    }

    pub(crate) fn public_key(&self) -> CodeWrapPublicKey {
        self.public
    }

    fn derive_wrapping_key(
        &self,
        peer_public_key: CodeWrapPublicKey,
        binding: &CodeWrapBinding,
    ) -> CryptoResult<Zeroizing<[u8; 32]>> {
        let shared = shared_secret(&self.secret, peer_public_key.to_bytes())?;
        let expires = binding.expires_at_ms_decimal();
        hkdf_expand(
            &shared[..],
            b"pinvou-code-wrap-salt-v1",
            b"pinvou-code-wrap-key-v1",
            &[
                binding.registration_request_id.as_bytes(),
                binding.pairing_id.as_bytes(),
                binding.remote_control_generation.as_bytes(),
                binding.relay_epoch.as_bytes(),
                expires.as_bytes(),
            ],
        )
    }

    pub(crate) fn seal_pairing_code(
        self,
        receiver_public_key: CodeWrapPublicKey,
        binding: &CodeWrapBinding,
        pairing_code: &str,
    ) -> CryptoResult<Vec<u8>> {
        self.seal_pairing_code_with_nonce(
            receiver_public_key,
            binding,
            pairing_code,
            random_nonce()?,
        )
    }

    fn seal_pairing_code_with_nonce(
        self,
        receiver_public_key: CodeWrapPublicKey,
        binding: &CodeWrapBinding,
        pairing_code: &str,
        nonce: [u8; AEAD_NONCE_BYTES],
    ) -> CryptoResult<Vec<u8>> {
        validate_pairing_code(pairing_code)?;
        let key = self.derive_wrapping_key(receiver_public_key, binding)?;
        let aad = binding.aad()?;
        let ciphertext = seal_aead(
            &key,
            nonce,
            &aad,
            pairing_code.as_bytes(),
            PAIRING_CODE_BYTES + AEAD_TAG_BYTES,
        )?;
        let mut envelope = Vec::with_capacity(SEALED_CODE_BYTES);
        envelope.push(SEALED_CODE_VERSION);
        envelope.extend_from_slice(&self.public.to_bytes());
        envelope.extend_from_slice(&nonce);
        envelope.extend_from_slice(&ciphertext);
        debug_assert_eq!(envelope.len(), SEALED_CODE_BYTES);
        Ok(envelope)
    }

    pub(crate) fn open_sealed_code(
        &self,
        binding: &CodeWrapBinding,
        envelope: &[u8],
    ) -> CryptoResult<SensitiveString> {
        if envelope.len() != SEALED_CODE_BYTES || envelope[0] != SEALED_CODE_VERSION {
            return Err(ControllerCryptoError::new(
                "invalid_ciphertext",
                "sealed_code_v1",
            ));
        }
        let relay_public_key: [u8; 32] = envelope[1..33]
            .try_into()
            .expect("sealed-code public key has fixed length");
        let nonce: [u8; AEAD_NONCE_BYTES] = envelope[33..45]
            .try_into()
            .expect("sealed-code nonce has fixed length");
        let key = self.derive_wrapping_key(CodeWrapPublicKey(relay_public_key), binding)?;
        let aad = binding.aad()?;
        let mut plaintext = open_aead(
            &key,
            nonce,
            &aad,
            &envelope[45..],
            PAIRING_CODE_BYTES + AEAD_TAG_BYTES,
        )?;
        let result = std::str::from_utf8(&plaintext)
            .map_err(|_| ControllerCryptoError::new("invalid_ciphertext", "sealed_code_v1"))
            .and_then(|code| {
                validate_pairing_code(code)?;
                SensitiveString::new(code)
            });
        plaintext.zeroize();
        result
    }

    #[cfg(test)]
    fn from_test_bytes(bytes: [u8; 32]) -> Self {
        let secret = StaticSecret::from(bytes);
        Self {
            public: CodeWrapPublicKey(PublicKey::from(&secret).to_bytes()),
            secret,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct CodeWrapBinding {
    registration_request_id: String,
    pairing_id: String,
    remote_control_generation: String,
    relay_epoch: String,
    expires_at_ms: u64,
}

impl CodeWrapBinding {
    pub(crate) fn new(
        registration_request_id: impl Into<String>,
        pairing_id: impl Into<String>,
        remote_control_generation: impl Into<String>,
        relay_epoch: impl Into<String>,
        expires_at_ms: u64,
    ) -> CryptoResult<Self> {
        let value = Self {
            registration_request_id: registration_request_id.into(),
            pairing_id: pairing_id.into(),
            remote_control_generation: remote_control_generation.into(),
            relay_epoch: relay_epoch.into(),
            expires_at_ms,
        };
        if value.expires_at_ms == 0
            || [
                &value.registration_request_id,
                &value.pairing_id,
                &value.remote_control_generation,
                &value.relay_epoch,
            ]
            .iter()
            .any(|field| field.is_empty() || field.len() > MAX_BINDING_BYTES)
        {
            return Err(ControllerCryptoError::new("invalid_binding", "code_wrap"));
        }
        Ok(value)
    }

    fn expires_at_ms_decimal(&self) -> String {
        self.expires_at_ms.to_string()
    }

    fn aad(&self) -> CryptoResult<Vec<u8>> {
        let mut aad = b"pinvou-sealed-code-aad-v1".to_vec();
        let expires = self.expires_at_ms_decimal();
        for field in [
            self.registration_request_id.as_bytes(),
            self.pairing_id.as_bytes(),
            self.remote_control_generation.as_bytes(),
            self.relay_epoch.as_bytes(),
            expires.as_bytes(),
        ] {
            lp(&mut aad, field)?;
        }
        Ok(aad)
    }
}

fn validate_pairing_code(value: &str) -> CryptoResult<()> {
    if value.len() != PAIRING_CODE_BYTES
        || !value
            .bytes()
            .all(|byte| b"23456789ABCDEFGHJKMNPQRSTUVWXYZ".contains(&byte))
    {
        return Err(ControllerCryptoError::new("invalid_field", "pairing_code"));
    }
    Ok(())
}

/// A key pair used only for A/B handshake and session-key derivation.
pub(crate) struct HandshakeKeyPair {
    secret: StaticSecret,
    public: HandshakePublicKey,
}

impl fmt::Debug for HandshakeKeyPair {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("HandshakeKeyPair([REDACTED])")
    }
}

impl HandshakeKeyPair {
    pub(crate) fn generate() -> CryptoResult<Self> {
        let bytes = random_32()?;
        let secret = StaticSecret::from(*bytes);
        Ok(Self {
            public: HandshakePublicKey(PublicKey::from(&secret).to_bytes()),
            secret,
        })
    }

    pub(crate) fn public_key(&self) -> HandshakePublicKey {
        self.public
    }

    pub(crate) fn into_session_keys(
        self,
        peer_public_key: HandshakePublicKey,
        binding: &HandshakeBinding,
    ) -> CryptoResult<SessionKeys> {
        let shared = shared_secret(&self.secret, peer_public_key.to_bytes())?;
        let info = binding.kdf_fields();
        let derive = |purpose: &'static [u8]| {
            let fields = [info[0], info[1], info[2], info[3], purpose];
            hkdf_expand(
                &shared[..],
                &binding.transcript_hash,
                b"pinvou-controller-handshake-kdf-v1",
                &fields,
            )
        };
        Ok(SessionKeys {
            transcript_hash: binding.transcript_hash,
            finished_a: derive(b"finished/A")?,
            finished_b: derive(b"finished/B")?,
            business_a_to_b: derive(b"business/A-to-B")?,
            business_b_to_a: derive(b"business/B-to-A")?,
            resume_secret: derive(b"resume")?,
        })
    }

    #[cfg(test)]
    fn from_test_bytes(bytes: [u8; 32]) -> Self {
        let secret = StaticSecret::from(bytes);
        Self {
            public: HandshakePublicKey(PublicKey::from(&secret).to_bytes()),
            secret,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct HandshakeBinding {
    relay_epoch: String,
    pairing_id: String,
    reservation_id: String,
    transcript_hash: [u8; 32],
}

impl HandshakeBinding {
    pub(crate) fn new(
        relay_epoch: impl Into<String>,
        pairing_id: impl Into<String>,
        reservation_id: impl Into<String>,
        transcript_hash: [u8; 32],
    ) -> CryptoResult<Self> {
        let value = Self {
            relay_epoch: relay_epoch.into(),
            pairing_id: pairing_id.into(),
            reservation_id: reservation_id.into(),
            transcript_hash,
        };
        for field in [&value.relay_epoch, &value.pairing_id, &value.reservation_id] {
            if field.is_empty() || field.len() > MAX_BINDING_BYTES {
                return Err(ControllerCryptoError::new("invalid_binding", "handshake"));
            }
        }
        Ok(value)
    }

    fn kdf_fields(&self) -> [&[u8]; 4] {
        [
            self.relay_epoch.as_bytes(),
            self.pairing_id.as_bytes(),
            self.reservation_id.as_bytes(),
            &self.transcript_hash,
        ]
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ControllerRole {
    A,
    B,
}

impl ControllerRole {
    fn wire(self) -> &'static [u8] {
        match self {
            Self::A => b"A",
            Self::B => b"B",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FrameDirection {
    AToB,
    BToA,
}

impl FrameDirection {
    fn wire(self) -> &'static [u8] {
        match self {
            Self::AToB => b"a_to_b",
            Self::BToA => b"b_to_a",
        }
    }
}

pub(crate) struct ReceiptSecret(Zeroizing<[u8; RECEIPT_SECRET_BYTES]>);

impl fmt::Debug for ReceiptSecret {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ReceiptSecret([REDACTED])")
    }
}

impl ReceiptSecret {
    pub(crate) fn generate() -> CryptoResult<Self> {
        Ok(Self(random_32()?))
    }

    pub(crate) fn commitment(
        &self,
        binding: &HandshakeBinding,
        sender: ControllerRole,
    ) -> CryptoResult<[u8; 32]> {
        let transcript_hash = hex_lower(&binding.transcript_hash);
        let mut preimage = build_receipt_commitment_preimage(
            &binding.relay_epoch,
            &binding.pairing_id,
            &binding.reservation_id,
            &transcript_hash,
            std::str::from_utf8(sender.wire()).expect("role is ASCII"),
            &self.0[..],
        )
        .map_err(|_| ControllerCryptoError::new("invalid_binding", "receipt"))?;
        let commitment = Sha256::digest(&preimage).into();
        preimage.zeroize();
        Ok(commitment)
    }

    #[cfg(test)]
    fn from_test_bytes(value: [u8; RECEIPT_SECRET_BYTES]) -> Self {
        Self(Zeroizing::new(value))
    }
}

pub(crate) struct SessionKeys {
    transcript_hash: [u8; 32],
    finished_a: Zeroizing<[u8; 32]>,
    finished_b: Zeroizing<[u8; 32]>,
    business_a_to_b: Zeroizing<[u8; 32]>,
    business_b_to_a: Zeroizing<[u8; 32]>,
    resume_secret: Zeroizing<[u8; 32]>,
}

impl fmt::Debug for SessionKeys {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SessionKeys([REDACTED])")
    }
}

impl SessionKeys {
    /// E2E-02 所需的冻结契约增量：receipt/Finished 阶段发生在 Relay 下发
    /// `connection_id/controller_lease_id/lease_epoch` 之前，因此必须能在没有
    /// `LeaseBinding` 的情况下拿到收据收发器。finished 密钥只依赖握手
    /// transcript 与角色，本方法把这个事实固化进类型：它不接受任何租约参数。
    pub(crate) fn finished_pair(
        &self,
        local_role: ControllerRole,
    ) -> (FinishedSender, FinishedReceiver) {
        let (sender_role, receiver_role, sender_key, receiver_key) = match local_role {
            ControllerRole::A => (
                ControllerRole::A,
                ControllerRole::B,
                &self.finished_a,
                &self.finished_b,
            ),
            ControllerRole::B => (
                ControllerRole::B,
                ControllerRole::A,
                &self.finished_b,
                &self.finished_a,
            ),
        };
        (
            FinishedSender {
                key: sender_key.clone(),
                transcript_hash: self.transcript_hash,
                sender: sender_role,
            },
            FinishedReceiver {
                key: receiver_key.clone(),
                transcript_hash: self.transcript_hash,
                expected_sender: receiver_role,
            },
        )
    }

    pub(crate) fn into_role_ciphers(
        self,
        local_role: ControllerRole,
        binding: LeaseBinding,
    ) -> CryptoResult<RoleCiphers> {
        let Self {
            transcript_hash,
            finished_a,
            finished_b,
            business_a_to_b,
            business_b_to_a,
            resume_secret,
        } = self;
        let (
            sender_role,
            receiver_role,
            sender_finished,
            receiver_finished,
            send_direction,
            receive_direction,
            send_root,
            receive_root,
        ) = match local_role {
            ControllerRole::A => (
                ControllerRole::A,
                ControllerRole::B,
                finished_a,
                finished_b,
                FrameDirection::AToB,
                FrameDirection::BToA,
                business_a_to_b,
                business_b_to_a,
            ),
            ControllerRole::B => (
                ControllerRole::B,
                ControllerRole::A,
                finished_b,
                finished_a,
                FrameDirection::BToA,
                FrameDirection::AToB,
                business_b_to_a,
                business_a_to_b,
            ),
        };
        let send_key = derive_business_key(&send_root, &transcript_hash, &binding, send_direction)?;
        let receive_key =
            derive_business_key(&receive_root, &transcript_hash, &binding, receive_direction)?;
        Ok(RoleCiphers {
            finished_sender: FinishedSender {
                key: sender_finished,
                transcript_hash,
                sender: sender_role,
            },
            finished_receiver: FinishedReceiver {
                key: receiver_finished,
                transcript_hash,
                expected_sender: receiver_role,
            },
            send: SendCipher {
                key: send_key,
                binding: binding.clone(),
                direction: send_direction,
                last_sequence: 0,
                exhausted: false,
            },
            receive: ReceiveCipher {
                key: receive_key,
                binding,
                direction: receive_direction,
                replay: ReplayWindow::default(),
            },
            resume_secret: ResumeSecret(resume_secret),
        })
    }
}

fn derive_business_key(
    root: &[u8; 32],
    transcript_hash: &[u8; 32],
    binding: &LeaseBinding,
    direction: FrameDirection,
) -> CryptoResult<Zeroizing<[u8; 32]>> {
    let epoch = binding.lease_epoch.to_string();
    hkdf_expand(
        root,
        transcript_hash,
        b"pinvou-controller-business-key-v1",
        &[
            binding.connection_id.as_bytes(),
            binding.controller_lease_id.as_bytes(),
            epoch.as_bytes(),
            binding.relay_epoch.as_bytes(),
            binding.remote_control_generation.as_bytes(),
            direction.wire(),
        ],
    )
}

pub(crate) struct RoleCiphers {
    pub(crate) finished_sender: FinishedSender,
    pub(crate) finished_receiver: FinishedReceiver,
    pub(crate) send: SendCipher,
    pub(crate) receive: ReceiveCipher,
    pub(crate) resume_secret: ResumeSecret,
}

pub(crate) struct ResumeSecret(Zeroizing<[u8; 32]>);

impl fmt::Debug for ResumeSecret {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ResumeSecret([REDACTED])")
    }
}

pub(crate) struct FinishedSender {
    key: Zeroizing<[u8; 32]>,
    transcript_hash: [u8; 32],
    sender: ControllerRole,
}

impl FinishedSender {
    pub(crate) fn seal(self, receipt: &ReceiptSecret) -> CryptoResult<Vec<u8>> {
        self.seal_with_nonce(receipt, random_nonce()?)
    }

    fn seal_with_nonce(
        self,
        receipt: &ReceiptSecret,
        nonce: [u8; AEAD_NONCE_BYTES],
    ) -> CryptoResult<Vec<u8>> {
        let mut plaintext = Zeroizing::new(b"pinvou-controller-finished-v1".to_vec());
        lp(&mut plaintext, &self.transcript_hash)?;
        lp(&mut plaintext, self.sender.wire())?;
        lp(&mut plaintext, &receipt.0[..])?;
        let aad = finished_aad(&self.transcript_hash, self.sender)?;
        let ciphertext = seal_aead(&self.key, nonce, &aad, &plaintext[..], 1024)?;
        let mut output = nonce.to_vec();
        output.extend_from_slice(&ciphertext);
        Ok(output)
    }
}

pub(crate) struct FinishedReceiver {
    key: Zeroizing<[u8; 32]>,
    transcript_hash: [u8; 32],
    expected_sender: ControllerRole,
}

impl FinishedReceiver {
    pub(crate) fn open(self, encrypted: &[u8]) -> CryptoResult<ReceiptSecret> {
        if encrypted.len() < AEAD_NONCE_BYTES + AEAD_TAG_BYTES || encrypted.len() > 1024 {
            return Err(ControllerCryptoError::new("invalid_ciphertext", "finished"));
        }
        let mut nonce = [0_u8; AEAD_NONCE_BYTES];
        nonce.copy_from_slice(&encrypted[..AEAD_NONCE_BYTES]);
        let aad = finished_aad(&self.transcript_hash, self.expected_sender)?;
        let mut plaintext =
            open_aead(&self.key, nonce, &aad, &encrypted[AEAD_NONCE_BYTES..], 1024)?;
        let parsed = parse_finished(&plaintext, &self.transcript_hash, self.expected_sender);
        plaintext.zeroize();
        parsed.map(|secret| ReceiptSecret(Zeroizing::new(secret)))
    }
}

fn finished_aad(transcript_hash: &[u8; 32], sender: ControllerRole) -> CryptoResult<Vec<u8>> {
    let mut output = b"pinvou-controller-finished-aad-v1".to_vec();
    lp(&mut output, transcript_hash)?;
    lp(&mut output, sender.wire())?;
    Ok(output)
}

fn parse_finished(
    plaintext: &[u8],
    transcript_hash: &[u8; 32],
    sender: ControllerRole,
) -> CryptoResult<[u8; RECEIPT_SECRET_BYTES]> {
    const DOMAIN: &[u8] = b"pinvou-controller-finished-v1";
    if !plaintext.starts_with(DOMAIN) {
        return Err(ControllerCryptoError::new("invalid_finished", "domain"));
    }
    let mut cursor = DOMAIN.len();
    let transcript = take_lp(plaintext, &mut cursor)?;
    let role = take_lp(plaintext, &mut cursor)?;
    let secret = take_lp(plaintext, &mut cursor)?;
    if cursor != plaintext.len()
        || transcript != transcript_hash
        || role != sender.wire()
        || secret.len() != RECEIPT_SECRET_BYTES
    {
        return Err(ControllerCryptoError::new("invalid_finished", "binding"));
    }
    let mut output = [0_u8; RECEIPT_SECRET_BYTES];
    output.copy_from_slice(secret);
    Ok(output)
}

fn take_lp<'a>(input: &'a [u8], cursor: &mut usize) -> CryptoResult<&'a [u8]> {
    let length_bytes = input
        .get(*cursor..*cursor + 4)
        .ok_or_else(|| ControllerCryptoError::new("invalid_finished", "length"))?;
    let length = u32::from_be_bytes(length_bytes.try_into().expect("four bytes")) as usize;
    *cursor += 4;
    let value = input
        .get(*cursor..*cursor + length)
        .ok_or_else(|| ControllerCryptoError::new("invalid_finished", "length"))?;
    *cursor += length;
    Ok(value)
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct LeaseBinding {
    connection_id: String,
    controller_lease_id: String,
    lease_epoch: u64,
    relay_epoch: String,
    remote_control_generation: String,
}

impl LeaseBinding {
    pub(crate) fn new(
        connection_id: impl Into<String>,
        controller_lease_id: impl Into<String>,
        lease_epoch: u64,
        relay_epoch: impl Into<String>,
        remote_control_generation: impl Into<String>,
    ) -> CryptoResult<Self> {
        let value = Self {
            connection_id: connection_id.into(),
            controller_lease_id: controller_lease_id.into(),
            lease_epoch,
            relay_epoch: relay_epoch.into(),
            remote_control_generation: remote_control_generation.into(),
        };
        if value.lease_epoch == 0
            || [
                &value.connection_id,
                &value.controller_lease_id,
                &value.relay_epoch,
                &value.remote_control_generation,
            ]
            .iter()
            .any(|field| field.is_empty() || field.len() > MAX_BINDING_BYTES)
        {
            return Err(ControllerCryptoError::new("invalid_binding", "lease"));
        }
        Ok(value)
    }
}

pub(crate) struct SendCipher {
    key: Zeroizing<[u8; 32]>,
    binding: LeaseBinding,
    direction: FrameDirection,
    last_sequence: u64,
    exhausted: bool,
}

impl fmt::Debug for SendCipher {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SendCipher([REDACTED])")
    }
}

impl SendCipher {
    pub(crate) fn seal(
        &mut self,
        lease_epoch: u64,
        sequence: u64,
        payload_kind: &str,
        plaintext: &[u8],
    ) -> CryptoResult<Vec<u8>> {
        if self.exhausted {
            return Err(ControllerCryptoError::new(
                "sequence_exhausted",
                "frame_sequence",
            ));
        }
        check_epoch_sequence(&self.binding, lease_epoch, sequence)?;
        if sequence <= self.last_sequence {
            return Err(ControllerCryptoError::new("replay", "frame_sequence"));
        }
        if plaintext.len() > MAX_DECRYPTED_PAYLOAD_BYTES {
            return Err(ControllerCryptoError::new("plaintext_too_large", "frame"));
        }
        let nonce = frame_nonce(self.direction, lease_epoch, sequence);
        let aad = frame_aad(
            &self.binding,
            self.direction,
            lease_epoch,
            sequence,
            payload_kind,
        )?;
        let ciphertext = seal_aead(&self.key, nonce, &aad, plaintext, MAX_ENCRYPTED_FRAME_BYTES)?;
        self.last_sequence = sequence;
        self.exhausted = sequence == u64::MAX;
        Ok(ciphertext)
    }
}

pub(crate) struct ReceiveCipher {
    key: Zeroizing<[u8; 32]>,
    binding: LeaseBinding,
    direction: FrameDirection,
    replay: ReplayWindow,
}

impl fmt::Debug for ReceiveCipher {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ReceiveCipher([REDACTED])")
    }
}

impl ReceiveCipher {
    pub(crate) fn open(
        &mut self,
        lease_epoch: u64,
        sequence: u64,
        payload_kind: &str,
        ciphertext: &[u8],
    ) -> CryptoResult<Vec<u8>> {
        check_epoch_sequence(&self.binding, lease_epoch, sequence)?;
        if ciphertext.len() < AEAD_TAG_BYTES
            || ciphertext.len() > MAX_DECRYPTED_PAYLOAD_BYTES + AEAD_TAG_BYTES
        {
            return Err(ControllerCryptoError::new("invalid_ciphertext", "frame"));
        }
        self.replay.check(sequence)?;
        let nonce = frame_nonce(self.direction, lease_epoch, sequence);
        let aad = frame_aad(
            &self.binding,
            self.direction,
            lease_epoch,
            sequence,
            payload_kind,
        )?;
        let plaintext = open_aead(
            &self.key,
            nonce,
            &aad,
            ciphertext,
            MAX_ENCRYPTED_FRAME_BYTES,
        )?;
        if plaintext.len() > MAX_DECRYPTED_PAYLOAD_BYTES {
            return Err(ControllerCryptoError::new("plaintext_too_large", "frame"));
        }
        self.replay.commit(sequence);
        Ok(plaintext)
    }
}

fn check_epoch_sequence(
    binding: &LeaseBinding,
    lease_epoch: u64,
    sequence: u64,
) -> CryptoResult<()> {
    if lease_epoch != binding.lease_epoch {
        return Err(ControllerCryptoError::new("unknown_epoch", "lease_epoch"));
    }
    if sequence == 0 {
        return Err(ControllerCryptoError::new("replay", "frame_sequence"));
    }
    Ok(())
}

fn frame_aad(
    binding: &LeaseBinding,
    direction: FrameDirection,
    lease_epoch: u64,
    sequence: u64,
    payload_kind: &str,
) -> CryptoResult<Vec<u8>> {
    if payload_kind.is_empty() || payload_kind.len() > MAX_BINDING_BYTES {
        return Err(ControllerCryptoError::new(
            "invalid_binding",
            "payload_kind",
        ));
    }
    let mut output = b"pinvou-controller-business-aad-v1".to_vec();
    let lease_epoch = lease_epoch.to_string();
    let sequence = sequence.to_string();
    for value in [
        binding.connection_id.as_bytes(),
        binding.controller_lease_id.as_bytes(),
        lease_epoch.as_bytes(),
        binding.relay_epoch.as_bytes(),
        binding.remote_control_generation.as_bytes(),
        direction.wire(),
        sequence.as_bytes(),
        payload_kind.as_bytes(),
    ] {
        lp(&mut output, value)?;
    }
    Ok(output)
}

fn frame_nonce(direction: FrameDirection, lease_epoch: u64, sequence: u64) -> [u8; 12] {
    let mut input = b"pinvou-controller-business-nonce-v1".to_vec();
    input.extend_from_slice(direction.wire());
    input.extend_from_slice(&lease_epoch.to_be_bytes());
    let digest = Sha256::digest(input);
    let mut nonce = [0_u8; 12];
    nonce[..4].copy_from_slice(&digest[..4]);
    nonce[4..].copy_from_slice(&sequence.to_be_bytes());
    nonce
}

#[derive(Default)]
struct ReplayWindow {
    highest: u64,
    bitmap: u64,
}

impl ReplayWindow {
    fn check(&self, sequence: u64) -> CryptoResult<()> {
        if sequence == 0 {
            return Err(ControllerCryptoError::new("replay", "frame_sequence"));
        }
        if sequence > self.highest {
            return Ok(());
        }
        let distance = self.highest - sequence;
        if distance >= REPLAY_WINDOW_BITS || self.bitmap & (1_u64 << distance) != 0 {
            return Err(ControllerCryptoError::new("replay", "frame_sequence"));
        }
        Ok(())
    }

    fn commit(&mut self, sequence: u64) {
        if sequence > self.highest {
            let shift = sequence - self.highest;
            self.bitmap = if shift >= REPLAY_WINDOW_BITS {
                1
            } else {
                (self.bitmap << shift) | 1
            };
            self.highest = sequence;
        } else {
            self.bitmap |= 1_u64 << (self.highest - sequence);
        }
    }
}

#[derive(Default)]
pub(crate) struct ReceiptReplayGuard {
    accepted: HashSet<[u8; 32]>,
}

impl ReceiptReplayGuard {
    pub(crate) fn accept(&mut self, commitment: [u8; 32]) -> CryptoResult<()> {
        if self.accepted.contains(&commitment) {
            return Err(ControllerCryptoError::new("replay", "receipt"));
        }
        if self.accepted.len() >= MAX_RECEIPT_REPLAYS {
            return Err(ControllerCryptoError::new("capacity", "receipt"));
        }
        self.accepted.insert(commitment);
        Ok(())
    }
}

fn seal_aead(
    key: &[u8; 32],
    nonce: [u8; AEAD_NONCE_BYTES],
    aad: &[u8],
    plaintext: &[u8],
    maximum: usize,
) -> CryptoResult<Vec<u8>> {
    let output_len = plaintext
        .len()
        .checked_add(AEAD_TAG_BYTES)
        .ok_or_else(|| ControllerCryptoError::new("plaintext_too_large", "aead"))?;
    if output_len > maximum {
        return Err(ControllerCryptoError::new("plaintext_too_large", "aead"));
    }
    Aes256Gcm::new_from_slice(key)
        .expect("AES-256 key length is fixed")
        .encrypt(
            Nonce::from_slice(&nonce),
            Payload {
                msg: plaintext,
                aad,
            },
        )
        .map_err(|_| ControllerCryptoError::new("encryption_failed", "aead"))
}

fn open_aead(
    key: &[u8; 32],
    nonce: [u8; AEAD_NONCE_BYTES],
    aad: &[u8],
    ciphertext: &[u8],
    maximum: usize,
) -> CryptoResult<Vec<u8>> {
    if ciphertext.len() < AEAD_TAG_BYTES || ciphertext.len() > maximum {
        return Err(ControllerCryptoError::new("invalid_ciphertext", "aead"));
    }
    Aes256Gcm::new_from_slice(key)
        .expect("AES-256 key length is fixed")
        .decrypt(
            Nonce::from_slice(&nonce),
            Payload {
                msg: ciphertext,
                aad,
            },
        )
        .map_err(|_| ControllerCryptoError::new("authentication_failed", "aead"))
}

pub(crate) struct SensitiveString {
    value: Zeroizing<String>,
    #[cfg(test)]
    drop_probe: Option<Arc<SensitiveDropProbe>>,
}

#[cfg(test)]
#[derive(Default)]
struct SensitiveDropProbe {
    drops: AtomicUsize,
    owned_bytes_at_drop: AtomicUsize,
}

#[cfg(test)]
impl SensitiveDropProbe {
    fn observation(&self) -> (usize, usize) {
        (
            self.drops.load(Ordering::SeqCst),
            self.owned_bytes_at_drop.load(Ordering::SeqCst),
        )
    }
}

impl SensitiveString {
    pub(crate) fn new(value: impl Into<String>) -> CryptoResult<Self> {
        Self::validate(Self {
            value: Zeroizing::new(value.into()),
            #[cfg(test)]
            drop_probe: None,
        })
    }

    fn validate(candidate: Self) -> CryptoResult<Self> {
        if candidate.value.is_empty() || candidate.value.len() > MAX_SENSITIVE_STRING_BYTES {
            return Err(ControllerCryptoError::new(
                "invalid_sensitive_value",
                "sensitive_string",
            ));
        }
        Ok(candidate)
    }

    #[cfg(test)]
    fn new_with_drop_probe(
        value: impl Into<String>,
        drop_probe: Arc<SensitiveDropProbe>,
    ) -> CryptoResult<Self> {
        Self::validate(Self {
            value: Zeroizing::new(value.into()),
            drop_probe: Some(drop_probe),
        })
    }

    pub(crate) fn expose(&self) -> &str {
        self.value.as_str()
    }
}

#[cfg(test)]
impl Drop for SensitiveString {
    fn drop(&mut self) {
        if let Some(probe) = &self.drop_probe {
            probe
                .owned_bytes_at_drop
                .store(self.value.len(), Ordering::SeqCst);
            probe.drops.fetch_add(1, Ordering::SeqCst);
        }
    }
}

impl fmt::Debug for SensitiveString {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SensitiveString([REDACTED])")
    }
}

impl fmt::Display for SensitiveString {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("[REDACTED]")
    }
}

impl Serialize for SensitiveString {
    fn serialize<S>(&self, _serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        Err(S::Error::custom("sensitive value serialization forbidden"))
    }
}

/// Opaque output of an authenticated enrollment/configuration loader. There is
/// deliberately no raw JSON/string constructor in DEV-05.
#[derive(Clone)]
pub(crate) struct AuthenticatedRelayTrustConfig {
    keys: BTreeMap<String, [u8; 32]>,
}

impl fmt::Debug for AuthenticatedRelayTrustConfig {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AuthenticatedRelayTrustConfig")
            .field("key_count", &self.keys.len())
            .finish_non_exhaustive()
    }
}

impl AuthenticatedRelayTrustConfig {
    /// E2E-02 所需的冻结契约增量：生产构建需要一个受控入口把管理员预置的
    /// Relay 签名公钥变成信任根。只接受严格 key id 与 32 字节公钥，任何
    /// 缺失、畸形或重复都失败关闭；不接受用户输入，也不接受原始 JSON。
    pub(crate) fn from_pinned_keys(
        entries: impl IntoIterator<Item = (String, String)>,
    ) -> CryptoResult<Self> {
        let mut keys = BTreeMap::new();
        for (key_id, encoded) in entries {
            if key_id.is_empty()
                || key_id.len() > 64
                || !key_id
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
            {
                return Err(ControllerCryptoError::new(
                    "invalid_pinned_key",
                    "relay_signing_key_id",
                ));
            }
            let raw = base64::engine::general_purpose::STANDARD
                .decode(encoded.as_bytes())
                .map_err(|_| {
                    ControllerCryptoError::new("invalid_pinned_key", "relay_signing_public_key")
                })?;
            if raw.len() != 32 {
                return Err(ControllerCryptoError::new(
                    "invalid_pinned_key",
                    "relay_signing_public_key",
                ));
            }
            let mut public_key = [0_u8; 32];
            public_key.copy_from_slice(&raw);
            if keys.insert(key_id, public_key).is_some() {
                return Err(ControllerCryptoError::new(
                    "invalid_pinned_key",
                    "relay_signing_key_id",
                ));
            }
        }
        if keys.is_empty() {
            return Err(ControllerCryptoError::new(
                "relay_trust_unconfigured",
                "relay_signing_public_key",
            ));
        }
        Ok(Self { keys })
    }

    #[cfg(test)]
    pub(super) fn from_test_enrollment(
        entries: impl IntoIterator<Item = (String, [u8; 32])>,
    ) -> Self {
        Self {
            keys: entries.into_iter().collect(),
        }
    }
}

/// Production trust-root entry point. The pin is deployment configuration
/// (administrator-provisioned), never user input; a missing or malformed pin
/// fails closed instead of degrading to an unauthenticated transcript.
pub(crate) fn load_relay_trust_config(
    lookup: impl Fn(&str) -> Option<String>,
) -> CryptoResult<AuthenticatedRelayTrustConfig> {
    let key_id = lookup("PINVOU_REMOTE_RELAY_SIGNING_KEY_ID")
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| {
            ControllerCryptoError::new("relay_trust_unconfigured", "relay_signing_key_id")
        })?;
    let public_key = lookup("PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64")
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| {
            ControllerCryptoError::new("relay_trust_unconfigured", "relay_signing_public_key")
        })?;
    AuthenticatedRelayTrustConfig::from_pinned_keys([(key_id.trim().to_owned(), public_key.trim().to_owned())])
}

/// Verifies a parsed `controller_pairing_reserved` transcript against the
/// pinned trust root and returns the authenticated lease-install context.
pub(crate) fn authenticate_relay_transcript(
    trust: &AuthenticatedRelayTrustConfig,
    parsed: ParsedTranscript,
) -> Result<LeaseInstallValidationContext> {
    TranscriptAuthenticator::new(trust.clone()).authenticate(parsed)
}

pub(crate) struct TranscriptAuthenticator {
    trust: AuthenticatedRelayTrustConfig,
}

impl TranscriptAuthenticator {
    fn new(trust: AuthenticatedRelayTrustConfig) -> Self {
        Self { trust }
    }

    pub(crate) fn authenticate(
        &self,
        parsed: ParsedTranscript,
    ) -> Result<LeaseInstallValidationContext> {
        let parts = parsed.into_parts();
        if parts.signature_algorithm != "ed25519" {
            return Err(ControllerProtocolError::new(
                "invalid_field",
                "signature_algorithm",
            ));
        }
        let public_key = self
            .trust
            .keys
            .get(&parts.relay_signing_key_id)
            .ok_or_else(|| ControllerProtocolError::new("invalid_field", "relay_signing_key_id"))?;
        UnparsedPublicKey::new(&signature::ED25519, public_key)
            .verify(&parts.signature_preimage, &parts.relay_signature)
            .map_err(|_| ControllerProtocolError::new("invalid_field", "relay_signature_b64"))?;
        Ok(LeaseInstallValidationContext::new(VerifiedTranscript {
            request_id: parts.request_id,
            expected_pairing_id: parts.expected_pairing_id,
            reservation_id: parts.reservation_id,
            remote_control_generation: parts.remote_control_generation,
            relay_epoch: parts.relay_epoch,
            transcript_hash: parts.transcript_hash,
            expected_controller_connection_identity: parts.expected_controller_connection_identity,
            negotiated_capabilities: parts.negotiated_capabilities,
            capability_hash: parts.capability_hash,
        }))
    }
}

#[cfg(test)]
pub(super) fn authenticate_transcript_for_test(
    parsed: ParsedTranscript,
    key_id: &str,
    public_key: [u8; 32],
) -> Result<LeaseInstallValidationContext> {
    TranscriptAuthenticator::new(AuthenticatedRelayTrustConfig::from_test_enrollment([(
        key_id.to_owned(),
        public_key,
    )]))
    .authenticate(parsed)
}

impl fmt::Debug for TranscriptAuthenticator {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TranscriptAuthenticator")
            .field("trust", &self.trust)
            .finish()
    }
}

pub(crate) struct VerifiedTranscript {
    request_id: String,
    expected_pairing_id: String,
    reservation_id: String,
    remote_control_generation: String,
    relay_epoch: String,
    transcript_hash: String,
    expected_controller_connection_identity: String,
    negotiated_capabilities: Vec<String>,
    capability_hash: String,
}

impl VerifiedTranscript {
    pub(crate) fn request_id(&self) -> &str {
        &self.request_id
    }
    pub(crate) fn expected_pairing_id(&self) -> &str {
        &self.expected_pairing_id
    }
    pub(crate) fn reservation_id(&self) -> &str {
        &self.reservation_id
    }
    pub(crate) fn remote_control_generation(&self) -> &str {
        &self.remote_control_generation
    }
    pub(crate) fn relay_epoch(&self) -> &str {
        &self.relay_epoch
    }
    pub(crate) fn transcript_hash(&self) -> &str {
        &self.transcript_hash
    }
    pub(crate) fn expected_controller_connection_identity(&self) -> &str {
        &self.expected_controller_connection_identity
    }
    pub(crate) fn negotiated_capabilities(&self) -> &[String] {
        &self.negotiated_capabilities
    }
    pub(crate) fn capability_hash(&self) -> &str {
        &self.capability_hash
    }
}

fn hex_lower(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    use super::super::protocol::parse_controller_pairing_transcript;

    fn fixture() -> Value {
        serde_json::from_str(include_str!(
            "../../../../../../remote-control-relay/test/fixtures/controller-protocol-v1.json"
        ))
        .unwrap()
    }

    fn decode_hex(value: &str) -> Vec<u8> {
        assert_eq!(value.len() % 2, 0);
        (0..value.len())
            .step_by(2)
            .map(|index| u8::from_str_radix(&value[index..index + 2], 16).unwrap())
            .collect()
    }

    fn binding() -> HandshakeBinding {
        HandshakeBinding::new(
            "epoch-test-1",
            "pairing-0001",
            "reservation-0001",
            [7_u8; 32],
        )
        .unwrap()
    }

    fn lease(epoch: u64) -> LeaseBinding {
        LeaseBinding::new(
            "connection-0001",
            "lease-0001",
            epoch,
            "epoch-test-1",
            "generation-0001",
        )
        .unwrap()
    }

    fn session_pair() -> (SessionKeys, SessionKeys) {
        let a = HandshakeKeyPair::from_test_bytes([1_u8; 32]);
        let b = HandshakeKeyPair::from_test_bytes([2_u8; 32]);
        let a_public = a.public_key();
        let b_public = b.public_key();
        let a_keys = a.into_session_keys(b_public, &binding()).unwrap();
        let b_keys = b.into_session_keys(a_public, &binding()).unwrap();
        (a_keys, b_keys)
    }

    fn role_pair(epoch: u64) -> (RoleCiphers, RoleCiphers) {
        let (a, b) = session_pair();
        (
            a.into_role_ciphers(ControllerRole::A, lease(epoch))
                .unwrap(),
            b.into_role_ciphers(ControllerRole::B, lease(epoch))
                .unwrap(),
        )
    }

    /// E2E-02 契约：receipt 阶段必须能在没有租约身份时完成，而业务密钥
    /// 必须依赖真实租约身份。两条不变量在同一条用例里锁死。
    #[test]
    fn finished_keys_ignore_the_lease_binding_while_business_keys_depend_on_it() {
        let receipt = ReceiptSecret::from_test_bytes([9_u8; 32]);
        let nonce = [3_u8; 12];

        // 1) finished 收发器可以完全不借助 LeaseBinding 生成，且与
        //    `into_role_ciphers` 产出的那一份逐字节一致。
        let (via_pair_keys, peer_keys) = session_pair();
        let (via_pair_sender, _) = via_pair_keys.finished_pair(ControllerRole::A);
        let sealed_without_lease = via_pair_sender
            .seal_with_nonce(&receipt, nonce)
            .unwrap();

        let (via_ciphers_keys, _) = session_pair();
        let via_ciphers = via_ciphers_keys
            .into_role_ciphers(ControllerRole::A, lease(7))
            .unwrap();
        let sealed_via_ciphers = via_ciphers
            .finished_sender
            .seal_with_nonce(&receipt, nonce)
            .unwrap();
        assert_eq!(
            sealed_without_lease, sealed_via_ciphers,
            "finished output must not depend on the lease binding"
        );

        // 2) 对端 B 仅凭同一 transcript 的 finished 收发器即可打开它。
        let peer_receiver = peer_keys.finished_pair(ControllerRole::B).1;
        peer_receiver.open(&sealed_without_lease).unwrap();

        // 3) 反向约束：业务密钥必须随租约身份变化，且同身份稳定。
        let root = [4_u8; 32];
        let transcript_hash = [7_u8; 32];
        let first = derive_business_key(&root, &transcript_hash, &lease(1), FrameDirection::AToB)
            .unwrap()
            .to_vec();
        let same = derive_business_key(&root, &transcript_hash, &lease(1), FrameDirection::AToB)
            .unwrap()
            .to_vec();
        let other_epoch =
            derive_business_key(&root, &transcript_hash, &lease(2), FrameDirection::AToB)
                .unwrap()
                .to_vec();
        assert_eq!(first, same, "the same lease identity must derive the same key");
        assert_ne!(
            first, other_epoch,
            "business keys must be bound to the issued lease epoch"
        );
    }

    fn code_binding() -> CodeWrapBinding {
        CodeWrapBinding::new(
            "registration-0001",
            "pairing-0001",
            "generation-0001",
            "epoch-test-1",
            300_000,
        )
        .unwrap()
    }

    #[test]
    fn frozen_cross_language_crypto_vector_matches_every_stage() {
        let fixture = fixture();
        let vector = &fixture["crypto_vectors"];
        let private_a: [u8; 32] = decode_hex(vector["private_a_hex"].as_str().unwrap())
            .try_into()
            .unwrap();
        let private_b: [u8; 32] = decode_hex(vector["private_b_hex"].as_str().unwrap())
            .try_into()
            .unwrap();
        let a = HandshakeKeyPair::from_test_bytes(private_a);
        let b = HandshakeKeyPair::from_test_bytes(private_b);
        assert_eq!(
            hex_lower(&a.public_key().to_bytes()),
            vector["public_a_hex"]
        );
        assert_eq!(
            hex_lower(&b.public_key().to_bytes()),
            vector["public_b_hex"]
        );
        let shared = shared_secret(&a.secret, b.public_key().to_bytes()).unwrap();
        assert_eq!(hex_lower(&shared[..]), vector["shared_secret_hex"]);
        let binding = binding();
        let keys = a.into_session_keys(b.public_key(), &binding).unwrap();
        assert_eq!(
            hex_lower(&keys.finished_a[..]),
            vector["finished"]["derived_key_hex"]
        );
        assert_eq!(
            hex_lower(&keys.business_a_to_b[..]),
            vector["business"]["derived_root_hex"]
        );
        assert_eq!(
            hex_lower(&keys.resume_secret[..]),
            vector["resume_secret_hex"]
        );
        assert_ne!(&keys.resume_secret[..], &keys.finished_a[..]);
        assert_ne!(&keys.resume_secret[..], &keys.finished_b[..]);
        assert_ne!(&keys.resume_secret[..], &keys.business_a_to_b[..]);
        assert_ne!(&keys.resume_secret[..], &keys.business_b_to_a[..]);
        let receipt = ReceiptSecret::from_test_bytes([5_u8; 32]);
        let mut ciphers = keys.into_role_ciphers(ControllerRole::A, lease(7)).unwrap();
        let finished = ciphers
            .finished_sender
            .seal_with_nonce(&receipt, [6_u8; 12])
            .unwrap();
        assert_eq!(
            hex_lower(&finished),
            vector["finished"]["ciphertext_with_nonce_hex"]
        );
        assert_eq!(
            hex_lower(&ciphers.send.key[..]),
            vector["business"]["derived_key_hex"]
        );
        assert_eq!(
            hex_lower(&frame_nonce(FrameDirection::AToB, 7, 1)),
            vector["business"]["nonce_hex"]
        );
        assert_eq!(
            hex_lower(
                &frame_aad(
                    &ciphers.send.binding,
                    FrameDirection::AToB,
                    7,
                    1,
                    "task_submit"
                )
                .unwrap()
            ),
            vector["business"]["aad_hex"]
        );
        assert_eq!(
            hex_lower(&ciphers.send.seal(7, 1, "task_submit", b"payload").unwrap()),
            vector["business"]["ciphertext_hex"]
        );
    }

    #[test]
    fn sealed_code_v1_matches_node_vector_and_rejects_every_binding_tamper() {
        let fixture = fixture();
        let vector = &fixture["sealed_code_vector"];
        let receiver_private: [u8; 32] =
            decode_hex(vector["receiver_private_hex"].as_str().unwrap())
                .try_into()
                .unwrap();
        let relay_private: [u8; 32] = decode_hex(vector["relay_private_hex"].as_str().unwrap())
            .try_into()
            .unwrap();
        let receiver = CodeWrapKeyPair::from_test_bytes(receiver_private);
        let relay = CodeWrapKeyPair::from_test_bytes(relay_private);
        assert_eq!(
            hex_lower(&receiver.public_key().to_bytes()),
            vector["receiver_public_hex"]
        );
        assert_eq!(
            hex_lower(&relay.public_key().to_bytes()),
            vector["relay_public_hex"]
        );
        let shared = shared_secret(&relay.secret, receiver.public_key().to_bytes()).unwrap();
        assert_eq!(hex_lower(&shared[..]), vector["shared_secret_hex"]);
        let binding = CodeWrapBinding::new(
            vector["binding"]["registration_request_id"]
                .as_str()
                .unwrap(),
            vector["binding"]["pairing_id"].as_str().unwrap(),
            vector["binding"]["remote_control_generation"]
                .as_str()
                .unwrap(),
            vector["binding"]["relay_epoch"].as_str().unwrap(),
            vector["binding"]["expires_at_ms"]
                .as_str()
                .unwrap()
                .parse()
                .unwrap(),
        )
        .unwrap();
        assert_eq!(hex_lower(&binding.aad().unwrap()), vector["aad_hex"]);
        assert_eq!(
            hex_lower(
                &relay
                    .derive_wrapping_key(receiver.public_key(), &binding)
                    .unwrap()[..],
            ),
            vector["derived_key_hex"]
        );
        let nonce: [u8; 12] = decode_hex(vector["nonce_hex"].as_str().unwrap())
            .try_into()
            .unwrap();
        let envelope = relay
            .seal_pairing_code_with_nonce(
                receiver.public_key(),
                &binding,
                vector["pairing_code"].as_str().unwrap(),
                nonce,
            )
            .unwrap();
        assert_eq!(hex_lower(&envelope), vector["envelope_hex"]);
        assert_eq!(
            base64::engine::general_purpose::STANDARD.encode(&envelope),
            vector["sealed_code_b64"]
        );
        assert_eq!(
            receiver
                .open_sealed_code(&binding, &envelope)
                .unwrap()
                .expose(),
            "2345678"
        );

        for index in [0, 1, 33, SEALED_CODE_BYTES - 1] {
            let mut tampered = envelope.clone();
            tampered[index] ^= 1;
            assert!(receiver.open_sealed_code(&binding, &tampered).is_err());
        }
        let stale_binding = CodeWrapBinding::new(
            "registration-request-0001",
            "pairing-other",
            "generation-0001",
            "epoch-test-1",
            30_000,
        )
        .unwrap();
        assert_eq!(
            receiver
                .open_sealed_code(&stale_binding, &envelope)
                .unwrap_err()
                .code,
            "authentication_failed"
        );
        assert_eq!(
            receiver
                .open_sealed_code(&binding, &envelope[..SEALED_CODE_BYTES - 1])
                .unwrap_err()
                .code,
            "invalid_ciphertext"
        );
        let source = include_str!("crypto.rs");
        assert!(source.contains("pub(crate) fn seal_pairing_code(\n        self,"));
        let forbidden = ["pub(crate) fn seal_pairing_code_with", "_nonce"].concat();
        assert!(!source.contains(&forbidden));
    }

    #[test]
    fn code_wrap_and_handshake_keys_are_domain_separated_types() {
        let receiver = CodeWrapKeyPair::from_test_bytes([3_u8; 32]);
        let relay = CodeWrapKeyPair::from_test_bytes([4_u8; 32]);
        let receiver_public = receiver.public_key();
        let relay_public = relay.public_key();
        let envelope = relay
            .seal_pairing_code_with_nonce(receiver_public, &code_binding(), "2345678", [9_u8; 12])
            .unwrap();
        assert_eq!(envelope[0], SEALED_CODE_VERSION);
        assert_eq!(&envelope[1..33], &relay_public.to_bytes());
        assert_eq!(
            receiver
                .open_sealed_code(&code_binding(), &envelope)
                .unwrap()
                .expose(),
            "2345678"
        );
        assert!(!format!("{receiver:?}").contains("030303"));

        let handshake = HandshakeKeyPair::from_test_bytes([3_u8; 32]);
        assert_eq!(
            receiver.public_key().to_bytes(),
            handshake.public_key().to_bytes(),
            "the same scalar demonstrates that domain isolation is enforced by distinct types"
        );
        assert_ne!(
            std::any::type_name_of_val(&receiver.public_key()),
            std::any::type_name_of_val(&handshake.public_key())
        );
        let (_a, _b) = session_pair();
    }

    #[test]
    fn production_sealed_code_sealer_is_one_shot_and_owns_its_nonce() {
        let receiver = CodeWrapKeyPair::from_test_bytes([3_u8; 32]);
        let receiver_public = receiver.public_key();
        let first = CodeWrapKeyPair::generate()
            .unwrap()
            .seal_pairing_code(receiver_public, &code_binding(), "2345678")
            .unwrap();
        let second = CodeWrapKeyPair::generate()
            .unwrap()
            .seal_pairing_code(receiver_public, &code_binding(), "2345678")
            .unwrap();
        assert_ne!(&first[1..45], &second[1..45]);
        assert_eq!(
            receiver
                .open_sealed_code(&code_binding(), &first)
                .unwrap()
                .expose(),
            "2345678"
        );
        assert_eq!(
            receiver
                .open_sealed_code(&code_binding(), &second)
                .unwrap()
                .expose(),
            "2345678"
        );
    }

    #[test]
    fn x25519_rejects_non_contributory_peer_points_for_both_key_types() {
        let code_wrap = CodeWrapKeyPair::from_test_bytes([3_u8; 32]);
        let handshake = HandshakeKeyPair::from_test_bytes([4_u8; 32]);
        for peer in [[0_u8; 32], {
            let mut low_order = [0_u8; 32];
            low_order[0] = 1;
            low_order
        }] {
            assert_eq!(
                code_wrap
                    .derive_wrapping_key(CodeWrapPublicKey::from_bytes(peer), &code_binding())
                    .unwrap_err()
                    .code,
                "invalid_peer_key"
            );
            let replacement = HandshakeKeyPair::from_test_bytes([4_u8; 32]);
            assert_eq!(
                replacement
                    .into_session_keys(HandshakePublicKey::from_bytes(peer), &binding())
                    .unwrap_err()
                    .code,
                "invalid_peer_key"
            );
        }
        assert_eq!(handshake.public_key().to_bytes().len(), 32);
    }

    #[test]
    fn generated_secret_owners_use_zeroizing_and_redacted_types() {
        fn require_zeroizing_array(_: &Zeroizing<[u8; 32]>) {}

        let random_owner = random_32().unwrap();
        require_zeroizing_array(&random_owner);
        let wrap = CodeWrapKeyPair::generate().unwrap();
        assert_eq!(format!("{wrap:?}"), "CodeWrapKeyPair([REDACTED])");
        let handshake = HandshakeKeyPair::generate().unwrap();
        assert_eq!(format!("{handshake:?}"), "HandshakeKeyPair([REDACTED])");
        let receipt = ReceiptSecret::generate().unwrap();
        require_zeroizing_array(&receipt.0);
        let (a, _b) = session_pair();
        require_zeroizing_array(&a.finished_a);
        require_zeroizing_array(&a.finished_b);
        require_zeroizing_array(&a.business_a_to_b);
        require_zeroizing_array(&a.business_b_to_a);
        require_zeroizing_array(&a.resume_secret);
        let ciphers = a.into_role_ciphers(ControllerRole::A, lease(1)).unwrap();
        require_zeroizing_array(&ciphers.resume_secret.0);
    }

    #[test]
    fn pinned_trust_config_is_strict_about_shape() {
        let key = base64::engine::general_purpose::STANDARD.encode([7_u8; 32]);
        assert_eq!(
            AuthenticatedRelayTrustConfig::from_pinned_keys(Vec::<(String, String)>::new())
                .unwrap_err()
                .code,
            "relay_trust_unconfigured"
        );
        for (key_id, value) in [
            ("bad id", key.clone()),
            ("relay-key-1", "not-base64!".to_owned()),
            (
                "relay-key-1",
                base64::engine::general_purpose::STANDARD.encode([7_u8; 31]),
            ),
        ] {
            assert_eq!(
                AuthenticatedRelayTrustConfig::from_pinned_keys([(key_id.to_owned(), value)])
                    .unwrap_err()
                    .code,
                "invalid_pinned_key"
            );
        }
        assert_eq!(
            AuthenticatedRelayTrustConfig::from_pinned_keys([
                ("relay-key-1".to_owned(), key.clone()),
                ("relay-key-1".to_owned(), key.clone()),
            ])
            .unwrap_err()
            .code,
            "invalid_pinned_key"
        );
        assert!(
            AuthenticatedRelayTrustConfig::from_pinned_keys([("relay-key-1".to_owned(), key)])
                .is_ok()
        );
    }

    #[test]
    fn relay_trust_loader_fails_closed_without_a_complete_pin() {
        assert_eq!(
            load_relay_trust_config(|_| None).unwrap_err().code,
            "relay_trust_unconfigured"
        );
        assert_eq!(
            load_relay_trust_config(|name| match name {
                "PINVOU_REMOTE_RELAY_SIGNING_KEY_ID" => Some("relay-key-1".to_owned()),
                _ => None,
            })
            .unwrap_err()
            .code,
            "relay_trust_unconfigured"
        );
        let public_key = base64::engine::general_purpose::STANDARD.encode([7_u8; 32]);
        assert!(load_relay_trust_config(|name| match name {
            "PINVOU_REMOTE_RELAY_SIGNING_KEY_ID" => Some("relay-key-1".to_owned()),
            "PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64" => Some(public_key.clone()),
            _ => None,
        })
        .is_ok());
    }

    #[test]
    fn pinned_trust_config_authenticates_the_frozen_transcript() {
        let fixture = fixture();
        let vector = &fixture["lease_install_vector"]["verification_input"];
        let message = serde_json::to_string(&vector["reserved_message"]).unwrap();
        let trusted_key = fixture["untrusted_authentication_inputs"][1]["value"]["keys"][0]
            ["public_key_b64"]
            .as_str()
            .unwrap()
            .to_owned();
        let trust = AuthenticatedRelayTrustConfig::from_pinned_keys([(
            "relay-key-test-1".to_owned(),
            trusted_key,
        )])
        .unwrap();
        let context =
            authenticate_relay_transcript(&trust, parse_controller_pairing_transcript(&message).unwrap())
                .unwrap();
        assert!(format!("{context:?}").contains("pairing-0001"));

        let attacker_key = fixture["untrusted_authentication_inputs"][2]["value"]["public_key_b64"]
            .as_str()
            .unwrap()
            .to_owned();
        let attacker = AuthenticatedRelayTrustConfig::from_pinned_keys([(
            "relay-key-test-1".to_owned(),
            attacker_key,
        )])
        .unwrap();
        assert_eq!(
            authenticate_relay_transcript(
                &attacker,
                parse_controller_pairing_transcript(&message).unwrap()
            )
            .unwrap_err()
            .field,
            "relay_signature_b64"
        );
    }

    #[test]
    fn enrolled_ed25519_key_authenticates_the_frozen_transcript() {
        let fixture = fixture();
        let vector = &fixture["lease_install_vector"]["verification_input"];
        let input = serde_json::to_string(&vector["reserved_message"]).unwrap();
        let parsed = parse_controller_pairing_transcript(&input).unwrap();
        let key: [u8; 32] = base64::engine::general_purpose::STANDARD
            .decode(
                fixture["untrusted_authentication_inputs"][1]["value"]["keys"][0]["public_key_b64"]
                    .as_str()
                    .unwrap(),
            )
            .unwrap()
            .try_into()
            .unwrap();
        let authenticator =
            TranscriptAuthenticator::new(AuthenticatedRelayTrustConfig::from_test_enrollment([(
                "relay-key-test-1".to_owned(),
                key,
            )]));
        let context = authenticator.authenticate(parsed).unwrap();
        assert!(format!("{context:?}").contains("pairing-0001"));

        let attacker_key: [u8; 32] = base64::engine::general_purpose::STANDARD
            .decode(
                fixture["untrusted_authentication_inputs"][2]["value"]["public_key_b64"]
                    .as_str()
                    .unwrap(),
            )
            .unwrap()
            .try_into()
            .unwrap();
        let parsed = parse_controller_pairing_transcript(&input).unwrap();
        let attacker =
            TranscriptAuthenticator::new(AuthenticatedRelayTrustConfig::from_test_enrollment([(
                "relay-key-test-1".to_owned(),
                attacker_key,
            )]));
        assert_eq!(
            attacker.authenticate(parsed).unwrap_err().field,
            "relay_signature_b64"
        );

        let mut unknown_key_message = vector["reserved_message"].clone();
        unknown_key_message["relay_signing_key_id"] = Value::String("relay-key-unknown".into());
        let parsed = parse_controller_pairing_transcript(
            &serde_json::to_string(&unknown_key_message).unwrap(),
        )
        .unwrap();
        assert_eq!(
            authenticator.authenticate(parsed).unwrap_err().field,
            "relay_signing_key_id"
        );

        let mutation = &fixture["lease_install_vector"]["unauthenticated_transcript_mutations"][0];
        let mut tampered = vector["reserved_message"].clone();
        tampered["transcript"] = mutation["transcript"].clone();
        tampered["transcript_hash"] = mutation["transcript_hash"].clone();
        tampered["relay_signature_b64"] = mutation["relay_signature_b64"].clone();
        let parsed =
            parse_controller_pairing_transcript(&serde_json::to_string(&tampered).unwrap())
                .unwrap();
        assert_eq!(
            authenticator.authenticate(parsed).unwrap_err().field,
            "relay_signature_b64"
        );

        let crypto_source = include_str!("crypto.rs");
        let protocol_source = include_str!("protocol.rs");
        let public_constructor = ["pub(crate) fn ", "new(trust:"].concat();
        let replaceable_trait = ["trait Transcript", "Authenticator"].concat();
        assert!(!crypto_source.contains(&public_constructor));
        assert!(!crypto_source.contains(&replaceable_trait));
        assert!(!protocol_source.contains(&replaceable_trait));
        let concrete_type = ["struct Transcript", "Authenticator"].concat();
        assert_eq!(crypto_source.matches(&concrete_type).count(), 1);
    }

    #[test]
    fn finished_binds_role_transcript_and_receipt() {
        let (a, b) = role_pair(7);
        let receipt = ReceiptSecret::from_test_bytes([5_u8; 32]);
        let encrypted = a
            .finished_sender
            .seal_with_nonce(&receipt, [6_u8; 12])
            .unwrap();
        let opened = b.finished_receiver.open(&encrypted).unwrap();
        assert_eq!(
            opened.commitment(&binding(), ControllerRole::A).unwrap(),
            receipt.commitment(&binding(), ControllerRole::A).unwrap()
        );

        let (_a, b_receiver) = role_pair(7);
        let (_a_other, b_sender) = role_pair(7);
        let wrong_role = b_sender
            .finished_sender
            .seal_with_nonce(&receipt, [6_u8; 12])
            .unwrap();
        assert_eq!(
            b_receiver
                .finished_receiver
                .open(&wrong_role)
                .unwrap_err()
                .code,
            "authentication_failed"
        );

        let (a, b) = role_pair(7);
        let encrypted = a
            .finished_sender
            .seal_with_nonce(&receipt, [6_u8; 12])
            .unwrap();
        let last = encrypted.len() - 1;
        let mut damaged = encrypted;
        damaged[last] ^= 1;
        assert_eq!(
            b.finished_receiver.open(&damaged).unwrap_err().code,
            "authentication_failed"
        );
    }

    #[test]
    fn receipt_commitment_matches_the_shared_vector_and_replay_is_bounded() {
        let fixture = fixture();
        let vector = &fixture["receipt_commitment_vector"];
        let secret: [u8; 32] = decode_hex(vector["secret_hex"].as_str().unwrap())
            .try_into()
            .unwrap();
        let transcript_hash: [u8; 32] = decode_hex(vector["transcript_hash"].as_str().unwrap())
            .try_into()
            .unwrap();
        let binding = HandshakeBinding::new(
            vector["relay_epoch"].as_str().unwrap(),
            vector["pairing_id"].as_str().unwrap(),
            vector["reservation_id"].as_str().unwrap(),
            transcript_hash,
        )
        .unwrap();
        let commitment = ReceiptSecret::from_test_bytes(secret)
            .commitment(&binding, ControllerRole::A)
            .unwrap();
        assert_eq!(
            hex_lower(&commitment),
            vector["commitment_hex"].as_str().unwrap()
        );
        let mut guard = ReceiptReplayGuard::default();
        guard.accept(commitment).unwrap();
        assert_eq!(guard.accept(commitment).unwrap_err().code, "replay");
    }

    #[test]
    fn business_aead_binds_direction_epoch_sequence_and_replay_window() {
        let (a, b) = role_pair(7);
        let mut sender = a.send;
        let mut receiver = b.receive;
        let mut opposite_sender = b.send;
        let ciphertext = sender.seal(7, 1, "task_submit", b"payload").unwrap();
        assert_eq!(
            receiver.open(7, 1, "task_submit", &ciphertext).unwrap(),
            b"payload"
        );
        assert_eq!(
            receiver
                .open(7, 1, "task_submit", &ciphertext)
                .unwrap_err()
                .code,
            "replay"
        );
        let wrong_direction = opposite_sender
            .seal(7, 1, "task_submit", b"wrong-direction")
            .unwrap();
        assert_eq!(
            receiver
                .open(7, 2, "task_submit", &wrong_direction)
                .unwrap_err()
                .code,
            "authentication_failed"
        );
        assert_eq!(
            receiver
                .open(8, 2, "task_submit", &ciphertext)
                .unwrap_err()
                .code,
            "unknown_epoch"
        );
        assert_ne!(
            frame_nonce(FrameDirection::AToB, 7, 1),
            frame_nonce(FrameDirection::AToB, 8, 1)
        );

        let frame_2 = sender.seal(7, 2, "task_submit", b"too-old").unwrap();
        let frame_65 = sender.seal(7, 65, "task_submit", b"previous").unwrap();
        let frame_66 = sender.seal(7, 66, "task_submit", b"newest").unwrap();
        assert_eq!(
            receiver.open(7, 66, "task_submit", &frame_66).unwrap(),
            b"newest"
        );
        assert_eq!(
            receiver.open(7, 65, "task_submit", &frame_65).unwrap(),
            b"previous"
        );
        assert_eq!(
            receiver
                .open(7, 2, "task_submit", &frame_2)
                .unwrap_err()
                .code,
            "replay"
        );
        assert_eq!(
            sender
                .seal(7, 66, "task_submit", b"nonce-reuse")
                .unwrap_err()
                .code,
            "replay"
        );
    }

    #[test]
    fn session_keys_split_once_and_max_sequence_exhaustion_is_terminal() {
        let (a, _b) = session_pair();
        let mut first_sender = a
            .into_role_ciphers(ControllerRole::A, lease(7))
            .unwrap()
            .send;
        assert!(
            first_sender
                .seal(7, u64::MAX, "task_submit", b"last")
                .is_ok()
        );
        assert_eq!(
            first_sender
                .seal(7, u64::MAX, "task_submit", b"again")
                .unwrap_err()
                .code,
            "sequence_exhausted"
        );
        assert_eq!(
            first_sender
                .seal(7, 1, "task_submit", b"rewind")
                .unwrap_err()
                .code,
            "sequence_exhausted"
        );
        assert_eq!(
            first_sender
                .seal(8, 1, "task_submit", b"other-epoch")
                .unwrap_err()
                .code,
            "sequence_exhausted"
        );

        let fresh_a = HandshakeKeyPair::from_test_bytes([11_u8; 32]);
        let fresh_b = HandshakeKeyPair::from_test_bytes([12_u8; 32]);
        let mut replacement = fresh_a
            .into_session_keys(fresh_b.public_key(), &binding())
            .unwrap()
            .into_role_ciphers(ControllerRole::A, lease(8))
            .unwrap()
            .send;
        assert_ne!(&first_sender.key[..], &replacement.key[..]);
        assert!(replacement.seal(8, 1, "task_submit", b"new").is_ok());

        let source = include_str!("crypto.rs");
        assert!(source.contains("pub(crate) fn into_role_ciphers(\n        self,"));
        let reusable_factory = ["fn business_", "cipher(&self"].concat();
        assert!(!source.contains(&reusable_factory));
        let clone_impl = ["impl Clone for Session", "Keys"].concat();
        assert!(!source.contains(&clone_impl));
    }

    #[test]
    fn ciphertext_lengths_are_rejected_before_aead_and_failed_auth_does_not_consume_sequence() {
        let (a, b) = role_pair(1);
        let mut sender = a.send;
        let mut receiver = b.receive;
        assert_eq!(
            receiver.open(1, 1, "task_submit", &[]).unwrap_err().code,
            "invalid_ciphertext"
        );
        let mut invalid = vec![0_u8; AEAD_TAG_BYTES];
        assert_eq!(
            receiver
                .open(1, 1, "task_submit", &invalid)
                .unwrap_err()
                .code,
            "authentication_failed"
        );
        let valid = sender.seal(1, 1, "task_submit", b"ok").unwrap();
        assert_eq!(receiver.open(1, 1, "task_submit", &valid).unwrap(), b"ok");
        invalid.resize(MAX_ENCRYPTED_FRAME_BYTES + 1, 0);
        assert_eq!(
            receiver
                .open(1, 2, "task_submit", &invalid)
                .unwrap_err()
                .code,
            "invalid_ciphertext"
        );
    }

    #[test]
    fn sensitive_strings_never_debug_display_or_serialize_the_secret() {
        fn require_zeroizing_string(_: &Zeroizing<String>) {}

        let secret = SensitiveString::new("top-secret-value").unwrap();
        require_zeroizing_string(&secret.value);
        assert_eq!(secret.expose(), "top-secret-value");
        assert_eq!(format!("{secret}"), "[REDACTED]");
        assert_eq!(format!("{secret:?}"), "SensitiveString([REDACTED])");
        let error = serde_json::to_string(&secret).unwrap_err().to_string();
        assert!(!error.contains("top-secret-value"));
        assert!(error.contains("serialization forbidden"));
        assert!(SensitiveString::new("界".repeat(MAX_SENSITIVE_STRING_BYTES / 3)).is_ok());

        for _ in 0..256 {
            let empty_probe = Arc::new(SensitiveDropProbe::default());
            assert_eq!(
                SensitiveString::new_with_drop_probe("", Arc::clone(&empty_probe))
                    .unwrap_err()
                    .code,
                "invalid_sensitive_value"
            );
            assert_eq!(
                empty_probe.observation(),
                (1, 0),
                "this empty rejected candidate ran exactly one instance drop"
            );

            let oversized = "界".repeat(MAX_SENSITIVE_STRING_BYTES / 3 + 1);
            let oversized_bytes = oversized.len();
            let oversized_probe = Arc::new(SensitiveDropProbe::default());
            assert_eq!(
                SensitiveString::new_with_drop_probe(oversized, Arc::clone(&oversized_probe),)
                    .unwrap_err()
                    .code,
                "invalid_sensitive_value"
            );
            assert_eq!(
                oversized_probe.observation(),
                (1, oversized_bytes),
                "this over-limit rejected candidate ran exactly one instance drop"
            );
        }
    }
}
