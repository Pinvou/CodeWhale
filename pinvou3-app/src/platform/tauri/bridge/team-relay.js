/** Team Relay desktop bridge. Hub state remains authoritative; local state is a cache. */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};

  registry["team-relay"] = function (context) {
    var state = context.state;
    var notify = context.notify;
    var invoke = context.invoke;
    var listen = context.listen;
    var dialogOpen = context.dialogOpen;
    var dialogSave = context.dialogSave;
    var refreshHistoryList = context.refreshHistoryList;
    var refreshPromise = null;
    var joinPasswordReadPromise = null;
    var refreshTimer = null;
    var pendingRequests = {};
    var legacyReadCursorKey = "pinvou.team-relay.read-cursors.v1";
    var legacyReadCursors = loadLegacyReadCursors();
    var connectionGeneration = 0;
    var AVATAR_CACHE_MAX_BYTES = 32 * 1024 * 1024;
    var AVATAR_CACHE_MAX_ENTRIES = 256;
    var AVATAR_CACHE_ENTRY_OVERHEAD_BYTES = 256;
    var avatarSources = new Map();
    var avatarRequests = new Map();
    var avatarPrincipalVersions = new Map();
    var avatarCacheBytes = 0;
    var avatarCacheEpoch = 0;

    function uuid() {
      if (root.crypto && typeof root.crypto.randomUUID === "function") return root.crypto.randomUUID();
      throw new Error("This platform cannot generate a secure request identifier");
    }

    function patch(value) {
      if (value && value.snapshot) {
        value = Object.assign({}, value, { snapshot: applyLegacyUnreadFallback(value.snapshot) });
      }
      state.teamRelay = Object.assign({}, state.teamRelay, value);
      notify();
    }

    function loadLegacyReadCursors() {
      try {
        var parsed = JSON.parse(root.localStorage && root.localStorage.getItem(legacyReadCursorKey) || "{}");
        return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed : {};
      } catch (_) {
        return {};
      }
    }

    function saveLegacyReadCursors() {
      try {
        if (!root.localStorage) return;
        var entries = Object.entries(legacyReadCursors);
        if (entries.length > 2000) legacyReadCursors = Object.fromEntries(entries.slice(-2000));
        root.localStorage.setItem(legacyReadCursorKey, JSON.stringify(legacyReadCursors));
      } catch (_) {
        /* A denied local cache must not block Team Relay. */
      }
    }

    function setLegacyReadCursor(threadId, sequence) {
      var previous = Number(legacyReadCursors[threadId] || 0);
      legacyReadCursors[threadId] = Math.max(previous, Number(sequence || 0));
      saveLegacyReadCursors();
      return legacyReadCursors[threadId];
    }

    function applyLegacyUnreadFallback(snapshot) {
      if (!snapshot) return snapshot;
      var initialized = false;
      function patchBootstrap(bootstrap) {
        if (!bootstrap || !Array.isArray(bootstrap.threads)) return bootstrap;
        var meId = bootstrap.me && bootstrap.me.id;
        var changed = false;
        var threads = bootstrap.threads.map(function (detail) {
          if (!detail || !detail.thread || Object.prototype.hasOwnProperty.call(detail, "unread_count")) return detail;
          changed = true;
          var actions = detail.actions || [];
          var revision = Number(detail.thread.revision || 0);
          var cursor = Number(legacyReadCursors[detail.thread.id]);
          if (!Number.isFinite(cursor)) {
            var latest = actions[actions.length - 1];
            cursor = latest && latest.actor_id !== meId ? Math.max(0, revision - 1) : revision;
            legacyReadCursors[detail.thread.id] = cursor;
            initialized = true;
          }
          var unreadCount = actions.filter(function (action) {
            return Number(action.sequence || 0) > cursor && action.actor_id !== meId;
          }).length;
          return Object.assign({}, detail, {
            read_state: { thread_id: detail.thread.id, last_read_sequence: cursor, last_read_at: null },
            unread_count: unreadCount,
          });
        });
        return changed ? Object.assign({}, bootstrap, { threads: threads }) : bootstrap;
      }
      var next = Object.assign({}, snapshot, {
        bootstrap: patchBootstrap(snapshot.bootstrap),
        team_bootstraps: (snapshot.team_bootstraps || []).map(patchBootstrap),
      });
      if (initialized) saveLegacyReadCursors();
      return next;
    }

    async function load(options) {
      if (refreshPromise) return refreshPromise;
      var silent = options && options.silent === true;
      var generation = connectionGeneration;
      function patchCurrent(value) {
        if (generation === connectionGeneration) patch(value);
      }
      var request = (async function () {
        if (!silent) patchCurrent({ loading: true, error: null });
        try {
          var status = await invoke("team_relay_status");
          if (!status.configured) {
            patchCurrent({ loading: false, status: status, snapshot: null, account: null });
            return null;
          }
          if (!status.team_id) {
            var account = await invoke("team_relay_account");
            if (account.teams && account.teams.length) {
              var restoredSnapshot = await invoke("team_relay_switch_team", { teamId: account.teams[0].id });
              var restoredStatus = Object.assign({}, status, { team_id: restoredSnapshot.bootstrap.team.id });
              patchCurrent({ loading: false, status: restoredStatus, snapshot: restoredSnapshot, account: { principal: restoredSnapshot.bootstrap.me, teams: restoredSnapshot.teams || account.teams }, lastSyncedAt: Date.now() });
              return restoredSnapshot;
            }
            patchCurrent({ loading: false, status: status, snapshot: null, account: account, lastSyncedAt: Date.now() });
            return account;
          }
          var snapshot = await invoke("team_relay_bootstrap");
          patchCurrent({ loading: false, status: status, snapshot: snapshot, account: { principal: snapshot.bootstrap.me, teams: snapshot.teams || [] }, lastSyncedAt: Date.now() });
          return snapshot;
        } catch (error) {
          if (!silent) patchCurrent({ loading: false, error: String(error) });
          throw error;
        } finally {
          if (refreshPromise === request) refreshPromise = null;
        }
      })();
      refreshPromise = request;
      return request;
    }

    function verifyAccess(accessCode) {
      return invoke("team_relay_verify_beta_access", { accessCode: accessCode });
    }

    async function registerAccount(input) {
      patch({ loading: true, error: null });
      try {
        var account = await invoke("team_relay_register_account", { input: input });
        if (account.teams && account.teams.length) {
          var snapshot = await invoke("team_relay_switch_team", { teamId: account.teams[0].id });
          patch({ loading: false, status: { configured: true, hub_url: input.hub_url, team_id: snapshot.bootstrap.team.id }, snapshot: snapshot, account: { principal: snapshot.bootstrap.me, teams: snapshot.teams || account.teams }, lastSyncedAt: Date.now() });
          return snapshot;
        }
        patch({ loading: false, status: { configured: true, hub_url: input.hub_url, team_id: null }, snapshot: null, account: account, lastSyncedAt: Date.now() });
        return account;
      } catch (error) {
        patch({ loading: false, error: String(error) });
        throw error;
      }
    }

    async function claim(input) {
      patch({ loading: true, error: null });
      try {
        var snapshot = await invoke("team_relay_claim", { input: input });
        patch({ loading: false, status: { configured: true, hub_url: input.hub_url, team_id: snapshot.bootstrap.team.id }, snapshot: snapshot, lastSyncedAt: Date.now() });
        await invoke("team_relay_bootstrap");
        return snapshot;
      } catch (error) {
        var recoveredStatus = null;
        try { recoveredStatus = await invoke("team_relay_status"); } catch (_) {}
        patch({ loading: false, status: recoveredStatus || state.teamRelay.status, error: String(error) });
        if (recoveredStatus && recoveredStatus.configured) load().catch(function () {});
        throw error;
      }
    }

    async function createFirstTeam(input) {
      patch({ loading: true, error: null });
      try {
        var snapshot = await invoke("team_relay_create_first_team", { input: input });
        patch({ loading: false, status: { configured: true, hub_url: input.hub_url, team_id: snapshot.bootstrap.team.id }, snapshot: snapshot, lastSyncedAt: Date.now() });
        await invoke("team_relay_bootstrap");
        return snapshot;
      } catch (error) {
        patch({ loading: false, error: String(error) });
        throw error;
      }
    }

    async function updateProfile(input) {
      return mutate("team_relay_update_profile", { input: input });
    }

    function avatarDigest(avatar) {
      var digest = String(avatar && avatar.sha256 || "").trim().toLowerCase();
      return /^[0-9a-f]{64}$/.test(digest) ? digest : "";
    }

    function avatarDisplaySize(value) {
      var size = Number(value);
      return size === 48 || size === 96 ? size : null;
    }

    function avatarCacheKey(principalId, digest, displaySize) {
      var id = String(principalId || "").trim();
      if (!id || !digest) return "";
      return id + ":" + digest + ":" + (displaySize || "original");
    }

    function removeAvatarCacheEntry(key) {
      var entry = avatarSources.get(key);
      if (!entry) return;
      avatarCacheBytes = Math.max(0, avatarCacheBytes - entry.byteSize);
      avatarSources.delete(key);
    }

    function forgetAvatarSources(principalId) {
      var id = String(principalId || "").trim();
      if (!id || !avatarPrincipalVersions.has(id)) return;
      avatarPrincipalVersions.delete(id);
      for (var entry of avatarSources.entries()) {
        if (entry[1].principalId === id) removeAvatarCacheEntry(entry[0]);
      }
      for (var pending of avatarRequests.entries()) {
        if (pending[1].principalId === id) avatarRequests.delete(pending[0]);
      }
    }

    function clearAvatarSources() {
      avatarCacheEpoch += 1;
      avatarCacheBytes = 0;
      avatarSources.clear();
      avatarRequests.clear();
      avatarPrincipalVersions.clear();
    }

    function currentAvatarVersion(principalId, digest) {
      var current = avatarPrincipalVersions.get(principalId);
      if (current && current.digest === digest) return current;
      if (current) forgetAvatarSources(principalId);
      var version = { digest: digest };
      avatarPrincipalVersions.set(principalId, version);
      return version;
    }

    function avatarSourceByteSize(key, source) {
      // JavaScript engines may store strings as UTF-16. Count that conservative
      // representation plus a small entry/key allowance when enforcing the cap.
      return ((key.length + source.length) * 2) + AVATAR_CACHE_ENTRY_OVERHEAD_BYTES;
    }

    function rememberAvatarSource(key, entry) {
      entry.byteSize = avatarSourceByteSize(key, entry.source);
      if (entry.byteSize > AVATAR_CACHE_MAX_BYTES) return;
      removeAvatarCacheEntry(key);
      avatarSources.set(key, entry);
      avatarCacheBytes += entry.byteSize;
      while (avatarSources.size > AVATAR_CACHE_MAX_ENTRIES || avatarCacheBytes > AVATAR_CACHE_MAX_BYTES) {
        var oldestKey = avatarSources.keys().next().value;
        if (oldestKey === undefined) break;
        removeAvatarCacheEntry(oldestKey);
      }
    }

    async function avatarSource(principalId, avatar, requestedDisplaySize) {
      var id = String(principalId || "").trim();
      var digest = avatarDigest(avatar);
      if (!id || !digest) {
        if (id) forgetAvatarSources(id);
        return null;
      }
      var displaySize = avatarDisplaySize(requestedDisplaySize);
      var version = currentAvatarVersion(id, digest);
      var key = avatarCacheKey(id, digest, displaySize);
      var cached = avatarSources.get(key);
      if (cached) {
        // Map insertion order doubles as an O(1) LRU queue.
        avatarSources.delete(key);
        avatarSources.set(key, cached);
        return cached.source;
      }
      var pending = avatarRequests.get(key);
      if (pending) return pending.promise;
      var epoch = avatarCacheEpoch;
      var requestEntry = {
        principalId: id,
        digest: digest,
        displaySize: displaySize,
        version: version,
        promise: null,
      };
      var args = {
        principalId: id,
        expectedSha256: digest,
      };
      if (displaySize) args.displaySize = displaySize;
      requestEntry.promise = invoke("team_relay_avatar_data_url", args).then(function (source) {
        var normalizedSource = typeof source === "string" && source ? source : null;
        var latest = avatarPrincipalVersions.get(id);
        if (normalizedSource
          && avatarRequests.get(key) === requestEntry
          && avatarCacheEpoch === epoch
          && latest === requestEntry.version) {
          rememberAvatarSource(key, {
            source: normalizedSource,
            principalId: id,
            digest: digest,
            displaySize: displaySize,
            byteSize: 0,
          });
        }
        return normalizedSource;
      }).finally(function () {
        if (avatarRequests.get(key) === requestEntry) avatarRequests.delete(key);
      });
      avatarRequests.set(key, requestEntry);
      return requestEntry.promise;
    }

    async function uploadAvatar() {
      if (!dialogOpen) throw new Error("File picker is unavailable");
      var selected = await dialogOpen({
        multiple: false,
        filters: [{ name: "PNG, JPEG, WebP, GIF", extensions: ["png", "jpg", "jpeg", "webp", "gif"] }],
      });
      var path = Array.isArray(selected) ? selected[0] : selected;
      if (!path) return null;
      var principalId = state.teamRelay.snapshot && state.teamRelay.snapshot.bootstrap
        ? state.teamRelay.snapshot.bootstrap.me.id
        : null;
      var result = await mutate("team_relay_upload_avatar", { path: String(path) });
      forgetAvatarSources(principalId);
      return result;
    }

    async function removeAvatar() {
      var principalId = state.teamRelay.snapshot && state.teamRelay.snapshot.bootstrap
        ? state.teamRelay.snapshot.bootstrap.me.id
        : null;
      var result = await mutate("team_relay_remove_avatar");
      forgetAvatarSources(principalId);
      return result;
    }

    async function mutate(command, args) {
      patch({ mutating: true, error: null });
      try {
        var result = await invoke(command, args);
        if (result && result.bootstrap && result.attachments) patch({ snapshot: result });
        else load().catch(function () {});
        return result;
      } catch (error) {
        patch({ error: String(error) });
        if (String(error).indexOf("revision_conflict:") === 0) load().catch(function () {});
        throw error;
      } finally {
        patch({ mutating: false });
      }
    }

    async function query(command, args) {
      patch({ error: null });
      try {
        return await invoke(command, args);
      } catch (error) {
        patch({ error: String(error) });
        throw error;
      }
    }

    function createInvite(expiresInHours) {
      return mutate("team_relay_create_invite", { expiresInHours: expiresInHours || 24 });
    }

    async function createTeam(name) {
      clearAvatarSources();
      var snapshot = await mutate("team_relay_create_team", { name: name });
      if (snapshot && snapshot.bootstrap) patch({ status: Object.assign({}, state.teamRelay.status, { team_id: snapshot.bootstrap.team.id }), account: { principal: snapshot.bootstrap.me, teams: snapshot.teams || [] } });
      return snapshot;
    }

    async function joinTeam(joinCode) {
      clearAvatarSources();
      var snapshot = await mutate("team_relay_join_team", { joinCode: joinCode });
      if (snapshot && snapshot.bootstrap) patch({ status: Object.assign({}, state.teamRelay.status, { team_id: snapshot.bootstrap.team.id }), account: { principal: snapshot.bootstrap.me, teams: snapshot.teams || [] } });
      return snapshot;
    }

    async function switchTeam(teamId) {
      clearAvatarSources();
      var snapshot = await mutate("team_relay_switch_team", { teamId: teamId });
      if (snapshot && snapshot.bootstrap) {
        patch({ status: Object.assign({}, state.teamRelay.status, { team_id: snapshot.bootstrap.team.id }) });
      }
      return snapshot;
    }

    function removeMember(principalId) {
      return mutate("team_relay_remove_member", { principalId: principalId });
    }

    function rotateJoinPassword() {
      return mutate("team_relay_rotate_join_password");
    }

    function getJoinPassword() {
      if (joinPasswordReadPromise) return joinPasswordReadPromise;
      var request = query("team_relay_get_join_password");
      joinPasswordReadPromise = request;
      request.then(function () {
        if (joinPasswordReadPromise === request) joinPasswordReadPromise = null;
      }, function () {
        if (joinPasswordReadPromise === request) joinPasswordReadPromise = null;
      });
      return request;
    }

    function setJoinPasswordVisibility(visibility) {
      return mutate("team_relay_set_join_password_visibility", { visibility: visibility });
    }

    function disableJoinPassword() {
      return mutate("team_relay_disable_join_password");
    }

    function createThread(input) {
      var fingerprint = JSON.stringify(input);
      if (!pendingRequests.createThread || pendingRequests.createThread.fingerprint !== fingerprint) {
        pendingRequests.createThread = { fingerprint: fingerprint, id: uuid() };
      }
      var request = mutate("team_relay_create_thread", {
        input: Object.assign({}, input, { idempotency_key: pendingRequests.createThread.id }),
      });
      request.then(function () { delete pendingRequests.createThread; }, function () {});
      return request;
    }

    function createAction(threadId, input) {
      var fingerprint = threadId + ":" + JSON.stringify(input);
      if (!pendingRequests.createAction || pendingRequests.createAction.fingerprint !== fingerprint) {
        pendingRequests.createAction = { fingerprint: fingerprint, id: uuid() };
      }
      var request = mutate("team_relay_create_action", {
        threadId: threadId,
        input: Object.assign({}, input, { idempotency_key: pendingRequests.createAction.id }),
      });
      request.then(function () { delete pendingRequests.createAction; }, function () {});
      return request;
    }

    function setThreadPreference(threadId, input) {
      return mutate("team_relay_set_thread_preference", { threadId: threadId, input: input });
    }

    function patchThreadReadState(snapshot, threadId, readState) {
      if (!snapshot || !readState) return snapshot;
      function patchBootstrap(bootstrap) {
        if (!bootstrap || !Array.isArray(bootstrap.threads)) return bootstrap;
        var meId = bootstrap.me && bootstrap.me.id;
        var changed = false;
        var threads = bootstrap.threads.map(function (detail) {
          if (!detail || !detail.thread || detail.thread.id !== threadId) return detail;
          changed = true;
          var cursor = Math.max(
            Number(detail.read_state && detail.read_state.last_read_sequence || 0),
            Number(readState.last_read_sequence || 0)
          );
          var unreadCount = (detail.actions || []).filter(function (action) {
            return Number(action.sequence || 0) > cursor && action.actor_id !== meId;
          }).length;
          return Object.assign({}, detail, {
            read_state: Object.assign({}, readState, { last_read_sequence: cursor }),
            unread_count: unreadCount,
          });
        });
        return changed ? Object.assign({}, bootstrap, { threads: threads }) : bootstrap;
      }
      return Object.assign({}, snapshot, {
        bootstrap: patchBootstrap(snapshot.bootstrap),
        team_bootstraps: (snapshot.team_bootstraps || []).map(patchBootstrap),
      });
    }

    async function markThreadRead(threadId, readThroughSequence) {
      patch({ error: null });
      var localReadState = {
        thread_id: threadId,
        last_read_sequence: setLegacyReadCursor(threadId, readThroughSequence),
        last_read_at: new Date().toISOString(),
      };
      patch({ snapshot: patchThreadReadState(state.teamRelay.snapshot, threadId, localReadState) });
      try {
        var readState = await invoke("team_relay_mark_thread_read", {
          threadId: threadId,
          input: { read_through_sequence: readThroughSequence },
        });
        patch({ snapshot: patchThreadReadState(state.teamRelay.snapshot, threadId, readState) });
        return readState;
      } catch (error) {
        if (String(error).indexOf("resource_not_found:") !== -1) return localReadState;
        patch({ error: String(error) });
        throw error;
      }
    }

    async function pickDraftAttachments() {
      if (!dialogOpen) throw new Error("File picker is unavailable");
      var selected = await dialogOpen({
        multiple: true,
        filters: [{ name: "Team Relay", extensions: MATERIAL_EXTENSIONS }],
      });
      var paths = !selected ? [] : (Array.isArray(selected) ? selected : [selected]);
      return paths.map(function (path) {
        var localPath = String(path || "");
        var name = pathName(localPath);
        return {
          id: "manual:" + uuid(),
          name: name,
          type: materialType(name),
          size: 0,
          source: "manual",
          status: "selected",
          staged: false,
          localPath: localPath,
          uploadId: uuid(),
        };
      }).filter(function (material) { return !!material.localPath && !!material.name; });
    }

    async function downloadAttachment(attachment) {
      if (!dialogSave) throw new Error("Save dialog is unavailable");
      var destination = await dialogSave({ defaultPath: attachment.original_name });
      if (!destination) return false;
      await invoke("team_relay_download_attachment", { attachmentId: attachment.id, destination: destination });
      return true;
    }

    function rememberTaskWorkspace(workspace) {
      if (!workspace || !workspace.thread_id) return workspace;
      var next = Object.assign({}, state.teamRelay.taskWorkspaces || {});
      next[workspace.thread_id] = workspace;
      patch({ taskWorkspaces: next });
      return workspace;
    }

    async function taskWorkspaceStatus(threadId, teamId) {
      var workspace = await invoke("team_relay_task_workspace_status", { threadId: threadId, teamId: teamId || null });
      return rememberTaskWorkspace(workspace);
    }

    async function prepareTaskWorkspace(threadId) {
      patch({ preparingWorkspace: threadId, workspaceError: null });
      try {
        var workspace = await invoke("team_relay_prepare_task_workspace", { threadId: threadId });
        return rememberTaskWorkspace(workspace);
      } catch (error) {
        patch({ workspaceError: String(error) });
        throw error;
      } finally {
        patch({ preparingWorkspace: null });
      }
    }

    async function setTaskAgentSession(threadId, input, teamId) {
      var workspace = await invoke("team_relay_set_task_agent_session", {
        threadId: threadId,
        input: input,
        teamId: teamId || null,
      });
      return rememberTaskWorkspace(workspace);
    }

    async function pickAgentWorkspace() {
      if (!dialogOpen) throw new Error("Folder picker is unavailable");
      var selected = await dialogOpen({ multiple: false, directory: true });
      return Array.isArray(selected) ? selected[0] || null : selected || null;
    }

    function openPreparedMaterial(path) {
      var ext = (String(path || "").split(".").pop() || "").toLowerCase();
      var command = ext === "html" || ext === "htm" ? "open_artifact_window" : "open_in_system";
      return invoke(command, { path: path, sessionId: null });
    }

    function revealPreparedMaterial(threadId, attachmentId, teamId) {
      return invoke("team_relay_reveal_prepared_material", {
        threadId: threadId,
        attachmentId: attachmentId,
        teamId: teamId || null,
      });
    }

    async function disconnect() {
      patch({ mutating: true, error: null });
      try {
        await invoke("team_relay_disconnect");
        clearAvatarSources();
        connectionGeneration += 1;
        refreshPromise = null;
        if (refreshTimer) root.clearTimeout(refreshTimer);
        refreshTimer = null;
        joinPasswordReadPromise = null;
        pendingRequests = {};
        patch({
          status: { configured: false, hub_url: null, team_id: null },
          snapshot: null,
          account: null,
          taskWorkspaces: {},
          uploadProgress: null,
          preparingWorkspace: null,
          workspaceError: null,
          error: null,
        });
      } catch (error) {
        patch({ error: String(error) });
        throw error;
      } finally {
        patch({ mutating: false });
      }
    }

    function pathName(path) {
      return String(path || "").replace(/\\/g, "/").split("/").filter(Boolean).pop() || "";
    }

    var MATERIAL_EXTENSIONS = [
      "md", "txt", "csv", "pdf", "docx", "xlsx", "pptx", "png", "jpg", "jpeg", "webp", "zip",
      "html", "htm", "css", "js", "mjs", "cjs", "jsx", "ts", "tsx", "json", "yaml", "yml",
    ];

    function materialType(name) {
      var dot = String(name || "").lastIndexOf(".");
      return dot > 0 ? name.slice(dot + 1).toLowerCase() : "";
    }

    function joinWorkspacePath(rootPath, relativePath) {
      var rootText = String(rootPath || "").replace(/[\\/]+$/, "");
      var separator = rootText.indexOf("\\") >= 0 ? "\\" : "/";
      return rootText + separator + String(relativePath || "").replace(/[\\/]+/g, separator);
    }

    function artifactMaterialCandidates(artifacts) {
      return (artifacts || []).slice(0, 50).map(function (artifact, index) {
        var path = typeof artifact === "string" ? artifact : artifact && (artifact.storage_path || artifact.path) || "";
        var name = pathName(path);
        var type = materialType(name);
        if (!path || MATERIAL_EXTENSIONS.indexOf(type) < 0) return null;
        return {
          id: "artifact:" + index,
          name: name,
          type: type,
          size: Number(artifact && artifact.byte_size || 0),
          source: "session_artifact",
          status: "produced",
          staged: false,
          localPath: path,
          uploadId: uuid(),
        };
      }).filter(Boolean);
    }

    function sessionWorkspaceMaterialCandidates(paths) {
      return (paths || []).slice(0, 50).map(function (path, index) {
        var localPath = String(path || "");
        var name = pathName(localPath);
        var type = materialType(name);
        if (!localPath || MATERIAL_EXTENSIONS.indexOf(type) < 0) return null;
        return {
          id: "session_workspace:" + index,
          name: name,
          type: type,
          size: 0,
          source: "session_workspace",
          status: "produced",
          staged: false,
          localPath: localPath,
          uploadId: uuid(),
        };
      }).filter(Boolean);
    }

    function artifactPath(artifact) {
      return typeof artifact === "string" ? artifact : artifact && (artifact.storage_path || artifact.path) || "";
    }

    function workspaceMaterialCandidates(value, rootPath) {
      if (!value || !Array.isArray(value.changes) || !rootPath) return [];
      return value.changes.slice(0, 50).map(function (change, index) {
        var relativePath = String(change && change.relativePath || "");
        var name = pathName(relativePath);
        var type = materialType(name);
        var status = String(change && change.status || "unknown");
        if (!relativePath || status === "deleted" || MATERIAL_EXTENSIONS.indexOf(type) < 0) return null;
        return {
          id: "workspace:" + index,
          name: name,
          relativePath: relativePath,
          type: type,
          size: 0,
          source: "workspace_change",
          status: status,
          staged: change && change.staged === true,
          origin: String(change && change.origin || "unknown"),
          localPath: joinWorkspacePath(rootPath, relativePath),
          uploadId: uuid(),
        };
      }).filter(Boolean);
    }

    function artifactMetadata(artifacts) {
      return (artifacts || []).slice(0, 50).map(function (artifact) {
        var path = typeof artifact === "string" ? artifact : artifact && (artifact.storage_path || artifact.path) || "";
        var name = pathName(path);
        var dot = name.lastIndexOf(".");
        return {
          name: name,
          type: dot > 0 ? name.slice(dot + 1).toLowerCase() : "file",
          size: Number(artifact && artifact.byte_size || 0),
        };
      }).filter(function (artifact) { return !!artifact.name; });
    }

    function workspaceMetadata(value) {
      if (!value || !Array.isArray(value.changes)) return null;
      return {
        git: value.git === true,
        branch: String(value.branch || ""),
        repositoryUrl: String(value.repositoryUrl || ""),
        commitSha: String(value.commitSha || ""),
        baselineAvailable: value.baselineAvailable === true,
        changes: value.changes.slice(0, 50).map(function (change) {
          return {
            relativePath: String(change && change.relativePath || ""),
            status: String(change && change.status || "unknown"),
            staged: change && change.staged === true,
            origin: String(change && change.origin || "unknown"),
          };
        }).filter(function (change) { return !!change.relativePath; }),
      };
    }

    async function listDraftSourceSessions() {
      if (!refreshHistoryList) throw new Error("team_relay_sessions_unavailable");
      await refreshHistoryList();
      var codingSessions = [];
      try { codingSessions = await invoke("list_codex_acp_sessions") || []; } catch (_) {}
      var sessions = (state.sessions || []).map(function (session) {
        return {
          id: session.id,
          kind: "chat",
          title: String(session.title || "Untitled chat"),
          updated_at: session.updated_at || "",
        };
      }).concat(codingSessions.map(function (session) {
        return {
          id: session.id,
          kind: "coding",
          title: String(session.title || session.agent_name || "Coding session"),
          updated_at: session.updated_at || "",
          agent_name: String(session.agent_name || ""),
          workspace_available: session.workspace_available === true,
          workspace_kind: String(session.workspace_kind || ""),
          workspace_path: String(session.workspace_path || ""),
        };
      }));
      return sessions.filter(function (session) { return !!session.id; }).sort(function (left, right) {
        return String(right.updated_at).localeCompare(String(left.updated_at));
      });
    }

    async function readDraftSourceSession(source) {
      var id = String(source && source.id || "").trim();
      var kind = String(source && source.kind || "").trim();
      if (!id || (kind !== "chat" && kind !== "coding")) throw new Error("team_relay_session_invalid");
      var artifacts = [];
      var workspace = null;
      var materials = [];
      if (kind === "coding") {
        if (source.workspace_available !== false) {
          try {
            var workspaceChanges = await invoke("get_codex_workspace_changes", { sessionId: id });
            workspace = workspaceMetadata(workspaceChanges);
            materials = workspaceMaterialCandidates(workspaceChanges, source.workspace_path);
          }
          catch (_) { workspace = null; }
        }
      } else {
        var saved = await invoke("load_session", { id: id, setActive: false });
        var savedArtifacts = saved && saved.artifacts || [];
        var savedPaths = new Set(savedArtifacts.map(artifactPath).filter(Boolean));
        var workspaceFiles = [];
        try { workspaceFiles = await invoke("list_workspace_files", { sessionId: id }) || []; }
        catch (_) {}
        var workspaceOnly = workspaceFiles.filter(function (path) { return !savedPaths.has(String(path || "")); });
        artifacts = artifactMetadata(savedArtifacts.concat(workspaceOnly));
        materials = artifactMaterialCandidates(savedArtifacts)
          .concat(sessionWorkspaceMaterialCandidates(workspaceOnly))
          .slice(0, 50);
      }
      return {
        id: id,
        kind: kind,
        title: String(source.title || ""),
        artifacts: artifacts,
        workspace: workspace,
        materials: materials,
      };
    }

    async function mountDraftMaterials(materials, teamId) {
      var selected = Array.isArray(materials) ? materials : [];
      teamId = teamId || state.teamRelay?.snapshot?.bootstrap?.team?.id || state.teamRelay?.status?.team_id;
      if (!teamId) throw new Error("team_relay_team_required");
      var uploaded = [];
      for (var index = 0; index < selected.length; index++) {
        var material = selected[index] || {};
        if (!material.localPath || !material.uploadId) throw new Error("team_relay_material_invalid");
        uploaded.push(await invoke("team_relay_upload_attachment", {
          path: material.localPath,
          uploadId: material.uploadId,
          teamId: teamId,
        }));
      }
      return uploaded;
    }

    listen("team_relay:changed", function () {
      if (refreshTimer) root.clearTimeout(refreshTimer);
      refreshTimer = root.setTimeout(function () { load().catch(function () {}); }, 150);
    });
    listen("team_relay:upload-progress", function (event) {
      patch({ uploadProgress: event && event.payload ? event.payload : null });
    });

    return {
      load: load,
      refresh: load,
      verifyAccess: verifyAccess,
      registerAccount: registerAccount,
      claim: claim,
      createFirstTeam: createFirstTeam,
      updateProfile: updateProfile,
      avatarSource: avatarSource,
      uploadAvatar: uploadAvatar,
      removeAvatar: removeAvatar,
      createTeam: createTeam,
      joinTeam: joinTeam,
      switchTeam: switchTeam,
      removeMember: removeMember,
      createInvite: createInvite,
      getJoinPassword: getJoinPassword,
      rotateJoinPassword: rotateJoinPassword,
      setJoinPasswordVisibility: setJoinPasswordVisibility,
      disableJoinPassword: disableJoinPassword,
      createThread: createThread,
      createAction: createAction,
      setThreadPreference: setThreadPreference,
      markThreadRead: markThreadRead,
      listDraftSourceSessions: listDraftSourceSessions,
      readDraftSourceSession: readDraftSourceSession,
      mountDraftMaterials: mountDraftMaterials,
      pickDraftAttachments: pickDraftAttachments,
      downloadAttachment: downloadAttachment,
      taskWorkspaceStatus: taskWorkspaceStatus,
      prepareTaskWorkspace: prepareTaskWorkspace,
      setTaskAgentSession: setTaskAgentSession,
      pickAgentWorkspace: pickAgentWorkspace,
      openPreparedMaterial: openPreparedMaterial,
      revealPreparedMaterial: revealPreparedMaterial,
      disconnect: disconnect,
    };
  };
})(window);
