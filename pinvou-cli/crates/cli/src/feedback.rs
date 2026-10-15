//! `feedback` family: user feedback submission, mirroring
//! `pinvou3-app/src-tauri/src/app/commands/settings.rs::submit_feedback` →
//! `features::feedback::submit_feedback`.
//!
//! The feature function validates the request, packages it (attachments and
//! manifest) under `$PINVOU3_HOME/feedback/pending/`, hands the package to the
//! official feedback channel's uploader, and answers with a receipt. The CLI
//! calls exactly that function and discloses two surface deviations:
//! - the GUI keeps the receipt in-app; the CLI persists one file per
//!   submission under `$PINVOU3_HOME/feedback/receipts/<id>.json`, holding the
//!   returned receipt plus the request it answers;
//! - the CLI's own one-shot staging file (below) is removed once the run
//!   concludes; the feature's own package directory follows the feature's
//!   retention rules (a retryable upload failure intentionally keeps the
//!   package for a later retry, same as the GUI).
//!
//! Retention: the request is staged under `$PINVOU3_HOME/feedback/pending/`
//! only for the duration of the (consuming) feature call, so the user's text
//! survives a crash in between, and is removed once the run concludes — on
//! every exit path, not only the one where the receipt lands. The receipt
//! directory's writability is verified BEFORE anything is staged, so an
//! unwritable receipt target fails the command with zero side effects instead
//! of leaking a staged bundle. Both files are written `0600` on unix: they
//! contain free text the user wrote, and the default umask would publish it to
//! every account on the machine.
//!
//! `--attach` registers a path and the feature copies it into the package, so
//! attachments are held to the same credential-path policy the GUI's picker
//! enforces (`artifacts::check_sensitive_path`), and the registered set is
//! echoed back in both output modes so `--attach` is not silently a no-op.
//!
//! The CLI fixes the request `entry_point` to `"settings"` (the only two
//! values the feature accepts are the GUI's settings page and error banner)
//! because a terminal invocation has no banner context.
//!
//! 测试阀：`PINVOU_TEST_FEEDBACK_UPLOADER=always_fail` 把上传通道换成一台必败
//! 的假上传器（与官方测试用 `submit_feedback_with_uploader` 注入等价），让契约
//! 测试在没有内网通道的机器上确定性地钉住 `failed_retryable` 合同；生产环境
//! 不设置该变量，走真实上传器。

use std::path::{Path, PathBuf};

use pinvou3_lib::features::feedback::{
    FeedbackAttachmentRequest, FeedbackError, FeedbackStatus, FeedbackSubmitRequest, FeedbackType,
    FeedbackUploader,
};

use crate::support::{render, sandbox_home, success};
use crate::{CliError, CliOutcome, OutputMode};

/// Deterministic test uploader: fails before touching the network, so the
/// `failed_retryable` receipt contract is hermetic on any machine.
struct AlwaysFailUploader;

impl FeedbackUploader for AlwaysFailUploader {
    fn upload<'a>(
        &'a self,
        _package: &'a pinvou3_lib::features::feedback::PreparedFeedbackPackage,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = Result<(), FeedbackError>> + Send + 'a>>
    {
        Box::pin(async {
            Err(FeedbackError::Upload(
                "PINVOU_TEST_FEEDBACK_UPLOADER=always_fail".to_owned(),
            ))
        })
    }
}

fn submit_with_configured_uploader(
    request: FeedbackSubmitRequest,
) -> std::pin::Pin<
    Box<
        dyn std::future::Future<
                Output = Result<pinvou3_lib::features::feedback::FeedbackReceipt, FeedbackError>,
            > + Send,
    >,
> {
    if std::env::var_os("PINVOU_TEST_FEEDBACK_UPLOADER").is_some_and(|value| value == "always_fail")
    {
        Box::pin(
            pinvou3_lib::features::feedback::submit_feedback_with_uploader(
                request,
                &AlwaysFailUploader,
            ),
        )
    } else {
        Box::pin(pinvou3_lib::features::feedback::submit_feedback(request))
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FeedbackCommand {
    feedback_type: FeedbackType,
    title: String,
    body_file: PathBuf,
    attachments: Vec<PathBuf>,
}

const USAGE: &str = "usage: pinvou feedback submit --type issue|suggestion --title T --body-file F [--attach PATH...]";

/// The community feature function's message points here; the const is
/// crate-private upstream, so the URL is mirrored verbatim.
const COMMUNITY_ISSUES_URL: &str = "https://github.com/Pinvou/pinvou-agent/issues";

pub fn parse(values: &[String]) -> Result<FeedbackCommand, CliError> {
    let subcommand = values.get(1).ok_or_else(|| CliError::usage(USAGE))?;
    if subcommand != "submit" {
        return Err(CliError::usage(USAGE));
    }
    // Per-value semantics run at their token through the shared parser's
    // validator hook, so the left-to-right error precedence of the former
    // hand-rolled loop is preserved: an invalid `--type` still wins over a
    // later token's parse error. `--attach` is the one repeatable flag.
    let mut validate = |token: &str, value: &str| -> Result<(), CliError> {
        match token {
            "--type" => match value {
                "issue" | "suggestion" => Ok(()),
                other => Err(CliError::usage(format!(
                    "feedback submit --type must be issue or suggestion (got {other})"
                ))),
            },
            _ => Ok(()),
        }
    };
    let (options, _flags) = crate::support::parse_family_flags_extended(
        &values[2..],
        &["--type", "--title", "--body-file", "--attach"],
        &[],
        &["--attach"],
        "feedback",
        &mut validate,
        &[],
        false,
    )?;
    Ok(FeedbackCommand {
        // An invalid `--type` value cannot reach here — the hook above
        // refused it at the flag's own token.
        feedback_type: match crate::support::family_option(&options, "--type") {
            Some("issue") => FeedbackType::Issue,
            Some("suggestion") => FeedbackType::Suggestion,
            _ => {
                return Err(CliError::usage(
                    "feedback submit requires --type issue|suggestion",
                ));
            }
        },
        title: crate::support::family_option(&options, "--title")
            .ok_or_else(|| CliError::usage("feedback submit requires --title T"))?
            .to_owned(),
        body_file: crate::support::family_option(&options, "--body-file")
            .ok_or_else(|| CliError::usage("feedback submit requires --body-file F"))?
            .into(),
        attachments: crate::support::family_options(&options, "--attach")
            .into_iter()
            .map(PathBuf::from)
            .collect(),
    })
}

/// One registered `--attach` path with the facts the command derived from
/// it: the single record the three outputs render from — the persisted
/// request's attachment entry, the JSON echo row, and the human
/// "Attachment: …" line — so the three renderings cannot drift apart (the
/// human renderer used to re-read these facts out of the JSON rows through
/// `unwrap_or_default`s that could never fire).
struct AttachmentRecord {
    path: String,
    name: String,
    media_type: String,
    size_bytes: u64,
}

pub fn execute(command: FeedbackCommand, output: OutputMode) -> Result<CliOutcome, CliError> {
    // Validates the sandbox-home contract (absolute PINVOU3_HOME) before any
    // feedback paths are resolved.
    sandbox_home()?;
    let FeedbackCommand {
        feedback_type,
        title,
        body_file,
        attachments,
    } = command;
    // The body file gets the same credential-path policy as `--attach` (the
    // canonicalized gate below): its content is embedded in the same
    // permanent receipt, and a scripted `--body-file ~/.aws/credentials`
    // is the same footgun the attachment lane refuses.
    let body_canonical = crate::support::gate_canonical_path(
        &body_file,
        "feedback submit",
        "body file",
        "feedback submit",
        "body file",
    )?;
    // The body is validated further down — oversized bodies are REJECTED,
    // not truncated; the cap only stops an unbounded file from being loaded
    // in the first place. The CANONICAL path is what was policy-checked
    // (round-41 review): reading the original spelling would let a symlink
    // swapped in between the two calls route unverified content past the
    // refusal.
    let description =
        crate::support::read_text_file_capped(&body_canonical, 64 * 1024, "feedback submit")?;
    // The facts are derived once per attachment here — the request itself is
    // consumed by the feature call, so the request entries, the JSON echo
    // and the human line are all rendered from these records.
    let attachment_records = attachments
        .iter()
        .map(|path| -> Result<AttachmentRecord, CliError> {
            // Round-41 review: only regular files are registered. The GUI picker
            // cannot produce a directory, `metadata().len()` on one reports the
            // inode size, and the entry would land in a receipt that stays on
            // disk forever — the same regular-file rule every sibling ingest
            // lane enforces.
            let attachment_meta = std::fs::metadata(path).map_err(|error| {
                CliError::failed(format!(
                    "feedback submit: cannot read attachment {}: {error}",
                    path.display()
                ))
            })?;
            if !attachment_meta.is_file() {
                return Err(CliError::failed(format!(
                    "feedback_submit_not_a_file: attachment {} is not a regular file",
                    path.display()
                )));
            }
            let size = attachment_meta.len();
            // Nothing is read or uploaded here — only the path is recorded — but
            // it is recorded into a receipt that stays on disk forever, and the
            // GUI's attachment picker refuses these locations outright. A CLI that
            // happily writes `/home/u/.ssh/id_rsa` into a persisted file is a way
            // around a rule the other surface enforces, so the same path policy
            // applies (`artifacts::check_sensitive_path`, the mirror of
            // `platform::path_policy::check_sensitive_components`). The
            // canonical form is what the policy is defined over, so resolve
            // first. Nothing downstream reads the canonical path here — only
            // the gate runs on it (the request records the argv spelling).
            crate::support::gate_canonical_path(
                path,
                "feedback submit",
                "attachment",
                "feedback submit",
                "attachment",
            )?;
            Ok(AttachmentRecord {
                path: path.display().to_string(),
                name: path
                    .file_name()
                    .and_then(|name| name.to_str())
                    .unwrap_or("attachment")
                    .to_owned(),
                media_type: attachment_class(path)?.to_owned(),
                size_bytes: size,
            })
        })
        .collect::<Result<Vec<_>, CliError>>()?;
    let request = FeedbackSubmitRequest {
        feedback_type,
        title: Some(title),
        description,
        // See module docs: the CLI has no error-banner context.
        entry_point: "settings".to_owned(),
        error_summary: None,
        attachments: attachment_records
            .iter()
            .map(|record| FeedbackAttachmentRequest {
                path: record.path.clone(),
                name: record.name.clone(),
                media_type: record.media_type.clone(),
                // The upload lane guesses the MIME from the stored copy
                // (`guess_mime`), same as the app's own `mime: None` builders.
                mime: None,
                size_bytes: Some(record.size_bytes),
            })
            .collect::<Vec<_>>(),
        privacy_notice_version: "2026-06-24".to_owned(),
    };
    pinvou3_lib::features::feedback::validate_feedback_request(&request).map_err(|error| {
        CliError::failed(format!(
            "feedback submit: {}",
            translate_feedback_text(&error.to_string())
        ))
    })?;
    let feedback_id = new_feedback_id();
    let pending_dir = pinvou3_lib::platform::paths::feedback_pending_dir();
    let receipts_dir = pinvou3_lib::platform::paths::feedback_receipts_dir();
    let pending_path = pending_dir.join(format!("{feedback_id}.json"));
    let receipt_path = receipts_dir.join(format!("{feedback_id}.json"));
    let request_value = serde_json::to_value(&request)
        .map_err(|error| CliError::failed(format!("feedback submit: serialize: {error}")))?;
    // The receipt target must be writable BEFORE anything is staged: a
    // submission whose receipt cannot land fails here with zero side effects
    // (the feature's own package staging runs the same `create_dir_all` and
    // would otherwise fail one step later, after already creating its bundle
    // under `feedback/pending/`).
    std::fs::create_dir_all(&receipts_dir).map_err(|error| {
        CliError::failed(format!(
            "feedback submit: the receipt directory is not writable: {error}"
        ))
    })?;
    // Stage the request under `feedback/pending/` before the (consuming)
    // feature call, so the submitted content survives a crash or an error
    // between here and the receipt. That directory means "packages that failed
    // to upload or are still being prepared" (`platform::paths`), which is
    // exactly what this file is *while the call is in flight* — and nothing
    // more. The guard below removes it again on every exit from here on,
    // whatever the run concludes with.
    write_json(&pending_path, &request_value)?;
    // From this point to the end of the function, every return path removes
    // the staged bundle: the feature-level `Err` in the match below, the `?`s
    // between there and the receipt, and the receipt path itself. See
    // `PendingBundleGuard` for why the removal is unconditional.
    let _pending_guard = PendingBundleGuard { path: pending_path };
    // The official feature is async (the upload lane awaits the channel). A
    // one-shot current-thread runtime per call keeps the command synchronous
    // with no global runtime state, so repeated in-process contract runs stay
    // independent. `PINVOU_TEST_FEEDBACK_UPLOADER=always_fail` swaps in the
    // deterministic failing uploader for the contract tests (see the module
    // docs); production unset means the real official channel.
    //
    // The explicit `validate_feedback_request` above stays *in addition to*
    // the validation inside `submit_feedback`: the explicit call rejects a bad
    // request before anything is staged on disk (so an invalid submission
    // leaves no file under `feedback/pending/` at all), while this call is
    // the real feature entry point — the receipt semantics belong to
    // `features/feedback` and are not reconstructed here. Inlining a
    // `FeedbackReceipt` literal instead would make the CLI the second place
    // that owns that copy.
    let receipt = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|error| CliError::failed(format!("feedback submit: runtime: {error}")))?
        .block_on(submit_with_configured_uploader(request))
    {
        Ok(receipt) => receipt,
        Err(error) => {
            return Err(CliError::failed(format!(
                "feedback submit failed: {}",
                translate_feedback_text(&error.to_string())
            )));
        }
    };
    let mut receipt_value = serde_json::to_value(&receipt)
        .map_err(|error| CliError::failed(format!("feedback submit: serialize: {error}")))?;
    // The community feature answers with an empty id (`features/feedback`
    // owns no id generation), while the receipt file is named after the
    // CLI-generated id — persist that id in the payload so scripts reading
    // the file see the same id the filename and stdout carry.
    let object = receipt_value
        .as_object_mut()
        .ok_or_else(|| CliError::failed("feedback submit: the receipt is not a JSON object"))?;
    object.insert(
        "feedback_id".to_owned(),
        serde_json::Value::String(feedback_id.clone()),
    );
    // The receipt carries the request it is a receipt for. Once the staged
    // copy is dropped (below) this is the only record of what the user wrote,
    // and they need that text to paste into the issue tracker.
    object.insert("request".to_owned(), request_value);
    // A failed receipt write is simply propagated from here: the guard drops
    // the staged bundle with it, because the run has concluded either way.
    // Nothing the user wrote is lost with the stage — the description came
    // from `--body-file`, which is still on disk, and the attachments were
    // only ever referenced by path.
    write_json(&receipt_path, &receipt_value)?;

    let status = status_label(receipt.status);
    let message = translate_feedback_text(&receipt.message);
    // `status` is for humans. Scripts branch on `uploaded`: a retryable
    // channel failure is still a concluded, exit-0 submission with a persisted
    // receipt, so `.status` alone cannot separate "sent" from "kept for
    // retry". `uploaded` answers the one question a caller actually has — did
    // this leave the machine.
    let uploaded = receipt.status == FeedbackStatus::Submitted;
    // The name and path cells come from argv (the `--attach` path and its
    // file name), so the human lines go through the same cell sanitizer
    // every other family row uses; JSON stays verbatim by contract.
    let attachment_lines = attachment_records
        .iter()
        .map(|record| {
            format!(
                "\nAttachment: {} ({}, {} bytes) {}",
                crate::support::collapse_control_characters(&record.name),
                record.media_type,
                record.size_bytes,
                crate::support::collapse_control_characters(&record.path),
            )
        })
        .collect::<String>();
    // No `pending_path`: the staged file is gone by now, and a key naming a
    // path that does not exist is worse than no key.
    let value = serde_json::json!({
        "feedback_id": feedback_id,
        "status": status,
        "uploaded": uploaded,
        "receipt_path": receipt_path.display().to_string(),
        "issue_url": COMMUNITY_ISSUES_URL,
        "message": message,
        // Registered attachments, echoed back: the command stats each one and
        // records it into the persisted request, and without this the user has
        // no evidence that `--attach` did anything at all.
        "attachments": attachment_records
            .iter()
            .map(|record| {
                serde_json::json!({
                    "path": record.path,
                    "name": record.name,
                    "media_type": record.media_type,
                    "size_bytes": record.size_bytes,
                })
            })
            .collect::<Vec<_>>(),
    });
    let mut human = format!(
        "Feedback: {feedback_id}\nStatus: {status}\nUploaded: {uploaded}\nReceipt: {}\nFeedback packages go to the official feedback channel; issues can also be filed at: {COMMUNITY_ISSUES_URL}",
        receipt_path.display(),
    );
    human.push_str(&attachment_lines);
    // `failed_retryable`（通道不可达）与 `submitted` 一样是收据已落盘的完成态：
    // 命令做了它承诺的事，脚本按 `uploaded`/`status` 分支，所以保持 exit 0。
    // 真正的功能级 `Err` 在上面已被拒绝。
    if receipt.status == FeedbackStatus::FailedValidation {
        human.push_str(&format!("\n{message}"));
    }
    Ok(success(render(output, human, &value)))
}

/// Removes the CLI's staged request file under `feedback/pending/` on every
/// exit of [`execute`] once it has been written — the receipt path, a failed
/// receipt write, a feature-level error, and any other early return alike.
///
/// The feature's own package directory under the same parent follows the
/// feature's retention rules (a retryable upload failure intentionally keeps
/// the package for a later retry, same as the GUI); this guard owns only the
/// CLI's `<feedback_id>.json` staging file, whose lifetime is exactly this
/// call. The removal is unconditional — the Drop-based guard shape other
/// parts of this workspace use for scoped cleanup (`adapter-gaia`'s
/// `AcquisitionLock`, the test suites' `HomeGuard`), so no future early
/// return between staging and the receipt can silently leak the user's free
/// text into a permanent file.
struct PendingBundleGuard {
    path: PathBuf,
}

impl Drop for PendingBundleGuard {
    fn drop(&mut self) {
        // A removal failure is only a note: the run's own outcome (receipt or
        // error) has already been reported, and failing the command here as
        // well would tell the user their feedback was not recorded when it
        // was. The file is `0600` and stays theirs; the warning only says
        // that nothing will ever retry it.
        if let Err(error) = std::fs::remove_file(&self.path)
            && error.kind() != std::io::ErrorKind::NotFound
        {
            note!(
                "warning: feedback submit: the staged bundle {} could not be removed \
                     after the run concluded ({error}); delete it manually — \
                     nothing will retry it",
                self.path.display()
            );
        }
    }
}

/// The feature layer's user-facing strings are Chinese (GUI copy); the CLI is
/// an English tool, so the known messages are translated at this boundary and
/// anything unrecognized passes through unchanged rather than being dropped.
fn translate_feedback_text(text: &str) -> String {
    if text.contains("请填写反馈说明") {
        "a feedback description is required".to_owned()
    } else if text.contains("反馈说明最多") || text.contains("反馈标题最多") {
        // The character count is embedded in the original message; the
        // marker decides the English noun ("description" / "title").
        let noun = if text.contains("反馈说明最多") {
            "description"
        } else {
            "title"
        };
        let count = text
            .chars()
            .filter(char::is_ascii_digit)
            .collect::<String>();
        format!("the feedback {noun} allows at most {count} characters")
    } else if text.contains("反馈入口来源无效") {
        "invalid feedback entry point".to_owned()
    } else if text.contains("社区版不会上传反馈") {
        format!(
            "The community edition does not upload feedback, logs, or attachments. Submit an issue at {COMMUNITY_ISSUES_URL}."
        )
    } else {
        text.to_owned()
    }
}

fn status_label(status: FeedbackStatus) -> &'static str {
    match status {
        FeedbackStatus::Submitted => "submitted",
        FeedbackStatus::FailedRetryable => "failed_retryable",
        FeedbackStatus::FailedValidation => "failed_validation",
    }
}

/// Official 适配：官方反馈通道只收图片/视频附件（`classify_attachment` 的
/// 扩展名白名单，GUI 选择器同款），且请求里的 `media_type` 必须是与文件
/// 分类一致的粗粒度类。CLI 在注册时用同一张表分类，其余扩展名在注册时
/// 即被拒绝（早于任何暂存），语义与官方通道一致。
fn attachment_class(path: &Path) -> Result<&'static str, CliError> {
    let extension = path
        .extension()
        .and_then(|extension| extension.to_str())
        .unwrap_or("")
        .to_ascii_lowercase();
    match extension.as_str() {
        "png" | "jpg" | "jpeg" | "gif" | "webp" => Ok("image"),
        "mp4" | "mov" | "webm" => Ok("video"),
        other if other.is_empty() => Err(CliError::usage(
            "attachment format not supported: the file has no extension (the official channel accepts png/jpg/jpeg/gif/webp images and mp4/mov/webm videos)",
        )),
        other => Err(CliError::usage(format!(
            "attachment format not supported: .{other} (the official channel accepts png/jpg/jpeg/gif/webp images and mp4/mov/webm videos)"
        ))),
    }
}

/// Writes one feedback file atomically, owner-readable only.
///
/// Both files this command produces carry the user's free-text feedback (the
/// staged request holds it directly; the receipt embeds it). On a shared
/// machine the default umask leaves that world-readable for as long as the
/// file exists — which, since nothing ever uploads or prunes it, is forever.
/// `0600` matches how the app stores user-authored content it does not intend
/// to publish.
fn write_json(path: &Path, value: &serde_json::Value) -> Result<(), CliError> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).map_err(|error| {
            CliError::failed(format!(
                "feedback submit: cannot create {}: {error}",
                parent.display()
            ))
        })?;
    }
    let mut bytes = serde_json::to_vec_pretty(value)
        .map_err(|error| CliError::failed(format!("feedback submit: serialize: {error}")))?;
    bytes.push(b'\n');
    // The app's shared stage+rename writer (the same helper the artifact
    // write lane uses): a crash mid-write must not leave a truncated
    // feedback file behind, and the staging step must not follow a planted
    // symlink or swallow a failing fsync.
    pinvou3_lib::platform::atomic_write_private(path, &bytes).map_err(|error| {
        CliError::failed(format!(
            "feedback submit: cannot write {}: {error}",
            path.display()
        ))
    })
}

fn new_feedback_id() -> String {
    // The process-wide call counter suffixes the millis+pid id: two
    // `execute` calls inside one process within the same millisecond
    // (contract tests mint several back-to-back; a future in-process
    // library consumer likewise) used to mint the SAME id, and the staged
    // bundle's atomic write silently renamed the second receipt over the
    // first. Real CLI usage spawns one process per invocation, so pids
    // already disambiguate there — the counter closes the in-process case.
    use std::sync::atomic::{AtomicU32, Ordering};
    static CALL_SEQ: AtomicU32 = AtomicU32::new(0);
    let millis = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis();
    format!(
        "feedback-{millis}-{}-{}",
        std::process::id(),
        CALL_SEQ.fetch_add(1, Ordering::Relaxed)
    )
}

#[cfg(test)]
mod tests {
    use super::PendingBundleGuard;

    /// The invariant that keeps `feedback/pending/` transient: dropping the
    /// guard removes the staged bundle, whatever the run concluded with.
    #[test]
    fn pending_guard_removes_the_staged_bundle_when_dropped() {
        let path = crate::support::test_support::scratch_dir("feedback").join("removes");
        std::fs::write(&path, b"{}\n").expect("write scratch bundle");
        drop(PendingBundleGuard { path: path.clone() });
        assert!(!path.exists(), "the guard must remove the staged bundle");
    }

    /// A bundle that is already gone, or one `remove_file` cannot remove at
    /// all (a directory — unlink(2) refuses directories on every unix), must
    /// not panic the drop: the run's own outcome still propagates.
    #[test]
    fn pending_guard_tolerates_a_missing_or_unremovable_bundle() {
        let missing = crate::support::test_support::scratch_dir("feedback").join("missing");
        drop(PendingBundleGuard {
            path: missing.clone(),
        });
        assert!(!missing.exists());

        let stuck = crate::support::test_support::scratch_dir("feedback").join("stuck");
        std::fs::create_dir(&stuck).expect("create scratch directory");
        drop(PendingBundleGuard {
            path: stuck.clone(),
        });
        assert!(
            stuck.is_dir(),
            "the guard must leave behind what it cannot remove, not panic"
        );
        std::fs::remove_dir(&stuck).expect("scratch cleanup");
    }
}
