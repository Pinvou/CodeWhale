import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { openControlledDeviceChatWithRetry } from './remote-control-logic.mjs';
import ControlledOpenAlerts from './ControlledOpenAlerts.jsx';
import { retryControlledChatOpen as retryChat, retryControlledSessionOpen as retrySession } from './controlled-open-recovery.mjs';

const noNavigationSubscription = () => () => {};
const initialNavigationEpoch = () => 0;

export default function ControlledOpenRecovery({ bs, bsRef, bridge, currentView, currentViewRef, controlledDeviceStatus, controlledDeviceGeneration, controlledTaskSessionId, localNavigationEpoch, captureLocalNavigation, navigateFromScheduledRun, runBrowserUiTransition, setCodeModeOn, setCurrentView, setActiveChat, setRemoteConversationOpen, closeMobileSidebar, copy }) {
      const bridgeNavigationEpoch = useSyncExternalStore(
        bridge.sessions?.onLocalNavigation || noNavigationSubscription,
        bridge.sessions?.getLocalNavigationEpoch || initialNavigationEpoch,
        initialNavigationEpoch,
      );
      const controlledDeviceAutoOpenRef = useRef(null);
      const controlledDeviceAutoOpenInFlightRef = useRef(null);
      const controlledSessionAutoOpenRef = useRef(null);
      const controlledSessionAutoOpenInFlightRef = useRef(null);
      const controlledSessionAttemptedRef = useRef(null);
      const controlledChatAttemptsRef = useRef({ generation: null, used: 0, navigationEpoch: null });
      const [controlledChatRevision, setControlledChatRevision] = useState(0);
      const [controlledSessionRevision, setControlledSessionRevision] = useState(0);
      const [controlledChatOpenFailure, setControlledChatOpenFailure] = useState(null);
      const [controlledSessionOpenFailure, setControlledSessionOpenFailure] = useState(null);
      const [controlledRecoveryBusy, setControlledRecoveryBusy] = useState(false);
      useEffect(() => {
        setControlledChatOpenFailure(null);
        setControlledSessionOpenFailure(null);
      }, [controlledDeviceGeneration, controlledDeviceStatus]);
      useEffect(() => {
        setControlledSessionOpenFailure(null);
      }, [controlledTaskSessionId]);
      // B 端建立连接后进入普通会话页，便于立即看到远程请求及执行过程。
      // 以 remote-control generation 去重：liveness 更新和链路恢复不会反复抢走用户当前页面。
      useEffect(() => {
        if (controlledDeviceStatus !== 'connected' || bs?.controlledNavigationPending) {
          return;
        }
        if (!controlledDeviceGeneration) return;
        const connectionKey = controlledDeviceGeneration;
        if (
          controlledDeviceAutoOpenRef.current === connectionKey
          || controlledDeviceAutoOpenInFlightRef.current?.key === connectionKey
        ) return;
        const originView = currentViewRef.current;
        let cancelled = false;
        const remoteOwner = bridge.sessions?.captureControlledNavigationOwner?.(connectionKey);
        if (!remoteOwner) return undefined;
        const navigation = captureLocalNavigation();
        const owner = { ...remoteOwner, bridgeNavigationEpoch: remoteOwner.bridgeNavigationEpoch || 0, navigationEpoch: navigation.epoch, navigationIsCurrent: navigation.isCurrent };
        const isCurrentOwner = () => {
          if (cancelled) return false;
          const latest = bsRef.current && bsRef.current.controlledDevice;
          const viewIsCurrent = currentViewRef.current === originView || currentViewRef.current === 'chat';
          return !!(latest && latest.status === 'connected' && latest.generation === connectionKey && viewIsCurrent
            && owner.navigationIsCurrent() && bridge.sessions?.isControlledNavigationOwnerCurrent?.(owner));
        };
        const operation = { key: connectionKey };
        if (controlledChatAttemptsRef.current.generation !== connectionKey) {
          controlledChatAttemptsRef.current = { generation: connectionKey, used: 0, navigationEpoch: navigation.epoch, bridgeNavigationEpoch: owner.bridgeNavigationEpoch };
        }
        if (controlledChatAttemptsRef.current.navigationEpoch !== navigation.epoch
          || controlledChatAttemptsRef.current.bridgeNavigationEpoch !== owner.bridgeNavigationEpoch) return undefined;
        const remaining = 3 - controlledChatAttemptsRef.current.used;
        if (remaining <= 0) {
          if (isCurrentOwner()) setControlledChatOpenFailure({ generation: connectionKey, originView, owner });
          return undefined;
        }
        controlledDeviceAutoOpenInFlightRef.current = operation;
        setRemoteConversationOpen(false);
        void openControlledDeviceChatWithRetry({
          navigate: () => {
            controlledChatAttemptsRef.current.used += 1;
            return navigateFromScheduledRun('chat', undefined, isCurrentOwner, owner);
          },
          isCurrent: isCurrentOwner,
          maxAttempts: remaining,
          onError: (error) => console.warn('[remote-control] B-side chat auto-open failed', error),
        }).then((opened) => {
          if (controlledDeviceAutoOpenInFlightRef.current === operation) {
            controlledDeviceAutoOpenInFlightRef.current = null;
          }
          if (!isCurrentOwner()) return;
          if (opened) {
            controlledDeviceAutoOpenRef.current = connectionKey;
            setControlledChatOpenFailure(null);
          } else setControlledChatOpenFailure({ generation: connectionKey, originView, owner });
        });
        return () => {
          cancelled = true;
          if (controlledDeviceAutoOpenInFlightRef.current === operation) {
            controlledDeviceAutoOpenInFlightRef.current = null;
          }
        };
      }, [controlledDeviceGeneration, controlledDeviceStatus, bs?.controlledNavigationIntent, bs?.controlledNavigationPending, controlledChatRevision, navigateFromScheduledRun]);

      // A remote session is provisioned only when B receives the first task.
      // Connection-time navigation therefore cannot select it yet; once the
      // admitted task publishes its authoritative session id, select that
      // session exactly once so filtering never hides the received command.
      useEffect(() => {
        if (controlledDeviceStatus !== 'connected' || bs?.controlledNavigationPending || !controlledDeviceGeneration || !controlledTaskSessionId) return undefined;
        const sessionKey = `${controlledDeviceGeneration}:${controlledTaskSessionId}`;
        if (
          controlledSessionAutoOpenRef.current === sessionKey
          || controlledSessionAutoOpenInFlightRef.current?.key === sessionKey
        ) return undefined;
        let cancelled = false;
        const remoteOwner = bridge.sessions?.captureControlledNavigationOwner?.(controlledDeviceGeneration);
        if (!remoteOwner) return undefined;
        const navigation = captureLocalNavigation();
        const owner = { ...remoteOwner, bridgeNavigationEpoch: remoteOwner.bridgeNavigationEpoch || 0, navigationEpoch: navigation.epoch, navigationIsCurrent: navigation.isCurrent };
        const isCurrentOwner = () => {
          const latest = bsRef.current;
          const tasks = Array.isArray(latest?.controlledTasks) ? latest.controlledTasks : [];
          const fallbackTask = [...tasks].reverse().find(task => task?.sessionId);
          const latestSessionId = latest?.controlledSession?.sessionId || fallbackTask?.sessionId;
          return !cancelled && latest?.controlledDevice?.status === 'connected'
            && latest?.controlledDevice?.generation === controlledDeviceGeneration
            && latestSessionId === controlledTaskSessionId
            && owner.navigationIsCurrent() && bridge.sessions?.isControlledNavigationOwnerCurrent?.(owner);
        };
        // Local navigation retires this attempt, even if the intent later refreshes.
        if (controlledSessionAttemptedRef.current?.key === sessionKey) {
          if (controlledSessionAttemptedRef.current.navigationEpoch !== navigation.epoch
            || controlledSessionAttemptedRef.current.bridgeNavigationEpoch !== owner.bridgeNavigationEpoch) return undefined;
          if (isCurrentOwner()) setControlledSessionOpenFailure({ generation: controlledDeviceGeneration, sessionId: controlledTaskSessionId, owner });
          return undefined;
        }
        controlledSessionAttemptedRef.current = { key: sessionKey, navigationEpoch: navigation.epoch, bridgeNavigationEpoch: owner.bridgeNavigationEpoch };
        const operation = { key: sessionKey };
        controlledSessionAutoOpenInFlightRef.current = operation;
        void runBrowserUiTransition(async ({ isCurrent }) => {
          const latest = bsRef.current;
          if (latest?.scheduledRunContext) {
            if (!bridge.available || !bridge.sessions?.exitControlledScheduledRunChat) return false;
            const exited = await bridge.sessions.exitControlledScheduledRunChat(owner);
            if (!exited || !isCurrent() || cancelled) return false;
          }
          if (!bridge.available || !bridge.sessions?.switchToControlledSession || !isCurrent() || !isCurrentOwner()) {
            return false;
          }
          setCodeModeOn(false);
          setCurrentView('chat');
          closeMobileSidebar();
          const switched = await bridge.sessions.switchToControlledSession(controlledTaskSessionId, owner);
          if (!switched || !isCurrent() || !isCurrentOwner()) return false;
          setActiveChat(controlledTaskSessionId);
          return true;
        }, {
          channel: 'session',
          hideMode: 'workspace',
          serialize: true,
          sessionTarget: controlledTaskSessionId,
          controlledNavigation: true,
        }).then((opened) => {
          if (controlledSessionAutoOpenInFlightRef.current === operation) {
            controlledSessionAutoOpenInFlightRef.current = null;
          }
          if (!isCurrentOwner()) return;
          if (opened) {
            controlledSessionAutoOpenRef.current = sessionKey;
            setControlledSessionOpenFailure(null);
          } else setControlledSessionOpenFailure({ generation: controlledDeviceGeneration, sessionId: controlledTaskSessionId, owner });
        }).catch((error) => {
          if (controlledSessionAutoOpenInFlightRef.current === operation) {
            controlledSessionAutoOpenInFlightRef.current = null;
          }
          console.warn('[remote-control] B-side session open failed', error);
          if (isCurrentOwner()) setControlledSessionOpenFailure({ generation: controlledDeviceGeneration, sessionId: controlledTaskSessionId, owner });
        });
        return () => {
          cancelled = true;
          if (controlledSessionAutoOpenInFlightRef.current === operation) {
            controlledSessionAutoOpenInFlightRef.current = null;
          }
        };
      }, [
        controlledDeviceGeneration,
        controlledDeviceStatus,
        bs?.controlledNavigationIntent,
        bs?.controlledNavigationPending,
        controlledSessionRevision,
        controlledTaskSessionId,
        closeMobileSidebar,
        runBrowserUiTransition,
        setCurrentView,
      ]);

      async function retryControlledChatOpen() {
        return retryChat({ failure: controlledChatOpenFailure, controlledDeviceAutoOpenInFlightRef, controlledRecoveryBusy, bsRef, currentViewRef, bridge, setControlledRecoveryBusy, navigateFromScheduledRun, controlledDeviceAutoOpenRef, setControlledChatOpenFailure, onReleased: () => setControlledChatRevision(value => value + 1) });
      }
      async function retryControlledSessionOpen() {
        return retrySession({ failure: controlledSessionOpenFailure, controlledSessionAutoOpenInFlightRef, controlledRecoveryBusy, bsRef, bridge, setControlledRecoveryBusy, runBrowserUiTransition, setCodeModeOn, setCurrentView, setActiveChat, closeMobileSidebar, controlledSessionAutoOpenRef, setControlledSessionOpenFailure, onReleased: () => setControlledSessionRevision(value => value + 1) });
      }

  if (!controlledChatOpenFailure && !controlledSessionOpenFailure) return null;
  return <ControlledOpenAlerts chat={controlledChatOpenFailure} session={controlledSessionOpenFailure}
    status={controlledDeviceStatus} generation={controlledDeviceGeneration} intent={bs?.controlledNavigationIntent}
    navigationEpoch={localNavigationEpoch} bridgeNavigationEpoch={bridgeNavigationEpoch}
    sessionId={controlledTaskSessionId} view={currentView} busy={controlledRecoveryBusy} copy={copy}
    onChat={retryControlledChatOpen} onSession={retryControlledSessionOpen} />;
}
