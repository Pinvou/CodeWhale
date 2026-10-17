//! Typed, host-independent controller events. Pure connection events cannot
//! carry a Session; B and A session events have separate binding types.

use super::ports::RemotePortError;

fn invalid() -> RemotePortError {
    RemotePortError::new("remote_event_invalid", false)
}

fn valid_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"._:-".contains(&byte))
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct EventLease {
    peer_id: String,
    resume_boundary_id: String,
    connection_id: String,
    controller_lease_id: String,
    lease_epoch: String,
    relay_epoch: String,
    generation: String,
}

impl EventLease {
    pub(crate) fn new(
        peer_id: String,
        resume_boundary_id: String,
        connection_id: String,
        controller_lease_id: String,
        lease_epoch: String,
        relay_epoch: String,
        generation: String,
    ) -> Result<Self, RemotePortError> {
        if [
            &peer_id,
            &resume_boundary_id,
            &connection_id,
            &controller_lease_id,
            &lease_epoch,
            &relay_epoch,
            &generation,
        ]
        .iter()
        .any(|field| !valid_id(field))
        {
            return Err(invalid());
        }
        Ok(Self {
            peer_id,
            resume_boundary_id,
            connection_id,
            controller_lease_id,
            lease_epoch,
            relay_epoch,
            generation,
        })
    }

    pub(crate) fn peer_id(&self) -> &str {
        &self.peer_id
    }
    pub(crate) fn resume_boundary_id(&self) -> &str {
        &self.resume_boundary_id
    }
    pub(crate) fn connection_id(&self) -> &str {
        &self.connection_id
    }
    pub(crate) fn controller_lease_id(&self) -> &str {
        &self.controller_lease_id
    }
    pub(crate) fn lease_epoch(&self) -> &str {
        &self.lease_epoch
    }
    pub(crate) fn relay_epoch(&self) -> &str {
        &self.relay_epoch
    }
    pub(crate) fn generation(&self) -> &str {
        &self.generation
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ConnectionEventKind {
    Connected,
    Reconnecting,
    Disconnected,
}

impl ConnectionEventKind {
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            Self::Connected => "connected",
            Self::Reconnecting => "reconnecting",
            Self::Disconnected => "disconnected",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ConnectionRole {
    Controller,
    Controlled,
}

impl ConnectionRole {
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            Self::Controller => "controller",
            Self::Controlled => "controlled",
        }
    }
}

/// No session/ref field exists on the pure control plane.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ConnectionEvent {
    lease: EventLease,
    role: ConnectionRole,
    kind: ConnectionEventKind,
}

impl ConnectionEvent {
    pub(crate) fn new(lease: EventLease, role: ConnectionRole, kind: ConnectionEventKind) -> Self {
        Self { lease, role, kind }
    }

    pub(crate) fn lease(&self) -> &EventLease {
        &self.lease
    }
    pub(crate) fn role(&self) -> ConnectionRole {
        self.role
    }
    pub(crate) fn kind(&self) -> ConnectionEventKind {
        self.kind
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ControlledSessionBinding {
    lease: EventLease,
    local_session_id: String,
    remote_session_ref: String,
}

impl ControlledSessionBinding {
    pub(crate) fn new(
        lease: EventLease,
        local_session_id: String,
        remote_session_ref: String,
    ) -> Result<Self, RemotePortError> {
        if !valid_id(&local_session_id) || !valid_id(&remote_session_ref) {
            return Err(invalid());
        }
        Ok(Self {
            lease,
            local_session_id,
            remote_session_ref,
        })
    }

    pub(crate) fn lease(&self) -> &EventLease {
        &self.lease
    }

    /// A B-authoritative session event can only name the controlled role.
    /// The role is derived from this type, never supplied by its caller.
    pub(crate) fn role(&self) -> ConnectionRole {
        ConnectionRole::Controlled
    }
    pub(crate) fn local_session_id(&self) -> &str {
        &self.local_session_id
    }
    pub(crate) fn remote_session_ref(&self) -> &str {
        &self.remote_session_ref
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
enum ControlledSessionEventKind {
    Capability {
        binding: ControlledSessionBinding,
        revision: u64,
    },
    Task {
        binding: ControlledSessionBinding,
        task_id: String,
    },
    Journal {
        binding: ControlledSessionBinding,
        task_id: String,
        journal_epoch: String,
        sequence: u64,
        event_id: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ControlledSessionEvent {
    inner: ControlledSessionEventKind,
}

pub(crate) enum ControlledSessionView<'a> {
    Capability {
        binding: &'a ControlledSessionBinding,
        revision: u64,
    },
    Task {
        binding: &'a ControlledSessionBinding,
        task_id: &'a str,
    },
    Journal {
        binding: &'a ControlledSessionBinding,
        task_id: &'a str,
        journal_epoch: &'a str,
        sequence: u64,
        event_id: &'a str,
    },
}

impl ControlledSessionView<'_> {
    pub(crate) fn binding(&self) -> &ControlledSessionBinding {
        match self {
            Self::Capability { binding, .. }
            | Self::Task { binding, .. }
            | Self::Journal { binding, .. } => binding,
        }
    }
}

impl ControlledSessionEvent {
    pub(crate) fn capability(binding: ControlledSessionBinding, revision: u64) -> Self {
        // Zero is the initial authenticated capability revision; higher values
        // supersede it. Freshness is checked by the injected binding owner.
        Self {
            inner: ControlledSessionEventKind::Capability { binding, revision },
        }
    }

    pub(crate) fn task(
        binding: ControlledSessionBinding,
        task_id: String,
    ) -> Result<Self, RemotePortError> {
        if !valid_id(&task_id) {
            return Err(invalid());
        }
        Ok(Self {
            inner: ControlledSessionEventKind::Task { binding, task_id },
        })
    }

    pub(crate) fn journal(
        binding: ControlledSessionBinding,
        task_id: String,
        journal_epoch: String,
        sequence: u64,
        event_id: String,
    ) -> Result<Self, RemotePortError> {
        if !valid_id(&task_id) || !valid_id(&journal_epoch) || sequence == 0 || !valid_id(&event_id)
        {
            return Err(invalid());
        }
        Ok(Self {
            inner: ControlledSessionEventKind::Journal {
                binding,
                task_id,
                journal_epoch,
                sequence,
                event_id,
            },
        })
    }

    pub(crate) fn view(&self) -> ControlledSessionView<'_> {
        match &self.inner {
            ControlledSessionEventKind::Capability { binding, revision } => {
                ControlledSessionView::Capability {
                    binding,
                    revision: *revision,
                }
            }
            ControlledSessionEventKind::Task { binding, task_id } => {
                ControlledSessionView::Task { binding, task_id }
            }
            ControlledSessionEventKind::Journal {
                binding,
                task_id,
                journal_epoch,
                sequence,
                event_id,
            } => ControlledSessionView::Journal {
                binding,
                task_id,
                journal_epoch,
                sequence: *sequence,
                event_id,
            },
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ProxySessionBinding {
    lease: EventLease,
    local_session_id: String,
    remote_session_ref: String,
    controlled_session_id: String,
}

impl ProxySessionBinding {
    pub(crate) fn new(
        lease: EventLease,
        local_session_id: String,
        remote_session_ref: String,
        controlled_session_id: String,
    ) -> Result<Self, RemotePortError> {
        if !valid_id(&local_session_id)
            || !local_session_id.starts_with("remote-")
            || !valid_id(&remote_session_ref)
            || !valid_id(&controlled_session_id)
        {
            return Err(invalid());
        }
        Ok(Self {
            lease,
            local_session_id,
            remote_session_ref,
            controlled_session_id,
        })
    }

    pub(crate) fn lease(&self) -> &EventLease {
        &self.lease
    }
    pub(crate) fn local_session_id(&self) -> &str {
        &self.local_session_id
    }
    pub(crate) fn remote_session_ref(&self) -> &str {
        &self.remote_session_ref
    }
    pub(crate) fn controlled_session_id(&self) -> &str {
        &self.controlled_session_id
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DisplayKind {
    UserMessage,
    AssistantMessage,
    Reasoning,
    Tool,
    Approval,
    Status,
    Error,
    Attachment,
    Artifact,
    SnapshotMarker,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct AttachmentIndex {
    id: String,
    display_name: String,
    content_type: String,
    byte_length: u64,
    checksum_sha256: String,
}

impl AttachmentIndex {
    pub(crate) fn new(
        id: String,
        display_name: String,
        content_type: String,
        byte_length: u64,
        checksum_sha256: String,
    ) -> Result<Self, RemotePortError> {
        let item = Self {
            id,
            display_name,
            content_type,
            byte_length,
            checksum_sha256,
        };
        item.validate()?;
        Ok(item)
    }

    fn validate(&self) -> Result<(), RemotePortError> {
        if !valid_id(&self.id)
            || self.display_name.is_empty()
            || self.display_name.len() > 256
            || self.content_type.is_empty()
            || self.content_type.len() > 128
            || self.checksum_sha256.len() != 64
            || !self
                .checksum_sha256
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit())
        {
            return Err(invalid());
        }
        Ok(())
    }

    pub(crate) fn id(&self) -> &str {
        &self.id
    }
    pub(crate) fn display_name(&self) -> &str {
        &self.display_name
    }
    pub(crate) fn content_type(&self) -> &str {
        &self.content_type
    }
    pub(crate) fn byte_length(&self) -> u64 {
        self.byte_length
    }
    pub(crate) fn checksum_sha256(&self) -> &str {
        &self.checksum_sha256
    }
}

/// Display-only data selected after B authentication. The verifier must also
/// attest that transport/model credentials are absent before projection.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct JournalDisplay {
    journal_epoch: String,
    sequence: u64,
    event_id: String,
    kind: DisplayKind,
    task_id: String,
    body: Option<String>,
    attachments: Vec<AttachmentIndex>,
}

impl JournalDisplay {
    pub(crate) fn new(
        journal_epoch: String,
        sequence: u64,
        event_id: String,
        kind: DisplayKind,
        task_id: String,
        body: Option<String>,
        attachments: Vec<AttachmentIndex>,
    ) -> Result<Self, RemotePortError> {
        let display = Self {
            journal_epoch,
            sequence,
            event_id,
            kind,
            task_id,
            body,
            attachments,
        };
        display.validate()?;
        Ok(display)
    }

    fn validate(&self) -> Result<(), RemotePortError> {
        if !valid_id(&self.journal_epoch)
            || self.sequence == 0
            || !valid_id(&self.event_id)
            || !valid_id(&self.task_id)
            || self
                .body
                .as_ref()
                .is_some_and(|text| text.len() > 16 * 1024)
            || self.attachments.len() > 16
        {
            return Err(invalid());
        }
        for attachment in &self.attachments {
            attachment.validate()?;
        }
        Ok(())
    }

    pub(crate) fn journal_epoch(&self) -> &str {
        &self.journal_epoch
    }
    pub(crate) fn sequence(&self) -> u64 {
        self.sequence
    }
    pub(crate) fn event_id(&self) -> &str {
        &self.event_id
    }
    pub(crate) fn kind(&self) -> DisplayKind {
        self.kind
    }
    pub(crate) fn task_id(&self) -> &str {
        &self.task_id
    }
    pub(crate) fn body(&self) -> Option<&str> {
        self.body.as_deref()
    }
    pub(crate) fn attachments(&self) -> &[AttachmentIndex] {
        &self.attachments
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TerminalStatus {
    Succeeded,
    Failed,
    Cancelled,
    Interrupted,
}

impl TerminalStatus {
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
            Self::Interrupted => "interrupted",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct TerminalDisplay {
    journal: JournalDisplay,
    client_request_id: String,
    request_fingerprint: String,
    status: TerminalStatus,
}

impl TerminalDisplay {
    pub(crate) fn new(
        mut journal: JournalDisplay,
        client_request_id: String,
        request_fingerprint: String,
        status: TerminalStatus,
    ) -> Result<Self, RemotePortError> {
        journal.body = Some(status.as_str().to_owned());
        let display = Self {
            journal,
            client_request_id,
            request_fingerprint,
            status,
        };
        display.validate()?;
        Ok(display)
    }

    fn validate(&self) -> Result<(), RemotePortError> {
        self.journal.validate()?;
        if self.journal.kind != DisplayKind::Status
            || self.journal.body.as_deref() != Some(self.status.as_str())
            || !valid_id(&self.client_request_id)
            || self.request_fingerprint.len() != 64
            || !self
                .request_fingerprint
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        {
            return Err(invalid());
        }
        Ok(())
    }

    pub(crate) fn journal(&self) -> &JournalDisplay {
        &self.journal
    }
    pub(crate) fn client_request_id(&self) -> &str {
        &self.client_request_id
    }
    pub(crate) fn request_fingerprint(&self) -> &str {
        &self.request_fingerprint
    }
    pub(crate) fn status(&self) -> TerminalStatus {
        self.status
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct SnapshotDisplay {
    journal_epoch: String,
    sequence: u64,
    entries: Vec<JournalDisplay>,
}

impl SnapshotDisplay {
    pub(crate) fn new(
        journal_epoch: String,
        sequence: u64,
        entries: Vec<JournalDisplay>,
    ) -> Result<Self, RemotePortError> {
        let display = Self {
            journal_epoch,
            sequence,
            entries,
        };
        display.validate()?;
        Ok(display)
    }

    fn validate(&self) -> Result<(), RemotePortError> {
        if !valid_id(&self.journal_epoch) || self.entries.len() > 512 {
            return Err(invalid());
        }
        let mut previous = None;
        for entry in &self.entries {
            entry.validate()?;
            if entry.journal_epoch != self.journal_epoch
                || previous.is_some_and(|old: u64| old.checked_add(1) != Some(entry.sequence))
            {
                return Err(invalid());
            }
            previous = Some(entry.sequence);
        }
        if previous.unwrap_or(0) != self.sequence {
            return Err(invalid());
        }
        Ok(())
    }

    pub(crate) fn journal_epoch(&self) -> &str {
        &self.journal_epoch
    }
    pub(crate) fn sequence(&self) -> u64 {
        self.sequence
    }
    pub(crate) fn entries(&self) -> &[JournalDisplay] {
        &self.entries
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum ProxyEventKind {
    Journal(JournalDisplay),
    Terminal(TerminalDisplay),
    Snapshot(SnapshotDisplay),
}

/// Per-variant constructors avoid a general envelope with optional fields.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ProxyEventCandidate {
    binding: ProxySessionBinding,
    kind: ProxyEventKind,
}

impl ProxyEventCandidate {
    pub(crate) fn journal(
        binding: ProxySessionBinding,
        journal: JournalDisplay,
    ) -> Result<Self, RemotePortError> {
        journal.validate()?;
        Ok(Self {
            binding,
            kind: ProxyEventKind::Journal(journal),
        })
    }

    pub(crate) fn terminal(
        binding: ProxySessionBinding,
        terminal: TerminalDisplay,
    ) -> Result<Self, RemotePortError> {
        terminal.validate()?;
        Ok(Self {
            binding,
            kind: ProxyEventKind::Terminal(terminal),
        })
    }

    pub(crate) fn snapshot(
        binding: ProxySessionBinding,
        snapshot: SnapshotDisplay,
    ) -> Result<Self, RemotePortError> {
        snapshot.validate()?;
        Ok(Self {
            binding,
            kind: ProxyEventKind::Snapshot(snapshot),
        })
    }

    pub(crate) fn binding(&self) -> &ProxySessionBinding {
        &self.binding
    }
    pub(crate) fn kind(&self) -> &ProxyEventKind {
        &self.kind
    }
}

pub(crate) trait ProxyEventVerificationPort: Send + Sync {
    /// Verify the encrypted ingress proof and redacted display projection.
    fn verify(&self, candidate: &ProxyEventCandidate) -> Result<(), RemotePortError>;
}

/// Private fields prevent a caller from asserting `authenticated: true`.
pub(crate) struct AuthenticatedProxyEvent {
    candidate: ProxyEventCandidate,
}

impl AuthenticatedProxyEvent {
    pub(crate) fn verify(
        candidate: ProxyEventCandidate,
        verifier: &dyn ProxyEventVerificationPort,
    ) -> Result<Self, RemotePortError> {
        verifier.verify(&candidate)?;
        Ok(Self { candidate })
    }

    pub(crate) fn candidate(&self) -> &ProxyEventCandidate {
        &self.candidate
    }
}
