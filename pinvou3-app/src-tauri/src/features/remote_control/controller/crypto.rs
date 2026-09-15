//! Cryptographic primitives for desktop-to-desktop remote control.
//!
//! This module has no transport or connection-state ownership. Callers must
//! first validate wire schemas in `protocol`; the types here bind already
//! validated identities, roles, epochs and sequences to audited primitives.

use std::collections::{BTreeMap, HashSet};
use std::fmt;
#[cfg(test)]
use std::sync::atomic::{AtomicUsize, Ordering};

use aes_gcm::aead::{Aead, KeyInit, Payload};
use aes_gcm::{Aes256Gcm, Nonce};
use hkdf::Hkdf;
use ring::rand::{SecureRandom, SystemRandom};
use ring::signature::{self, UnparsedPublicKey};
use serde::Serialize;
use serde::ser::{Error as _, Serializer};
use sha2::{Digest, Sha256};
use sha2_10::Sha256 as Sha256V10;
use x25519_dalek::{PublicKey, StaticSecret};
use zeroize::{Zeroize, Zeroizing};

use super::protocol::transcript_authentication::{TranscriptAuthenticator, private::Sealed};
use super::protocol::{
    ControllerProtocolError, MAX_DECRYPTED_PAYLOAD_BYTES, MAX_ENCRYPTED_FRAME_BYTES, Result,
    build_receipt_commitment_preimage,
};

const AEAD_NONCE_BYTES: usize = 12;
const AEAD_TAG_BYTES: usize = 16;
const RECEIPT_SECRET_BYTES: usize = 32;
const REPLAY_WINDOW_BITS: u64 = 64;
const MAX_RECEIPT_REPLAYS: usize = 8;
const MAX_BINDING_BYTES: usize = 128;
#[cfg(test)]
static SECRET_OWNER_DROPS: AtomicUsize = AtomicUsize::new(0);

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

fn random_32() -> CryptoResult<[u8; 32]> {
    let mut value = [0_u8; 32];
    SystemRandom::new()
        .fill(&mut value)
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
    let shared = secret.diffie_hellman(&PublicKey::from(peer)).to_bytes();
    if shared.iter().all(|byte| *byte == 0) {
        return Err(ControllerCryptoError::new("invalid_peer_key", "x25519"));
    }
    Ok(Zeroizing::new(shared))
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
    hkdf.expand(&info, output.as_mut())
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

impl Drop for CodeWrapKeyPair {
    fn drop(&mut self) {
        #[cfg(test)]
        SECRET_OWNER_DROPS.fetch_add(1, Ordering::Relaxed);
    }
}

impl CodeWrapKeyPair {
    pub(crate) fn generate() -> CryptoResult<Self> {
        let mut bytes = random_32()?;
        let secret = StaticSecret::from(bytes);
        bytes.zeroize();
        Ok(Self {
            public: CodeWrapPublicKey(PublicKey::from(&secret).to_bytes()),
            secret,
        })
    }

    pub(crate) fn public_key(&self) -> CodeWrapPublicKey {
        self.public
    }

    pub(crate) fn derive_wrapping_key(
        &self,
        peer_public_key: CodeWrapPublicKey,
        registration_request_id: &str,
        remote_control_generation: &str,
    ) -> CryptoResult<CodeWrapKey> {
        if [registration_request_id, remote_control_generation]
            .iter()
            .any(|field| field.is_empty() || field.len() > MAX_BINDING_BYTES)
        {
            return Err(ControllerCryptoError::new("invalid_binding", "code_wrap"));
        }
        let shared = shared_secret(&self.secret, peer_public_key.to_bytes())?;
        let key = hkdf_expand(
            shared.as_ref(),
            b"pinvou-code-wrap-salt-v1",
            b"pinvou-code-wrap-key-v1",
            &[
                registration_request_id.as_bytes(),
                remote_control_generation.as_bytes(),
            ],
        )?;
        Ok(CodeWrapKey { key })
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

impl Drop for HandshakeKeyPair {
    fn drop(&mut self) {
        #[cfg(test)]
        SECRET_OWNER_DROPS.fetch_add(1, Ordering::Relaxed);
    }
}

impl HandshakeKeyPair {
    pub(crate) fn generate() -> CryptoResult<Self> {
        let mut bytes = random_32()?;
        let secret = StaticSecret::from(bytes);
        bytes.zeroize();
        Ok(Self {
            public: HandshakePublicKey(PublicKey::from(&secret).to_bytes()),
            secret,
        })
    }

    pub(crate) fn public_key(&self) -> HandshakePublicKey {
        self.public
    }

    pub(crate) fn derive_session_keys(
        &self,
        peer_public_key: HandshakePublicKey,
        binding: &HandshakeBinding,
    ) -> CryptoResult<SessionKeys> {
        let shared = shared_secret(&self.secret, peer_public_key.to_bytes())?;
        let info = binding.kdf_fields();
        let derive = |purpose: &'static [u8]| {
            let fields = [info[0], info[1], info[2], info[3], purpose];
            hkdf_expand(
                shared.as_ref(),
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

pub(crate) struct CodeWrapKey {
    key: Zeroizing<[u8; 32]>,
}

impl fmt::Debug for CodeWrapKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("CodeWrapKey([REDACTED])")
    }
}

impl CodeWrapKey {
    pub(crate) fn seal(
        &self,
        nonce: [u8; AEAD_NONCE_BYTES],
        aad: &[u8],
        plaintext: &[u8],
    ) -> CryptoResult<Vec<u8>> {
        seal_aead(&self.key, nonce, aad, plaintext, 4096)
    }

    pub(crate) fn open(
        &self,
        nonce: [u8; AEAD_NONCE_BYTES],
        aad: &[u8],
        ciphertext: &[u8],
    ) -> CryptoResult<Vec<u8>> {
        open_aead(&self.key, nonce, aad, ciphertext, 4096)
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

impl Drop for ReceiptSecret {
    fn drop(&mut self) {
        #[cfg(test)]
        SECRET_OWNER_DROPS.fetch_add(1, Ordering::Relaxed);
    }
}

impl fmt::Debug for ReceiptSecret {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ReceiptSecret([REDACTED])")
    }
}

impl ReceiptSecret {
    pub(crate) fn generate() -> CryptoResult<Self> {
        Ok(Self(Zeroizing::new(random_32()?)))
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
            self.0.as_ref(),
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
}

impl fmt::Debug for SessionKeys {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SessionKeys([REDACTED])")
    }
}

impl Drop for SessionKeys {
    fn drop(&mut self) {
        #[cfg(test)]
        SECRET_OWNER_DROPS.fetch_add(1, Ordering::Relaxed);
    }
}

impl SessionKeys {
    fn finished_key(&self, sender: ControllerRole) -> &[u8; 32] {
        match sender {
            ControllerRole::A => &self.finished_a,
            ControllerRole::B => &self.finished_b,
        }
    }

    pub(crate) fn seal_finished(
        &self,
        sender: ControllerRole,
        receipt: &ReceiptSecret,
    ) -> CryptoResult<Vec<u8>> {
        self.seal_finished_with_nonce(sender, random_nonce()?, receipt)
    }

    fn seal_finished_with_nonce(
        &self,
        sender: ControllerRole,
        nonce: [u8; AEAD_NONCE_BYTES],
        receipt: &ReceiptSecret,
    ) -> CryptoResult<Vec<u8>> {
        let mut plaintext = b"pinvou-controller-finished-v1".to_vec();
        lp(&mut plaintext, &self.transcript_hash)?;
        lp(&mut plaintext, sender.wire())?;
        lp(&mut plaintext, receipt.0.as_ref())?;
        let aad = finished_aad(&self.transcript_hash, sender)?;
        let ciphertext = seal_aead(self.finished_key(sender), nonce, &aad, &plaintext, 1024)?;
        plaintext.zeroize();
        let mut output = nonce.to_vec();
        output.extend_from_slice(&ciphertext);
        Ok(output)
    }

    pub(crate) fn open_finished(
        &self,
        expected_sender: ControllerRole,
        encrypted: &[u8],
    ) -> CryptoResult<ReceiptSecret> {
        if encrypted.len() < AEAD_NONCE_BYTES + AEAD_TAG_BYTES || encrypted.len() > 1024 {
            return Err(ControllerCryptoError::new("invalid_ciphertext", "finished"));
        }
        let mut nonce = [0_u8; AEAD_NONCE_BYTES];
        nonce.copy_from_slice(&encrypted[..AEAD_NONCE_BYTES]);
        let aad = finished_aad(&self.transcript_hash, expected_sender)?;
        let mut plaintext = open_aead(
            self.finished_key(expected_sender),
            nonce,
            &aad,
            &encrypted[AEAD_NONCE_BYTES..],
            1024,
        )?;
        let parsed = parse_finished(&plaintext, &self.transcript_hash, expected_sender);
        plaintext.zeroize();
        parsed.map(|secret| ReceiptSecret(Zeroizing::new(secret)))
    }

    pub(crate) fn business_cipher(
        &self,
        direction: FrameDirection,
        binding: LeaseBinding,
    ) -> CryptoResult<BusinessCipher> {
        let root = match direction {
            FrameDirection::AToB => &self.business_a_to_b,
            FrameDirection::BToA => &self.business_b_to_a,
        };
        let epoch = binding.lease_epoch.to_string();
        let key = hkdf_expand(
            root.as_ref(),
            &self.transcript_hash,
            b"pinvou-controller-business-key-v1",
            &[
                binding.connection_id.as_bytes(),
                binding.controller_lease_id.as_bytes(),
                epoch.as_bytes(),
                binding.relay_epoch.as_bytes(),
                binding.remote_control_generation.as_bytes(),
                direction.wire(),
            ],
        )?;
        Ok(BusinessCipher {
            key,
            binding,
            direction,
            replay: ReplayWindow::default(),
            last_sealed_sequence: 0,
        })
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

pub(crate) struct BusinessCipher {
    key: Zeroizing<[u8; 32]>,
    binding: LeaseBinding,
    direction: FrameDirection,
    replay: ReplayWindow,
    last_sealed_sequence: u64,
}

impl fmt::Debug for BusinessCipher {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("BusinessCipher([REDACTED])")
    }
}

impl BusinessCipher {
    pub(crate) fn seal(
        &mut self,
        lease_epoch: u64,
        sequence: u64,
        payload_kind: &str,
        plaintext: &[u8],
    ) -> CryptoResult<Vec<u8>> {
        self.check_epoch_sequence(lease_epoch, sequence)?;
        if sequence <= self.last_sealed_sequence {
            return Err(ControllerCryptoError::new("replay", "frame_sequence"));
        }
        if plaintext.len() > MAX_DECRYPTED_PAYLOAD_BYTES {
            return Err(ControllerCryptoError::new("plaintext_too_large", "frame"));
        }
        let nonce = frame_nonce(self.direction, lease_epoch, sequence);
        let aad = self.frame_aad(lease_epoch, sequence, payload_kind)?;
        let ciphertext = seal_aead(&self.key, nonce, &aad, plaintext, MAX_ENCRYPTED_FRAME_BYTES)?;
        self.last_sealed_sequence = sequence;
        Ok(ciphertext)
    }

    pub(crate) fn open(
        &mut self,
        lease_epoch: u64,
        sequence: u64,
        payload_kind: &str,
        ciphertext: &[u8],
    ) -> CryptoResult<Vec<u8>> {
        self.check_epoch_sequence(lease_epoch, sequence)?;
        if ciphertext.len() < AEAD_TAG_BYTES
            || ciphertext.len() > MAX_DECRYPTED_PAYLOAD_BYTES + AEAD_TAG_BYTES
        {
            return Err(ControllerCryptoError::new("invalid_ciphertext", "frame"));
        }
        self.replay.check(sequence)?;
        let nonce = frame_nonce(self.direction, lease_epoch, sequence);
        let aad = self.frame_aad(lease_epoch, sequence, payload_kind)?;
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

    fn check_epoch_sequence(&self, lease_epoch: u64, sequence: u64) -> CryptoResult<()> {
        if lease_epoch != self.binding.lease_epoch {
            return Err(ControllerCryptoError::new("unknown_epoch", "lease_epoch"));
        }
        if sequence == 0 {
            return Err(ControllerCryptoError::new("replay", "frame_sequence"));
        }
        Ok(())
    }

    fn frame_aad(
        &self,
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
            self.binding.connection_id.as_bytes(),
            self.binding.controller_lease_id.as_bytes(),
            lease_epoch.as_bytes(),
            self.binding.relay_epoch.as_bytes(),
            self.binding.remote_control_generation.as_bytes(),
            self.direction.wire(),
            sequence.as_bytes(),
            payload_kind.as_bytes(),
        ] {
            lp(&mut output, value)?;
        }
        Ok(output)
    }
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

pub(crate) struct SensitiveString(Zeroizing<String>);

impl SensitiveString {
    pub(crate) fn new(value: impl Into<String>) -> Self {
        Self(Zeroizing::new(value.into()))
    }

    pub(crate) fn expose(&self) -> &str {
        self.0.as_str()
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
    #[cfg(test)]
    fn from_test_enrollment(entries: impl IntoIterator<Item = (String, [u8; 32])>) -> Self {
        Self {
            keys: entries.into_iter().collect(),
        }
    }
}

pub(crate) struct EnrolledTranscriptAuthenticator {
    trust: AuthenticatedRelayTrustConfig,
}

impl EnrolledTranscriptAuthenticator {
    pub(crate) fn new(trust: AuthenticatedRelayTrustConfig) -> Self {
        Self { trust }
    }
}

impl fmt::Debug for EnrolledTranscriptAuthenticator {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("EnrolledTranscriptAuthenticator")
            .field("trust", &self.trust)
            .finish()
    }
}

impl Sealed for EnrolledTranscriptAuthenticator {}

impl TranscriptAuthenticator for EnrolledTranscriptAuthenticator {
    fn verify_relay_signature(
        &self,
        key_id: &str,
        preimage: &[u8],
        signature_bytes: &[u8],
    ) -> Result<()> {
        let public_key =
            self.trust.keys.get(key_id).ok_or_else(|| {
                ControllerProtocolError::new("invalid_field", "relay_signing_key_id")
            })?;
        UnparsedPublicKey::new(&signature::ED25519, public_key)
            .verify(preimage, signature_bytes)
            .map_err(|_| ControllerProtocolError::new("invalid_field", "relay_signature_b64"))
    }
}

fn hex_lower(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine as _;
    use serde_json::Value;

    use super::super::protocol::{
        parse_controller_pairing_transcript, transcript_authentication::TranscriptAuthenticator,
    };

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
        let a_keys = a.derive_session_keys(b.public_key(), &binding()).unwrap();
        let b_keys = b.derive_session_keys(a.public_key(), &binding()).unwrap();
        (a_keys, b_keys)
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
        assert_eq!(
            hex_lower(
                shared_secret(&a.secret, b.public_key().to_bytes())
                    .unwrap()
                    .as_ref()
            ),
            vector["shared_secret_hex"]
        );
        let binding = binding();
        let keys = a.derive_session_keys(b.public_key(), &binding).unwrap();
        assert_eq!(
            hex_lower(keys.finished_a.as_ref()),
            vector["finished"]["derived_key_hex"]
        );
        assert_eq!(
            hex_lower(keys.business_a_to_b.as_ref()),
            vector["business"]["derived_root_hex"]
        );
        let receipt = ReceiptSecret::from_test_bytes([5_u8; 32]);
        let finished = keys
            .seal_finished_with_nonce(ControllerRole::A, [6_u8; 12], &receipt)
            .unwrap();
        assert_eq!(
            hex_lower(&finished),
            vector["finished"]["ciphertext_with_nonce_hex"]
        );
        let mut business = keys
            .business_cipher(FrameDirection::AToB, lease(7))
            .unwrap();
        assert_eq!(
            hex_lower(business.key.as_ref()),
            vector["business"]["derived_key_hex"]
        );
        assert_eq!(
            hex_lower(&frame_nonce(FrameDirection::AToB, 7, 1)),
            vector["business"]["nonce_hex"]
        );
        assert_eq!(
            hex_lower(&business.frame_aad(7, 1, "task_submit").unwrap()),
            vector["business"]["aad_hex"]
        );
        assert_eq!(
            hex_lower(&business.seal(7, 1, "task_submit", b"payload").unwrap()),
            vector["business"]["ciphertext_hex"]
        );
    }

    #[test]
    fn code_wrap_and_handshake_keys_are_domain_separated_types() {
        let wrap_a = CodeWrapKeyPair::from_test_bytes([3_u8; 32]);
        let wrap_b = CodeWrapKeyPair::from_test_bytes([4_u8; 32]);
        let key_a = wrap_a
            .derive_wrapping_key(wrap_b.public_key(), "registration-1", "generation-1")
            .unwrap();
        let key_b = wrap_b
            .derive_wrapping_key(wrap_a.public_key(), "registration-1", "generation-1")
            .unwrap();
        let nonce = [9_u8; 12];
        let ciphertext = key_a.seal(nonce, b"wrap-aad", b"2345678").unwrap();
        assert_eq!(
            key_b.open(nonce, b"wrap-aad", &ciphertext).unwrap(),
            b"2345678"
        );
        assert!(!format!("{wrap_a:?}").contains("030303"));

        let handshake = HandshakeKeyPair::from_test_bytes([3_u8; 32]);
        assert_eq!(
            wrap_a.public_key().to_bytes(),
            handshake.public_key().to_bytes(),
            "the same scalar demonstrates that domain isolation is enforced by distinct types"
        );
        assert_ne!(
            std::any::type_name_of_val(&wrap_a.public_key()),
            std::any::type_name_of_val(&handshake.public_key())
        );
        let (_a, _b) = session_pair();
    }

    #[test]
    fn generated_secret_owners_run_zeroizing_drop_paths() {
        let before = SECRET_OWNER_DROPS.load(Ordering::Relaxed);
        {
            let _wrap = CodeWrapKeyPair::generate().unwrap();
            let _handshake = HandshakeKeyPair::generate().unwrap();
            let _receipt = ReceiptSecret::generate().unwrap();
            let (a, _b) = session_pair();
            let _cipher = a.business_cipher(FrameDirection::AToB, lease(1)).unwrap();
        }
        assert!(SECRET_OWNER_DROPS.load(Ordering::Relaxed) >= before + 5);
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
        let authenticator = EnrolledTranscriptAuthenticator::new(
            AuthenticatedRelayTrustConfig::from_test_enrollment([(
                "relay-key-test-1".to_owned(),
                key,
            )]),
        );
        let verified = authenticator.authenticate(parsed).unwrap();
        let context = super::super::protocol::LeaseInstallValidationContext::new(verified);
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
        let attacker = EnrolledTranscriptAuthenticator::new(
            AuthenticatedRelayTrustConfig::from_test_enrollment([(
                "relay-key-test-1".to_owned(),
                attacker_key,
            )]),
        );
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
    }

    #[test]
    fn finished_binds_role_transcript_and_receipt() {
        let (a, b) = session_pair();
        let receipt = ReceiptSecret::from_test_bytes([5_u8; 32]);
        let encrypted = a
            .seal_finished_with_nonce(ControllerRole::A, [6_u8; 12], &receipt)
            .unwrap();
        let opened = b.open_finished(ControllerRole::A, &encrypted).unwrap();
        assert_eq!(
            opened.commitment(&binding(), ControllerRole::A).unwrap(),
            receipt.commitment(&binding(), ControllerRole::A).unwrap()
        );
        assert_eq!(
            b.open_finished(ControllerRole::B, &encrypted)
                .unwrap_err()
                .code,
            "authentication_failed"
        );
        let last = encrypted.len() - 1;
        let mut damaged = encrypted;
        damaged[last] ^= 1;
        assert_eq!(
            b.open_finished(ControllerRole::A, &damaged)
                .unwrap_err()
                .code,
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
        let (a, b) = session_pair();
        let mut sender = a.business_cipher(FrameDirection::AToB, lease(7)).unwrap();
        let mut receiver = b.business_cipher(FrameDirection::AToB, lease(7)).unwrap();
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
        let mut wrong_direction = b.business_cipher(FrameDirection::BToA, lease(7)).unwrap();
        assert_eq!(
            wrong_direction
                .open(
                    7,
                    2,
                    "task_submit",
                    &sender.seal(7, 2, "task_submit", b"x").unwrap()
                )
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

        let mut reordered = b.business_cipher(FrameDirection::AToB, lease(7)).unwrap();
        let mut old_sender = a.business_cipher(FrameDirection::AToB, lease(7)).unwrap();
        let frame_2 = old_sender.seal(7, 2, "task_submit", b"too-old").unwrap();
        let frame_65 = sender.seal(7, 65, "task_submit", b"previous").unwrap();
        let frame_66 = sender.seal(7, 66, "task_submit", b"newest").unwrap();
        assert_eq!(
            reordered.open(7, 66, "task_submit", &frame_66).unwrap(),
            b"newest"
        );
        assert_eq!(
            reordered.open(7, 65, "task_submit", &frame_65).unwrap(),
            b"previous"
        );
        assert_eq!(
            reordered
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
    fn ciphertext_lengths_are_rejected_before_aead_and_failed_auth_does_not_consume_sequence() {
        let (a, b) = session_pair();
        let mut sender = a.business_cipher(FrameDirection::AToB, lease(1)).unwrap();
        let mut receiver = b.business_cipher(FrameDirection::AToB, lease(1)).unwrap();
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
        let secret = SensitiveString::new("top-secret-value");
        assert_eq!(secret.expose(), "top-secret-value");
        assert_eq!(format!("{secret}"), "[REDACTED]");
        assert_eq!(format!("{secret:?}"), "SensitiveString([REDACTED])");
        let error = serde_json::to_string(&secret).unwrap_err().to_string();
        assert!(!error.contains("top-secret-value"));
        assert!(error.contains("serialization forbidden"));
    }
}
