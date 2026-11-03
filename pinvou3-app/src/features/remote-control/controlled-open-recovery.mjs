export async function retryControlledChatOpen({ failure, controlledDeviceAutoOpenInFlightRef, controlledRecoveryBusy, bsRef, currentViewRef, bridge, setControlledRecoveryBusy, navigateFromScheduledRun, controlledDeviceAutoOpenRef, setControlledChatOpenFailure, onReleased = () => {} }) {
        if (!failure || controlledDeviceAutoOpenInFlightRef.current || controlledRecoveryBusy) return;
        const isCurrentOwner = () => {
          const device = bsRef.current?.controlledDevice;
          return device?.status === 'connected' && device.generation === failure.generation
            && (currentViewRef.current === failure.originView || currentViewRef.current === 'chat')
            && failure.owner.navigationIsCurrent?.() !== false
            && bridge.sessions?.isControlledNavigationOwnerCurrent?.(failure.owner);
        };
        if (!isCurrentOwner()) return;
        const operation = { key: failure.generation };
        controlledDeviceAutoOpenInFlightRef.current = operation;
        setControlledRecoveryBusy(true);
        try {
          const opened = await navigateFromScheduledRun('chat', undefined, isCurrentOwner, failure.owner);
          if (opened && isCurrentOwner()) {
            controlledDeviceAutoOpenRef.current = failure.generation;
            setControlledChatOpenFailure(null);
          }
        } catch (_) {
          // Keep the current, localized recovery action visible.
        } finally {
          if (controlledDeviceAutoOpenInFlightRef.current === operation) { controlledDeviceAutoOpenInFlightRef.current = null; onReleased(); }
          setControlledRecoveryBusy(false);
        }
      }

export async function retryControlledSessionOpen({ failure, controlledSessionAutoOpenInFlightRef, controlledRecoveryBusy, bsRef, bridge, setControlledRecoveryBusy, runBrowserUiTransition, setCodeModeOn, setCurrentView, setActiveChat, closeMobileSidebar, controlledSessionAutoOpenRef, setControlledSessionOpenFailure, onReleased = () => {} }) {
        if (!failure || controlledSessionAutoOpenInFlightRef.current || controlledRecoveryBusy) return;
        const sessionKey = `${failure.generation}:${failure.sessionId}`;
        const isCurrentOwner = () => {
          const latest = bsRef.current;
          const tasks = Array.isArray(latest?.controlledTasks) ? latest.controlledTasks : [];
          const fallbackTask = [...tasks].reverse().find(task => task?.sessionId);
          return latest?.controlledDevice?.status === 'connected'
            && latest.controlledDevice.generation === failure.generation
            && (latest?.controlledSession?.sessionId || fallbackTask?.sessionId) === failure.sessionId
            && failure.owner.navigationIsCurrent?.() !== false
            && bridge.sessions?.isControlledNavigationOwnerCurrent?.(failure.owner);
        };
        if (!isCurrentOwner()) return;
        const operation = { key: sessionKey };
        controlledSessionAutoOpenInFlightRef.current = operation;
        setControlledRecoveryBusy(true);
        try {
          const opened = await runBrowserUiTransition(async ({ isCurrent }) => {
            const latest = bsRef.current;
            if (latest?.scheduledRunContext) {
              if (!bridge.available || !bridge.sessions?.exitControlledScheduledRunChat) return false;
              const exited = await bridge.sessions.exitControlledScheduledRunChat(failure.owner);
              if (!exited || !isCurrent() || !isCurrentOwner()) return false;
            }
            if (!isCurrent() || !isCurrentOwner() || !bridge.sessions?.switchToControlledSession) return false;
            const switched = await bridge.sessions.switchToControlledSession(failure.sessionId, failure.owner);
            if (!switched || !isCurrent() || !isCurrentOwner()) return false;
            setCodeModeOn(false);
            setCurrentView('chat');
            setActiveChat(failure.sessionId);
            closeMobileSidebar();
            return true;
          }, { channel: 'session', hideMode: 'workspace', serialize: true, sessionTarget: failure.sessionId, controlledNavigation: true });
          if (opened && isCurrentOwner()) {
            controlledSessionAutoOpenRef.current = sessionKey;
            setControlledSessionOpenFailure(null);
          }
        } catch (_) {
          // Keep the current, localized recovery action visible.
        } finally {
          if (controlledSessionAutoOpenInFlightRef.current === operation) { controlledSessionAutoOpenInFlightRef.current = null; onReleased(); }
          setControlledRecoveryBusy(false);
        }
      }
