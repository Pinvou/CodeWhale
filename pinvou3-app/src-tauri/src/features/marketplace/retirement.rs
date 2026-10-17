//! Retirement of embedded marketplace packages; owns store/consent/landing cleanup.
//! Runtime boot supplies no arbitrary package id or filesystem path.

use crate::platform::paths;

const RETIRED_TOOL_ID: &str = "data_analysis";

/// 清理已从工具市场移除的预置 MCP 工具残留。
///
/// 不能删除所有未知目录:未来/本地可能有自定义 MCP 工具。这里只精确处理曾经内置、
/// 现在源码资源已经移除的 marketplace 工具。
pub(crate) fn cleanup_retired_embedded_tools() -> std::io::Result<()> {
    let tool_id = RETIRED_TOOL_ID;
    // 退役 id 保护：`bundles/` 已是用户上传落盘区，用户可能上传过
    // 同名包——其 Upload 记录存在时跳过整段清理（不删登记、不删 mcp.json），
    // 只清理确定无主的内嵌退役残留。
    // Fail-closed like the uninstall path's `source_may_be_upload`: an
    // unreadable store means "may be an upload" → skip the cleanup
    // entirely (its last step deletes `bundles/<id>` wholesale).
    if user_upload_record_exists(tool_id) {
        return Ok(());
    }
    // The id's import lock (the
    // same lock the import path serializes its landing under) is
    // held from here down — the top probe above is pre-lock and
    // advisory only; the LOAD-BEARING piece is the under-lock
    // re-check below (the earlier, narrower lock covered only the final
    // re-probe + delete pair, so an Upload import completing
    // between the top probe and the uninstall still got its
    // just-landed pack recycled wholesale by that uninstall).
    // Landing lease over the whole sweep, before the import mutex:
    // a same-id import landing in another process must not be
    // uninstalled mid-landing; failure defers fail-closed.
    let defer = |detail: &str| {
        crate::platform::startup::mark_with_detail("rust", "retired_tool_cleanup:deferred", detail);
    };
    let mut landing_lease = match super::plugin_import::open_landing_lease(tool_id) {
        Ok(lease) => lease,
        Err(error) => {
            log::warn!(
                "[cleanup] retired tool '{tool_id}': the landing lease is \
                 unavailable, deferring the residue sweep to the next startup: {error}"
            );
            defer("landing lease unavailable");
            return Ok(());
        }
    };
    let _landing_lease_guard = match landing_lease.try_write() {
        Ok(guard) => guard,
        // Both refusals defer fail-closed, each with its own wording.
        Err(error) => {
            let contended = error.kind() == std::io::ErrorKind::WouldBlock;
            if contended {
                log::warn!(
                    "[cleanup] retired tool '{tool_id}': an import for the same id \
                     holds the landing lease; deferring the residue sweep to the next \
                     startup"
                );
            } else {
                log::warn!(
                    "[cleanup] retired tool '{tool_id}': the landing lease is \
                     un-lockable, deferring the residue sweep to the next startup: {error}"
                );
            }
            defer(if contended {
                "landing lease contended"
            } else {
                "landing lease un-lockable"
            });
            return Ok(());
        }
    };
    let import_lock = super::plugin_import::import_lock_for(tool_id);
    let _import_guard = import_lock.lock().unwrap_or_else(|p| p.into_inner());
    // Match marketplace writers: landing lease → import mutex → market lock.
    // Keep the cross-process lock across every destructive retirement step.
    let _market_write_lock = super::plugin_import::MarketplaceWriteLock::acquire()
        .map_err(|e| std::io::Error::other(format!("retired-tool cleanup lock: {e}")))?;
    // The pre-lock probe above may have read a no-record store
    // while an import for the same id was mid-landing (it had not
    // committed when the probe ran, and holds this very lock
    // until it does). Re-check under the lock before any
    // destructive step, same defer-with-retry contract as the
    // deletion-point arm below.
    if user_upload_record_exists(tool_id) {
        log::warn!(
            "[cleanup] retired tool '{tool_id}': an Upload record for the same id is \
             present under the import lock; keeping bundles/<id> and deferring the \
             residue sweep to the next startup"
        );
        defer("upload-record present under import lock");
        return Ok(());
    }
    // 廉价残留探测:所有清理面都干净时直接返回——uninstall 会无条件重写
    // installed.json / mcp.json(manifest 声明 secret_targets 时还会清
    // 系统 keyring),不值得每次启动都实例化 MarketplaceManager 跑一遍。
    // 探测只读私有布局文件,不实例化管理器;BundleStore 记录的读取开销
    // 与探测同量级,保护判定前置不亏。
    if !marketplace_tool_residue_present(tool_id) {
        return Ok(());
    }
    // Log, don't swallow, an uninstall failure (every boot would
    // silently recur); `uninstall_leased` — the lease is held above.
    let uninstall_error = super::MarketplaceManager::new()
        .uninstall_leased(tool_id)
        .err();
    // Strip only when the uninstall did NOT fail (pinned by
    // cleanup_keeps_consent_rows_when_uninstall_rolls_back): a
    // rollback leaves the rows as current consent. Persist
    // failures are logged (best-effort); removal is exact (the
    // normalized fallback could re-own a foreign pack's rows).
    if uninstall_error.is_none() {
        // A same-id import registers deny-first rows BEFORE landing
        // (the pre-land gate); a vacuous uninstall would strip them
        // as "residue". Defer while the landing mark is present —
        // covering the deletion legs below too. Pinned by
        // cleanup_defers_consent_strip_while_a_same_id_import_is_mid_landing.
        if super::plugin_import::landing_in_progress(tool_id) {
            log::warn!(
                "[cleanup] retired tool '{tool_id}': an import for the same id is mid-landing; deferring the rest of the cleanup to the next startup"
            );
            defer("import landing mark present");
            return Ok(());
        } else if let Err(e) = super::scope::remove_bundle_from_disabled_scopes_exact(tool_id) {
            log::warn!(
                "[runtime-bundle] persisting the post-retirement switch/visibility cleanup for {tool_id} failed (a stale entry would resurrect the retired tool in the scope): {e}"
            );
        }
    }
    if let Some(error) = uninstall_error {
        // 回滚说明工具仍登记在册:目录删除随之跳过,不销毁在册工具的
        // 包目录;登记与目录都是残留探测面,下次启动会重试整套清理。
        // 打包版 Windows GUI 无 stderr:timeline 是用户能看到这条
        // 推迟的唯一渠道(与 reconcile 失败的上报同一范式)。
        log::warn!(
            "[cleanup] retired tool '{tool_id}' uninstall deferred: {error}; \
             retrying on the next startup"
        );
        defer(&error);
        return Ok(());
    }

    let _ = std::fs::remove_dir_all(paths::bundle_mcp_servers_dir().join(tool_id));
    // 按包聚合新布局的退役残留：`migrate_custom_mcp_layout` 会先把旧目录
    // 搬进 bundles/<id>/mcp/，而 uninstall 的 can_redeliver=false 规则
    // （非内嵌 id 不可重释放）保留包目录——不删则 manifest 存活、退役
    // 工具以「自定义 MCP 卡」复活（G8a）。Upload 保护已在上方判定。
    // The top probe is checked ONCE, but
    // a same-id Upload import can land in the window (the uninstall
    // above is not instantaneous) — deleting wholesale at this
    // point would destroy the user's unique uploaded copy, the
    // exact loss the guard exists to prevent. Re-probe AT the
    // deletion point, fail-closed on an unreadable store (same
    // `source_may_be_upload` direction as the top probe): keep
    // `bundles/<id>` and defer the residue sweep to the next
    // startup, the same defer-with-retry contract as the
    // uninstall-deferred arm above.
    // The re-probe alone was check-then-act, and holding the
    // import lock only here left the uninstall itself outside —
    // an Upload import completing before this point still lost
    // its just-landed pack to the uninstall above. The lock is
    // now held from the decision top; this re-probe + defer arm
    // stays as defense-in-depth for any path that ever reaches
    // the delete without the full-span lock.
    if user_upload_record_exists(tool_id) {
        log::warn!(
            "[cleanup] retired tool '{tool_id}': an Upload record for the same id \
             appeared during cleanup; keeping bundles/<id> and deferring the \
             residue sweep to the next startup"
        );
        defer("upload-record appeared mid-cleanup");
        return Ok(());
    }
    let _ = std::fs::remove_dir_all(paths::bundles_root().join(tool_id));
    Ok(())
}

/// Upload-record probe (shared verdict for the retired-id protection,
/// round-30 m5): an Upload record exists → true; the store is unreadable
/// → true (fail-closed, same direction as the uninstall path's
/// `source_may_be_upload`: when it cannot be read, treat it as a possible
/// upload).
fn user_upload_record_exists(tool_id: &str) -> bool {
    super::store::BundleStore::new()
        .records()
        .map(|records| {
            records.iter().any(|r| {
                r.id == tool_id && matches!(r.source, super::store::BundleSource::Upload(_))
            })
        })
        .unwrap_or(true)
}

/// 探测已下架 marketplace 工具是否还有任何残留清理面:安装目录、installed.json、
/// 禁用/隐藏列表落盘、mcp.json server 条目。全干净 → false(调用方据此跳过
/// MarketplaceManager 实例化 + uninstall)。installed.json 与禁用集落盘是
/// marketplace 模块的私有布局,这里按其落盘路径直读做 contains 级探测——宁可
/// 误报(多跑一次幂等清理)也不漏报(残留永驻)。
fn marketplace_tool_residue_present(tool_id: &str) -> bool {
    if paths::bundle_mcp_servers_dir().join(tool_id).exists() {
        return true;
    }
    // 新布局包目录（含迁移搬入的 bundles/<id>/mcp/）也是残留面——否则只剩
    // 它时探测漏报，退役工具的 manifest 会以自定义 MCP 卡复活。
    if paths::bundles_root().join(tool_id).exists() {
        return true;
    }
    let home = paths::pinvou3_home();
    // installed.json = ~/.pinvou3/marketplace/installed.json(镜像 MarketplaceManager
    // 私有 installed_file 布局)。禁用/隐藏残留的落盘真相源是 scope 收敛后的单一
    // disabled_bundles.json(plain + 所有 scope 的 disabled/hidden 集,镜像
    // marketplace::scope 私有布局);漏掉它会把「仅禁用/隐藏集有残留」的退役工具
    // 误判干净、跳过清理,陈旧条目继续误隐藏未来同名重装(#522)。旧布局
    // disabled_connectors.json 不探测:首个 scope 读路径「读到即迁移」把其内容
    // 并进统一文件,此后它只剩死数据(uninstall 与 scope 写方都不再碰它),探测
    // 死数据只会对已迁移用户产生永不收敛的误报;同样的残留最晚由统一文件面在
    // 下一次启动收敛。
    for probe in ["marketplace/installed.json", "disabled_bundles.json"] {
        let path = home.join(probe);
        if !path.exists() {
            continue; // 文件不存在 = 该清理面本就干净,不算误报
        }
        // Hardened read — family rationale on `open_private_data_file_within`.
        // A rejected read errs toward "residue present", the
        // conservative direction.
        match crate::platform::filesystem::read_private_data_file(&path) {
            Ok(content) => {
                if content.contains(tool_id) {
                    return true;
                }
            }
            // 文件存在但读失败(权限/损坏):保守视为有残留——宁可误报(多跑
            // 一次幂等清理)也不漏报(残留永驻)。
            Err(_) => return true,
        }
    }
    // mcp.json 按结构探测:server key 存在即残留;坏 json 保守视为有残留——
    // uninstall 会在写入器处拒绝并整体回滚,清理推迟到文件修复后的下次启动。
    // Hardened read — family rationale on `open_private_data_file_within` — so
    // this probe shares the two probes' convention above and a future
    // edit cannot change only one of them.
    if !paths::mcp_config_path().is_file() {
        return false;
    }
    match crate::platform::filesystem::read_private_data_file(&paths::mcp_config_path())
        .ok()
        .and_then(|content| serde_json::from_str::<serde_json::Value>(&content).ok())
    {
        Some(mcp) => mcp
            .get("servers")
            .and_then(|servers| servers.get(tool_id))
            .is_some(),
        None => true,
    }
}

/// Runtime boot's residue regressions inspect the same fixed retired id.
#[cfg(test)]
pub(crate) fn retired_tool_residue_present_for_test() -> bool {
    marketplace_tool_residue_present(RETIRED_TOOL_ID)
}
